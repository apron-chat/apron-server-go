// Package external is the external sign-in scheme (PROTOCOL.md §4.12), a
// profile of OAuth 2.1 in which the server is its own authorization server.
// A client begins, opens the authorization endpoint at /oauth/authorize
// with PKCE, and redeems the code it gets back, on a connection or at
// /oauth/token.
//
// The page confirms who is asking, then hands the person to the extension
// that offers the sign-in, which signs them in however it likes, such as
// with an OpenID Connect provider, and completes the sign-in with the
// identity it proved (Complete). The page then shows the account, and on
// Allow redirects to the client with a code that proves the identity.
//
// A server may instead name another authorization server (Foreign).
package external

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/apron-chat/apron-server-go/internal/auth/oauth"
	"github.com/apron-chat/apron-server-go/internal/authn"
)

// Name is the scheme's name, and its identities'.
const Name = "external"

// Disabled says why the scheme may be off.
const Disabled = "External sign-in is not offered"

// The authorization server's paths, on the server's origin.
const (
	AuthorizePath = "/oauth/authorize"
	TokenPath     = "/oauth/token"
	MetadataPath  = "/.well-known/oauth-authorization-server"
)

// ErrUnknownSignIn is returned by Complete and Cancel for a sign-in that
// is unknown, expired, or completed.
var ErrUnknownSignIn = errors.New("external: the sign-in is unknown, expired, or completed")

// ErrNoSubject is returned by Complete for an identity without a Subject.
var ErrNoSubject = errors.New("external: an identity needs a Subject")

// Request is a sign-in the page hands to the extension, once the person
// chose to continue.
type Request struct {
	// ID names the sign-in to Complete. It is unguessable and works once,
	// so it may serve as the state of the extension's own OAuth request.
	ID string
	// Client is who is asking, as the page showed it, such as
	// chat.alice.dev or "An app on this device (com.example.app)".
	Client string
}

// Scheme is external sign-in with the server as its own authorization
// server.
type Scheme struct {
	start func(w http.ResponseWriter, r *http.Request, req Request)
	core  authn.Core
	// flows are the pages people have open, by the ID their forms post;
	// signIns the same flows, by the ID the extension completes.
	flows   oauth.Table[*flow]
	signIns oauth.Table[*flow]
	codes   oauth.Codes
}

// flow is one person's way through the page: the client's request, and
// the identity the extension proved, once it did.
type flow struct {
	id  string
	req *oauth.Request

	mu     sync.Mutex
	signIn string
	proof  *authn.Proof
	done   bool
}

// New is external sign-in whose page hands people to start.
func New(start func(w http.ResponseWriter, r *http.Request, req Request)) *Scheme {
	return &Scheme{start: start}
}

// Bind gives the scheme the server's side of sign-in.
func (s *Scheme) Bind(core authn.Core) { s.core = core }

func (*Scheme) Name() string  { return Name }
func (*Scheme) Label() string { return "external sign-in" }

// Auth begins (§4.12.1), or redeems a code from the page (§4.12.4).
func (s *Scheme) Auth(_ context.Context, r *authn.Request) (authn.Result, error) {
	_, hasCode := r.Params["code"]
	_, hasToken := r.Params["token"]
	switch {
	case hasCode:
		return s.redeem(r)
	case hasToken:
		if _, err := authn.String(r.Params, "token", true); err != nil {
			return nil, err
		}
		// This server issues its own bearer tokens, for scheme "token".
		return nil, authn.Denied("This server takes no external tokens; sign in with its page, and use its token with scheme token")
	}
	return &authn.Reply{Result: map[string]any{"external": map[string]any{"url": r.BaseURL + AuthorizePath}}}, nil
}

// redeem uses up a code on the connection: it signs in with the identity,
// or adds it to the connection's account. A token from a code resumes from
// any origin (§4.12.4).
func (s *Scheme) redeem(r *authn.Request) (authn.Result, error) {
	params, err := redemption(r.Params)
	if err != nil {
		return nil, err
	}
	value, grant, redeemErr := s.codes.Redeem(params.code, params.verifier, params.redirectURI, params.clientID, r.Now)
	if redeemErr != nil || !slices.Contains(oauth.Resources(oauth.Origin(r.BaseURL)), grant.Resource) {
		return nil, authn.Denied("The sign-in code is invalid, used, or expired; sign in again")
	}
	proof := value.(authn.Proof)
	if r.Account != nil {
		// An addition names nobody: the account keeps its name. A guest
		// that becomes an account gets a token to come back with.
		proof.Name, proof.UserID = "", ""
		return &authn.Add{Proof: proof, Session: r.Account.Guest, AnyOrigin: true}, nil
	}
	// A new account takes the name and user_id the connection requested,
	// else the extension's suggestion.
	proof.Name, proof.UserID = cmp.Or(r.Name, proof.Name), cmp.Or(r.UserID, proof.UserID)
	return &authn.SignIn{Proof: proof, Create: true, AnyOrigin: true}, nil
}

// codeParams are a code's redemption (§4.12.2).
type codeParams struct{ code, verifier, redirectURI, clientID string }

// redemption reads a redemption's params: all four strings, none empty.
func redemption(params map[string]jsontext.Value) (codeParams, *authn.Error) {
	var p codeParams
	for _, field := range []struct {
		name string
		into *string
	}{{"code", &p.code}, {"code_verifier", &p.verifier}, {"redirect_uri", &p.redirectURI}, {"client_id", &p.clientID}} {
		value, err := authn.String(params, field.name, true)
		if err != nil {
			return p, err
		}
		if value == "" {
			return p, authn.InvalidParams("%s must be a non-empty string", field.name)
		}
		*field.into = value
	}
	return p, nil
}

// Routes serves the authorization server: its metadata, the page, and the
// token endpoint.
func (s *Scheme) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+MetadataPath, s.metadata)
	mux.HandleFunc("GET "+AuthorizePath, s.authorize)
	mux.HandleFunc("POST "+AuthorizePath, s.decide)
	mux.HandleFunc("POST "+TokenPath, s.token)
	mux.HandleFunc("OPTIONS "+TokenPath, func(w http.ResponseWriter, _ *http.Request) {
		cors(w)
		w.Header().Set("Access-Control-Allow-Methods", "POST")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.WriteHeader(http.StatusNoContent)
	})
}

func cors(w http.ResponseWriter) { w.Header().Set("Access-Control-Allow-Origin", "*") }

// metadata is the authorization server's metadata (RFC 8414).
func (s *Scheme) metadata(w http.ResponseWriter, r *http.Request) {
	issuer := oauth.Origin(s.core.BaseURL(r))
	cors(w)
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         issuer,
		"authorization_endpoint":                         issuer + AuthorizePath,
		"token_endpoint":                                 issuer + TokenPath,
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"authorization_response_iss_parameter_supported": true,
	})
}

// authorize opens the page for an authorization request: its first
// screen, which says who is asking and what they get.
func (s *Scheme) authorize(w http.ResponseWriter, r *http.Request) {
	req, problem := oauth.Parse(r.URL.Query(), oauth.Origin(s.core.BaseURL(r)))
	if problem != nil {
		oauth.RenderProblem(w, problem)
		return
	}
	f := &flow{req: req}
	id, err := s.flows.Put(f, oauth.FlowLifetime, time.Now())
	if err != nil {
		busy(w)
		return
	}
	f.id = id
	oauth.Render(w, http.StatusOK, s.consent(f))
}

// consent is the first screen.
func (s *Scheme) consent(f *flow) oauth.Page {
	host := hostOf(f.req.Issuer)
	return oauth.Page{
		Title: "Sign in to " + cmp.Or(s.core.Title(), host),
		Lines: []template.HTML{oauth.Line(
			oauth.Strong(f.req.Client()), " wants to sign in to your account on ", oauth.Strong(host),
			". It will be able to read and send messages as you.",
		)},
		Note:    host + " has not verified this app.",
		Warning: "Only continue if you just started signing in on " + f.req.Started() + ".",
		Action:  AuthorizePath, Fields: map[string]string{"flow": f.id},
		Buttons: []oauth.Button{{Op: "continue", Label: "Continue", Primary: true}, {Op: "cancel", Label: "Cancel"}},
	}
}

// decide takes the page's buttons: continue to the extension's sign-in,
// sign in as someone else, allow, or cancel.
func (s *Scheme) decide(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	f, ok := s.flows.Get(r.PostFormValue("flow"), time.Now())
	if !ok {
		oauth.Expired(w)
		return
	}
	f.mu.Lock()
	if f.done {
		f.mu.Unlock()
		oauth.Expired(w)
		return
	}
	switch r.PostFormValue("op") {
	case "continue", "restart":
		// A new sign-in replaces any the extension had, and the identity
		// it proved.
		if f.signIn != "" {
			s.signIns.Delete(f.signIn)
		}
		f.proof = nil
		signIn, err := s.signIns.Put(f, oauth.FlowLifetime, time.Now())
		if err != nil {
			f.mu.Unlock()
			busy(w)
			return
		}
		f.signIn = signIn
		f.mu.Unlock()
		s.start(w, r, Request{ID: signIn, Client: f.req.Client()})
		return
	case "allow":
		if f.proof == nil {
			f.mu.Unlock()
			http.Error(w, "Sign in first", http.StatusBadRequest)
			return
		}
		code, err := s.codes.Issue(f.req.Grant(), *f.proof, time.Now())
		if err != nil {
			f.mu.Unlock()
			busy(w)
			return
		}
		s.finishLocked(f)
		f.mu.Unlock()
		f.req.Return(w, r, url.Values{"code": {code}})
		return
	case "cancel":
		s.finishLocked(f)
		f.mu.Unlock()
		f.req.Deny(w, r)
		return
	}
	f.mu.Unlock()
	http.Error(w, "Unknown action", http.StatusBadRequest)
}

// finishLocked forgets a flow whose page redirected. Called holding f.mu.
func (s *Scheme) finishLocked(f *flow) {
	f.done = true
	s.flows.Delete(f.id)
	if f.signIn != "" {
		s.signIns.Delete(f.signIn)
	}
}

// Complete gives the sign-in named id the identity the extension proved,
// and answers the person's browser with the page's next screen: the
// account the identity signs in to and Allow, or that it makes a new
// account, or that it may not sign in here. For a sign-in that is no
// longer open, it answers with a page that says so, and returns
// ErrUnknownSignIn.
func (s *Scheme) Complete(w http.ResponseWriter, id string, proof authn.Proof) error {
	if proof.Subject == "" {
		oauth.Render(w, http.StatusInternalServerError, oauth.Page{Title: "Sign-in failed", Lines: []template.HTML{oauth.Line("Go back to the app and try again.")}})
		return ErrNoSubject
	}
	f, ok := s.signIns.Take(id, time.Now())
	if !ok {
		oauth.Expired(w)
		return ErrUnknownSignIn
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.done || f.signIn != id {
		oauth.Expired(w)
		return ErrUnknownSignIn
	}
	f.signIn = ""
	host := hostOf(f.req.Issuer)
	client := f.req.Client()
	page := oauth.Page{
		Title:  "Sign in to " + cmp.Or(s.core.Title(), host),
		Action: AuthorizePath, Fields: map[string]string{"flow": f.id},
	}
	account, known := s.core.Lookup(proof.Subject)
	switch {
	case !s.core.Admits(proof.Subject, true):
		page.Lines = []template.HTML{oauth.Line("You can't sign in to ", oauth.Strong(host), " with this account.")}
		page.Buttons = []oauth.Button{{Op: "restart", Label: "Sign in as someone else", Primary: true}, {Op: "cancel", Label: "Cancel"}}
		oauth.Render(w, http.StatusForbidden, page)
		return nil
	case known:
		page.Lines = []template.HTML{oauth.Line("Allow ", oauth.Strong(client), " to use ", oauth.Strong(oauth.AccountName(account.Name, account.UserID)), "’s account?")}
		page.Alt = &oauth.Button{Op: "restart", Label: "Not you? Sign in as someone else"}
		page.Buttons = []oauth.Button{{Op: "allow", Label: "Allow", Primary: true}, {Op: "cancel", Label: "Cancel"}}
	default:
		page.Lines = []template.HTML{oauth.Line(
			oauth.Strong(client), " wants to create an account for you on ", oauth.Strong(host),
			". It will be able to read and send messages as you.",
		)}
		page.Alt = &oauth.Button{Op: "restart", Label: "Not you? Sign in as someone else"}
		page.Buttons = []oauth.Button{{Op: "allow", Label: "Continue", Primary: true}, {Op: "cancel", Label: "Cancel"}}
	}
	f.proof = &proof
	oauth.Render(w, http.StatusOK, page)
	return nil
}

// Cancel ends the sign-in named id, such as when the person declined at
// the identity provider, and returns the person to the client with
// error=access_denied.
func (s *Scheme) Cancel(w http.ResponseWriter, r *http.Request, id string) error {
	f, ok := s.signIns.Take(id, time.Now())
	if !ok {
		oauth.Expired(w)
		return ErrUnknownSignIn
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.done {
		oauth.Expired(w)
		return ErrUnknownSignIn
	}
	s.finishLocked(f)
	f.req.Deny(w, r)
	return nil
}

// token is the token endpoint (§4.12.3): it redeems a code for a bearer
// token for scheme "token", which resumes from any origin, as one redeemed
// on a connection does (§4.12.4).
func (s *Scheme) token(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	cors(w)
	w.Header().Set("Cache-Control", "no-store")
	refuse := func(status int, code, description string) {
		writeJSON(w, status, map[string]string{"error": code, "error_description": description})
	}
	if err := r.ParseForm(); err != nil {
		refuse(http.StatusBadRequest, "invalid_request", "The body must be form-encoded")
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" {
		refuse(http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code")
		return
	}
	value, grant, err := s.codes.Redeem(r.PostForm.Get("code"), r.PostForm.Get("code_verifier"), r.PostForm.Get("redirect_uri"), r.PostForm.Get("client_id"), time.Now())
	if err != nil {
		refuse(http.StatusBadRequest, "invalid_grant", "The code is invalid, used, or expired")
		return
	}
	if resource := r.PostForm.Get("resource"); resource != "" && resource != grant.Resource {
		refuse(http.StatusBadRequest, "invalid_target", "resource is not the one the code was issued for")
		return
	}
	token, err := s.core.Issue(value.(authn.Proof), true)
	if err != nil {
		description := "This account may not sign in"
		if e, ok := errors.AsType[*authn.Error](err); ok {
			description = e.Message
		}
		refuse(http.StatusBadRequest, "invalid_grant", description)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"access_token": token, "token_type": "Bearer"})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	body, _ := json.Marshal(value)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func busy(w http.ResponseWriter) {
	oauth.Render(w, http.StatusServiceUnavailable, oauth.Page{
		Title: "Too many sign-ins in progress",
		Lines: []template.HTML{oauth.Line("Wait a minute, then go back to the app and try again.")},
	})
}

// hostOf is a URL's host, as the page names the server.
func hostOf(base string) string {
	if parsed, err := url.Parse(base); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return base
}
