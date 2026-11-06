package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/internal/wstest"
)

// fakeGitHub signs in octocat, a member of acme, whatever is asked.
func fakeGitHub(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
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
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
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
	page := call(map[string]any{"scheme": "external"})["url"].(string)

	// The browser follows the page to GitHub and back, then shows a code.
	jar, _ := cookiejar.New(nil)
	response, err := (&http.Client{Jar: jar}).Get(page)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	code := regexp.MustCompile(`class="code">(\d{6})<`).FindStringSubmatch(string(body))
	if code == nil {
		t.Fatalf("page = %d %s", response.StatusCode, body)
	}
	you := call(map[string]any{"scheme": "external", "token": code[1]})["you"].(map[string]any)
	if you["user_id"] != "octocat" || you["name"] != "The Octocat" || !strings.Contains(strings.Join(toStrings(you["roles"]), ","), "admin") {
		t.Fatalf("you = %#v", you)
	}
}

func toStrings(value any) []string {
	var out []string
	for _, item := range value.([]any) {
		out = append(out, item.(string))
	}
	return out
}
