package external

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
)

// discover finds the issuer's metadata in its RFC 8414 document or its
// OpenID Connect one, whichever it serves, and takes userinfo_endpoint
// from the second when the first lacks it.
func TestForeignDiscover(t *testing.T) {
	const tenant = "/tenant"
	oauthPath := "/.well-known/oauth-authorization-server" + tenant
	oidcPath := tenant + "/.well-known/openid-configuration"
	for _, test := range []struct {
		name string
		// serve changes each document, by path; nil serves none.
		serve    map[string]func(issuer string, doc map[string]any) map[string]any
		userinfo string
		asked    []string
		fails    bool
	}{
		{
			name: "RFC 8414 only",
			serve: map[string]func(string, map[string]any) map[string]any{
				oauthPath: func(_ string, doc map[string]any) map[string]any { return doc },
			},
			asked: []string{oauthPath, oidcPath},
		},
		{
			name: "OpenID Connect only",
			serve: map[string]func(string, map[string]any) map[string]any{
				oidcPath: func(issuer string, doc map[string]any) map[string]any {
					doc["userinfo_endpoint"] = issuer + "/userinfo"
					return doc
				},
			},
			userinfo: "/userinfo",
			asked:    []string{oauthPath, oidcPath},
		},
		{
			name: "both, userinfo only in OpenID Connect's",
			serve: map[string]func(string, map[string]any) map[string]any{
				oauthPath: func(_ string, doc map[string]any) map[string]any { return doc },
				oidcPath: func(issuer string, doc map[string]any) map[string]any {
					doc["userinfo_endpoint"] = issuer + "/userinfo"
					return doc
				},
			},
			userinfo: "/userinfo",
			asked:    []string{oauthPath, oidcPath},
		},
		{
			name: "both, userinfo in RFC 8414's",
			serve: map[string]func(string, map[string]any) map[string]any{
				oauthPath: func(issuer string, doc map[string]any) map[string]any {
					doc["userinfo_endpoint"] = issuer + "/userinfo"
					return doc
				},
				oidcPath: func(_ string, doc map[string]any) map[string]any { return doc },
			},
			userinfo: "/userinfo",
			asked:    []string{oauthPath},
		},
		{
			name: "RFC 8414's for another issuer",
			serve: map[string]func(string, map[string]any) map[string]any{
				oauthPath: func(_ string, doc map[string]any) map[string]any {
					doc["issuer"] = "https://other.example"
					return doc
				},
				oidcPath: func(_ string, doc map[string]any) map[string]any { return doc },
			},
			asked: []string{oauthPath, oidcPath},
		},
		{
			name:  "neither",
			asked: []string{oauthPath, oidcPath},
			fails: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			var asked []string
			var server *httptest.Server
			server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				asked = append(asked, r.URL.Path)
				mu.Unlock()
				change := test.serve[r.URL.Path]
				if change == nil {
					http.NotFound(w, r)
					return
				}
				issuer := server.URL + tenant
				body, _ := json.Marshal(change(issuer, map[string]any{
					"issuer": issuer, "authorization_endpoint": issuer + "/authorize",
					"token_endpoint": issuer + "/token", "jwks_uri": issuer + "/jwks",
				}))
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(body)
			}))
			defer server.Close()
			issuer := server.URL + tenant
			f, err := NewForeign(issuer, "", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			metadata, problem := f.discover(context.Background())
			if !slices.Equal(asked, test.asked) {
				t.Errorf("asked %v, want %v", asked, test.asked)
			}
			if test.fails {
				if problem == nil {
					t.Fatalf("metadata = %#v", metadata)
				}
				return
			}
			if problem != nil || metadata.Issuer != issuer || metadata.AuthorizationEndpoint != issuer+"/authorize" || metadata.TokenEndpoint != issuer+"/token" {
				t.Fatalf("metadata = %#v, %v", metadata, problem)
			}
			if want := map[bool]string{true: issuer + test.userinfo, false: ""}[test.userinfo != ""]; metadata.UserinfoEndpoint != want {
				t.Errorf("userinfo_endpoint = %q, want %q", metadata.UserinfoEndpoint, want)
			}
		})
	}
}
