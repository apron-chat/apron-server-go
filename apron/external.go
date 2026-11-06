package apron

import (
	"crypto/rand"
	"errors"
	"fmt"
	"html/template"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

// External sign-in (PROTOCOL.md §4.11.5): a client proposes it and opens
// the page the result names, at externalPath. The server hands the page to
// the extension that offers the sign-in (Registrar.ExternalSignIn), which signs
// the person in however it likes, such as with OAuth or OpenID Connect, and
// completes the proposal with the identity it proved
// (ExternalSignIn.Complete). The server then gives the person the proposal's
// temporary token, as a link back to the client or a code to type, which
// the client presents to sign in. Proposals, tokens, and their limits are
// email sign-in's (email.go).

// externalPath serves the pages of external proposals:
// /external/<proposal>.
const externalPath = "/external/"

// externalSignIn is the extension that offers external sign-in, and the
// pages of the proposals not yet completed, by name.
type externalSignIn struct {
	host  *Host
	start func(w http.ResponseWriter, r *http.Request, p ExternalProposal)
	pages map[string]*emailProposal
}

// ExternalSignIn is the external sign-in an extension offers
// (Registrar.ExternalSignIn), which completes the proposals it was given.
type ExternalSignIn struct{ host *Host }

// ExternalProposal is an external sign-in a client proposed, whose page a
// person opened.
type ExternalProposal struct {
	// ID names the proposal to ExternalSignIn.Complete. It is unguessable, so it
	// may serve as the state of an OAuth request.
	ID string
	// Add reports a proposal to add the identity to the account that is
	// signed in where it was proposed, rather than to sign in with it.
	Add bool
	// ReturnHost is the host of the client the person returns to, if any.
	ReturnHost string
}

// Identity is who an external sign-in proved someone is.
type Identity struct {
	// Subject names the identity, uniquely and for good, such as
	// "github:583231": the same Subject signs in to the same account.
	Subject string
	// Name and UserID are the display name and the user_id requested for
	// a new account, if the client requested none.
	Name, UserID string
	// Roles are the server roles (§3.3.4) the account holds, as of each
	// sign-in, besides those in Config.Roles.
	Roles []string
}

// ExternalSignIn offers external sign-in (§4.11.5), which start begins: it
// is called with the page a person opened for a proposal, and typically
// redirects to an identity provider, whose answer comes back to a route of
// the extension's (Handle) that calls the returned ExternalSignIn's
// Complete. One extension may offer it. Account creation is the
// extension's policy: an identity it completes signs in, making an account
// if it has none.
func (r *Registrar) ExternalSignIn(start func(w http.ResponseWriter, r *http.Request, p ExternalProposal)) *ExternalSignIn {
	h := r.Host
	r.register("external sign-in", func() error {
		switch {
		case start == nil:
			return errors.New("external sign-in needs a start function")
		case h.s.external != nil:
			return fmt.Errorf("extension %s offers external sign-in already", h.s.external.host.name)
		}
		h.s.external = &externalSignIn{host: h, start: start, pages: make(map[string]*emailProposal)}
		return nil
	})
	return &ExternalSignIn{host: h}
}

// authenticateExternal runs the external scheme under s.mu: a request with
// token approves a proposal (§4.11.3), and one without proposes.
func (s *Server) authenticateExternal(c *client, req request) (any, *Error) {
	if _, has := req.params["token"]; has {
		return s.approveLocked(c, req)
	}
	return s.proposeExternalLocked(c, req)
}

// proposeExternalLocked replaces the connection's proposal with an external
// one (§4.11.1) and returns the url of its page. return_url must be on the
// connection's origin, or on Config.PublicURL's.
func (s *Server) proposeExternalLocked(c *client, req request) (any, *Error) {
	name, requested, err := parseRequested(req)
	if err != nil {
		return nil, err
	}
	state, err := parseString(req.params, "state", false)
	if err != nil {
		return nil, err
	}
	if len(state) > maxStateBytes {
		return nil, &Error{Code: CodeTooLarge, Message: fmt.Sprintf("state is at most %d bytes", maxStateBytes)}
	}
	returnURL, err := parseString(req.params, "return_url", false)
	if err != nil {
		return nil, err
	}
	var origin string
	if returnURL != "" {
		parsed, parseErr := url.Parse(returnURL)
		if parseErr != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil {
			return nil, invalidParams("return_url must be an http or https URL")
		}
		origin = parsed.Scheme + "://" + parsed.Host
		if !s.trustedOriginLocked(c, origin) {
			return nil, &Error{Code: CodeDenied, Message: "return_url must be on the origin of this client"}
		}
	}
	now := time.Now()
	if c.emailSends == nil {
		c.emailSends = rate.NewLimiter(rate.Every(emailSendRefill), emailSendsPerConnection)
	}
	if delay := wait(c.emailSends, now); delay > 0 {
		return nil, retryAfter("Too many sign-ins proposed; try again later", delay)
	}
	pages := s.external.pages
	if len(pages) >= maxEmailLinks {
		for _, p := range pages {
			if !now.Before(p.expires) {
				s.dropProposalLocked(p)
			}
		}
		if len(pages) >= maxEmailLinks {
			return nil, retryAfter("Too many sign-ins in progress; try again shortly", emailResendInterval)
		}
	}
	c.emailSends.AllowN(now, 1)
	if c.proposal != nil {
		s.dropProposalLocked(c.proposal)
	}
	p := &emailProposal{
		external: true, add: c.user != nil, state: state, expires: now.Add(emailCodeLifetime),
		name: name, userID: requested, returnURL: returnURL, returnOrigin: origin,
		server: webSocketURL(c.baseURL), page: rand.Text(),
	}
	if p.add {
		p.accountID = c.user.id
	}
	pages[p.page] = p
	c.proposal = p
	result := map[string]any{"url": c.baseURL + externalPath + p.page}
	c.sendResult(req, result)
	return result, nil
}

// trustedOriginLocked reports whether a return_url on origin may receive a
// sign-in link: the connection's own origin, or the server's.
func (s *Server) trustedOriginLocked(c *client, origin string) bool {
	if c.origin != "" && strings.EqualFold(c.origin, origin) {
		return true
	}
	if public, err := url.Parse(s.config.PublicURL); err == nil && public.Host != "" {
		return strings.EqualFold(public.Scheme+"://"+public.Host, origin)
	}
	return false
}

// handleExternal serves a proposal's page by handing it to the extension.
func (s *Server) handleExternal(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	page := strings.TrimPrefix(r.URL.Path, externalPath)
	s.mu.Lock()
	if s.external == nil {
		s.unlock()
		http.NotFound(w, r)
		return
	}
	p := s.external.pages[page]
	if p == nil || p.dropped || !time.Now().Before(p.expires) {
		if p != nil {
			s.dropProposalLocked(p)
		}
		s.unlock()
		renderExternalPage(w, http.StatusNotFound, externalPage{Title: "This sign-in expired", Text: "Start again in your app."})
		return
	}
	proposal := ExternalProposal{ID: page, Add: p.add}
	if parsed, err := url.Parse(p.returnURL); err == nil {
		proposal.ReturnHost = parsed.Host
	}
	start := s.external.start
	s.unlock()
	start(w, r, proposal)
}

// ErrUnknownProposal is returned by Complete for a proposal that is
// unknown, expired, or completed.
var ErrUnknownProposal = errors.New("apron: the sign-in proposal is unknown, expired, or completed")

// Complete completes the proposal named id with the identity the
// extension proved, and answers the person's browser: with the client's
// return_url, a page that asks them to continue to it with the sign-in
// link (§4.11.5), and without, a page that shows the code to type in the
// client. For a proposal that is no longer open, it answers with a page
// that says so, and returns ErrUnknownProposal.
func (x *ExternalSignIn) Complete(w http.ResponseWriter, r *http.Request, id string, identity Identity) error {
	h := x.host
	s := h.s
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if identity.Subject == "" {
		renderExternalPage(w, http.StatusInternalServerError, externalPage{Title: "Sign-in failed", Text: "Start again in your app."})
		return errors.New("apron: an identity needs a Subject")
	}
	number, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		renderExternalPage(w, http.StatusInternalServerError, externalPage{Title: "Sign-in failed", Text: "Start again in your app."})
		return err
	}
	s.mu.Lock()
	if s.external == nil || s.external.host != h {
		s.unlock()
		renderExternalPage(w, http.StatusInternalServerError, externalPage{Title: "Sign-in failed", Text: "Start again in your app."})
		return errors.New("apron: this extension does not offer the server's external sign-in")
	}
	p := s.external.pages[id]
	now := time.Now()
	if p == nil || p.dropped || !now.Before(p.expires) {
		if p != nil {
			s.dropProposalLocked(p)
		}
		s.unlock()
		renderExternalPage(w, http.StatusNotFound, externalPage{Title: "This sign-in expired", Text: "Start again in your app."})
		return ErrUnknownProposal
	}
	identity.Roles = normalizeRoles(identity.Roles)
	p.identity = &identity
	delete(s.external.pages, p.page)
	p.page = ""
	// The tokens expire within minutes of their delivery (§4.11.2).
	p.expires = now.Add(emailCodeLifetime)
	p.code = fmt.Sprintf("%06d", number)
	p.handle, p.secret = s.newLinkHandleLocked(), rand.Text()
	s.email.links[p.handle] = p
	page := externalPage{Add: p.add}
	if p.returnURL != "" {
		parsed, _ := url.Parse(p.returnURL)
		page.Host = parsed.Host
		page.Link = template.URL(signInLink(p.returnURL, p.handle+p.secret, p.state, p.server))
	} else {
		page.Code = p.code
	}
	s.unlock()
	switch {
	case page.Link != "" && page.Add:
		page.Title, page.Text = "Add this sign-in to your account", "Continue to "+page.Host+" to add it."
	case page.Link != "":
		page.Title, page.Text = "Signed in", "Continue to "+page.Host+" to finish signing in."
	default:
		page.Title, page.Text = "Your code", "Type this code in the app where you started, and nowhere else."
	}
	renderExternalPage(w, http.StatusOK, page)
	return nil
}

// approveExternalLocked approves a completed external proposal (§4.11.3):
// an addition adds the identity to the proposing account, and a sign-in
// signs in to the identity's account, making one for a new identity, which
// takes a requested user_id as email accounts do, else the identity's.
func (s *Server) approveExternalLocked(c *client, req request, p *emailProposal, name, requested string, now time.Time) (any, *Error) {
	identity := p.identity
	if identity == nil {
		return nil, &Error{Code: CodeDenied, Message: "The code is invalid or expired; ask for a new one"}
	}
	// A link token works only where its client is (§4.11.5).
	if p.returnOrigin != "" && c.proposal != p && !strings.EqualFold(c.origin, p.returnOrigin) {
		return nil, &Error{Code: CodeDenied, Message: "Open this link in the app that asked for it"}
	}
	if p.add {
		account := c.user
		if account == nil || account.id != p.accountID {
			return nil, &Error{Code: CodeDenied, Message: "Open this link where the account that asked for it is signed in"}
		}
		s.dropProposalLocked(p)
		switch {
		case s.identities[identity.Subject] != nil:
			return nil, &Error{Code: CodeDenied, Message: "This sign-in belongs to an account"}
		case account.external != "":
			return nil, &Error{Code: CodeDenied, Message: "This account has an external sign-in already"}
		}
		account.external, account.granted = identity.Subject, identity.Roles
		s.identities[identity.Subject] = account
		s.touchUser(account.id)
		if s.grantRolesLocked(account) {
			s.notifyProfileLocked(account, nil, nil)
		}
		result := map[string]any{}
		c.sendResult(req, result)
		return result, nil
	}
	if c.user != nil {
		return nil, &Error{Code: CodeDenied, Message: "Sign-in codes work on a connection that is not signed in"}
	}
	own := c.proposal == p
	s.dropProposalLocked(p)
	user := s.identities[identity.Subject]
	if user == nil {
		if own {
			if name == "" {
				name = p.name
			}
			if requested == "" {
				requested = p.userID
			}
		}
		if name == "" {
			name = identity.Name
		}
		if requested == "" {
			requested = identity.UserID
		}
		user = newUserState(s.assignAccountIDLocked(requested), normalizeName(name))
		user.external, user.granted = identity.Subject, identity.Roles
		s.users[user.id] = user
		s.identities[identity.Subject] = user
		s.touchUser(user.id)
		s.grantRolesLocked(user)
		return s.signInLocked(c, req, user, now, true, true)
	}
	if !slices.Equal(user.granted, identity.Roles) {
		user.granted = identity.Roles
		s.touchUser(user.id)
		if s.grantRolesLocked(user) {
			s.notifyProfileLocked(user, nil, nil)
		}
	}
	return s.signInLocked(c, req, user, now, true, false)
}

// normalizeRoles lowercases and sorts roles, without blanks or repeats.
func normalizeRoles(roles []string) []string {
	var normalized []string
	for _, role := range roles {
		if role = strings.ToLower(strings.TrimSpace(role)); role != "" {
			normalized = append(normalized, role)
		}
	}
	slices.Sort(normalized)
	return slices.Compact(normalized)
}

type externalPage struct {
	Title, Text string
	Add         bool
	// Host and Link continue to the client; Code is typed in it.
	Host string
	Link template.URL
	Code string
}

var externalTemplate = template.Must(template.New("external").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
body{font:16px/1.5 system-ui,sans-serif;max-width:28rem;margin:4rem auto;padding:0 1rem;color:#1d1d1f;background:#fff}
@media (prefers-color-scheme:dark){body{color:#eee;background:#161616}a.button{background:#4f7cff}}
a.button{display:inline-block;padding:.6rem 1.2rem;border-radius:.4rem;background:#2f5be0;color:#fff;text-decoration:none}
.code{font:600 2rem/1 ui-monospace,monospace;letter-spacing:.2em}
</style></head><body>
<h1>{{.Title}}</h1>
<p>{{.Text}}</p>
{{if .Link}}<p><a class="button" href="{{.Link}}">Continue to {{.Host}}</a></p>{{end}}
{{if .Code}}<p class="code">{{.Code}}</p>{{end}}
</body></html>
`))

func renderExternalPage(w http.ResponseWriter, status int, page externalPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.WriteHeader(status)
	_ = externalTemplate.Execute(w, page)
}
