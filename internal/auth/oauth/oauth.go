// Package oauth is what the server's sign-in pages share as OAuth 2.1
// authorization endpoints (PROTOCOL.md §4.12.3): the external sign-in page
// at /oauth/authorize and the passkey page at /passkey. It reads and checks
// an authorization request, names its client the way the confirmation
// shows it, issues codes bound to the request and redeems them with PKCE,
// redirects back, and renders the pages with the headers they need.
//
// Each page keeps codes of its own (Codes), so a code from one page never
// finishes another page's scheme.
package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// CodeLifetime is how long a code may be redeemed (§4.12.3: within
	// minutes).
	CodeLifetime = 5 * time.Minute
	// FlowLifetime is how long a page's own state lasts while a person is
	// on it, such as between its first screen and the identity provider's
	// answer.
	FlowLifetime = 10 * time.Minute
	// MaxEntries bounds each table of codes or flows.
	MaxEntries = 10_000
	// maxParamBytes bounds each parameter of an authorization request.
	maxParamBytes = 2048
)

// ErrBusy is returned when a table is full of entries that have not
// expired.
var ErrBusy = errors.New("oauth: too many sign-ins in progress")

// ErrInvalidGrant is returned for a code that is unknown, used, expired,
// or redeemed with the wrong verifier, redirect_uri, or client_id.
var ErrInvalidGrant = errors.New("oauth: the code is invalid, used, or expired")

// Request is a checked authorization request (§4.12.2).
type Request struct {
	ClientID, RedirectURI string
	// Challenge is the PKCE S256 code_challenge.
	Challenge string
	State     string
	// Resource is the server's origin, as the client sent it.
	Resource string
	// Issuer is the server's origin, sent back as iss (RFC 9207).
	Issuer string
}

// Problem is why an authorization request is refused. The page shows it
// (RenderProblem), and never redirects it, even once the redirect_uri is
// known to be good: any client may name an https redirect_uri, so a page
// that sent errors there would redirect anywhere for anyone. The page
// redirects only after the person acts, with a code or with
// error=access_denied (§4.12.3).
type Problem struct {
	// Code is the OAuth error, such as invalid_request, for the client's
	// developer.
	Code string
	// Description explains it to the person on the page.
	Description string
}

// Resources are the values of resource that name a server whose origin is
// issuer: the issuer itself and, for a server on http, https:// with the
// same host. §4.12.1 gives a ws:// address on a loopback host an http://
// origin, but clients written to its earlier drafts, which gave every
// address an https:// origin, send https://.
func Resources(issuer string) []string {
	resources := []string{issuer}
	if parsed, err := url.Parse(issuer); err == nil && parsed.Scheme == "http" {
		resources = append(resources, "https://"+parsed.Host)
	}
	return resources
}

// Origin is the scheme and host of a base URL, such as https://chat.example
// for https://chat.example/apron.
func Origin(base string) string {
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" {
		return base
	}
	return parsed.Scheme + "://" + parsed.Host
}

// EncodeQuery encodes query as url.Values.Encode does, but writes a space
// as %20, not +, for a URL a client opens: some clients decode the query and
// encode it again, and read + as itself. Encode already writes a + as %2B,
// so every + it leaves is a space.
func EncodeQuery(query url.Values) string {
	return strings.ReplaceAll(query.Encode(), "+", "%20")
}

// Parse reads an authorization request from a page's query, for a server
// whose origin is issuer.
func Parse(query url.Values, issuer string) (*Request, *Problem) {
	single := func(name string) (string, bool) {
		values := query[name]
		if len(values) > 1 || len(values) == 1 && len(values[0]) > maxParamBytes {
			return "", false
		}
		if len(values) == 0 {
			return "", true
		}
		return values[0], true
	}
	refuse := func(code, description string) (*Request, *Problem) {
		return nil, &Problem{Code: code, Description: "The app that sent you here " + description}
	}
	clientID, ok := single("client_id")
	if !ok || clientID == "" {
		return refuse("invalid_request", "did not say who it is (client_id).")
	}
	redirectURI, ok := single("redirect_uri")
	if !ok || !AllowedRedirect(redirectURI) {
		return refuse("invalid_request", "asked to return to an address this server does not send sign-ins to (redirect_uri).")
	}
	r := &Request{ClientID: clientID, RedirectURI: redirectURI, Issuer: issuer}
	if state, ok := single("state"); ok {
		r.State = state
	}
	if r.State == "" {
		return refuse("invalid_request", "did not name its sign-in (state is required).")
	}
	if value, ok := single("response_type"); !ok || value != "code" {
		return refuse("unsupported_response_type", "asked for a kind of sign-in this server does not offer (response_type must be code).")
	}
	if value, ok := single("code_challenge_method"); !ok || value != "S256" {
		return refuse("invalid_request", "did not protect its sign-in the way this server needs (code_challenge_method must be S256).")
	}
	challenge, ok := single("code_challenge")
	if !ok || !validChallenge(challenge) {
		return refuse("invalid_request", "did not protect its sign-in the way this server needs (code_challenge must be a base64url SHA-256 digest).")
	}
	r.Challenge = challenge
	resource, ok := single("resource")
	accepted := false
	for _, candidate := range Resources(issuer) {
		accepted = accepted || ok && resource == candidate
	}
	if !accepted {
		return refuse("invalid_target", "asked to sign in to another server (resource must be this server's origin, "+issuer+").")
	}
	r.Resource = resource
	return r, nil
}

// validChallenge reports whether a code_challenge is an S256 digest: 32
// bytes in unpadded base64url.
func validChallenge(challenge string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(challenge)
	return err == nil && len(decoded) == sha256.Size
}

// AllowedRedirect reports whether a redirect_uri is one any client may use
// (§4.12.3): an https URL, a private-use scheme with a "." in it, or http on
// a loopback address with any port; none with a fragment or credentials.
func AllowedRedirect(redirectURI string) bool {
	parsed, err := url.Parse(redirectURI)
	if err != nil || parsed.Fragment != "" || strings.Contains(redirectURI, "#") || parsed.User != nil {
		return false
	}
	switch scheme := strings.ToLower(parsed.Scheme); {
	case scheme == "https":
		return parsed.Host != ""
	case scheme == "http":
		return isLoopback(parsed.Hostname())
	case strings.Contains(scheme, "."):
		// A private-use scheme, such as com.example.app:/signed-in (RFC
		// 8252 §7.1).
		return true
	}
	return false
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.Equal(net.IPv4(127, 0, 0, 1)) || ip.Equal(net.IPv6loopback))
}

// isLoopbackHost reports whether host names this device, for remembering:
// localhost and its subdomains, or any loopback address.
func isLoopbackHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Client names who is asking, as the confirmation shows it at the start
// of a sentence: an https redirect's host, "An app on this device
// (com.example.app)" for a private-use scheme, or "An app on this device"
// for a loopback address.
func (r *Request) Client() string {
	parsed, _ := url.Parse(r.RedirectURI)
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		return parsed.Host
	case "http":
		return "An app on this device"
	}
	return "An app on this device (" + parsed.Scheme + ")"
}

// ClientInline is Client within a sentence, such as "Allow an app on this
// device (com.example.app) to use …".
func (r *Request) ClientInline() string {
	client := r.Client()
	if rest, ok := strings.CutPrefix(client, "An app"); ok {
		return "an app" + rest
	}
	return client
}

// Started names where the person would have started signing in, for "Only
// continue if you just started signing in on …": an https redirect's
// host, or "an app on this device".
func (r *Request) Started() string {
	parsed, _ := url.Parse(r.RedirectURI)
	if strings.ToLower(parsed.Scheme) == "https" {
		return parsed.Host
	}
	return "an app on this device"
}

// RememberedRedirect is the redirect_uri, with its scheme and host
// lowercased, under which a page may remember that an account allowed this
// request's client (§4.12.3): an https redirect, not on a loopback host. It
// is the whole redirect_uri, not its host, so that another page on the same
// host is asked again. It is empty for an app's scheme or a loopback
// address, which any app on the device can claim, so a page never
// remembers them.
func (r *Request) RememberedRedirect() string {
	parsed, err := url.Parse(r.RedirectURI)
	if err != nil || strings.ToLower(parsed.Scheme) != "https" || parsed.Host == "" || isLoopbackHost(strings.ToLower(parsed.Hostname())) {
		return ""
	}
	parsed.Scheme, parsed.Host = "https", strings.ToLower(parsed.Host)
	return parsed.String()
}

// Grant is what a code is bound to (§4.12.3).
func (r *Request) Grant() Grant {
	return Grant{ClientID: r.ClientID, RedirectURI: r.RedirectURI, Challenge: r.Challenge, Resource: r.Resource}
}

// ReturnURL is the client's redirect_uri with params, the request's state,
// and iss.
func (r *Request) ReturnURL(params url.Values) string {
	target, _ := url.Parse(r.RedirectURI)
	query := target.Query()
	for name, values := range params {
		query[name] = values
	}
	if r.State != "" {
		query.Set("state", r.State)
	}
	query.Set("iss", r.Issuer)
	target.RawQuery = query.Encode()
	return target.String()
}

// Return redirects the person's browser to the client with params, the
// request's state, and iss.
func (r *Request) Return(w http.ResponseWriter, req *http.Request, params url.Values) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, req, r.ReturnURL(params), http.StatusSeeOther)
}

// Deny returns to the client with error=access_denied: the person
// declined or cancelled.
func (r *Request) Deny(w http.ResponseWriter, req *http.Request) {
	r.Return(w, req, url.Values{"error": {"access_denied"}})
}

// Grant binds a code to its request (§4.12.3).
type Grant struct {
	ClientID, RedirectURI, Challenge, Resource string
}

// Codes are a page's authorization codes: unguessable, single use, and
// expiring after CodeLifetime, each holding what redeeming it proves.
type Codes struct {
	table Table[code]
}

type code struct {
	grant Grant
	value any
}

// Issue makes a code for a request, which redeeming hands back value.
func (c *Codes) Issue(grant Grant, value any, now time.Time) (string, error) {
	return c.table.Put(code{grant: grant, value: value}, CodeLifetime, now)
}

// Redeem uses up a code, and returns what it holds if verifier, redirectURI,
// and clientID match its request (RFC 7636 §4.6). Any attempt uses it up.
func (c *Codes) Redeem(value, verifier, redirectURI, clientID string, now time.Time) (any, Grant, error) {
	issued, ok := c.table.Take(value, now)
	if !ok || issued.grant.RedirectURI != redirectURI || issued.grant.ClientID != clientID || !VerifyPKCE(verifier, issued.grant.Challenge) {
		return nil, Grant{}, ErrInvalidGrant
	}
	return issued.value, issued.grant, nil
}

// VerifyPKCE reports whether verifier is the code_verifier of an S256
// challenge.
func VerifyPKCE(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	for _, r := range verifier {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("-._~", r)) {
			return false
		}
	}
	digest := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(digest[:])), []byte(challenge)) == 1
}

// Table holds values under unguessable IDs until they expire, at most
// MaxEntries of them.
type Table[V any] struct {
	mu      sync.Mutex
	entries map[string]entry[V]
}

type entry[V any] struct {
	value   V
	expires time.Time
}

// Put keeps value for ttl under a new ID, or fails with ErrBusy when the
// table is full of live entries.
func (t *Table[V]) Put(value V, ttl time.Duration, now time.Time) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = make(map[string]entry[V])
	}
	if len(t.entries) >= MaxEntries {
		for id, e := range t.entries {
			if !now.Before(e.expires) {
				delete(t.entries, id)
			}
		}
		if len(t.entries) >= MaxEntries {
			return "", ErrBusy
		}
	}
	id := rand.Text() + rand.Text()
	t.entries[id] = entry[V]{value: value, expires: now.Add(ttl)}
	return id, nil
}

// Get returns the live value under id.
func (t *Table[V]) Get(id string, now time.Time) (V, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[id]
	if ok && !now.Before(e.expires) {
		delete(t.entries, id)
		ok = false
	}
	if !ok {
		var zero V
		return zero, false
	}
	return e.value, true
}

// Take returns the live value under id, and forgets it.
func (t *Table[V]) Take(id string, now time.Time) (V, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[id]
	delete(t.entries, id)
	if !ok || !now.Before(e.expires) {
		var zero V
		return zero, false
	}
	return e.value, true
}

// Delete forgets the value under id.
func (t *Table[V]) Delete(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, id)
}

// Len counts the entries, live or not, for tests.
func (t *Table[V]) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.entries)
}
