package apron

import (
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
// extension and Allow, and returns the code.
func externalCode(t *testing.T, app *oauthtest.Client, page, issuer string) string {
	t.Helper()
	screen := app.Get(app.URL(page, nil))
	if screen.Status != http.StatusOK {
		t.Fatalf("page = %d %s", screen.Status, screen.Body)
	}
	return app.Code(app.Press(app.Press(screen, "continue"), "allow"), issuer)
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

	// The page says who is asking, and refuses to be framed.
	app := oauthtest.New(t, issuer)
	screen := app.Get(app.URL(page, nil))
	for _, want := range []string{
		"<h1>Sign in to 127.0.0.1:",
		"<strong>An app on this device (com.example.app)</strong> wants to sign in to your account on <strong>127.0.0.1:",
		"It will be able to read and send messages as you.",
		"has not verified this app.",
		"Only continue if you just started signing in on com.example.app.",
	} {
		if !strings.Contains(screen.Body, want) {
			t.Fatalf("first screen lacks %q: %s", want, screen.Body)
		}
	}
	if csp := screen.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") || screen.Header.Get("X-Frame-Options") != "DENY" || screen.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("headers = %v", screen.Header)
	}
	// A new identity: the page says it makes an account.
	screen = app.Press(screen, "continue")
	if !strings.Contains(screen.Body, "<strong>An app on this device (com.example.app)</strong> wants to create an account for you") {
		t.Fatalf("second screen = %d %s", screen.Status, screen.Body)
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
	screen = again.Press(again.Get(again.URL(page, nil)), "continue")
	if !strings.Contains(screen.Body, "Allow <strong>An app on this device (com.example.app)</strong> to use <strong>Ada (@ada)</strong>’s account?") ||
		!strings.Contains(screen.Body, "Not you? Sign in as someone else") {
		t.Fatalf("known account = %s", screen.Body)
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

	// Declining at the identity provider returns access_denied.
	screen := app.Press(app.Get(app.URL(page, nil)), "continue")
	if screen.Return == nil || screen.Return.Query().Get("error") != "access_denied" {
		t.Fatalf("declined = %d %v %s", screen.Status, screen.Return, screen.Body)
	}
	// Cancel on the first screen too.
	app.State = "second"
	if r := app.Press(app.Get(app.URL(page, nil)), "cancel"); r.Return == nil || r.Return.Query().Get("error") != "access_denied" || r.Return.Query().Get("state") != "second" {
		t.Fatalf("cancel = %d %v", r.Status, r.Return)
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
