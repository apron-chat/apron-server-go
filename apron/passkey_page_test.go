package apron

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/apron-chat/apron-server-go/internal/oauthtest"
)

// pageOrigin is the server's own origin in the passkey page tests, which
// the RP accepts: the page runs the ceremony there.
const pageOrigin = "http://localhost:8080"

// passkeyPage is a server with the passkey page, and external sign-in for
// the codes that must not cross over.
type passkeyPage struct {
	app  *Server
	http *httptest.Server
	host *Host
}

func passkeyPageServer(t *testing.T) *passkeyPage {
	t.Helper()
	p := &passkeyPage{}
	config := extensionConfig(testExtension{"sso", func(r *Registrar) error {
		p.host = r.Host
		signIn := r.ExternalSignIn(func(w http.ResponseWriter, req *http.Request, x ExternalRequest) {
			http.Redirect(w, req, "/sso/callback?state="+url.QueryEscape(x.ID), http.StatusFound)
		})
		r.Handle("/sso/callback", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			_ = signIn.Complete(w, req, req.URL.Query().Get("state"), Identity{Subject: "idp:9", Name: "Ext"})
		}))
		return nil
	}})
	w, err := webauthn.New(&webauthn.Config{RPID: "localhost", RPDisplayName: "Apron", RPOrigins: []string{testPasskeyOrigin, pageOrigin}})
	if err != nil {
		t.Fatal(err)
	}
	config.WebAuthn = w
	// The page's links name the server's public URL, which the tests
	// reach at the test server's address.
	config.PublicURL = pageOrigin
	config.AllowAnyOrigin = true
	p.app, p.http = newTestServer(t, config)
	return p
}

// at is a page's address on the test server.
func (p *passkeyPage) at(page string) string {
	return p.http.URL + strings.TrimPrefix(page, pageOrigin)
}

// open opens the page a begin result named, as the app would, and returns
// its first screen.
func (p *passkeyPage) open(t *testing.T, client *oauthtest.Client, begin map[string]any) oauthtest.Response {
	t.Helper()
	external, ok := begin["external"].(map[string]any)
	if !ok {
		t.Fatalf("begin has no external: %#v", begin)
	}
	screen := client.Get(p.at(client.URL(external["url"].(string), nil)))
	if screen.Status != http.StatusOK || screen.Data("flow") == "" {
		t.Fatalf("first screen = %d %s", screen.Status, screen.Body)
	}
	return screen
}

// script posts as the page's script does, and returns its JSON answer.
func (p *passkeyPage) script(t *testing.T, client *oauthtest.Client, screen oauthtest.Response, op string, extra url.Values) (int, map[string]any) {
	t.Helper()
	form := url.Values{"flow": {screen.Data("flow")}, "op": {op}}
	for key, value := range extra {
		form[key] = value
	}
	r := client.Post(p.at(screen.Data("post")), form)
	var answer map[string]any
	if err := json.Unmarshal([]byte(r.Body), &answer); err != nil {
		t.Fatalf("%s: %d %s", op, r.Status, r.Body)
	}
	return r.Status, answer
}

// ceremony runs the passkey on the page with authenticator a, and returns
// the page's answer to it: next, redirect, or error.
func (p *passkeyPage) ceremony(t *testing.T, client *oauthtest.Client, screen oauthtest.Response, a *testAuthenticator, flags byte) (int, map[string]any) {
	t.Helper()
	status, options := p.script(t, client, screen, "options", nil)
	if status != http.StatusOK {
		t.Fatalf("options = %d %#v", status, options)
	}
	var credential map[string]any
	if screen.Data("ceremony") == "create" {
		credential = a.registration(t, options, pageOrigin)
	} else {
		credential = a.assertion(t, options, pageOrigin, "localhost", flags)
	}
	encoded, _ := json.Marshal(credential)
	return p.script(t, client, screen, "verify", url.Values{"credential": {string(encoded)}})
}

// returned is the code a registration's redirect carries.
func returned(t *testing.T, client *oauthtest.Client, answer map[string]any) string {
	t.Helper()
	redirect, _ := answer["redirect"].(string)
	target, err := url.Parse(redirect)
	if err != nil || !strings.HasPrefix(redirect, client.RedirectURI) || target.Query().Get("state") != client.State || target.Query().Get("iss") != pageOrigin {
		t.Fatalf("redirect = %#v", answer)
	}
	return target.Query().Get("code")
}

func finishWithCode(client *oauthtest.Client, action, code string) map[string]any {
	params := client.Redemption(code)
	params["scheme"], params["action"], params["step"] = "webauthn", action, "finish"
	return params
}

// A client that cannot use the RP ID signs in on the passkey page, and
// finishes with its code.
func TestPasskeyPageLogin(t *testing.T) {
	p := passkeyPageServer(t)
	owner, _ := dialOrigin(t, p.http, testPasskeyOrigin)
	a := newTestAuthenticator(t)
	registered := registerTestPasskey(t, owner, a)
	owner.result(t, "me", "name", map[string]any{"name": "Ada"})
	ownerID := registered["you"].(map[string]any)["user_id"].(string)

	// A client the RP accepts gets both ways; one it does not, only the
	// page.
	local, _ := dialOrigin(t, p.http, testPasskeyOrigin)
	begin := passkeyResult(t, passkeyCall(t, local, "b", "login", "begin", nil))
	if begin["public_key"] == nil || begin["challenge_id"] == nil || begin["external"].(map[string]any)["url"] != pageOrigin+"/passkey?action=login" {
		t.Fatalf("local begin = %#v", begin)
	}
	native, _ := dialRaw(t, p.http)
	begin = passkeyResult(t, passkeyCall(t, native, "b", "login", "begin", nil))
	if begin["public_key"] != nil || begin["challenge_id"] != nil || begin["external"].(map[string]any)["url"] != pageOrigin+"/passkey?action=login" {
		t.Fatalf("remote begin = %#v", begin)
	}

	client := oauthtest.New(t, pageOrigin)
	screen := p.open(t, client, begin)
	for _, want := range []string{
		"<strong>An app on this device (com.example.app)</strong> wants to sign in to your account on <strong>localhost:8080</strong>. It will be able to read and send messages as you.",
		"localhost:8080 has not verified this app.",
		"Only continue if you just started signing in on com.example.app.",
		">Continue with passkey</button>",
	} {
		if !strings.Contains(screen.Body, want) {
			t.Fatalf("first screen lacks %q: %s", want, screen.Body)
		}
	}
	if csp := screen.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "script-src 'nonce-") || screen.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("headers = %v", screen.Header)
	}

	// The page requires user verification, its own origin, and no frame.
	for name, test := range map[string]struct {
		flags      byte
		clientData map[string]any
	}{
		"no user verification": {0x01, nil},
		"cross-origin":         {0x05, map[string]any{"crossOrigin": true}},
		"top origin":           {0x05, map[string]any{"topOrigin": "https://evil.example"}},
		"other origin":         {0x05, map[string]any{"origin": testPasskeyOrigin}},
	} {
		a.clientData = test.clientData
		if status, answer := p.ceremony(t, client, screen, a, test.flags); status != http.StatusBadRequest || answer["error"] == nil {
			t.Errorf("%s: %d %#v", name, status, answer)
		}
	}
	a.clientData = nil
	// A verify without options has no ceremony to verify.
	if status, _ := p.script(t, client, screen, "verify", url.Values{"credential": {"{}"}}); status != http.StatusBadRequest {
		t.Fatalf("verify without options = %d", status)
	}

	status, answer := p.ceremony(t, client, screen, a, 0x05)
	next, _ := answer["next"].(string)
	if status != http.StatusOK || next == "" {
		t.Fatalf("verify = %d %#v", status, answer)
	}
	allow := client.Get(p.at(pageOrigin + next))
	if !strings.Contains(allow.Body, "Allow <strong>An app on this device (com.example.app)</strong> to use <strong>Ada (@"+ownerID+")</strong>’s account?") ||
		!strings.Contains(allow.Body, "Not you? Use another passkey") {
		t.Fatalf("allow screen = %d %s", allow.Status, allow.Body)
	}
	// "Not you?" goes back to the passkey.
	if again := client.Press(allow, "again"); !strings.Contains(again.Body, ">Continue with passkey</button>") {
		t.Fatalf("another passkey = %s", again.Body)
	}
	p.ceremony(t, client, screen, a, 0x05)
	code := client.Code(client.Press(allow, "allow"), pageOrigin)

	// The code finishes only a login, once, on any connection.
	native.expectError(t, "auth", "wrong-action", finishWithCode(client, "register", code), CodeDenied)
	native.expectError(t, "auth", "used", finishWithCode(client, "login", code), CodeDenied)
	code = p.loginCode(t, client, a)
	external := client.Redemption(code)
	external["scheme"] = "external"
	native.expectError(t, "auth", "as-external", external, CodeDenied)
	code = p.loginCode(t, client, a)
	_, result := native.request(t, "auth", "finish", finishWithCode(client, "login", code))
	if result["you"].(map[string]any)["user_id"] != ownerID || result["token"] == nil {
		t.Fatalf("finish = %#v", result)
	}
	// A code from the external sign-in page does not finish webauthn.
	other, _ := dialRaw(t, p.http)
	externalCode := externalCode(t, client, p.at(pageOrigin+"/oauth/authorize"), pageOrigin)
	other.expectError(t, "auth", "cross", finishWithCode(client, "login", externalCode), CodeDenied)

	// Cancel returns access_denied.
	screen = p.open(t, client, begin)
	if r := client.Press(screen, "cancel"); r.Return == nil || r.Return.Query().Get("error") != "access_denied" || r.Return.Query().Get("iss") != pageOrigin {
		t.Fatalf("cancel = %d %v", r.Status, r.Return)
	}
}

// loginCode signs in on the page with a, and returns the code.
func (p *passkeyPage) loginCode(t *testing.T, client *oauthtest.Client, a *testAuthenticator) string {
	t.Helper()
	screen := client.Get(p.at(client.URL(pageOrigin+"/passkey?action=login", nil)))
	status, answer := p.ceremony(t, client, screen, a, 0x05)
	if status != http.StatusOK {
		t.Fatalf("verify = %d %#v", status, answer)
	}
	return client.Code(client.Press(client.Get(p.at(pageOrigin+answer["next"].(string))), "allow"), pageOrigin)
}

// A passkey added on the page goes to the account that began it, only on a
// connection signed in to it.
func TestPasskeyPageAddition(t *testing.T) {
	p := passkeyPageServer(t)
	guest, _ := dialRaw(t, p.http)
	guestID := guestAuth(t, guest)["you"].(map[string]any)["user_id"].(string)
	guest.result(t, "me", "name", map[string]any{"name": "Grace"})
	client := oauthtest.New(t, pageOrigin)

	add := func() (string, oauthtest.Response) {
		t.Helper()
		guest.drain(t)
		begin := passkeyResult(t, passkeyCall(t, guest, guest.nextID("b"), "register", "begin", nil))
		page := begin["external"].(map[string]any)["url"].(string)
		if !strings.HasPrefix(page, pageOrigin+"/passkey?action=register&request=") || begin["public_key"] != nil {
			t.Fatalf("begin = %#v", begin)
		}
		screen := p.open(t, client, begin)
		if !strings.Contains(screen.Body, "<strong>An app on this device (com.example.app)</strong> wants to add a passkey to <strong>Grace (@"+guestID+")</strong>’s account on <strong>localhost:8080</strong>.") ||
			!strings.Contains(screen.Body, ">Create passkey</button>") {
			t.Fatalf("addition screen = %s", screen.Body)
		}
		return page, screen
	}

	// On another account's connection, the code is denied.
	_, screen := add()
	a := newTestAuthenticator(t)
	status, answer := p.ceremony(t, client, screen, a, 0)
	if status != http.StatusOK {
		t.Fatalf("verify = %d %#v", status, answer)
	}
	stranger, _ := dialRaw(t, p.http)
	guestAuth(t, stranger)
	stranger.expectError(t, "auth", "stranger", finishWithCode(client, "register", returned(t, client, answer)), CodeDenied)

	// On its own, it adds the passkey, and the guest becomes an account.
	page, screen := add()
	b := newTestAuthenticator(t)
	_, answer = p.ceremony(t, client, screen, b, 0)
	code := returned(t, client, answer)
	guest.drain(t)
	guest.expectError(t, "auth", "as-login", finishWithCode(client, "login", code), CodeDenied)
	_, screen = add()
	_, answer = p.ceremony(t, client, screen, b, 0)
	_, result := guest.request(t, "auth", "finish", finishWithCode(client, "register", returned(t, client, answer)))
	if you := result["you"].(map[string]any); you["user_id"] != guestID || you["name"] != "Grace" || result["token"] == nil {
		t.Fatalf("addition = %#v", result)
	}
	// Its request is used up.
	if r := client.Get(p.at(client.URL(page, nil))); r.Status != http.StatusNotFound {
		t.Fatalf("used request = %d", r.Status)
	}
	// The passkey signs in, on the page too.
	code = p.loginCode(t, client, b)
	fresh, _ := dialRaw(t, p.http)
	if _, result := fresh.request(t, "auth", "login", finishWithCode(client, "login", code)); result["you"].(map[string]any)["user_id"] != guestID {
		t.Fatalf("login = %#v", result)
	}
}

// A registration on the page makes a new account with the invitation the
// finishing connection presents, as a registration in the client does.
func TestPasskeyPageRegistration(t *testing.T) {
	p := passkeyPageServer(t)
	invitation, err := p.host.Invite(context.Background(), InvitationOptions{Name: "Newcomer"})
	if err != nil {
		t.Fatal(err)
	}
	native, _ := dialRaw(t, p.http)
	// Without an invitation, nobody may register on this server.
	passkeyDenied(t, passkeyCall(t, native, "none", "register", "begin", nil))
	begin := passkeyResult(t, passkeyCall(t, native, "b", "register", "begin", map[string]any{"invitation": invitation.Token, "name": "Ada"}))
	if begin["external"].(map[string]any)["url"] != pageOrigin+"/passkey?action=register&name=Ada" {
		t.Fatalf("begin = %#v", begin)
	}
	client := oauthtest.New(t, pageOrigin)
	screen := p.open(t, client, begin)
	if !strings.Contains(screen.Body, "<strong>An app on this device (com.example.app)</strong> wants to create an account for you on <strong>localhost:8080</strong>, with a new passkey. It will be able to read and send messages as you.") {
		t.Fatalf("registration screen = %s", screen.Body)
	}
	a := newTestAuthenticator(t)
	status, answer := p.ceremony(t, client, screen, a, 0)
	if status != http.StatusOK {
		t.Fatalf("verify = %d %#v", status, answer)
	}
	code := returned(t, client, answer)
	// It needs the invitation, and a connection that is not signed in.
	native.expectError(t, "auth", "no-invitation", finishWithCode(client, "register", code), CodeDenied)
	screen = p.open(t, client, begin)
	_, answer = p.ceremony(t, client, screen, a, 0)
	finish := finishWithCode(client, "register", returned(t, client, answer))
	finish["invitation"] = invitation.Token
	_, result := native.request(t, "auth", "finish", finish)
	if you := result["you"].(map[string]any); you["name"] != "Ada" || result["token"] == nil {
		t.Fatalf("registration = %#v", result)
	}
	if len(p.app.Invitations()) != 0 {
		t.Fatal("the invitation outlived its use")
	}
}

// The page refuses what it must, on the page and never by redirecting: a
// request without a valid redirect, a bad action or resource, and unknown
// requests.
func TestPasskeyPageRequests(t *testing.T) {
	p := passkeyPageServer(t)
	client := oauthtest.New(t, pageOrigin)
	if r := client.Get(p.at(client.URL(pageOrigin+"/passkey?action=login", url.Values{"redirect_uri": {"http://evil.example/"}}))); r.Status != http.StatusBadRequest || r.Return != nil {
		t.Fatalf("bad redirect = %d %v", r.Status, r.Return)
	}
	for name, page := range map[string]string{
		"no action":         "/passkey",
		"other action":      "/passkey?action=delete",
		"login with name":   "/passkey?action=login&name=Ada",
		"login with handle": "/passkey?action=login&request=x",
	} {
		if r := client.Get(p.at(client.URL(pageOrigin+page, nil))); r.Status != http.StatusBadRequest || r.Return != nil || !strings.Contains(r.Body, "Error: invalid_request") {
			t.Errorf("%s: %d %v", name, r.Status, r.Return)
		}
	}
	if r := client.Get(p.at(client.URL(pageOrigin+"/passkey?action=register&request=unknown", nil))); r.Status != http.StatusNotFound || r.Return != nil {
		t.Fatalf("unknown request = %d %v", r.Status, r.Return)
	}
	if r := client.Get(p.at(client.URL(pageOrigin+"/passkey?action=login", url.Values{"resource": {"https://other.example"}}))); r.Status != http.StatusBadRequest || r.Return != nil || !strings.Contains(r.Body, "Error: invalid_target") {
		t.Fatalf("other resource = %d %v", r.Status, r.Return)
	}

	// Without the page's origin among the RP's, there is no page.
	_, plain := passkeyTestServer(t)
	local, _ := dialOrigin(t, plain, testPasskeyOrigin)
	if begin := passkeyResult(t, passkeyCall(t, local, "b", "login", "begin", nil)); begin["external"] != nil {
		t.Fatalf("begin without the page = %#v", begin)
	}
}
