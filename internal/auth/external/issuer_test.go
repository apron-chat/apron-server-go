package external

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/apron-chat/apron-server-go/internal/authn"
)

// testOrigin is the server's origin, the resource its tokens are for.
const testOrigin = "https://chat.example"

// testIssuer is an authorization server for Foreign, over TLS: its
// metadata, key set, token endpoint, and userinfo.
type testIssuer struct {
	*httptest.Server

	mu sync.Mutex
	// noRFC8414 and noOIDC leave out a metadata document; only the OpenID
	// Connect one names userinfo.
	noRFC8414, noOIDC bool
	// keys is the key set; jwksCalls counts its fetches, each jwksDelay long.
	keys      []jose.JSONWebKey
	jwksCalls int
	jwksDelay time.Duration
	// tokenEndpoint, if set, is named instead of the issuer's own;
	// tokenStatus and tokenAnswer are its answer, and tokenRequest the
	// last request to it.
	tokenEndpoint string
	tokenStatus   int
	tokenAnswer   map[string]any
	tokenRequest  *http.Request
}

func newTestIssuer(t *testing.T) *testIssuer {
	t.Helper()
	i := &testIssuer{tokenStatus: http.StatusOK, tokenAnswer: map[string]any{"access_token": "at", "token_type": "Bearer"}}
	write := func(w http.ResponseWriter, v any) {
		body, _ := json.Marshal(v)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
	metadata := func(oidc bool) map[string]any {
		doc := map[string]any{
			"issuer": i.URL, "authorization_endpoint": i.URL + "/authorize",
			"token_endpoint": i.URL + "/token", "jwks_uri": i.URL + "/jwks",
		}
		if i.tokenEndpoint != "" {
			doc["token_endpoint"] = i.tokenEndpoint
		}
		if oidc {
			doc["userinfo_endpoint"] = i.URL + "/userinfo"
		}
		return doc
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		i.mu.Lock()
		defer i.mu.Unlock()
		if i.noRFC8414 {
			http.NotFound(w, r)
			return
		}
		write(w, metadata(false))
	})
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		i.mu.Lock()
		defer i.mu.Unlock()
		if i.noOIDC {
			http.NotFound(w, r)
			return
		}
		write(w, metadata(true))
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		i.mu.Lock()
		i.jwksCalls++
		set, delay := jose.JSONWebKeySet{Keys: i.keys}, i.jwksDelay
		i.mu.Unlock()
		time.Sleep(delay)
		write(w, set)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		i.mu.Lock()
		defer i.mu.Unlock()
		i.tokenRequest = r
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(i.tokenStatus)
		body, _ := json.Marshal(i.tokenAnswer)
		_, _ = w.Write(body)
	})
	mux.HandleFunc("GET /userinfo", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"sub": "user-42", "name": "Pat"})
	})
	i.Server = httptest.NewTLSServer(mux)
	t.Cleanup(i.Close)
	return i
}

// foreign is external sign-in with the issuer.
func (i *testIssuer) foreign(t *testing.T) *Foreign {
	t.Helper()
	f, err := NewForeign(i.URL, "", i.Client())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// The server redeems a code at the issuer as a public client, with PKCE
// and its origin as the resource, in the form alone: HTTP Basic first
// would spend the single-use code.
func TestForeignRedeem(t *testing.T) {
	p := codeParams{code: "c", verifier: "v", redirectURI: "https://app.example/back", clientID: "https://app.example/client.json"}
	i := newTestIssuer(t)
	f := i.foreign(t)
	token, err := f.redeem(context.Background(), p, testOrigin)
	if err != nil || token != "at" {
		t.Fatalf("redeem = %q, %v", token, err)
	}
	r := i.tokenRequest
	want := url.Values{
		"grant_type": {"authorization_code"}, "code": {"c"}, "code_verifier": {"v"},
		"redirect_uri": {p.redirectURI}, "client_id": {p.clientID}, "resource": {testOrigin},
	}
	if r.Header.Get("Authorization") != "" || len(r.PostForm) != len(want) {
		t.Fatalf("token request = %v %v", r.Header, r.PostForm)
	}
	for key, value := range want {
		if r.PostForm.Get(key) != value[0] {
			t.Errorf("%s = %q, want %q", key, r.PostForm.Get(key), value[0])
		}
	}
	// A refused code, an answer without a token, and a token bound to a
	// key, or of no type, are denied.
	for name, answer := range map[string]func(*testIssuer){
		"refused": func(i *testIssuer) {
			i.tokenStatus, i.tokenAnswer = http.StatusBadRequest, map[string]any{"error": "invalid_grant"}
		},
		"no token":      func(i *testIssuer) { delete(i.tokenAnswer, "access_token") },
		"DPoP":          func(i *testIssuer) { i.tokenAnswer["token_type"] = "DPoP" },
		"no token_type": func(i *testIssuer) { delete(i.tokenAnswer, "token_type") },
		"server error":  func(i *testIssuer) { i.tokenStatus = http.StatusInternalServerError },
	} {
		i := newTestIssuer(t)
		answer(i)
		if token, err := i.foreign(t).redeem(context.Background(), p, testOrigin); err == nil || err.Code != authn.CodeDenied {
			t.Errorf("%s: %q, %v", name, token, err)
		}
	}
	// An unreachable token endpoint is for later.
	gone := httptest.NewTLSServer(http.NotFoundHandler())
	gone.Close()
	i = newTestIssuer(t)
	i.tokenEndpoint = gone.URL + "/token"
	if token, err := i.foreign(t).redeem(context.Background(), p, testOrigin); err == nil || err.Code != authn.CodeRetryAfter {
		t.Errorf("unreachable: %q, %v", token, err)
	}
}

// testKey is a signing key, with its public JWK as the issuer lists it.
type testKey struct {
	private any
	alg     jose.SignatureAlgorithm
	public  jose.JSONWebKey
}

func newTestKey(t *testing.T, kid string) testKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return testKey{key, jose.ES256, jose.JSONWebKey{Key: &key.PublicKey, KeyID: kid, Use: "sig"}}
}

// token is an access token for testOrigin from the issuer, signed with k,
// with its claims changed by change.
func (i *testIssuer) token(t *testing.T, k testKey, change ...func(claims map[string]any)) string {
	t.Helper()
	claims := map[string]any{"iss": i.URL, "sub": "user-42", "aud": testOrigin, "exp": time.Now().Add(5 * time.Minute).Unix()}
	for _, c := range change {
		c(claims)
	}
	payload, _ := json.Marshal(claims)
	if k.alg == algEd25519 {
		// go-jose signs with no Ed25519 alg: by hand.
		header, _ := json.Marshal(map[string]any{"alg": k.alg, "kid": k.public.KeyID, "typ": "at+jwt"})
		signed := b64(header) + "." + b64(payload)
		return signed + "." + b64(ed25519.Sign(k.private.(ed25519.PrivateKey), []byte(signed)))
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: k.alg, Key: jose.JSONWebKey{Key: k.private, KeyID: k.public.KeyID}},
		(&jose.SignerOptions{}).WithType("at+jwt"))
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	token, _ := signed.CompactSerialize()
	return token
}

// publish makes keys the issuer's key set.
func (i *testIssuer) publish(keys ...testKey) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.keys = nil
	for _, k := range keys {
		i.keys = append(i.keys, k.public)
	}
}

// fetches is how many times the key set was fetched.
func (i *testIssuer) fetches() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.jwksCalls
}

// accepts reports whether the server takes the token.
func accepts(f *Foreign, token string) bool {
	_, _, err := f.check(context.Background(), token, testOrigin, time.Now())
	return err == nil
}

// age makes the server's key set old enough to fetch again.
func age(f *Foreign) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keyed = f.keyed.Add(-keysRefresh)
}

// An issuer that serves only one of the metadata documents works: its
// keys verify a token, and only the OpenID Connect one names userinfo.
func TestForeignOneMetadataDocument(t *testing.T) {
	for name, only := range map[string]func(*testIssuer){
		"RFC 8414":       func(i *testIssuer) { i.noOIDC = true },
		"OpenID Connect": func(i *testIssuer) { i.noRFC8414 = true },
	} {
		t.Run(name, func(t *testing.T) {
			i := newTestIssuer(t)
			only(i)
			key := newTestKey(t, "k1")
			i.publish(key)
			f := i.foreign(t)
			proof, sub, err := f.check(context.Background(), i.token(t, key), testOrigin, time.Now())
			if err != nil || proof.Subject != i.URL+"#user-42" || sub != "user-42" {
				t.Fatalf("check = %#v %q %v", proof, sub, err)
			}
			if got, _ := f.userinfo(context.Background(), "at", sub); (got == "Pat") != (name == "OpenID Connect") {
				t.Errorf("name from userinfo = %q", got)
			}
		})
	}
}

// A key the issuer rotates in is fetched for the first token that names
// it once the server's key set is a minute old, and not before: a token
// that names an unknown kid costs at most one fetch a minute.
func TestForeignKeyRotation(t *testing.T) {
	i := newTestIssuer(t)
	old, rotated := newTestKey(t, "k1"), newTestKey(t, "k2")
	i.publish(old)
	f := i.foreign(t)
	if !accepts(f, i.token(t, old)) || i.fetches() != 1 {
		t.Fatalf("first token: %d fetches", i.fetches())
	}
	i.publish(rotated)
	if accepts(f, i.token(t, rotated)) || i.fetches() != 1 {
		t.Fatalf("within the minute: %d fetches", i.fetches())
	}
	age(f)
	if !accepts(f, i.token(t, rotated)) || i.fetches() != 2 {
		t.Fatalf("after the minute: %d fetches", i.fetches())
	}
	if accepts(f, i.token(t, old)) || !accepts(f, i.token(t, rotated)) || i.fetches() != 2 {
		t.Fatalf("the old key: %d fetches", i.fetches())
	}
}

// Tokens with unknown kids, which anyone can make, cost the issuer at most
// one key set fetch a minute, however many arrive at once.
func TestForeignUnknownKidsFetchOnce(t *testing.T) {
	i := newTestIssuer(t)
	key := newTestKey(t, "k1")
	i.publish(key)
	f := i.foreign(t)
	if !accepts(f, i.token(t, key)) {
		t.Fatal("token refused")
	}
	age(f)
	i.mu.Lock()
	i.jwksDelay = 50 * time.Millisecond
	i.mu.Unlock()
	before := i.fetches()
	const forged = 20
	var wg sync.WaitGroup
	for n := range forged {
		token := i.token(t, newTestKey(t, fmt.Sprint("forged-", n)))
		wg.Go(func() {
			if accepts(f, token) {
				t.Errorf("forged token %d accepted", n)
			}
		})
	}
	wg.Wait()
	if calls := i.fetches() - before; calls > 1 {
		t.Fatalf("%d forged tokens caused %d key set fetches", forged, calls)
	}
	if !accepts(f, i.token(t, key)) {
		t.Fatal("token refused after the forged ones")
	}
}

// The server uses only the issuer's public signing keys, and no RSA key
// under 2048 bits; Ed25519 keys verify EdDSA and Ed25519 tokens.
func TestForeignKeySet(t *testing.T) {
	rsaKey := func(bits int) testKey {
		key, err := rsa.GenerateKey(rand.Reader, bits)
		if err != nil {
			t.Fatal(err)
		}
		return testKey{key, jose.RS256, jose.JSONWebKey{Key: &key.PublicKey, KeyID: "k", Use: "sig"}}
	}
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	edKey := testKey{private, jose.EdDSA, jose.JSONWebKey{Key: public, KeyID: "k"}}
	encryption := newTestKey(t, "k")
	encryption.public.Use = "enc"
	for name, test := range map[string]struct {
		key testKey
		ok  bool
	}{
		"EC P-256":    {newTestKey(t, "k"), true},
		"EC, use enc": {encryption, false},
		"RSA 2048":    {rsaKey(2048), true},
		"RSA 1024":    {rsaKey(1024), false},
		"Ed25519":     {edKey, true},
		"Ed25519 alg": {testKey{private, algEd25519, edKey.public}, true},
	} {
		i := newTestIssuer(t)
		i.publish(test.key)
		if got := accepts(i.foreign(t), i.token(t, test.key)); got != test.ok {
			t.Errorf("%s: accepted %v, want %v", name, got, test.ok)
		}
	}
}

// A token is the issuer's, for this server, for a subject, and current
// within a minute's leeway, and bound to no key.
func TestForeignClaims(t *testing.T) {
	i := newTestIssuer(t)
	key := newTestKey(t, "k1")
	i.publish(key)
	f := i.foreign(t)
	set := func(name string, value any) func(map[string]any) {
		return func(claims map[string]any) {
			if value == nil {
				delete(claims, name)
			} else {
				claims[name] = value
			}
		}
	}
	in := func(d time.Duration) int64 { return time.Now().Add(d).Unix() }
	for name, test := range map[string]struct {
		change func(map[string]any)
		ok     bool
	}{
		"as issued":            {func(map[string]any) {}, true},
		"other issuer":         {set("iss", "https://other.example"), false},
		"no sub":               {set("sub", nil), false},
		"empty sub":            {set("sub", ""), false},
		"no exp":               {set("exp", nil), false},
		"expired":              {set("exp", in(-2*time.Minute)), false},
		"expired, in leeway":   {set("exp", in(-30*time.Second)), true},
		"fractional exp":       {set("exp", float64(in(time.Minute))+0.5), true},
		"nbf ahead":            {set("nbf", in(2*time.Minute)), false},
		"nbf ahead, in leeway": {set("nbf", in(30*time.Second)), true},
		// Only exp and nbf bound the time: an iat ahead is no reason.
		"iat ahead":        {set("iat", in(5*time.Minute)), true},
		"no aud":           {set("aud", nil), false},
		"other aud":        {set("aud", "https://other.example"), false},
		"aud with a path":  {set("aud", testOrigin+"/"), false},
		"aud among others": {set("aud", []string{"https://other.example", testOrigin}), true},
		"aud not a string": {set("aud", 1), false},
		"cnf":              {set("cnf", map[string]any{"jkt": "x"}), false},
		"cnf null":         {set("cnf", jsontext.Value("null")), false},
	} {
		if got := accepts(f, i.token(t, key, test.change)); got != test.ok {
			t.Errorf("%s: accepted %v, want %v", name, got, test.ok)
		}
	}
}
