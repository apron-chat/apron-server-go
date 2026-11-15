package apron

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/apron-chat/apron-server-go/internal/authn"
	"github.com/apron-chat/apron-server-go/internal/oauthtest"
	"github.com/apron-chat/apron-server-go/store"
)

// externalTestServer offers external sign-in through an extension whose
// "identity provider" signs in whoever identity names, at once.
func externalTestServer(t *testing.T, identity *Identity) (*Server, *httptest.Server) {
	t.Helper()
	return newTestServer(t, extensionConfig(ssoExtension(identity)))
}

// ssoExtension is the extension of externalTestServer.
func ssoExtension(identity *Identity) testExtension {
	return testExtension{"sso", func(r *Registrar) error {
		signIn := r.ExternalSignIn(func(w http.ResponseWriter, req *http.Request, p ExternalRequest) {
			http.Redirect(w, req, "/sso/callback?state="+url.QueryEscape(p.ID), http.StatusFound)
		})
		r.Handle("/sso/callback", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if identity.Subject == "" {
				_ = signIn.Cancel(w, req, req.URL.Query().Get("state"))
				return
			}
			_ = signIn.Complete(w, req, req.URL.Query().Get("state"), *identity)
		}))
		return nil
	}}
}

// tokenFromEndpoint redeems a code at the token endpoint, with origin as
// the request's Origin header unless empty, and returns the access token.
func tokenFromEndpoint(t *testing.T, app *oauthtest.Client, endpoint, code, origin string) string {
	t.Helper()
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {app.Verifier}, "redirect_uri": {app.RedirectURI}, "client_id": {app.ClientID}}
	request, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var token map[string]any
	if err := json.UnmarshalRead(response.Body, &token); err != nil || response.StatusCode != http.StatusOK || token["access_token"] == nil {
		t.Fatalf("token = %d %v %#v", response.StatusCode, err, token)
	}
	return token["access_token"].(string)
}

// resumes reports whether token resumes a sign-in on a new connection
// from origin, none for empty.
func resumes(t *testing.T, httpServer *httptest.Server, token any, origin string) bool {
	t.Helper()
	c, _ := dialOrigin(t, httpServer, origin)
	c.write(t, map[string]any{"method": "auth", "id": "resume", "params": map[string]any{"scheme": "token", "token": token}})
	for {
		if frame := c.read(t); frame["id"] == "resume" {
			return frame["error"] == nil
		}
	}
}

// A token from the token endpoint resumes from any origin, or none,
// whatever the Origin of its request, and does so after a restart too
// (§4.12.4).
func TestExternalEndpointTokenSurvivesRestart(t *testing.T) {
	identity := &Identity{Subject: "idp:6", Name: "Ida"}
	memory, dir := store.NewMemory(), t.TempDir()
	configure := func(c *Config) {
		c.Extensions = []Extension{ssoExtension(identity)}
		c.AllowAnyOrigin = true
	}
	_, httpServer, stop := startWith(t, memory, dir, configure)
	app := oauthtest.New(t, httpServer.URL)
	code := externalCode(t, app, httpServer.URL+"/oauth/authorize", httpServer.URL)
	token := tokenFromEndpoint(t, app, httpServer.URL+"/oauth/token", code, "https://app.example")
	stop()

	_, httpServer, _ = startWith(t, memory, dir, configure)
	for _, origin := range []string{"", "https://app.example", otherOrigin} {
		if !resumes(t, httpServer, token, origin) {
			t.Errorf("after a restart, the token did not resume from %q", origin)
		}
	}
}

// externalCode signs in on the page as the app would, through the
// extension and Allow, unless the page remembered the app, and returns the
// code.
func externalCode(t *testing.T, app *oauthtest.Client, page, issuer string) string {
	t.Helper()
	screen := app.Get(app.URL(page, nil))
	if screen.Return != nil {
		return app.Code(screen, issuer)
	}
	if screen.Status != http.StatusOK {
		t.Fatalf("page = %d %s", screen.Status, screen.Body)
	}
	return app.Code(app.Press(screen, "allow"), issuer)
}

func redemption(app *oauthtest.Client, code string) map[string]any {
	params := app.Redemption(code)
	params["scheme"] = "external"
	return params
}

func TestExternalSignIn(t *testing.T) {
	identity := &Identity{Subject: "idp:1", Name: "Ada", UserID: "ada", Roles: []string{"Staff"}}
	_, httpServer := externalTestServer(t, identity)
	issuer := httpServer.URL

	c, frame := dialRaw(t, httpServer)
	if auth := asList(frame["params"].(map[string]any)["auth"]); !slices.Contains(auth, any("external")) || !slices.Contains(auth, any("token")) {
		t.Fatalf("auth = %#v", auth)
	}
	// A begin authenticates nothing, and names the page.
	external := c.result(t, "auth", "b", map[string]any{"scheme": "external"})["external"].(map[string]any)
	page := external["url"].(string)
	if page != issuer+"/oauth/authorize" || len(external) != 1 {
		t.Fatalf("external = %#v", external)
	}

	// The page goes straight to the extension's sign-in, then asks once,
	// saying who is asking, and refuses to be framed. For a new identity it
	// says what is true whether the code makes an account or is added to
	// one.
	app := oauthtest.New(t, issuer)
	screen := app.Get(app.URL(page, nil))
	for _, want := range []string{
		"<h1>Sign in to 127.0.0.1:",
		"<strong>An app on this device (com.example.app)</strong> wants to sign in to <strong>127.0.0.1:",
		"</strong> as you. It will be able to read and send messages as you.",
		"has not verified this app.",
		"Only continue if you just started signing in on an app on this device.",
		"Not you? Sign in as someone else",
		`value="allow" class="primary">Continue</button>`,
	} {
		if !strings.Contains(screen.Body, want) {
			t.Fatalf("new identity's screen lacks %q: %d %s", want, screen.Status, screen.Body)
		}
	}
	if csp := screen.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") || screen.Header.Get("X-Frame-Options") != "DENY" || screen.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("headers = %v", screen.Header)
	}
	code := app.Code(app.Press(screen, "allow"), issuer)

	// The code works once, with its verifier, redirect_uri, and client_id.
	signIn, _ := dialRaw(t, httpServer)
	wrong := redemption(app, code)
	wrong["code_verifier"] = strings.Repeat("x", 43)
	signIn.expectError(t, "auth", "a0", wrong, CodeDenied)
	signIn.expectError(t, "auth", "a1", redemption(app, code), CodeDenied)

	code = externalCode(t, app, page, issuer)
	for name, value := range map[string]string{"redirect_uri": "com.example.app:/other", "client_id": "com.example.other"} {
		params := redemption(app, code)
		params[name] = value
		signIn.expectError(t, "auth", "a-"+name, params, CodeDenied)
		code = externalCode(t, app, page, issuer)
	}
	for _, missing := range []string{"code_verifier", "redirect_uri", "client_id"} {
		params := redemption(app, code)
		delete(params, missing)
		signIn.expectError(t, "auth", "m-"+missing, params, CodeInvalidParams)
	}
	_, result := signIn.request(t, "auth", "a2", redemption(app, code))
	you := result["you"].(map[string]any)
	if you["user_id"] != "ada" || you["name"] != "Ada" || result["token"] == nil || !slices.Equal(asList(you["roles"]), []any{"staff"}) {
		t.Fatalf("sign-in = %#v", result)
	}
	// Its token resumes the account.
	resumed, _ := dialRaw(t, httpServer)
	if _, result := resumed.request(t, "auth", "r", map[string]any{"scheme": "token", "token": result["token"]}); result["you"].(map[string]any)["user_id"] != "ada" {
		t.Fatalf("resume = %#v", result)
	}

	// The next sign-in shows the account, and offers someone else.
	again := oauthtest.New(t, issuer)
	screen = again.Get(again.URL(page, nil))
	for _, want := range []string{
		"Allow <strong>an app on this device (com.example.app)</strong> to use <strong>Ada (@ada)</strong>’s account?",
		"has not verified this app.",
		"Only allow if you just started signing in on an app on this device.",
		"Not you? Sign in as someone else",
		`value="allow" class="primary">Allow</button>`,
	} {
		if !strings.Contains(screen.Body, want) {
			t.Fatalf("known account's screen lacks %q: %s", want, screen.Body)
		}
	}
	// "Not you?" starts the extension's sign-in again, which asks again.
	if restarted := again.Press(screen, "restart"); !strings.Contains(restarted.Body, "to use <strong>Ada (@ada)</strong>’s account?") || restarted.Field("flow") != screen.Field("flow") {
		t.Fatalf("restart = %d %s", restarted.Status, restarted.Body)
	}
	// Cancel returns access_denied, with state and iss.
	declined := again.Press(screen, "cancel")
	if declined.Return == nil || declined.Return.Query().Get("error") != "access_denied" || declined.Return.Query().Get("state") != again.State || declined.Return.Query().Get("iss") != issuer {
		t.Fatalf("cancel = %d %v", declined.Status, declined.Return)
	}
	// The flow is over.
	if expired := again.Press(screen, "allow"); expired.Status != http.StatusNotFound || expired.Return != nil {
		t.Fatalf("allow after cancel = %d %v", expired.Status, expired.Return)
	}

	// An external token is not this server's.
	signIn.drain(t)
	signIn.expectError(t, "auth", "t", map[string]any{"scheme": "external", "token": "anything"}, CodeDenied)
}

// On a signed-in connection, guests included, a code adds the identity to
// the account (§4.12.4), and never signs in to another.
func TestExternalAddsToSignedInAccount(t *testing.T) {
	identity := &Identity{Subject: "idp:2", Name: "Ev‮il\u0007" + strings.Repeat("x", 200)}
	app, httpServer := externalTestServer(t, identity)
	issuer := httpServer.URL
	page := issuer + "/oauth/authorize"

	guest, _ := dialRaw(t, httpServer)
	guest.request(t, "auth", "g", map[string]any{"scheme": "guest", "name": "Grace"})
	client := oauthtest.New(t, issuer)
	_, result := guest.request(t, "auth", "a", redemption(client, externalCode(t, client, page, issuer)))
	you := result["you"].(map[string]any)
	// A guest becoming an account keeps its name, and gets a token to come
	// back with.
	if you["name"] != "Grace" || result["token"] == nil {
		t.Fatalf("addition = %#v", result)
	}
	app.mu.RLock()
	added := app.accounts[authn.Identity{Scheme: "external", Subject: "idp:2"}]
	app.mu.RUnlock()
	if added == nil || added.name != "Grace" || added.id != you["user_id"] {
		t.Fatalf("the account the identity was added to = %#v", added)
	}

	// An identity that belongs to another account is denied, and so is a
	// second one of the scheme.
	other, _ := dialRaw(t, httpServer)
	other.request(t, "auth", "g", map[string]any{"scheme": "guest"})
	other.drain(t)
	other.expectError(t, "auth", "a", redemption(client, externalCode(t, client, page, issuer)), CodeDenied)
	identity.Subject = "idp:3"
	guest.drain(t)
	guest.expectError(t, "auth", "b", redemption(client, externalCode(t, client, page, issuer)), CodeDenied)

	// A new account's name is normalized as a client's is.
	identity.Subject = "idp:4"
	fresh, _ := dialRaw(t, httpServer)
	_, result = fresh.request(t, "auth", "a", redemption(client, externalCode(t, client, page, issuer)))
	if got := result["you"].(map[string]any)["name"]; got != normalizeName(identity.Name) || got == identity.Name {
		t.Fatalf("new account's name = %q", got)
	}
}

// The page refuses requests it must not redirect, shows the problem itself,
// and returns other problems to the client (§4.12.3).
func TestExternalAuthorizationRequests(t *testing.T) {
	identity := &Identity{}
	_, httpServer := externalTestServer(t, identity)
	issuer := httpServer.URL
	page := issuer + "/oauth/authorize"
	app := oauthtest.New(t, issuer)

	for name, overrides := range map[string]url.Values{
		"no client_id":      {"client_id": {""}},
		"no redirect_uri":   {"redirect_uri": nil},
		"plain http":        {"redirect_uri": {"http://evil.example/cb"}},
		"no dot in scheme":  {"redirect_uri": {"myapp:/cb"}},
		"javascript":        {"redirect_uri": {"javascript:alert(1)"}},
		"fragment":          {"redirect_uri": {"https://chat.alice.dev/cb#x"}},
		"two redirect_uris": {"redirect_uri": {"https://a.example/cb", "https://b.example/cb"}},
	} {
		if r := app.Get(app.URL(page, overrides)); r.Status != http.StatusBadRequest || r.Return != nil || r.Header.Get("Location") != "" {
			t.Errorf("%s: %d %v", name, r.Status, r.Header)
		}
	}
	for name, test := range map[string]struct {
		overrides url.Values
		error     string
	}{
		"other resource":  {url.Values{"resource": {"https://other.example"}}, "invalid_target"},
		"no resource":     {url.Values{"resource": nil}, "invalid_target"},
		"plain PKCE":      {url.Values{"code_challenge_method": {"plain"}}, "invalid_request"},
		"no challenge":    {url.Values{"code_challenge": nil}, "invalid_request"},
		"token response":  {url.Values{"response_type": {"token"}}, "unsupported_response_type"},
		"short challenge": {url.Values{"code_challenge": {"abc"}}, "invalid_request"},
		"no state":        {url.Values{"state": nil}, "invalid_request"},
	} {
		// The page shows every problem itself, even with a good
		// redirect_uri: it returns to the app only once the person acts
		// (§4.12.3).
		r := app.Get(app.URL(page, test.overrides))
		if r.Status != http.StatusBadRequest || r.Return != nil || r.Header.Get("Location") != "" || !strings.Contains(r.Body, "Error: "+test.error) {
			t.Errorf("%s: %d %v %s", name, r.Status, r.Return, r.Body)
		}
	}

	// Who is asking, by redirect_uri.
	identity.Subject = "idp:who"
	for redirect, want := range map[string]string{
		"https://chat.alice.dev/callback":  "<strong>chat.alice.dev</strong> wants to sign in",
		"http://127.0.0.1:53682/callback":  "<strong>An app on this device</strong> wants to sign in",
		"http://[::1]:53682/callback":      "<strong>An app on this device</strong> wants to sign in",
		"http://localhost/callback":        "<strong>An app on this device</strong> wants to sign in",
		"com.example.myapp:/signed-in":     "<strong>An app on this device (com.example.myapp)</strong> wants to sign in",
		"https://chat.alice.dev:8443/back": "Only continue if you just started signing in on chat.alice.dev:8443.",
	} {
		if r := app.Get(app.URL(page, url.Values{"redirect_uri": {redirect}})); r.Status != http.StatusOK || !strings.Contains(r.Body, want) {
			t.Errorf("%s: %d, want %q in %s", redirect, r.Status, want, r.Body)
		}
	}
	// https:// with the host is the server's origin too, for a loopback
	// server in development (§4.12.1).
	if r := app.Get(app.URL(page, url.Values{"resource": {"https://" + strings.TrimPrefix(issuer, "http://")}})); r.Status != http.StatusOK {
		t.Fatalf("https resource: %d %v", r.Status, r.Return)
	}

	// Cancel on the confirmation returns access_denied.
	app.State = "second"
	if r := app.Press(app.Get(app.URL(page, nil)), "cancel"); r.Return == nil || r.Return.Query().Get("error") != "access_denied" || r.Return.Query().Get("state") != "second" {
		t.Fatalf("cancel = %d %v", r.Status, r.Return)
	}
	// So does declining at the identity provider.
	identity.Subject = ""
	if screen := app.Get(app.URL(page, nil)); screen.Return == nil || screen.Return.Query().Get("error") != "access_denied" {
		t.Fatalf("declined = %d %v %s", screen.Status, screen.Return, screen.Body)
	}
	// A stale or unknown flow is a page, not a redirect.
	if r := app.Post(page, url.Values{"flow": {"nope"}, "op": {"allow"}}); r.Status != http.StatusNotFound || r.Return != nil {
		t.Fatalf("unknown flow = %d", r.Status)
	}
}

// The server publishes its authorization server metadata, and redeems codes
// at its token endpoint too, for bearer tokens for scheme "token"
// (§4.12.3).
func TestExternalMetadataAndTokenEndpoint(t *testing.T) {
	identity := &Identity{Subject: "idp:5", Name: "Tess", UserID: "tess"}
	_, httpServer := externalTestServer(t, identity)
	issuer := httpServer.URL

	status, header, body := httpDo(t, http.MethodGet, issuer+"/.well-known/oauth-authorization-server", nil, "")
	var metadata map[string]any
	if err := json.Unmarshal(body, &metadata); status != http.StatusOK || err != nil || header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("metadata = %d %v %s", status, header, body)
	}
	for name, want := range map[string]any{
		"issuer": issuer, "authorization_endpoint": issuer + "/oauth/authorize", "token_endpoint": issuer + "/oauth/token",
		"response_types_supported": []any{"code"}, "grant_types_supported": []any{"authorization_code"},
		"code_challenge_methods_supported": []any{"S256"}, "token_endpoint_auth_methods_supported": []any{"none"},
		"authorization_response_iss_parameter_supported": true,
	} {
		if got, _ := json.Marshal(metadata[name]); string(got) != string(must(json.Marshal(want))) {
			t.Errorf("metadata %s = %s", name, got)
		}
	}

	app := oauthtest.New(t, issuer)
	code := externalCode(t, app, issuer+"/oauth/authorize", issuer)
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {app.Verifier}, "redirect_uri": {app.RedirectURI}, "client_id": {app.ClientID}, "resource": {issuer}}
	status, header, body = httpDo(t, http.MethodPost, issuer+"/oauth/token", strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
	var token map[string]any
	if err := json.Unmarshal(body, &token); status != http.StatusOK || err != nil || token["token_type"] != "Bearer" || header.Get("Access-Control-Allow-Origin") != "*" || header.Get("Cache-Control") != "no-store" {
		t.Fatalf("token = %d %v %s", status, header, body)
	}
	c, _ := dialRaw(t, httpServer)
	_, result := c.request(t, "auth", "r", map[string]any{"scheme": "token", "token": token["access_token"]})
	if result["you"].(map[string]any)["user_id"] != "tess" {
		t.Fatalf("resume = %#v", result)
	}

	// The code is used up.
	status, _, body = httpDo(t, http.MethodPost, issuer+"/oauth/token", strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
	if status != http.StatusBadRequest || !strings.Contains(string(body), `"invalid_grant"`) {
		t.Fatalf("reuse = %d %s", status, body)
	}
	for name, change := range map[string]url.Values{
		"grant type": {"grant_type": {"refresh_token"}},
		"resource":   {"resource": {"https://other.example"}},
	} {
		form := url.Values{"grant_type": {"authorization_code"}, "code": {externalCode(t, app, issuer+"/oauth/authorize", issuer)}, "code_verifier": {app.Verifier}, "redirect_uri": {app.RedirectURI}, "client_id": {app.ClientID}}
		for key, value := range change {
			form[key] = value
		}
		if status, _, body := httpDo(t, http.MethodPost, issuer+"/oauth/token", strings.NewReader(form.Encode()), "application/x-www-form-urlencoded"); status != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, status, body)
		}
	}
	if status, header, _ := httpDo(t, http.MethodOptions, issuer+"/oauth/token", nil, ""); status != http.StatusNoContent || header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("preflight = %d %v", status, header)
	}
}

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

// The page remembers that an account allowed an app at an https host, and
// then returns to it at once, across restarts, until the account signs out
// everywhere; never for an app's scheme or a loopback redirect, and never
// for another host or another account.
func TestExternalRemembersConsent(t *testing.T) {
	identity := &Identity{Subject: "idp:7", Name: "Rem", UserID: "rem"}
	memory, dir := store.NewMemory(), t.TempDir()
	configure := func(c *Config) { c.Extensions = []Extension{ssoExtension(identity)} }
	app, httpServer, stop := startWith(t, memory, dir, configure)
	defer func() { stop() }()
	client := func(redirect string) *oauthtest.Client {
		c := oauthtest.New(t, httpServer.URL)
		c.ClientID, c.RedirectURI = "https://chat.alice.dev/", redirect
		return c
	}
	// asks signs in from redirect, allowing it if the page asks, and
	// reports whether it did.
	asks := func(redirect string) bool {
		t.Helper()
		c := client(redirect)
		screen := c.Get(c.URL(httpServer.URL+"/oauth/authorize", nil))
		if screen.Return != nil {
			c.Code(screen, httpServer.URL)
			return false
		}
		if !strings.Contains(screen.Body, `value="allow"`) {
			t.Fatalf("%s: %d %s", redirect, screen.Status, screen.Body)
		}
		c.Code(c.Press(screen, "allow"), httpServer.URL)
		return true
	}

	// A new identity has no account to remember for: it asks, and so does
	// the account it makes, the first time.
	c := client("https://chat.alice.dev/cb")
	code := externalCode(t, c, httpServer.URL+"/oauth/authorize", httpServer.URL)
	fresh, _ := dialRaw(t, httpServer)
	fresh.request(t, "auth", "a", redemption(c, code))
	if !asks("https://chat.alice.dev/cb") {
		t.Fatal("did not ask the first time")
	}
	// Then not, at any path of that host; another host or port asks.
	if asks("https://chat.alice.dev/cb") || asks("https://CHAT.alice.dev/elsewhere") {
		t.Fatal("asked again for an allowed host")
	}
	if !asks("https://chat.alice.dev:8443/cb") || !asks("https://evil.example/cb") {
		t.Fatal("did not ask for another host")
	}
	// An app's scheme or a loopback redirect asks every time.
	for _, redirect := range []string{"com.example.app:/signed-in", "http://127.0.0.1:5173/cb", "https://localhost:8443/cb", "https://127.0.0.1/cb"} {
		if !asks(redirect) || !asks(redirect) {
			t.Errorf("%s: remembered", redirect)
		}
	}
	// Another account at the same host asks.
	identity.Subject, identity.UserID = "idp:8", "other"
	c = client("https://chat.alice.dev/cb")
	fresh, _ = dialRaw(t, httpServer)
	fresh.request(t, "auth", "a", redemption(c, externalCode(t, c, httpServer.URL+"/oauth/authorize", httpServer.URL)))
	if !asks("https://chat.alice.dev/cb") {
		t.Fatal("another account did not ask")
	}
	identity.Subject = "idp:7"

	// It is kept with the account.
	stop()
	app, httpServer, stop = startWith(t, memory, dir, configure)
	if asks("https://chat.alice.dev/cb") {
		t.Fatal("asked again after a restart")
	}
	// Signing out everywhere forgets it, as does signing out the other
	// sessions.
	if err := app.SignOut(context.Background(), "rem"); err != nil {
		t.Fatal(err)
	}
	if !asks("https://chat.alice.dev/cb") || asks("https://chat.alice.dev/cb") {
		t.Fatal("signing out everywhere did not forget the app, or allowing it did not remember")
	}
	app.mu.Lock()
	app.signOutOthersLocked(app.users["rem"], nil)
	app.unlock()
	if !asks("https://chat.alice.dev/cb") {
		t.Fatal("signing out the other sessions did not forget the app")
	}
}
