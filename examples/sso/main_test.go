package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/apron-chat/apron-server-go/aprond"
	"github.com/apron-chat/apron-server-go/aprond/aprondtest"
)

// gitHub is a fake GitHub, which signs in octocat, a member of acme
// unless left, whatever is asked, unless the person declines. It redeems a
// code only with the callback URL it was asked for with.
type gitHub struct {
	*httptest.Server
	decline, left bool
	callback      string
}

func fakeGitHub(t *testing.T) *gitHub {
	mux := http.NewServeMux()
	g := &gitHub{}
	mux.HandleFunc("GET /login/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		g.callback = r.URL.Query().Get("redirect_uri")
		if g.decline {
			http.Redirect(w, r, r.URL.Query().Get("redirect_uri")+"?error=access_denied&state="+r.URL.Query().Get("state"), http.StatusFound)
			return
		}
		http.Redirect(w, r, r.URL.Query().Get("redirect_uri")+"?code=c0de&state="+r.URL.Query().Get("state"), http.StatusFound)
	})
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("code") != "c0de" || r.FormValue("client_secret") != "secret" || r.FormValue("redirect_uri") != g.callback {
			http.Error(w, "bad code", http.StatusBadRequest)
			return
		}
		json.MarshalWrite(w, map[string]any{"access_token": "t0ken"})
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		json.MarshalWrite(w, map[string]any{"id": 583231, "login": "octocat", "name": "The Octocat"})
	})
	mux.HandleFunc("GET /user/memberships/orgs/acme", func(w http.ResponseWriter, r *http.Request) {
		if g.left {
			http.NotFound(w, r)
			return
		}
		json.MarshalWrite(w, map[string]any{"state": "active"})
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
	config := aprond.DefaultConfig()
	config.DisableGuests = true
	server := aprondtest.Serve(t, config, s)

	// An app on this device opens the page, which sends the person to
	// GitHub and back, then says who is asking, and the person allows it.
	app := server.App()
	if app.URL != server.URL+"/oauth/authorize" {
		t.Fatalf("sign-in page = %s", app.URL)
	}
	page := app.Open()
	if page.Status != http.StatusOK || !strings.Contains(page.Body, "<strong>An app on this device (com.example.app)</strong> wants to sign in to") {
		t.Fatalf("after GitHub = %d %s", page.Status, page.Body)
	}
	result, err := app.Redeem(app.Press(page, "allow"))
	if err != nil || result.You == nil {
		t.Fatalf("redeeming the code: %v", err)
	}
	if you := result.You; you.UserID != "octocat" || you.Name != "The Octocat" || result.Token == "" || !slices.Contains(you.Roles, aprond.RoleAdmin) {
		t.Fatalf("you = %#v", you)
	}

	// The next sign-in shows the account.
	page = server.App().Open()
	if !strings.Contains(page.Body, "to use <strong>The Octocat (@octocat)</strong>’s account?") {
		t.Fatalf("second sign-in = %d %s", page.Status, page.Body)
	}

	// Declining at GitHub returns to the app with access_denied.
	declined := server.App()
	github.decline = true
	page = declined.Open()
	if page.Return == nil || page.Return.Query().Get("error") != "access_denied" || page.Return.Query().Get("state") != declined.State {
		t.Fatalf("declined = %d %v %s", page.Status, page.Return, page.Body)
	}

	// Once octocat left acme, the page refuses them and says why; back in
	// the app, which hears why too.
	github.decline, github.left = false, true
	outsider := server.App()
	page = outsider.Open()
	if page.Status != http.StatusForbidden || !strings.Contains(page.Body, "octocat is not a member of acme") {
		t.Fatalf("outsider = %d %s", page.Status, page.Body)
	}
	back := outsider.Press(page, "cancel")
	if back.Return == nil || back.Return.Query().Get("error") != "access_denied" || back.Return.Query().Get("error_description") != "octocat is not a member of acme" {
		t.Fatalf("outsider's cancel = %d %v", back.Status, back.Return)
	}
}
