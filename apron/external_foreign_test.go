package apron

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/internal/authn"
	"github.com/apron-chat/apron-server-go/internal/oauthtest"
)

// fakeIssuer is an in-process OAuth authorization server, such as Pocket
// ID, which signs in "user-42" for any app that names itself with an https
// client_id (a client ID metadata document, which it does not fetch), and
// binds access tokens to the resource asked for. Like Pocket ID's, its
// access tokens carry no profile claims; its userinfo endpoint, listed only
// in its OpenID Connect metadata, names the person. Its knobs make the
// tokens it issues, or its userinfo, wrong in one way.
type fakeIssuer struct {
	*httptest.Server
	key, other *ecdsa.PrivateKey

	mu    sync.Mutex
	codes map[string]url.Values
	// claims change the access token's claims; nil values remove them.
	claims map[string]any
	// tokenType replaces Bearer; foreignKey signs with a key not in the
	// issuer's set; typ, if set, replaces the header's at+jwt, and an empty
	// one leaves typ out.
	tokenType  string
	foreignKey bool
	typ        *string
	// issued are the access tokens it issued, which userinfo answers;
	// userinfo is its answer, userinfoStatus replaces 200, and
	// userinfoCalls counts the requests.
	issued         map[string]bool
	userinfo       map[string]any
	userinfoStatus int
	userinfoCalls  int
}

// appClientID is the app's client_id: its client ID metadata document.
const appClientID = "https://app.example/client.json"

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	f := &fakeIssuer{codes: make(map[string]url.Values), issued: make(map[string]bool)}
	f.resetUserinfo()
	f.key, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	f.other, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{
			"issuer": f.URL, "authorization_endpoint": f.URL + "/authorize", "token_endpoint": f.URL + "/token",
			"jwks_uri": f.URL + "/jwks", "code_challenge_methods_supported": []string{"S256"},
		})
	})
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{
			"issuer": f.URL, "authorization_endpoint": f.URL + "/authorize", "token_endpoint": f.URL + "/token",
			"jwks_uri": f.URL + "/jwks", "userinfo_endpoint": f.URL + "/userinfo",
		})
	})
	mux.HandleFunc("GET /userinfo", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.userinfoCalls++
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch {
		case !ok || !f.issued[token]:
			w.WriteHeader(http.StatusUnauthorized)
		case f.userinfoStatus != 0:
			w.WriteHeader(f.userinfoStatus)
		default:
			writeTestJSON(w, f.userinfo)
		}
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{"keys": []any{map[string]any{
			"kty": "EC", "crv": "P-256", "kid": "k1", "use": "sig", "alg": "ES256",
			"x": b64(f.key.X.FillBytes(make([]byte, 32))), "y": b64(f.key.Y.FillBytes(make([]byte, 32))),
		}}})
	})
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if !strings.HasPrefix(query.Get("client_id"), "https://") || query.Get("code_challenge_method") != "S256" || query.Get("scope") != "openid profile" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		code := rand.Text()
		f.mu.Lock()
		f.codes[code] = query
		f.mu.Unlock()
		back, _ := url.Parse(query.Get("redirect_uri"))
		back.RawQuery = url.Values{"code": {code}, "state": {query.Get("state")}, "iss": {f.URL}}.Encode()
		http.Redirect(w, r, back.String(), http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		asked := f.codes[r.FormValue("code")]
		delete(f.codes, r.FormValue("code"))
		f.mu.Unlock()
		digest := sha256.Sum256([]byte(r.FormValue("code_verifier")))
		if asked == nil || asked.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(digest[:]) ||
			asked.Get("redirect_uri") != r.FormValue("redirect_uri") || r.FormValue("client_id") != asked.Get("client_id") {
			w.WriteHeader(http.StatusBadRequest)
			writeTestJSON(w, map[string]any{"error": "invalid_grant"})
			return
		}
		// The token is for the resource the app asked for, which the
		// server sends again.
		resource := asked.Get("resource")
		if r.FormValue("resource") != "" && r.FormValue("resource") != resource {
			w.WriteHeader(http.StatusBadRequest)
			writeTestJSON(w, map[string]any{"error": "invalid_target"})
			return
		}
		writeTestJSON(w, map[string]any{"access_token": f.token(resource, asked.Get("client_id")), "token_type": f.tokenTypeOr("Bearer")})
	})
	f.Server = httptest.NewTLSServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeIssuer) tokenTypeOr(fallback string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tokenType != "" {
		return f.tokenType
	}
	return fallback
}

// resetUserinfo makes userinfo name the person, and answer.
func (f *fakeIssuer) resetUserinfo() {
	f.userinfo = map[string]any{"sub": "user-42", "name": "Pat", "preferred_username": "pat"}
	f.userinfoStatus = 0
}

// token is an ES256 JWT access token for resource, and for the issuer's
// userinfo, as an identity scope makes Pocket ID's, issued to clientID.
func (f *fakeIssuer) token(resource, clientID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	claims := map[string]any{
		"iss": f.URL, "sub": "user-42", "aud": []string{resource, f.URL}, "exp": time.Now().Add(5 * time.Minute).Unix(),
		"iat": time.Now().Unix(), "client_id": clientID,
	}
	for key, value := range f.claims {
		if value == nil {
			delete(claims, key)
		} else {
			claims[key] = value
		}
	}
	fields := map[string]any{"alg": "ES256", "kid": "k1", "typ": "at+jwt"}
	if f.typ != nil {
		fields["typ"] = *f.typ
		if *f.typ == "" {
			delete(fields, "typ")
		}
	}
	header, _ := json.Marshal(fields)
	payload, _ := json.Marshal(claims)
	signed := b64(header) + "." + b64(payload)
	key := f.key
	if f.foreignKey {
		key = f.other
	}
	digest := sha256.Sum256([]byte(signed))
	r, s, _ := ecdsa.Sign(rand.Reader, key, digest[:])
	token := signed + "." + b64(append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...))
	f.issued[token] = true
	return token
}

func writeTestJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	body, _ := json.Marshal(value)
	_, _ = w.Write(body)
}

func foreignTestServer(t *testing.T) (*Server, *httptest.Server, *fakeIssuer) {
	t.Helper()
	issuer := newFakeIssuer(t)
	config := DefaultConfig()
	config.ExternalIssuer = &ExternalIssuer{Issuer: issuer.URL, Scope: "openid profile", HTTPClient: issuer.Client()}
	app, httpServer := newTestServer(t, config)
	return app, httpServer, issuer
}

// foreignCode signs in at the issuer as the app would, and returns the code.
func foreignCode(t *testing.T, app *oauthtest.Client, external map[string]any) string {
	t.Helper()
	back := app.Get(app.URL(external["url"].(string), nil))
	return app.Code(back, external["issuer"].(string))
}

// A server that names another authorization server sends apps there, and
// redeems their codes at its token endpoint (§4.12.1, §4.12.4).
func TestExternalForeignIssuer(t *testing.T) {
	app, httpServer, issuer := foreignTestServer(t)
	origin := httpServer.URL

	c, frame := dialRaw(t, httpServer)
	if auth := asList(frame["params"].(map[string]any)["auth"]); !slicesContains(auth, "external") {
		t.Fatalf("auth = %#v", auth)
	}
	if capabilities := asList(frame["params"].(map[string]any)["capabilities"]); !slicesContains(capabilities, "ext:oauth") {
		t.Fatalf("capabilities = %#v", capabilities)
	}
	external := c.result(t, "auth", "b", map[string]any{"scheme": "external"})["external"].(map[string]any)
	page, _ := url.Parse(external["url"].(string))
	// The URL names no client_id: the app names itself (§4.12.2).
	if external["issuer"] != issuer.URL || !strings.HasPrefix(page.String(), issuer.URL+"/authorize?") ||
		page.Query().Has("client_id") || page.Query().Get("scope") != "openid profile" {
		t.Fatalf("external = %#v", external)
	}
	// The server serves no authorization server of its own.
	for _, path := range []string{"/oauth/authorize", "/.well-known/oauth-authorization-server"} {
		if status, _, _ := httpDo(t, http.MethodGet, origin+path, nil, ""); status != http.StatusNotFound {
			t.Fatalf("own %s = %d", path, status)
		}
	}
	// Its protected resource metadata (RFC 9728) names the issuer, as the
	// begin does.
	status, header, body := httpDo(t, http.MethodGet, origin+"/.well-known/oauth-protected-resource", nil, "")
	var resource map[string]any
	if err := json.Unmarshal(body, &resource); status != http.StatusOK || err != nil || header.Get("Access-Control-Allow-Origin") != "*" ||
		len(resource) != 2 || resource["resource"] != origin || len(asList(resource["authorization_servers"])) != 1 || asList(resource["authorization_servers"])[0] != external["issuer"] {
		t.Fatalf("protected resource metadata = %d %v %s", status, header, body)
	}

	client := oauthtest.New(t, origin)
	client.ClientID = appClientID
	client.HTTP.Transport = issuer.Client().Transport
	_, result := c.request(t, "auth", "a", redemption(client, foreignCode(t, client, external)))
	// The token names nobody; userinfo, found in the OpenID Connect
	// metadata, names the new account.
	you := result["you"].(map[string]any)
	if you["user_id"] != "pat" || you["name"] != "Pat" || result["token"] == nil {
		t.Fatalf("sign-in = %#v", result)
	}
	app.mu.RLock()
	account := app.accounts[authn.Identity{Scheme: "external", Subject: issuer.URL + "#user-42"}]
	app.mu.RUnlock()
	if account == nil || account.id != "pat" {
		t.Fatalf("the identity is the issuer and sub: %#v", account)
	}

	// A client that redeemed the code itself presents the access token.
	code := foreignCode(t, client, external)
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {client.Verifier}, "redirect_uri": {client.RedirectURI}, "client_id": {appClientID}}
	response, err := issuer.Client().PostForm(issuer.URL+"/token", form)
	if err != nil {
		t.Fatal(err)
	}
	var token map[string]any
	_ = json.UnmarshalRead(response.Body, &token)
	response.Body.Close()
	again, _ := dialRaw(t, httpServer)
	if _, result := again.request(t, "auth", "t", map[string]any{"scheme": "external", "token": token["access_token"]}); result["you"].(map[string]any)["user_id"] != "pat" {
		t.Fatalf("token sign-in = %#v", result)
	}

	// Tokens the server must refuse.
	for name, spoil := range map[string]func(){
		"another audience": func() { issuer.claims = map[string]any{"aud": []string{"https://other.example"}} },
		"no audience":      func() { issuer.claims = map[string]any{"aud": nil} },
		"expired":          func() { issuer.claims = map[string]any{"exp": time.Now().Add(-time.Hour).Unix()} },
		"no expiry":        func() { issuer.claims = map[string]any{"exp": nil} },
		"another issuer":   func() { issuer.claims = map[string]any{"iss": "https://evil.example"} },
		"no subject":       func() { issuer.claims = map[string]any{"sub": ""} },
		"bound to a key":   func() { issuer.claims = map[string]any{"cnf": map[string]any{"jkt": "abc"}} },
		"foreign key":      func() { issuer.foreignKey = true },
		"DPoP token type":  func() { issuer.tokenType = "DPoP" },
		// An ID token, signed with the same keys, is no access token
		// (RFC 9068 §2.1), even addressed to this server.
		"an ID token": func() { issuer.typ = new("JWT") },
		"no typ":      func() { issuer.typ = new("") },
	} {
		t.Run(name, func(t *testing.T) {
			issuer.mu.Lock()
			spoil()
			issuer.mu.Unlock()
			defer func() {
				issuer.mu.Lock()
				issuer.claims, issuer.foreignKey, issuer.tokenType, issuer.typ = nil, false, "", nil
				issuer.mu.Unlock()
			}()
			fresh, _ := dialRaw(t, httpServer)
			fresh.expectError(t, "auth", "a", redemption(client, foreignCode(t, client, external)), CodeDenied)
		})
	}
	// An ID token the client got itself is refused as a token too; the
	// access token's typ is matched in any case, with or without
	// application/.
	setTyp := func(typ *string) {
		issuer.mu.Lock()
		issuer.typ = typ
		issuer.mu.Unlock()
	}
	setTyp(new("JWT"))
	idToken := issuer.token(origin, appClientID)
	setTyp(nil)
	idCheck, _ := dialRaw(t, httpServer)
	idCheck.expectError(t, "auth", "id", map[string]any{"scheme": "external", "token": idToken}, CodeDenied)
	for _, typ := range []string{"AT+JWT", "application/at+jwt"} {
		setTyp(&typ)
		typed, _ := dialRaw(t, httpServer)
		typed.result(t, "auth", "a", redemption(client, foreignCode(t, client, external)))
	}
	setTyp(nil)

	// A used code, and a token that is no JWT, are refused too.
	code = foreignCode(t, client, external)
	used, _ := dialRaw(t, httpServer)
	used.result(t, "auth", "a", redemption(client, code))
	used2, _ := dialRaw(t, httpServer)
	used2.expectError(t, "auth", "a", redemption(client, code), CodeDenied)
	used2.expectError(t, "auth", "n", map[string]any{"scheme": "external", "token": "eyJhbGciOiJub25lIn0.e30."}, CodeDenied)

	// On a signed-in connection, the identity is added, as with the
	// server's own codes; here it belongs to pat already.
	guest, _ := dialRaw(t, httpServer)
	guestAuth(t, guest)
	guest.expectError(t, "auth", "add", redemption(client, foreignCode(t, client, external)), CodeDenied)
}

// A new account takes its name from the issuer's userinfo, asked with the
// access token, when the token names nobody; a failure there still signs
// in, without a name, and an existing account is not looked up.
func TestExternalForeignIssuerUserinfo(t *testing.T) {
	_, httpServer, issuer := foreignTestServer(t)
	c, _ := dialRaw(t, httpServer)
	external := c.result(t, "auth", "b", map[string]any{"scheme": "external"})["external"].(map[string]any)
	client := oauthtest.New(t, httpServer.URL)
	client.ClientID = appClientID
	client.HTTP.Transport = issuer.Client().Transport
	signIn := func(sub string, change func()) (map[string]any, int) {
		t.Helper()
		issuer.mu.Lock()
		issuer.resetUserinfo()
		issuer.claims = map[string]any{"sub": sub}
		issuer.userinfo["sub"] = sub
		change()
		before := issuer.userinfoCalls
		issuer.mu.Unlock()
		fresh, _ := dialRaw(t, httpServer)
		_, result := fresh.request(t, "auth", "a", redemption(client, foreignCode(t, client, external)))
		issuer.mu.Lock()
		defer issuer.mu.Unlock()
		return result["you"].(map[string]any), issuer.userinfoCalls - before
	}

	if you, calls := signIn("user-1", func() {}); you["user_id"] != "pat" || you["name"] != "Pat" || calls != 1 {
		t.Fatalf("named by userinfo: %#v, %d calls", you, calls)
	}
	// The same identity again: its account, with no lookup.
	if you, calls := signIn("user-1", func() { issuer.userinfo["name"] = "Changed" }); you["user_id"] != "pat" || you["name"] != "Pat" || calls != 0 {
		t.Fatalf("existing account: %#v, %d calls", you, calls)
	}
	// userinfo failing, or answering for someone else, names nobody.
	for sub, change := range map[string]func(){
		"user-2": func() { issuer.userinfoStatus = http.StatusInternalServerError },
		"user-3": func() { issuer.userinfo["sub"] = "user-1" },
		"user-4": func() { issuer.userinfo = map[string]any{"sub": "user-4", "name": 7} },
	} {
		you, calls := signIn(sub, change)
		if id, _ := you["user_id"].(string); !strings.HasPrefix(id, "user_") || you["name"] != nil || calls != 1 {
			t.Errorf("%s: %#v, %d calls", sub, you, calls)
		}
	}
	// A token that names the person needs no lookup.
	if you, calls := signIn("user-5", func() {
		issuer.claims["name"], issuer.claims["preferred_username"] = "Quinn", "quinn"
	}); you["user_id"] != "quinn" || you["name"] != "Quinn" || calls != 0 {
		t.Fatalf("named by the token: %#v, %d calls", you, calls)
	}
}

// No extension may offer external sign-in beside an issuer.
func TestExternalForeignIssuerExcludesExtensions(t *testing.T) {
	config := extensionConfig(testExtension{"sso", func(r *Registrar) error {
		r.ExternalSignIn(func(http.ResponseWriter, *http.Request, ExternalRequest) {})
		return nil
	}})
	config.ExternalIssuer = &ExternalIssuer{Issuer: "https://id.example"}
	if app, err := Open(config); err == nil {
		_ = app.Shutdown(context.Background())
		t.Fatal("Open succeeded")
	}
	config = DefaultConfig()
	config.ExternalIssuer = &ExternalIssuer{Issuer: "http://id.example"}
	if app, err := Open(config); err == nil {
		_ = app.Shutdown(context.Background())
		t.Fatal("an http issuer was accepted")
	}
}

func slicesContains(list []any, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
