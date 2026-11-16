// Package oauthtest drives the server's sign-in pages in tests as a client
// and its user's browser would (PROTOCOL.md §4.12.2): it builds the
// authorization request with PKCE and state, follows the pages and their
// forms, and stops at the redirect back to the client.
package oauthtest

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// Client is one app signing in, with one authorization request.
type Client struct {
	t    *testing.T
	HTTP *http.Client
	// ClientID and RedirectURI identify the app; Resource is the server's
	// origin; Verifier and State are this request's.
	ClientID, RedirectURI, Resource string
	Verifier, State                 string
}

// New is an app on this device, com.example.app, signing in to the server
// whose origin is resource.
func New(t *testing.T, resource string) *Client {
	c := &Client{
		t: t, ClientID: "com.example.app", RedirectURI: "com.example.app:/signed-in", Resource: resource,
		Verifier: rand.Text() + rand.Text(), State: rand.Text(),
	}
	c.HTTP = &http.Client{CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		// The browser stops being ours once it returns to the app.
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" || strings.HasPrefix(req.URL.String(), c.RedirectURI) {
			return http.ErrUseLastResponse
		}
		return nil
	}}
	return c
}

// Challenge is the S256 code_challenge of the verifier.
func (c *Client) Challenge() string {
	digest := sha256.Sum256([]byte(c.Verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// URL is the sign-in page a begin result named, with the app's parameters
// added to its own query, as §4.12.2 says, and overrides applied. A
// client_id in the page's query becomes the app's.
func (c *Client) URL(page string, overrides url.Values) string {
	parsed, err := url.Parse(page)
	if err != nil {
		c.t.Fatal(err)
	}
	query := parsed.Query()
	// The app redeems with the client_id it sent: the page's own, if it
	// names one (§4.12.1).
	if id := query.Get("client_id"); id != "" {
		c.ClientID = id
	}
	for name, value := range map[string]string{
		"response_type": "code", "client_id": c.ClientID, "redirect_uri": c.RedirectURI,
		"code_challenge": c.Challenge(), "code_challenge_method": "S256", "state": c.State, "resource": c.Resource,
	} {
		if name != "client_id" || !query.Has(name) {
			query.Set(name, value)
		}
	}
	for name, values := range overrides {
		if len(values) == 0 {
			query.Del(name)
		} else {
			query[name] = values
		}
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// Response is where the browser is: a page, or a redirect back to the app.
type Response struct {
	Status int
	Header http.Header
	Body   string
	// URL is the page's address, which its form's action resolves against.
	URL *url.URL
	// Return is the redirect back to the app, if it is one.
	Return *url.URL
}

// Get opens a page.
func (c *Client) Get(page string) Response {
	c.t.Helper()
	response, err := c.HTTP.Get(page)
	if err != nil {
		c.t.Fatal(err)
	}
	return c.read(response)
}

// Post posts a form to a page.
func (c *Client) Post(page string, form url.Values) Response {
	c.t.Helper()
	response, err := c.HTTP.PostForm(page, form)
	if err != nil {
		c.t.Fatal(err)
	}
	return c.read(response)
}

func (c *Client) read(response *http.Response) Response {
	c.t.Helper()
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	r := Response{Status: response.StatusCode, Header: response.Header, Body: string(body), URL: response.Request.URL}
	if location := response.Header.Get("Location"); location != "" && strings.HasPrefix(location, c.RedirectURI) {
		r.Return, _ = url.Parse(location)
	}
	return r
}

var (
	formAction  = regexp.MustCompile(`<form method="post" action="([^"]+)"`)
	hiddenInput = regexp.MustCompile(`<input type="hidden" name="([^"]+)" value="([^"]*)"`)
	dataAttr    = regexp.MustCompile(`data-([a-z]+)="([^"]*)"`)
)

// Field is a hidden field of the page's form.
func (r Response) Field(name string) string {
	for _, match := range hiddenInput.FindAllStringSubmatch(r.Body, -1) {
		if html.UnescapeString(match[1]) == name {
			return html.UnescapeString(match[2])
		}
	}
	return ""
}

// Data is a data-* attribute of the page's script.
func (r Response) Data(name string) string {
	for _, match := range dataAttr.FindAllStringSubmatch(r.Body, -1) {
		if match[1] == name {
			return html.UnescapeString(match[2])
		}
	}
	return ""
}

// Action is where the page's form posts.
func (r Response) Action() string {
	match := formAction.FindStringSubmatch(r.Body)
	if match == nil {
		return ""
	}
	action, err := url.Parse(html.UnescapeString(match[1]))
	if err != nil {
		return ""
	}
	return r.URL.ResolveReference(action).String()
}

// Press presses one of the page's form buttons, which posts op.
func (c *Client) Press(page Response, op string) Response {
	c.t.Helper()
	action := page.Action()
	if action == "" {
		c.t.Fatalf("the page has no form: %d %s", page.Status, page.Body)
	}
	form := url.Values{"op": {op}}
	for _, match := range hiddenInput.FindAllStringSubmatch(page.Body, -1) {
		form.Set(html.UnescapeString(match[1]), html.UnescapeString(match[2]))
	}
	return c.Post(action, form)
}

// Code checks that the browser returned to the app with this request's
// state and the server as iss, and returns the code.
func (c *Client) Code(r Response, issuer string) string {
	c.t.Helper()
	if r.Return == nil {
		c.t.Fatalf("the browser did not return to the app: %d %s", r.Status, r.Body)
	}
	query := r.Return.Query()
	if query.Get("state") != c.State || query.Get("iss") != issuer || query.Get("code") == "" {
		c.t.Fatalf("returned to the app with %s", r.Return)
	}
	return query.Get("code")
}

// Redemption is the auth params that redeem a code on a connection, besides
// scheme (and action and step for webauthn).
func (c *Client) Redemption(code string) map[string]any {
	return map[string]any{"code": code, "code_verifier": c.Verifier, "redirect_uri": c.RedirectURI, "client_id": c.ClientID}
}
