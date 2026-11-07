package apron

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/big"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/apron-chat/apron-server-go/internal/authn"
)

// Sign-in (PROTOCOL.md §3.2) is split between the server and its schemes
// (internal/authn). A scheme, such as passkeys or email, proves who a
// connection is, outside s.mu, from a copy of the request. The server owns
// what follows: accounts and the identities that prove them, bearer tokens,
// the proposals of sign-ins that finish out of band (§4.11), the allowlist,
// and roles, which it changes in one place, applyProvenLocked, under s.mu.
// The guest and token schemes are the server's own.

const (
	// proposalLifetime is how long a proposal and its tokens stay valid.
	proposalLifetime = 10 * time.Minute
	// maxProposalAttempts wrong tokens, on any connection, cancel a
	// proposal.
	maxProposalAttempts = 5
	// linkHandleBytes starts each link token with a handle naming its
	// proposal, so a wrong secret behind it counts against that proposal
	// from any connection.
	linkHandleBytes = 10
	// maxLinks and maxPages bound the outstanding link tokens and sign-in
	// pages.
	maxLinks = 10_000
	maxPages = 10_000
	// accountIDPrefix starts the user_ids assigned to new accounts that did
	// not request one.
	accountIDPrefix = "user_"
)

// unsupportedSchemes say why each scheme this server knows may be off.
var unsupportedSchemes = map[string]string{
	"webauthn": "Passkeys are disabled",
	"email":    "Email sign-in is disabled",
	"external": "External sign-in is not offered",
}

// proposal is a pending sign-in that finishes out of band (§4.11.1): to
// sign in, or to add an identity to the proposing account. Its short code
// works only on its connection; its link token, a handle and an
// unguessable secret, works on any connection that may approve it
// (§4.11.3). It outlives its connection's disconnect, until it expires or
// is approved, and holds neither the connection nor the account, so a
// closed connection, and a retired guest, are not kept for it: the
// connection holds the proposal instead. Guarded by s.mu.
type proposal struct {
	// proven is what approving it proves; nil while its page waits for the
	// identity.
	proven *authn.Proven
	// accountID is the user_id of the proposing account, for an addition.
	// User_ids are never reissued, so only a connection signed in to that
	// account has it.
	accountID string
	add       bool
	code      string
	// handle and secret make up the link token; both are empty when no
	// link was made.
	handle, secret string
	// state is the client's, returned in the link.
	state    string
	expires  time.Time
	attempts int
	// dropped reports that the proposal was forgotten, which its
	// connection, still holding it, learns from this.
	dropped bool
	// name and userID are what the proposing connection requested for a
	// new account.
	name, userID string
	// page names the proposal at its scheme's page until its identity is
	// proven; returnURL, on returnOrigin, is where a link returns to, and
	// server the WebSocket URL a link names.
	page                    string
	returnURL, returnOrigin string
	server                  string
}

// proposals are the outstanding proposals' link tokens and pages.
type proposals struct {
	links map[string]*proposal
	pages map[string]*proposal
}

// addScheme offers a sign-in scheme. Called holding s.mu.
func (s *Server) addSchemeLocked(scheme authn.Scheme) {
	if binder, ok := scheme.(interface{ Bind(authn.Core) }); ok {
		binder.Bind(authCore{s})
	}
	s.schemes = append(s.schemes, scheme)
}

// schemeLocked returns the scheme named name, if the server offers it.
func (s *Server) schemeLocked(name string) authn.Scheme {
	for _, scheme := range s.schemes {
		if scheme.Name() == name {
			return scheme
		}
	}
	return nil
}

// authenticate runs an auth request (§3.2): the guest and token schemes
// under s.mu, and the others by their scheme, outside it, whose result is
// applied under it.
func (s *Server) authenticate(c *client, req request) (any, *Error) {
	s.mu.Lock()
	if c.closing.Load() {
		s.unlock()
		return nil, connectionClosing()
	}
	name, err := parseString(req.params, "scheme", true)
	if err != nil {
		s.unlock()
		return nil, err
	}
	// A scheme this document defines but the server does not offer is
	// unsupported (§3.2), whatever else the request holds.
	scheme := s.schemeLocked(name)
	switch {
	case name == "guest" && !s.guestsAllowedLocked():
		err = &Error{Code: CodeUnsupported, Message: "This server has no guests; sign in with an account"}
	case name == "token" && len(s.schemes) == 0:
		err = &Error{Code: CodeUnsupported, Message: "This server has no sign-in sessions; use guest"}
	case name == "guest", name == "token", scheme != nil:
	case unsupportedSchemes[name] != "":
		err = &Error{Code: CodeUnsupported, Message: unsupportedSchemes[name]}
	default:
		// A request that depends on a name the server does not know is
		// invalid_params (§1).
		err = invalidParams("Unknown authentication scheme %q", name)
	}
	// A user_id is never empty, nor contains whitespace or control
	// characters (§2.4): requesting one that could be no user_id is
	// invalid_params, with any scheme. One that could be, but that this
	// server would not assign, is ignored where the scheme assigns one.
	if _, has := req.params["user_id"]; has && err == nil {
		var requested string
		if requested, err = parseString(req.params, "user_id", true); err == nil && !validID(requested) {
			err = invalidParams("user_id must not be empty, or contain whitespace or control characters")
		}
	}
	if err != nil {
		s.unlock()
		return nil, err
	}
	switch name {
	case "guest":
		defer s.unlock()
		return s.authenticateGuestLocked(c, req)
	case "token":
		defer s.unlock()
		return s.authenticateToken(c, req)
	}
	r := s.authRequestLocked(c, req, name)
	s.unlock()

	result, schemeErr := scheme.Auth(s.requestContext(c), r)
	if schemeErr != nil {
		return nil, authError(schemeErr)
	}
	switch result := result.(type) {
	case authn.Reply:
		c.sendResult(req, result.Result)
		return result.Result, nil
	case authn.Approve:
		s.mu.Lock()
		defer s.unlock()
		if c.closing.Load() {
			return nil, connectionClosing()
		}
		return s.approveLocked(c, req, result.Token, result.Name, result.UserID)
	case authn.Proven:
		s.mu.Lock()
		defer s.unlock()
		if c.closing.Load() {
			return nil, connectionClosing()
		}
		return s.applyProvenLocked(c, req, result)
	}
	return nil, internalError()
}

// authenticateGuestLocked signs the connection in as a new guest, or
// answers with the identity a signed-in connection has.
func (s *Server) authenticateGuestLocked(c *client, req request) (any, *Error) {
	name, err := parseString(req.params, "name", false)
	if err != nil {
		return nil, err
	}
	requested, err := parseString(req.params, "user_id", false)
	if err != nil {
		return nil, err
	}
	if c.user != nil {
		// Guest auth on a signed-in connection answers with the identity it
		// has. It signs the connection in as no other user, so it is no
		// sign-in and sends nothing after its result (§4.5).
		result := map[string]any{"you": c.user.you()}
		c.sendResult(req, result)
		return result, nil
	}
	user := newUserState(s.assignUserIDLocked(requested), normalizeName(name))
	s.users[user.id] = user
	s.touchUser(user.id)
	// A new guest joins the default room, so their room list is not empty.
	return s.switchUserLocked(c, req, user, nil, true, true), nil
}

// authRequestLocked is the copy of an auth request a scheme sees.
func (s *Server) authRequestLocked(c *client, req request, scheme string) *authn.Request {
	r := &authn.Request{
		Params: req.params, Origin: c.origin, BaseURL: c.baseURL, ClientKey: c.clientKey,
		Now: time.Now(), Core: authCore{s}, Conn: c,
	}
	if c.user != nil {
		r.Account = &authn.Account{
			UserID: c.user.id, Name: c.user.name, Guest: !c.user.account(),
			Identities: maps.Clone(c.user.identities),
		}
	}
	if c.authStates == nil {
		c.authStates = make(map[string]*authn.State)
	}
	if c.authStates[scheme] == nil {
		c.authStates[scheme] = new(authn.State)
	}
	r.State = c.authStates[scheme]
	return r
}

// authError turns a scheme's error into a reply.
func authError(err error) *Error {
	if e, ok := errors.AsType[*authn.Error](err); ok && e != nil {
		return &Error{Code: e.Code, Message: e.Message, Data: e.Data}
	}
	slog.Error("sign-in failed", "error", err)
	return internalError()
}

// commit runs a proven identity's Commit, which returns the record its
// scheme keeps with the account.
func commit(proven authn.Proven, userID string) (jsontext.Value, *Error) {
	if proven.Commit == nil {
		return nil, nil
	}
	record, err := proven.Commit(userID)
	if err != nil {
		return nil, authError(err)
	}
	return record, nil
}

// applyProvenLocked applies an identity a scheme proved. An addition adds
// it to the connection's account, a guest's making it an account, unless
// another account has it or the account has another of its scheme. A
// sign-in signs the connection in to the identity's account, making one if
// none has it and the proof may (Create), which takes a requested user_id
// as guests do, or user_<n>. The allowlist is checked before anything
// changes (§3.2.2).
func (s *Server) applyProvenLocked(c *client, req request, proven authn.Proven) (any, *Error) {
	id := proven.Identity
	label := cmp.Or(proven.Label, "sign-in")
	owner := s.accounts[id]
	now := time.Now()
	if proven.Add {
		account := c.user
		switch {
		case account == nil:
			return nil, &Error{Code: CodeDenied, Message: "Sign in before adding a " + label}
		case owner != nil && owner != account:
			return nil, &Error{Code: CodeDenied, Message: "This " + label + " belongs to an account"}
		case account.identities[id.Scheme] != "" && account.identities[id.Scheme] != id.Subject:
			return nil, &Error{Code: CodeDenied, Message: "This account has another " + label + " already"}
		}
		if account.account() {
			if err := s.admitLocked(account); err != nil {
				return nil, err
			}
		}
		record, err := commit(proven, account.id)
		if err != nil {
			return nil, err
		}
		becomesAccount := !account.account()
		s.setIdentityLocked(account, id, record)
		renamed := becomesAccount && proven.Name != "" && proven.Name != account.name
		if renamed {
			account.name = proven.Name
			account.fromValue = nil
		}
		if proven.SetRoles {
			account.granted = proven.Roles
		}
		// An account takes the roles its user_id or new identity is
		// granted.
		changed := s.grantRolesLocked(account) || renamed
		if proven.Session {
			// The result carries the account as `you`, so its own
			// connection is told no more.
			if changed {
				s.notifyProfileLocked(account, c, nil)
			}
			return s.signInLocked(c, req, account, now, false, false)
		}
		if changed {
			s.notifyProfileLocked(account, nil, nil)
		}
		result := map[string]any{}
		c.sendResult(req, result)
		return result, nil
	}
	if owner == nil {
		if !proven.Create {
			return nil, &Error{Code: CodeDenied, Message: "This " + label + " belongs to no account"}
		}
		if !s.admitsNewLocked(id) {
			return nil, notAdmitted()
		}
		record, err := commit(proven, "")
		if err != nil {
			return nil, err
		}
		user := newUserState(s.assignAccountIDLocked(proven.UserID), normalizeName(proven.Name))
		s.users[user.id] = user
		s.setIdentityLocked(user, id, record)
		if proven.SetRoles {
			user.granted = proven.Roles
		}
		s.grantRolesLocked(user)
		// A new account joins the default room, as a new guest does,
		// after the result (§3.2.2).
		return s.signInLocked(c, req, user, now, true, true)
	}
	if err := s.admitLocked(owner); err != nil {
		return nil, err
	}
	record, err := commit(proven, owner.id)
	if err != nil {
		return nil, err
	}
	if record != nil {
		s.setIdentityLocked(owner, id, record)
	}
	if proven.SetRoles && !slices.Equal(owner.granted, proven.Roles) {
		owner.granted = proven.Roles
		s.touchUser(owner.id)
		if s.grantRolesLocked(owner) {
			s.notifyProfileLocked(owner, nil, nil)
		}
	}
	// Signing in again as the user the connection is signed in as changes
	// no user: it is no sign-in (§4.5).
	return s.signInLocked(c, req, owner, now, c.user != owner, false)
}

// setIdentityLocked gives an account an identity, with its scheme's record
// if any.
func (s *Server) setIdentityLocked(u *userState, id authn.Identity, record jsontext.Value) {
	if u.identities == nil {
		u.identities = make(map[string]string)
	}
	u.identities[id.Scheme] = id.Subject
	if record != nil {
		if u.records == nil {
			u.records = make(map[string]jsontext.Value)
		}
		u.records[id.Scheme] = record
	}
	s.accounts[id] = u
	s.touchUser(u.id)
}

// approveLocked approves a proposal with its temporary token (§4.11.3),
// under the scheme that proposed it or token: the code of the connection's
// own proposal, or the link token of a proposal made on any connection.
// Approving a sign-in signs this connection, which must not be signed in,
// in to the identity's account or a new one. Approving an addition, only on
// a connection signed in to the proposing account, adds the identity to
// that account and returns {}. A token presented on a connection that may
// not approve it is denied, and its proposal stays for one that may.
func (s *Server) approveLocked(c *client, req request, token, name, requested string) (any, *Error) {
	p := s.proposalLocked(c, token, time.Now())
	if p == nil || p.proven == nil {
		return nil, &Error{Code: CodeDenied, Message: "The code is invalid or expired; ask for a new one"}
	}
	// A link token works only where its client is (§4.11.5).
	if p.returnOrigin != "" && c.proposal != p && !strings.EqualFold(c.origin, p.returnOrigin) {
		return nil, &Error{Code: CodeDenied, Message: "Open this link in the app that asked for it"}
	}
	proven := *p.proven
	if p.add {
		// Only the proposing account approves an addition: otherwise
		// someone could lead a victim to add the victim's identity to the
		// other person's account.
		if c.user == nil || c.user.id != p.accountID {
			return nil, &Error{Code: CodeDenied, Message: "Open this link where the account that asked for it is signed in"}
		}
		s.dropProposalLocked(p)
		proven.Add, proven.Session = true, false
		return s.applyProvenLocked(c, req, proven)
	}
	// A sign-in authenticates a connection that is not signed in; the
	// proposal stays for one that is not.
	if c.user != nil {
		return nil, &Error{Code: CodeDenied, Message: "Sign-in codes work on a connection that is not signed in"}
	}
	own := c.proposal == p
	s.dropProposalLocked(p)
	// A proposal's own name and user_id apply only on its connection: a
	// link opened elsewhere is someone reading the email, whose account
	// the proposer must not name.
	if own {
		name, requested = cmp.Or(name, p.name), cmp.Or(requested, p.userID)
	}
	proven.Name, proven.UserID = cmp.Or(name, proven.Name), cmp.Or(requested, proven.UserID)
	proven.Add = false
	return s.applyProvenLocked(c, req, proven)
}

// mayApproveLocked reports whether token is one that proposalLocked would
// check against a proposal: the connection's own, or the one its link
// handle names. Then a wrong token counts as a failed attempt under any
// scheme; any other token is no temporary token.
func (s *Server) mayApproveLocked(c *client, token string) bool {
	return c.proposal != nil || len(token) > linkHandleBytes && s.proposals.links[token[:linkHandleBytes]] != nil
}

// proposalLocked returns the live proposal that token approves: the
// connection's own, by its code, or any, by its link token. Otherwise it
// returns nil, and counts a failed attempt against the proposal that token
// names by its link handle, else against the connection's own, whichever
// connection presents it (§4.11.1): maxProposalAttempts of them cancel it.
// Expired proposals are dropped.
func (s *Server) proposalLocked(c *client, token string, now time.Time) *proposal {
	own := c.proposal
	if own != nil && (own.dropped || !now.Before(own.expires)) {
		s.dropProposalLocked(own)
		c.proposal, own = nil, nil
	}
	if own != nil && own.code != "" && subtle.ConstantTimeCompare([]byte(token), []byte(own.code)) == 1 {
		return own
	}
	target := own
	if len(token) > linkHandleBytes {
		if p := s.proposals.links[token[:linkHandleBytes]]; p != nil {
			if !now.Before(p.expires) {
				s.dropProposalLocked(p)
				return nil
			}
			if subtle.ConstantTimeCompare([]byte(token[linkHandleBytes:]), []byte(p.secret)) == 1 {
				return p
			}
			target = p
		}
	}
	if target != nil {
		if target.attempts++; target.attempts >= maxProposalAttempts {
			s.dropProposalLocked(target)
		}
	}
	return nil
}

// dropProposalLocked forgets a proposal: its connection's, its link, and
// its page.
func (s *Server) dropProposalLocked(p *proposal) {
	p.dropped = true
	if p.page != "" && s.proposals.pages[p.page] == p {
		delete(s.proposals.pages, p.page)
	}
	if p.handle != "" && s.proposals.links[p.handle] == p {
		delete(s.proposals.links, p.handle)
	}
}

// newLinkHandleLocked returns a link handle no outstanding link has.
func (s *Server) newLinkHandleLocked() string {
	for {
		if handle := rand.Text()[:linkHandleBytes]; s.proposals.links[handle] == nil {
			return handle
		}
	}
}

// pruneProposalsLocked forgets expired proposals in a full table, and
// reports whether it has room now.
func (s *Server) pruneProposalsLocked(table map[string]*proposal, limit int, now time.Time) bool {
	if len(table) < limit {
		return true
	}
	for _, p := range table {
		if !now.Before(p.expires) {
			s.dropProposalLocked(p)
		}
	}
	return len(table) < limit
}

// sixDigits is a code to type.
func sixDigits() (string, error) {
	number, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", number), nil
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

// authCore is the server's side of sign-in, which schemes call outside
// s.mu.
type authCore struct{ s *Server }

func (a authCore) Logger() *slog.Logger { return a.s.logger }

func (a authCore) NormalizeName(name string) string { return normalizeName(name) }

func (a authCore) Go(fn func(ctx context.Context)) {
	s := a.s
	s.mailing.Add(1)
	go func() {
		defer s.mailing.Done()
		fn(s.extContext)
	}()
}

// Propose replaces the connection's proposal with a new one (§4.11.1). A
// return_url must be on the connection's origin, or on Config.PublicURL's.
func (a authCore) Propose(r *authn.Request, spec authn.Proposal) (authn.Tokens, error) {
	s := a.s
	c := r.Conn.(*client)
	s.mu.Lock()
	defer s.unlock()
	if c.closing.Load() {
		return authn.Tokens{}, authn.Denied("This connection is closing")
	}
	var origin string
	if spec.ReturnURL != "" {
		parsed, err := url.Parse(spec.ReturnURL)
		if err != nil {
			return authn.Tokens{}, authn.InvalidParams("return_url must be an http or https URL")
		}
		origin = parsed.Scheme + "://" + parsed.Host
		if !s.trustedOriginLocked(c, origin) {
			return authn.Tokens{}, authn.Denied("return_url must be on the origin of this client")
		}
	}
	now := r.Now
	busy := authn.RetryAfter("Too many sign-ins in progress; try again shortly", 30*time.Second)
	linked := spec.Proven != nil && spec.Link != ""
	if (linked && !s.pruneProposalsLocked(s.proposals.links, maxLinks, now)) || (spec.Page && !s.pruneProposalsLocked(s.proposals.pages, maxPages, now)) {
		return authn.Tokens{}, busy
	}
	p := &proposal{
		add: c.user != nil, state: spec.State, expires: now.Add(proposalLifetime),
		name: spec.Name, userID: spec.UserID, returnURL: spec.ReturnURL, returnOrigin: origin,
		server: webSocketURL(c.baseURL),
	}
	if p.add {
		p.accountID = c.user.id
	}
	tokens := authn.Tokens{Expires: p.expires, Add: p.add}
	if spec.Proven != nil {
		proven := *spec.Proven
		p.proven = &proven
		code, err := sixDigits()
		if err != nil {
			return authn.Tokens{}, authn.Internal("Unable to create a code")
		}
		p.code, tokens.Code = code, code
		if linked {
			p.handle, p.secret = s.newLinkHandleLocked(), rand.Text()
			s.proposals.links[p.handle] = p
			// The server's address goes in the link even without a public
			// URL, so a link that opens a client not tied to one server,
			// such as one on link.apron.chat, says where to sign in
			// (§4.11.2).
			tokens.Link = signInLink(spec.Link, p.handle+p.secret, p.state, p.server)
		}
	}
	if spec.Page {
		p.page = rand.Text()
		s.proposals.pages[p.page] = p
		tokens.Page = p.page
	}
	if c.proposal != nil {
		s.dropProposalLocked(c.proposal)
	}
	c.proposal = p
	return tokens, nil
}

// livePageLocked returns the proposal a page names while it waits for its
// identity, dropping it once expired.
func (s *Server) livePageLocked(page string) *proposal {
	p := s.proposals.pages[page]
	if p == nil {
		return nil
	}
	if p.dropped || !time.Now().Before(p.expires) {
		s.dropProposalLocked(p)
		return nil
	}
	return p
}

func returnHost(returnURL string) string {
	if parsed, err := url.Parse(returnURL); err == nil {
		return parsed.Host
	}
	return ""
}

func (a authCore) Page(page string) (authn.Pending, bool) {
	s := a.s
	s.mu.Lock()
	defer s.unlock()
	p := s.livePageLocked(page)
	if p == nil {
		return authn.Pending{}, false
	}
	return authn.Pending{Add: p.add, ReturnHost: returnHost(p.returnURL)}, true
}

// Complete gives a page's proposal its identity, and its tokens, which
// expire within minutes of their delivery (§4.11.2).
func (a authCore) Complete(page string, proven authn.Proven) (authn.Completed, error) {
	s := a.s
	code, err := sixDigits()
	if err != nil {
		return authn.Completed{}, err
	}
	s.mu.Lock()
	defer s.unlock()
	p := s.livePageLocked(page)
	if p == nil {
		return authn.Completed{}, errors.New("apron: the proposal is unknown, expired, or completed")
	}
	proven.Roles = normalizeRoles(proven.Roles)
	proven.Create = true
	p.proven = &proven
	delete(s.proposals.pages, p.page)
	p.page = ""
	p.expires = time.Now().Add(proposalLifetime)
	p.code = code
	p.handle, p.secret = s.newLinkHandleLocked(), rand.Text()
	s.proposals.links[p.handle] = p
	completed := authn.Completed{Tokens: authn.Tokens{Code: code, Expires: p.expires, Add: p.add}}
	if p.returnURL != "" {
		completed.ReturnHost = returnHost(p.returnURL)
		completed.Link = signInLink(p.returnURL, p.handle+p.secret, p.state, p.server)
	}
	return completed, nil
}

// CanProve reports whether proving id on r would work: an addition when no
// account has the identity and the connection's has none of its scheme, and
// a sign-in when the identity's account, or a new one for it, may sign in.
func (a authCore) CanProve(r *authn.Request, id authn.Identity) bool {
	s := a.s
	c := r.Conn.(*client)
	s.mu.RLock()
	defer s.mu.RUnlock()
	owner := s.accounts[id]
	if c.user != nil {
		return owner == nil && c.user.identities[id.Scheme] == ""
	}
	if owner != nil {
		return s.admittedLocked(owner)
	}
	return s.admitsNewLocked(id)
}

// signInLink builds a link from a page, never from request fields other
// than state, with the token in the fragment so it stays out of server logs
// (§4.11.2): #token=<token>, &state=<state> with the proposal's state,
// unchanged, when it carried one, and &server=<URL> with this server's
// WebSocket URL when it is known, so a client that speaks to several
// servers presents it to the right one. Values are form-encoded.
func signInLink(page, token, state, server string) string {
	page, _, _ = strings.Cut(page, "#")
	link := page + "#token=" + url.QueryEscape(token)
	if state != "" {
		link += "&state=" + url.QueryEscape(state)
	}
	if server != "" {
		link += "&server=" + url.QueryEscape(server)
	}
	return link
}

// webSocketURL is the WebSocket endpoint under base, such as
// wss://chat.example/ws for https://chat.example: a connection's baseURL,
// Config.PublicURL or else the scheme and host it arrived on. It is "" for a
// base that isn't an http or https URL.
func webSocketURL(base string) string {
	parsed, err := url.Parse(base)
	if base == "" || err != nil {
		return ""
	}
	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	default:
		return ""
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/ws"
	parsed.RawQuery, parsed.Fragment = "", ""
	return parsed.String()
}

// assignAccountIDLocked honors a requested user_id as assignUserIDLocked
// does, and otherwise assigns the next unused user_<n>.
func (s *Server) assignAccountIDLocked(requested string) string {
	if requestable(requested) && s.claimUserIDLocked(requested) {
		return requested
	}
	for {
		s.accountNumber++
		if id := fmt.Sprintf("%s%d", accountIDPrefix, s.accountNumber); s.claimUserIDLocked(id) {
			return id
		}
	}
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

// RetryAfter is a CodeRetryAfter error suggesting wait, in whole seconds.
func RetryAfter(message string, wait time.Duration) *Error {
	return retryAfter(message, wait)
}

// retryAfter is a retry_after error suggesting wait, in whole seconds.
func retryAfter(message string, wait time.Duration) *Error {
	return &Error{Code: CodeRetryAfter, Message: message, Data: map[string]any{"retry_after": authn.Seconds(wait)}}
}
