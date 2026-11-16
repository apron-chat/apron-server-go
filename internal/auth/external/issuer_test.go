package external

import (
	"context"
	"encoding/json/v2"
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
