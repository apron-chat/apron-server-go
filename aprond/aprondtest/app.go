package aprondtest

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

	"github.com/apron-chat/apron-server-go/apron"
)

// App is an app on the person's device, com.example.app, signing in with
// the server's external sign-in (§4.12.2) as the OAuth client it is, with
// PKCE and state, and a browser: Server.App begins on a connection of its
// own (Conn), Open opens the server's sign-in page in the browser, Press
// presses one of a page's buttons, and Redeem redeems the code the browser
// returned to the app with. The browser follows every redirect, between
// the server's page, the identity provider the extension sends people to,
// and the extension's routes, but the one back to the app (Page.Return).
type App struct {
	// Conn is the app's connection, on which Server.App began and Redeem
	// signs in.
	Conn *Client
	// URL is the sign-in page the begin result named (§4.12.1), such as
	// http://127.0.0.1:41234/oauth/authorize.
	URL string
	// ClientID and RedirectURI identify the app, as com.example.app and
	// com.example.app:/signed-in, unless the page's URL names a client_id,
	// which the app then sends as its own; State is its authorization
	// request's state.
	ClientID, RedirectURI, State string

	verifier string
	browser  *http.Client
	server   *Server
}

// App connects an app's client, and begins external sign-in on it (§4.12),
// or fails the test, as on a server that offers none.
func (s *Server) App() *App {
	s.t.Helper()
	c := s.Dial()
	begun, err := c.Auth(apron.AuthParams{Scheme: "external"})
	if err != nil {
		s.t.Fatalf("aprontest: beginning external sign-in: %v", err)
	}
	external, _ := begun.Extra["external"].(map[string]any)
	page, _ := external["url"].(string)
	if page == "" {
		s.t.Fatalf("aprontest: the external begin named no sign-in page: %v", begun.Extra)
	}
	a := &App{
		Conn: c, URL: page, ClientID: "com.example.app", RedirectURI: "com.example.app:/signed-in",
		State: rand.Text(), verifier: rand.Text() + rand.Text(), server: s,
	}
	a.browser = &http.Client{CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		// The browser stops being the app's once it returns to the app.
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" || strings.HasPrefix(req.URL.String(), a.RedirectURI) {
			return http.ErrUseLastResponse
		}
		return nil
	}}
	return a
}

// Page is where the app's browser is: a page, or the redirect back to the
// app.
type Page struct {
	// Status is the response's status code, and Body its body.
	Status int
	Body   string
	// URL is the page's address, which its form's action resolves
	// against.
	URL *url.URL
	// Return is the redirect back to the app, if the browser returned to
	// it, such as with a code, or with error=access_denied.
	Return *url.URL
}

// Open opens the sign-in page, URL, with the app's authorization request
// added to its query (§4.12.2): response_type, client_id, redirect_uri, the
// PKCE challenge, state, and the server's origin as the resource.
func (a *App) Open() Page {
	a.server.t.Helper()
	page, err := url.Parse(a.URL)
	if err != nil {
		a.server.t.Fatalf("aprontest: the sign-in page %q: %v", a.URL, err)
	}
	query := page.Query()
	// The app redeems with the client_id it sent: the page's own, if it
	// names one (§4.12.1).
	if id := query.Get("client_id"); id != "" {
		a.ClientID = id
	}
	challenge := sha256.Sum256([]byte(a.verifier))
	for name, value := range map[string]string{
		"response_type": "code", "client_id": a.ClientID, "redirect_uri": a.RedirectURI,
		"code_challenge": base64.RawURLEncoding.EncodeToString(challenge[:]), "code_challenge_method": "S256",
		"state": a.State, "resource": a.server.URL,
	} {
		query.Set(name, value)
	}
	page.RawQuery = query.Encode()
	return a.Get(page.String())
}

// Get opens a page in the app's browser.
func (a *App) Get(page string) Page {
	a.server.t.Helper()
	response, err := a.browser.Get(page)
	if err != nil {
		a.server.t.Fatalf("aprontest: opening %s: %v", page, err)
	}
	return a.read(response)
}

func (a *App) read(response *http.Response) Page {
	a.server.t.Helper()
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		a.server.t.Fatalf("aprontest: reading %s: %v", response.Request.URL, err)
	}
	p := Page{Status: response.StatusCode, Body: string(body), URL: response.Request.URL}
	if location := response.Header.Get("Location"); strings.HasPrefix(location, a.RedirectURI) {
		p.Return, _ = url.Parse(location)
	}
	return p
}

var (
	formAction  = regexp.MustCompile(`<form method="post" action="([^"]+)"`)
	hiddenInput = regexp.MustCompile(`<input type="hidden" name="([^"]+)" value="([^"]*)"`)
)

// Press presses the button of the page's form that posts op, such as
// "allow" or "cancel" on the server's page, or fails the test if the page
// has no form.
func (a *App) Press(page Page, op string) Page {
	a.server.t.Helper()
	match := formAction.FindStringSubmatch(page.Body)
	if match == nil {
		a.server.t.Fatalf("aprontest: the page has no form: %d %s", page.Status, page.Body)
	}
	action, err := url.Parse(html.UnescapeString(match[1]))
	if err != nil {
		a.server.t.Fatalf("aprontest: the form's action %q: %v", match[1], err)
	}
	form := url.Values{"op": {op}}
	for _, field := range hiddenInput.FindAllStringSubmatch(page.Body, -1) {
		form.Set(html.UnescapeString(field[1]), html.UnescapeString(field[2]))
	}
	response, err := a.browser.PostForm(page.URL.ResolveReference(action).String(), form)
	if err != nil {
		a.server.t.Fatalf("aprontest: pressing %s: %v", op, err)
	}
	return a.read(response)
}

// Redeem redeems the code that page, the browser's return to the app,
// carries, on Conn (§4.12.4), and returns the auth result, which signed
// Conn in, or the server's error reply. It fails the test when the browser
// did not return to the app with a code, this request's state, and the
// server as the issuer.
func (a *App) Redeem(page Page) (apron.AuthResult, error) {
	t := a.server.t
	t.Helper()
	if page.Return == nil {
		t.Fatalf("aprontest: the browser did not return to the app: %d %s", page.Status, page.Body)
	}
	query := page.Return.Query()
	if query.Get("state") != a.State || query.Get("iss") != a.server.URL || query.Get("code") == "" {
		t.Fatalf("aprontest: the browser returned to the app with %s", page.Return)
	}
	return a.Conn.Auth(struct {
		Scheme       string `json:"scheme"`
		Code         string `json:"code"`
		CodeVerifier string `json:"code_verifier"`
		RedirectURI  string `json:"redirect_uri"`
		ClientID     string `json:"client_id"`
	}{"external", query.Get("code"), a.verifier, a.RedirectURI, a.ClientID})
}
