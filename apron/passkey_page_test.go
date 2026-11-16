package apron

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/apron-chat/apron-server-go/internal/auth/oauth"
	"github.com/apron-chat/apron-server-go/internal/auth/passkey"
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

func passkeyPageServer(t *testing.T, configure ...func(*Config)) *passkeyPage {
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
	for _, f := range configure {
		f(&config)
	}
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
// the page's answer to it: redirect, or error.
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

// returned is the code a verified passkey's redirect carries.
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

	// The page's one screen: who is asking, before the passkey.
	client := oauthtest.New(t, pageOrigin)
	screen := p.open(t, client, begin)
	for _, want := range []string{
		"<h1>Sign in to localhost:8080 on an app on this device (com.example.app)?</h1>",
		"<p>It will be able to read and send messages as you.</p>",
		"localhost:8080 has not verified this app.",
		"Only continue if you just started signing in on an app on this device.",
		">Continue with passkey</button>",
		`value="cancel">Cancel</button>`,
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

	// A verified passkey returns to the app at once, and the flow is over.
	status, answer := p.ceremony(t, client, screen, a, 0x05)
	if status != http.StatusOK || answer["next"] != nil {
		t.Fatalf("verify = %d %#v", status, answer)
	}
	code := returned(t, client, answer)
	if status, _ := p.script(t, client, screen, "options", nil); status != http.StatusNotFound {
		t.Fatalf("options after the redirect = %d", status)
	}

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

// A token from a code is a bearer token that resumes from any origin, or
// none: from either page, redeemed at the token endpoint or on a
// connection (§4.12.4). One from a ceremony in the client stays bound to
// the ceremony's origin (§4.10).
func TestTokensFromCodesResumeFromAnyOrigin(t *testing.T) {
	p := passkeyPageServer(t)
	anywhere := []string{"", "https://other.example", testPasskeyOrigin}

	// A ceremony in the client.
	owner, _ := dialOrigin(t, p.http, testPasskeyOrigin)
	a := newTestAuthenticator(t)
	local := registerTestPasskey(t, owner, a)["token"]
	if !resumes(t, p.http, local, testPasskeyOrigin) || resumes(t, p.http, local, "") || resumes(t, p.http, local, "https://other.example") {
		t.Fatal("a token from a ceremony in the client resumed from another origin, or not from its own")
	}

	client := oauthtest.New(t, pageOrigin)
	authorize := p.at(pageOrigin + "/oauth/authorize")
	tokens := map[string]any{}
	// At the token endpoint, whatever the request's Origin.
	for _, origin := range []string{"", testPasskeyOrigin} {
		tokens["token endpoint, Origin "+origin] = tokenFromEndpoint(t, client, p.at(pageOrigin+"/oauth/token"), externalCode(t, client, authorize, pageOrigin), origin)
	}
	// On a connection, from either page.
	app, _ := dialOrigin(t, p.http, "https://app.example")
	_, result := app.request(t, "auth", "external", redemption(client, externalCode(t, client, authorize, pageOrigin)))
	tokens["external code on a connection"] = result["token"]
	native, _ := dialRaw(t, p.http)
	_, result = native.request(t, "auth", "finish", finishWithCode(client, "login", p.loginCode(t, client, a)))
	tokens["passkey page code on a connection"] = result["token"]

	for name, token := range tokens {
		if token == nil {
			t.Fatalf("%s: no token", name)
		}
		for _, origin := range anywhere {
			if !resumes(t, p.http, token, origin) {
				t.Errorf("%s: did not resume from %q", name, origin)
			}
		}
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
	return returned(t, client, answer)
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
	// Its request names the account sealed, which nobody can alter.
	target, _ := url.Parse(page)
	query := target.Query()
	request := []byte(query.Get("request"))
	request[0] ^= 1
	query.Set("request", string(request))
	target.RawQuery = query.Encode()
	if r := client.Get(p.at(client.URL(target.String(), nil))); r.Status != http.StatusNotFound {
		t.Fatalf("altered request = %d", r.Status)
	}
	// The passkey signs in, on the page too.
	code = p.loginCode(t, client, b)
	fresh, _ := dialRaw(t, p.http)
	if _, result := fresh.request(t, "auth", "login", finishWithCode(client, "login", code)); result["you"].(map[string]any)["user_id"] != guestID {
		t.Fatalf("login = %#v", result)
	}
}

// No number of register begins, on any number of connections, keeps a
// client from running its own ceremony, nor another account from adding a
// passkey on the page: an addition's request is sealed, and the server
// keeps nothing for it.
func TestPasskeyPageAdditionsFlood(t *testing.T) {
	p := passkeyPageServer(t)
	const connections, perConnection = 20, oauth.MaxEntries/20 + 1
	for range connections {
		flooder, _ := dialRaw(t, p.http)
		guestAuth(t, flooder)
		// In batches, which the connection's queues hold.
		for sent := 0; sent < perConnection; sent += 20 {
			ids := make([]string, min(20, perConnection-sent))
			for i := range ids {
				ids[i] = flooder.nextID("flood")
				flooder.write(t, map[string]any{"method": "auth", "id": ids[i], "params": map[string]any{"scheme": "webauthn", "action": "register", "step": "begin"}})
			}
			for _, id := range ids {
				frame := flooder.read(t)
				for frame["id"] == nil {
					// Such as an earlier flooder leaving general.
					frame = flooder.read(t)
				}
				if result, _ := frame["result"].(map[string]any); frame["id"] != id || result["external"] == nil {
					t.Fatalf("flooding begin %s = %#v", id, frame)
				}
			}
		}
		_ = flooder.ws.CloseNow()
	}

	// A guest whose origin the RP accepts runs its own ceremony, with the
	// page as another way.
	local, _ := dialOrigin(t, p.http, testPasskeyOrigin)
	guestAuth(t, local)
	begin := passkeyResult(t, passkeyCall(t, local, "local", "register", "begin", nil))
	if begin["challenge_id"] == nil || begin["public_key"] == nil || begin["external"] == nil {
		t.Fatalf("in-client begin after the flood = %#v", begin)
	}
	// One that only the page serves still adds its passkey there.
	native, _ := dialRaw(t, p.http)
	nativeID := guestAuth(t, native)["you"].(map[string]any)["user_id"].(string)
	begin = passkeyResult(t, passkeyCall(t, native, "native", "register", "begin", nil))
	client := oauthtest.New(t, pageOrigin)
	status, answer := p.ceremony(t, client, p.open(t, client, begin), newTestAuthenticator(t), 0)
	if status != http.StatusOK {
		t.Fatalf("verify = %d %#v", status, answer)
	}
	native.drain(t)
	if _, result := native.request(t, "auth", "finish", finishWithCode(client, "register", returned(t, client, answer))); result["you"].(map[string]any)["user_id"] != nativeID {
		t.Fatalf("addition after the flood = %#v", result)
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

// A login on the page with a passkey that no account has, nor may yet
// have, tells the page's script to signal it unknown (WebAuthn's
// signalUnknownCredential), with SignalUnknownPasskeys, and stays on the
// page. Anything less certain is the page's usual error, which signals
// nothing.
func TestPasskeyPageUnknownCredential(t *testing.T) {
	p := passkeyPageServer(t, func(c *Config) { c.SignalUnknownPasskeys = true })
	client := oauthtest.New(t, pageOrigin)
	login := func(a *testAuthenticator, rpID string, flags byte) map[string]any {
		t.Helper()
		screen := client.Get(p.at(client.URL(pageOrigin+"/passkey?action=login", nil)))
		status, options := p.script(t, client, screen, "options", nil)
		if status != http.StatusOK {
			t.Fatalf("options = %d %#v", status, options)
		}
		encoded, _ := json.Marshal(a.assertion(t, options, pageOrigin, rpID, flags))
		status, answer := p.script(t, client, screen, "verify", url.Values{"credential": {string(encoded)}})
		if status != http.StatusBadRequest || answer["redirect"] != nil || answer["error"] == nil {
			t.Fatalf("verify = %d %#v", status, answer)
		}
		// The page is still there, to try again or cancel.
		if status, _ := p.script(t, client, screen, "options", nil); status != http.StatusOK {
			t.Fatalf("options after the error = %d", status)
		}
		return answer
	}
	signals := func(answer map[string]any) bool { return answer["unknown_credential"] == true }

	// A passkey no account has, nor may: the page signals it.
	stranger := newTestAuthenticator(t)
	stranger.handle = b64([]byte("a user handle no account has"))
	if answer := login(stranger, "localhost", 0x05); !signals(answer) || answer["error"] != "This passkey isn't for an account on localhost:8080. Try another passkey, or cancel." {
		t.Fatalf("unknown passkey = %#v", answer)
	}
	// Unless the assertion was not made for this ceremony: another RP ID,
	// no user verification, no user presence, another challenge, another
	// ceremony's type, another origin (one the RP accepts, which only the
	// page refuses).
	for name, test := range map[string]struct {
		rpID       string
		flags      byte
		clientData map[string]any
	}{
		"other RP ID":          {"other.example", 0x05, nil},
		"no user verification": {"localhost", 0x01, nil},
		"no user presence":     {"localhost", 0x04, nil},
		"other challenge":      {"localhost", 0x05, map[string]any{"challenge": b64([]byte("another challenge"))}},
		"registration type":    {"localhost", 0x05, map[string]any{"type": "webauthn.create"}},
		"other origin":         {"localhost", 0x05, map[string]any{"origin": testPasskeyOrigin}},
	} {
		stranger.clientData = test.clientData
		if answer := login(stranger, test.rpID, test.flags); signals(answer) {
			t.Errorf("%s: signalled %#v", name, answer)
		}
	}
	stranger.clientData = nil

	// A passkey registered on the page may still become an account's:
	// while its code is live, and once a redemption has taken the code,
	// until the server commits it, or the code's and a ceremony's
	// lifetimes pass. A redemption refused after it took the code, for
	// want of an invitation, stays in that window.
	invitation, err := p.host.Invite(context.Background(), InvitationOptions{Name: "Newcomer"})
	if err != nil {
		t.Fatal(err)
	}
	native, _ := dialRaw(t, p.http)
	begin := passkeyResult(t, passkeyCall(t, native, "b", "register", "begin", map[string]any{"invitation": invitation.Token}))
	orphan := newTestAuthenticator(t)
	status, answer := p.ceremony(t, client, p.open(t, client, begin), orphan, 0)
	if status != http.StatusOK {
		t.Fatalf("registration = %d %#v", status, answer)
	}
	code := returned(t, client, answer)
	if answer := login(orphan, "localhost", 0x05); signals(answer) {
		t.Fatalf("a passkey whose registration's code is live was signalled: %#v", answer)
	}
	native.expectError(t, "auth", "no-invitation", finishWithCode(client, "register", code), CodeDenied)
	if answer := login(orphan, "localhost", 0x05); signals(answer) {
		t.Fatalf("a passkey whose registration's code was taken was signalled: %#v", answer)
	}

	// A known passkey that fails verification signals nothing: a wrong
	// signature, another user handle, no user verification.
	owner, _ := dialOrigin(t, p.http, testPasskeyOrigin)
	known := newTestAuthenticator(t)
	registerTestPasskey(t, owner, known)
	impostor := newTestAuthenticator(t)
	impostor.id, impostor.handle = known.id, known.handle
	otherHandle := *known
	otherHandle.handle = b64([]byte("another user handle"))
	for name, a := range map[string]*testAuthenticator{"wrong signature": impostor, "other user handle": &otherHandle} {
		if answer := login(a, "localhost", 0x05); signals(answer) {
			t.Errorf("%s: signalled %#v", name, answer)
		}
	}
	if answer := login(known, "localhost", 0x01); signals(answer) {
		t.Fatalf("a known passkey without user verification was signalled: %#v", answer)
	}
	// And it still signs in.
	p.loginCode(t, client, known)

	// A passkey removed with account.credential_remove is one no account
	// has.
	holder, _ := dialOrigin(t, p.http, testPasskeyOrigin)
	lost, kept := newTestAuthenticator(t), newTestAuthenticator(t)
	registerTestPasskey(t, holder, lost)
	holder.drain(t)
	passkeyResult(t, addTestPasskey(t, holder, kept))
	holder.drain(t)
	holder.result(t, "account.credential_remove", "remove", map[string]any{"id": b64(lost.id)})
	if answer := login(lost, "localhost", 0x05); !signals(answer) {
		t.Fatalf("a removed passkey was not signalled: %#v", answer)
	}
}

// By default the page signals nothing, and a passkey no account has gets
// exactly the answer of a known passkey that fails, so the page tells no
// one whether a credential ID is registered.
func TestPasskeyPageUnknownCredentialSignalOffByDefault(t *testing.T) {
	p := passkeyPageServer(t)
	client := oauthtest.New(t, pageOrigin)
	login := func(a *testAuthenticator) (int, map[string]any) {
		t.Helper()
		screen := client.Get(p.at(client.URL(pageOrigin+"/passkey?action=login", nil)))
		status, options := p.script(t, client, screen, "options", nil)
		if status != http.StatusOK {
			t.Fatalf("options = %d %#v", status, options)
		}
		encoded, _ := json.Marshal(a.assertion(t, options, pageOrigin, "localhost", 0x05))
		return p.script(t, client, screen, "verify", url.Values{"credential": {string(encoded)}})
	}
	owner, _ := dialOrigin(t, p.http, testPasskeyOrigin)
	known := newTestAuthenticator(t)
	registerTestPasskey(t, owner, known)
	impostor := newTestAuthenticator(t)
	impostor.id, impostor.handle = known.id, known.handle
	knownStatus, knownAnswer := login(impostor)
	if knownStatus != http.StatusBadRequest || knownAnswer["error"] == nil {
		t.Fatalf("a known passkey's bad signature = %d %#v", knownStatus, knownAnswer)
	}
	stranger := newTestAuthenticator(t)
	stranger.handle = b64([]byte("a user handle no account has"))
	status, answer := login(stranger)
	if status != knownStatus || !reflect.DeepEqual(answer, knownAnswer) {
		t.Fatalf("an unknown passkey = %d %#v, a known one that fails = %d %#v", status, answer, knownStatus, knownAnswer)
	}
}

// The page's verify step is limited per page, which then ends, and per
// client address, across pages.
func TestPasskeyPageVerifyLimits(t *testing.T) {
	p := passkeyPageServer(t, func(c *Config) { c.ClientIPHeader = "X-Forwarded-For" })
	client := oauthtest.New(t, pageOrigin)
	open := func() oauthtest.Response {
		t.Helper()
		return client.Get(p.at(client.URL(pageOrigin+"/passkey?action=login", nil)))
	}
	verify := func(screen oauthtest.Response, from string) (int, http.Header, map[string]any) {
		t.Helper()
		form := url.Values{"flow": {screen.Data("flow")}, "op": {"verify"}, "credential": {"{}"}}
		req, err := http.NewRequest(http.MethodPost, p.at(screen.Data("post")), strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Forwarded-For", from)
		response, err := client.HTTP.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var answer map[string]any
		if err := json.UnmarshalRead(response.Body, &answer); err != nil {
			t.Fatalf("verify = %d: %v", response.StatusCode, err)
		}
		return response.StatusCode, response.Header, answer
	}

	// One page takes passkey.MaxFlowVerifies tries, then ends.
	screen := open()
	for range passkey.MaxFlowVerifies {
		if status, _, answer := verify(screen, "198.51.100.1"); status != http.StatusBadRequest {
			t.Fatalf("verify = %d %#v", status, answer)
		}
	}
	if status, _, answer := verify(screen, "198.51.100.1"); status != http.StatusTooManyRequests || answer["error"] != "Too many tries. Go back to the app and start again." {
		t.Fatalf("a try too many = %d %#v", status, answer)
	}
	if status, _ := p.script(t, client, screen, "options", nil); status != http.StatusNotFound {
		t.Fatalf("options after the last try = %d", status)
	}

	// One client takes passkey.ClientVerifyBurst tries, across pages; then
	// it waits, and another client does not.
	for i := range passkey.ClientVerifyBurst {
		if i%passkey.MaxFlowVerifies == 0 {
			screen = open()
		}
		if status, _, answer := verify(screen, "198.51.100.2"); status != http.StatusBadRequest {
			t.Fatalf("verify %d = %d %#v", i, status, answer)
		}
	}
	screen = open()
	if status, header, answer := verify(screen, "198.51.100.2"); status != http.StatusTooManyRequests || header.Get("Retry-After") == "" || answer["error"] != "Too many tries. Wait a minute, then try again." {
		t.Fatalf("a client's try too many = %d %v %#v", status, header, answer)
	}
	if status, _, answer := verify(screen, "203.0.113.9"); status != http.StatusBadRequest {
		t.Fatalf("another client = %d %#v", status, answer)
	}
}
