package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/internal/oauthtest"
	"github.com/apron-chat/apron-server-go/internal/wstest"
)

// gitHub is a fake GitHub, which signs in octocat, a member of acme,
// whatever is asked, unless the person declines.
type gitHub struct {
	*httptest.Server
	decline bool
}

func fakeGitHub(t *testing.T) *gitHub {
	mux := http.NewServeMux()
	g := &gitHub{}
	mux.HandleFunc("GET /login/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		if g.decline {
			http.Redirect(w, r, r.URL.Query().Get("redirect_uri")+"?error=access_denied&state="+r.URL.Query().Get("state"), http.StatusFound)
			return
		}
		http.Redirect(w, r, r.URL.Query().Get("redirect_uri")+"?code=c0de&state="+r.URL.Query().Get("state"), http.StatusFound)
	})
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("code") != "c0de" || r.FormValue("client_secret") != "secret" {
			http.Error(w, "bad code", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "t0ken"})
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"id": 583231, "login": "octocat", "name": "The Octocat"})
	})
	mux.HandleFunc("GET /user/memberships/orgs/acme", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"state": "active"})
	})
	g.Server = httptest.NewServer(mux)
	t.Cleanup(g.Close)
	return g
}

func TestSignInWithGitHub(t *testing.T) {
	github := fakeGitHub(t)
	s := &SSO{}
	s.Settings.ClientID, s.Settings.ClientSecret, s.Settings.Org = "id", "secret", "acme"
	s.Settings.Admins = []string{"OctoCat"}
	s.github.web, s.github.api = github.URL, github.URL
	config := apron.DefaultConfig()
	config.DisableGuests = true
	// The OAuth callback is on this server, whose URL is known once it
	// listens, before anyone signs in.
	s.Settings.BaseURL = "http://placeholder"
	_, server := wstest.Serve(t, config, s)
	base := server.URL
	s.Settings.BaseURL = base

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	call := func(params map[string]any) map[string]any {
		payload, _ := json.Marshal(map[string]any{"method": "auth", "id": "a", "params": params})
		if err := ws.Write(ctx, websocket.MessageText, payload); err != nil {
			t.Fatal(err)
		}
		for {
			_, payload, err := ws.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var frame map[string]any
			json.Unmarshal(payload, &frame)
			if frame["id"] == "a" {
				result, ok := frame["result"].(map[string]any)
				if !ok {
					t.Fatalf("auth failed: %v", frame)
				}
				return result
			}
		}
	}
	external := call(map[string]any{"scheme": "external"})["external"].(map[string]any)
	if external["url"] != base+"/oauth/authorize" {
		t.Fatalf("external = %#v", external)
	}

	// An app on this device opens the page, which says who is asking; the
	// person continues to GitHub and back, then allows the app to make an
	// account.
	app := oauthtest.New(t, base)
	page := app.Get(app.URL(external["url"].(string), nil))
	if page.Status != http.StatusOK || !strings.Contains(page.Body, "<strong>An app on this device (com.example.app)</strong> wants to sign in") {
		t.Fatalf("page = %d %s", page.Status, page.Body)
	}
	page = app.Press(page, "continue")
	if !strings.Contains(page.Body, "wants to create an account for you") {
		t.Fatalf("after GitHub = %d %s", page.Status, page.Body)
	}
	code := app.Code(app.Press(page, "allow"), base)
	params := app.Redemption(code)
	params["scheme"] = "external"
	result := call(params)
	you := result["you"].(map[string]any)
	if you["user_id"] != "octocat" || you["name"] != "The Octocat" || result["token"] == nil || !strings.Contains(strings.Join(toStrings(you["roles"]), ","), "admin") {
		t.Fatalf("you = %#v", you)
	}

	// The next sign-in shows the account.
	again := oauthtest.New(t, base)
	page = again.Press(again.Get(again.URL(external["url"].(string), nil)), "continue")
	if !strings.Contains(page.Body, "to use <strong>The Octocat (@octocat)</strong>’s account?") {
		t.Fatalf("second sign-in = %d %s", page.Status, page.Body)
	}

	// Declining at GitHub returns to the app with access_denied.
	declined := oauthtest.New(t, base)
	github.decline = true
	page = declined.Press(declined.Get(declined.URL(external["url"].(string), nil)), "continue")
	if page.Return == nil || page.Return.Query().Get("error") != "access_denied" || page.Return.Query().Get("state") != declined.State {
		t.Fatalf("declined = %d %v %s", page.Status, page.Return, page.Body)
	}
}

func toStrings(value any) []string {
	var out []string
	for _, item := range value.([]any) {
		out = append(out, item.(string))
	}
	return out
}
