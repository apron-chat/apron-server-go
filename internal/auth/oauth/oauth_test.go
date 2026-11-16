package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
	"time"
)

func challengeOf(verifier string) string {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func TestAllowedRedirect(t *testing.T) {
	for redirect, want := range map[string]bool{
		"https://chat.alice.dev/callback": true,
		"https://chat.alice.dev":          true,
		"com.example.app:/signed-in":      true,
		"com.example.app://signed-in":     true,
		"http://127.0.0.1:53682/cb":       true,
		"http://[::1]:8000/cb":            true,
		"http://localhost/cb":             true,
		"http://chat.alice.dev/cb":        false,
		"http://127.0.0.2/cb":             false,
		"myapp:/cb":                       false,
		"javascript:alert(1)":             false,
		"data:text/html,x":                false,
		"https:///nohost":                 false,
		"https://a.example/cb#frag":       false,
		"https://user:pw@a.example/cb":    false,
		"":                                false,
		"/relative":                       false,
	} {
		if got := AllowedRedirect(redirect); got != want {
			t.Errorf("AllowedRedirect(%q) = %v", redirect, got)
		}
	}
}

func TestParse(t *testing.T) {
	verifier := strings.Repeat("v", 43)
	good := url.Values{
		"response_type": {"code"}, "client_id": {"any"}, "redirect_uri": {"com.example.app:/cb"},
		"code_challenge": {challengeOf(verifier)}, "code_challenge_method": {"S256"}, "state": {"s"},
		"resource": {"https://chat.example"},
	}
	r, problem := Parse(good, "https://chat.example")
	if problem != nil || r.Challenge != challengeOf(verifier) || r.Resource != "https://chat.example" {
		t.Fatalf("Parse = %#v, %#v", r, problem)
	}
	if got := r.ReturnURL(url.Values{"code": {"c"}}); got != "com.example.app:/cb?code=c&iss=https%3A%2F%2Fchat.example&state=s" {
		t.Fatalf("ReturnURL = %s", got)
	}
	// A loopback development server takes its https:// spelling too.
	dev := url.Values{}
	for k, v := range good {
		dev[k] = v
	}
	dev.Set("resource", "https://127.0.0.1:8080")
	if _, problem := Parse(dev, "http://127.0.0.1:8080"); problem != nil {
		t.Fatalf("dev resource: %#v", problem)
	}
	if _, problem := Parse(dev, "https://chat.example"); problem == nil || problem.Code != "invalid_target" {
		t.Fatalf("other resource: %#v", problem)
	}
	dev.Set("resource", "https://chat.example")
	dev.Set("redirect_uri", "https://chat.alice.dev/cb?keep=1")
	if r, _ := Parse(dev, "https://chat.example"); r.Client() != "chat.alice.dev" || r.ReturnURL(nil) != "https://chat.alice.dev/cb?iss=https%3A%2F%2Fchat.example&keep=1&state=s" {
		t.Fatalf("https client: %q %q", r.Client(), r.ReturnURL(nil))
	}
}

func TestCodes(t *testing.T) {
	verifier := strings.Repeat("v", 43)
	grant := Grant{ClientID: "c", RedirectURI: "com.example.app:/cb", Challenge: challengeOf(verifier), Resource: "https://chat.example"}
	var codes Codes
	now := time.Now()

	code, err := codes.Issue(grant, "proof", now)
	if err != nil || len(code) < 40 {
		t.Fatalf("Issue = %q, %v", code, err)
	}
	if value, got, err := codes.Redeem(code, verifier, grant.RedirectURI, grant.ClientID, now); err != nil || value != "proof" || got != grant {
		t.Fatalf("Redeem = %v %v %v", value, got, err)
	}
	if _, _, err := codes.Redeem(code, verifier, grant.RedirectURI, grant.ClientID, now); err != ErrInvalidGrant {
		t.Fatalf("second Redeem = %v", err)
	}

	// A wrong verifier uses the code up.
	code, _ = codes.Issue(grant, "proof", now)
	if _, _, err := codes.Redeem(code, strings.Repeat("w", 43), grant.RedirectURI, grant.ClientID, now); err != ErrInvalidGrant {
		t.Fatalf("wrong verifier = %v", err)
	}
	if _, _, err := codes.Redeem(code, verifier, grant.RedirectURI, grant.ClientID, now); err != ErrInvalidGrant {
		t.Fatalf("after a wrong verifier = %v", err)
	}

	// Codes expire within minutes.
	code, _ = codes.Issue(grant, "proof", now)
	isProof := func(value any) bool { return value == "proof" }
	if !codes.Holds(isProof, now) || codes.Holds(isProof, now.Add(CodeLifetime)) {
		t.Fatal("Holds counts expired codes, or not live ones")
	}
	if _, _, err := codes.Redeem(code, verifier, grant.RedirectURI, grant.ClientID, now.Add(CodeLifetime)); err != ErrInvalidGrant {
		t.Fatalf("expired = %v", err)
	}
	if VerifyPKCE("short", challengeOf("short")) {
		t.Fatal("a short verifier passed")
	}
}

func TestTableBounds(t *testing.T) {
	var table Table[int]
	now := time.Now()
	for i := range MaxEntries {
		if _, err := table.Put(i, time.Minute, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := table.Put(0, time.Minute, now); err != ErrBusy {
		t.Fatalf("full table = %v", err)
	}
	// Expired entries make room.
	if _, err := table.Put(0, time.Minute, now.Add(time.Minute)); err != nil || table.Len() != 1 {
		t.Fatalf("after expiry = %v, %d", err, table.Len())
	}
}

// The pages name the client by its redirect_uri, and remember only an
// https redirect_uri that is not on this device.
func TestClientNames(t *testing.T) {
	for redirect, want := range map[string][4]string{
		"https://Chat.Alice.dev:8443/cb": {"Chat.Alice.dev:8443", "Chat.Alice.dev:8443", "Chat.Alice.dev:8443", "https://chat.alice.dev:8443/cb"},
		"https://chat.alice.dev/cb":      {"chat.alice.dev", "chat.alice.dev", "chat.alice.dev", "https://chat.alice.dev/cb"},
		"com.example.app:/signed-in":     {"An app on this device (com.example.app)", "an app on this device (com.example.app)", "an app on this device", ""},
		"http://127.0.0.1:5173/cb":       {"An app on this device", "an app on this device", "an app on this device", ""},
		"https://localhost:8443/cb":      {"localhost:8443", "localhost:8443", "localhost:8443", ""},
		"https://127.0.0.1/cb":           {"127.0.0.1", "127.0.0.1", "127.0.0.1", ""},
		"https://[::1]:8443/cb":          {"[::1]:8443", "[::1]:8443", "[::1]:8443", ""},
		"https://app.localhost/cb":       {"app.localhost", "app.localhost", "app.localhost", ""},
	} {
		r := &Request{RedirectURI: redirect}
		if got := [4]string{r.Client(), r.ClientInline(), r.Started(), r.RememberedRedirect()}; got != want {
			t.Errorf("%s: %q, want %q", redirect, got, want)
		}
	}
}
