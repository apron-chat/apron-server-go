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
// ID, which signs in "user-42" for any app with its public client_id, and
// binds access tokens to the resource asked for. Its knobs make the tokens
// it issues wrong in one way.
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
}

const publicClientID = "apron-public"

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	f := &fakeIssuer{codes: make(map[string]url.Values)}
	f.key, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	f.other, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{
			"issuer": f.URL, "authorization_endpoint": f.URL + "/authorize", "token_endpoint": f.URL + "/token",
			"jwks_uri": f.URL + "/jwks", "code_challenge_methods_supported": []string{"S256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{"keys": []any{map[string]any{
			"kty": "EC", "crv": "P-256", "kid": "k1", "use": "sig", "alg": "ES256",
			"x": b64(f.key.X.FillBytes(make([]byte, 32))), "y": b64(f.key.Y.FillBytes(make([]byte, 32))),
		}}})
	})
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("client_id") != publicClientID || query.Get("code_challenge_method") != "S256" || query.Get("scope") != "openid" {
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
			asked.Get("redirect_uri") != r.FormValue("redirect_uri") || r.FormValue("client_id") != publicClientID {
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
		writeTestJSON(w, map[string]any{"access_token": f.token(resource), "token_type": f.tokenTypeOr("Bearer")})
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

// token is an ES256 JWT access token for resource.
func (f *fakeIssuer) token(resource string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	claims := map[string]any{
		"iss": f.URL, "sub": "user-42", "aud": []string{resource}, "exp": time.Now().Add(5 * time.Minute).Unix(),
		"iat": time.Now().Unix(), "name": "Pat", "preferred_username": "pat", "client_id": publicClientID,
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
	return signed + "." + b64(append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...))
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
	config.ExternalIssuer = &ExternalIssuer{Issuer: issuer.URL, ClientID: publicClientID, Scope: "openid", HTTPClient: issuer.Client()}
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
	external := c.result(t, "auth", "b", map[string]any{"scheme": "external"})["external"].(map[string]any)
	page, _ := url.Parse(external["url"].(string))
	if external["issuer"] != issuer.URL || !strings.HasPrefix(page.String(), issuer.URL+"/authorize?") ||
		page.Query().Get("client_id") != publicClientID || page.Query().Get("scope") != "openid" {
		t.Fatalf("external = %#v", external)
	}
	// The server serves no authorization server of its own.
	if status, _, _ := httpDo(t, http.MethodGet, origin+"/oauth/authorize", nil, ""); status != http.StatusNotFound {
		t.Fatalf("own page = %d", status)
	}

	client := oauthtest.New(t, origin)
	client.HTTP.Transport = issuer.Client().Transport
	_, result := c.request(t, "auth", "a", redemption(client, foreignCode(t, client, external)))
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
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {client.Verifier}, "redirect_uri": {client.RedirectURI}, "client_id": {publicClientID}}
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
	idToken := issuer.token(origin)
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
