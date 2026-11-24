package apron

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/apron-chat/apron-server-go/apron/internal/accounts"
	"github.com/apron-chat/apron-server-go/apron/internal/proposals"
	"github.com/apron-chat/apron-server-go/internal/auth/email"
	"github.com/apron-chat/apron-server-go/internal/auth/external"
	"github.com/apron-chat/apron-server-go/internal/auth/passkey"
	"github.com/apron-chat/apron-server-go/internal/authn"
	"github.com/apron-chat/apron-server-go/wire"
)

// Sign-in (PROTOCOL.md §3.2) is split between the server and its schemes
// (internal/authn). A scheme, such as passkeys or email, proves who a
// connection is, outside s.mu, from a copy of the request. The server owns
// what follows: accounts and the identities that prove them, bearer tokens,
// the proposals of sign-ins that finish out of band (§4.11, kept in a
// proposals.Table), the allowlist, and roles, which it changes in one
// place, applyProvenLocked, under s.mu. The guest and token schemes are
// the server's own.

const (
	// proposalsPerConnection proposals, of any scheme, may be made on one
	// connection at once, refilled one per proposalRefill.
	proposalsPerConnection = 5
	proposalRefill         = 2 * time.Minute
	// accountIDPrefix starts the user_ids assigned to new accounts that did
	// not request one.
	accountIDPrefix = accounts.AccountPrefix
)

// disabledSchemes say why each scheme this server knows may be off.
var disabledSchemes = map[string]string{
	passkey.Name:  passkey.Disabled,
	email.Name:    email.Disabled,
	external.Name: external.Disabled,
}

// reservedSchemeNames are no scheme's: the server's own schemes, and the
// kind of a Principal that names a user_id.
var reservedSchemeNames = []string{"guest", "token", principalUser}

// addSchemeLocked offers a sign-in scheme, under a name no other scheme
// has. Called holding s.mu.
func (s *Server) addSchemeLocked(scheme authn.Scheme) error {
	name := scheme.Name()
	switch {
	case name == "" || !validID(name) || strings.Contains(name, ":"):
		return fmt.Errorf("a sign-in scheme is named %q, which is no scheme name", name)
	case slices.Contains(reservedSchemeNames, name):
		return fmt.Errorf("a sign-in scheme is named %q, which is reserved", name)
	case s.schemeLocked(name) != nil:
		return fmt.Errorf("two sign-in schemes are named %q", name)
	}
	if binder, ok := scheme.(authn.Binder); ok {
		binder.Bind(schemeCore{s: s, scheme: scheme})
	}
	s.schemes = append(s.schemes, scheme)
	return nil
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

// restoreSchemesLocked hands each scheme the records it keeps with its
// identities (authn.Restorer), once every scheme is added: those the
// server offers, and those the extensions offer.
func (s *Server) restoreSchemesLocked() error {
	for _, scheme := range s.schemes {
		restorer, ok := scheme.(authn.Restorer)
		if !ok {
			continue
		}
		name := scheme.Name()
		for _, id := range s.users.IDs() {
			u := s.users.Get(id)
			if subject, record := u.Identities[name], u.Records[name]; subject != "" && record != nil {
				if err := restorer.Restore(subject, record); err != nil {
					return fmt.Errorf("stored user %q: %s record: %w", id, name, err)
				}
			}
		}
	}
	return nil
}

// authenticate runs an auth request (§3.2): the guest and token schemes
// under s.mu, and the others by their scheme, outside it, whose result is
// applied under it.
func (s *Server) authenticate(c *client, reply wire.Reply[wire.AuthResult]) *Error {
	s.mu.Lock()
	if c.conn.Closing() {
		s.unlock()
		return connectionClosing()
	}
	if c.restrictedService() {
		// It may not add to the account, nor become another.
		s.unlock()
		return serviceRefusal()
	}
	if c.session != nil {
		// A bot's session stays signed in as its bot (Session.Conn).
		s.unlock()
		return Denied("A bot's session stays signed in as its bot")
	}
	name, err := parseString(authParams(reply), "scheme", required)
	if err != nil {
		s.unlock()
		return err
	}
	// A scheme this document defines but the server does not offer is
	// unsupported (§3.2), whatever else the request holds.
	scheme := s.schemeLocked(name)
	switch {
	case name == "guest" && !s.guestsAllowedLocked():
		err = &Error{Code: CodeUnsupported, Message: "This server has no guests; sign in with an account"}
	case name == "token" && len(s.schemes) == 0 && !presentsServiceToken(reply) && !s.presentsSessionLocked(reply):
		err = &Error{Code: CodeUnsupported, Message: "This server has no sign-in sessions; use guest"}
	case name == "guest", name == "token", scheme != nil:
	case disabledSchemes[name] != "":
		err = &Error{Code: CodeUnsupported, Message: disabledSchemes[name]}
	default:
		// A scheme the server does not offer is unsupported, even one the
		// protocol does not define (§3.2).
		err = &Error{Code: CodeUnsupported, Message: fmt.Sprintf("Unknown authentication scheme %q", name)}
	}
	// A user_id is never empty, nor contains whitespace or control
	// characters (§2.4): requesting one that could be no user_id is
	// invalid_params, with any scheme. One that could be, but that this
	// server would not assign, is ignored where the scheme assigns one.
	if err == nil {
		d := authParams(reply).Decoder()
		if requested := d.String("user_id", optional); d.Has("user_id") && d.Err() == nil && !validID(requested) {
			d.Fail(invalidParams("user_id must not be empty, or contain whitespace or control characters"))
		}
		err = paramError(d.Err())
	}
	if err != nil {
		s.unlock()
		return err
	}
	switch name {
	case "guest":
		defer s.unlock()
		return s.authenticateGuestLocked(c, reply)
	case "token":
		defer s.unlock()
		return s.authenticateToken(c, reply)
	}
	requestedName, requested, err := parseRequested(reply)
	if err != nil {
		s.unlock()
		return err
	}
	presented, err := s.presentedInvitationLocked(c, reply)
	if err != nil {
		s.unlock()
		return err
	}
	r := s.authRequestLocked(c, reply, scheme, requestedName, requested)
	if presented != nil {
		r.Invitation = &authn.Invitation{ID: presented.ID, Name: presented.Name, UserID: presented.UserID}
	}
	s.unlock()

	result, schemeErr := scheme.Auth(s.requestContext(c), r)
	if schemeErr != nil {
		return authError(schemeErr)
	}
	if own, ok := result.(*authn.Reply); ok && own != nil {
		// A scheme's own result, such as a ceremony's options, signs
		// nothing in, and names neither you nor token.
		_ = reply.Send(wire.AuthResult{Extra: resultFields(own.Result)})
		return nil
	}
	s.mu.Lock()
	defer s.unlock()
	if c.conn.Closing() {
		return connectionClosing()
	}
	switch result := result.(type) {
	case *authn.Approve:
		if result != nil {
			return s.approveLocked(c, reply, result.Token, result.State, requestedName, requested)
		}
	case *authn.SignIn:
		if result != nil && result.Invitation != "" {
			return s.enrollLocked(c, reply, scheme, result.Proof, result.Invitation, result.AnyOrigin)
		}
		if result != nil {
			return s.signInProvenLocked(c, reply, scheme, result.Proof, result.Create, result.AnyOrigin)
		}
	case *authn.Add:
		if result != nil {
			return s.addProvenLocked(c, reply, scheme, result.Proof, result.Session, result.AnyOrigin)
		}
	}
	s.logger.Error("a sign-in scheme answered with no result", "scheme", name, "result", fmt.Sprintf("%T", result))
	return internalError()
}

// authParams are an auth request's params, as they arrived, which the
// schemes read too.
func authParams(reply wire.Reply[wire.AuthResult]) wire.Fields {
	return reply.Request().Fields()
}

// resultFields are the fields of a scheme's own result, an object, which
// an auth result sends beside you and token (wire.AuthResult.Extra).
func resultFields(result any) map[string]any {
	if fields, ok := result.(map[string]any); ok {
		return fields
	}
	var fields map[string]any
	_ = json.Unmarshal(encodeJSON(result), &fields)
	return fields
}

// presentsServiceToken reports whether a token auth presents a bot's
// service token, which signs in on a server without sign-in schemes too.
func presentsServiceToken(reply wire.Reply[wire.AuthResult]) bool {
	token, _ := parseString(authParams(reply), "token", optional)
	return strings.HasPrefix(token, serviceTokenPrefix)
}

// presentsSessionLocked reports whether a token auth presents the token of
// a session the server holds, which signs in on a server without sign-in
// schemes too, as one made in process does (testhook.Member), or one
// from before the server's schemes were taken away. Called holding s.mu.
func (s *Server) presentsSessionLocked(reply wire.Reply[wire.AuthResult]) bool {
	token, _ := parseString(authParams(reply), "token", optional)
	if token == "" {
		return false
	}
	_, ok := s.sessions.Get(sha256.Sum256([]byte(token)))
	return ok
}

// authenticateGuestLocked signs the connection in as a new guest, or
// answers with the identity a signed-in connection has.
func (s *Server) authenticateGuestLocked(c *client, reply wire.Reply[wire.AuthResult]) *Error {
	name, requested, err := parseRequested(reply)
	if err != nil {
		return err
	}
	if c.user != nil {
		// Guest auth on a signed-in connection answers with the identity it
		// has. It signs the connection in as no other user, so it is no
		// sign-in and sends nothing after its result (§4.5).
		you := c.user.you()
		result := wire.AuthResult{You: &you}
		_ = reply.Send(result)
		return nil
	}
	user := newUserState(s.assignUserIDLocked(requested), normalizeName(name))
	s.users.Add(user)
	// A new guest joins the default room, so their room list is not empty.
	s.switchUserLocked(c, reply, user, "", true, true)
	return nil
}

// authRequestLocked is the copy of an auth request a scheme sees.
func (s *Server) authRequestLocked(c *client, reply wire.Reply[wire.AuthResult], scheme authn.Scheme, name, requested string) *authn.Request {
	now := time.Now()
	r := &authn.Request{
		Params: maps.Clone(authParams(reply)), Name: normalizeName(name), UserID: requested,
		Origin: c.origin, BaseURL: c.baseURL, ClientKey: c.clientKey, Now: now,
		Conn:   &schemeConn{s: s, c: c, scheme: scheme, name: name, userID: requested, now: now},
		Guests: s.guestsAllowedLocked(),
	}
	if c.user != nil {
		r.Account = &authn.Account{
			UserID: c.user.id, Name: c.user.Name, Guest: !c.user.Account(),
			Identities: maps.Clone(c.user.Identities),
		}
	}
	if c.authStates == nil {
		c.authStates = make(map[string]*authn.State)
	}
	if c.authStates[scheme.Name()] == nil {
		c.authStates[scheme.Name()] = new(authn.State)
	}
	r.State = c.authStates[scheme.Name()]
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

// identityOf names the identity a scheme proved, by the scheme's own name,
// so that a scheme proves none of another's.
func identityOf(scheme authn.Scheme, proof authn.Proof) (authn.Identity, *Error) {
	if proof.Subject == "" {
		slog.Error("a sign-in scheme proved an empty subject", "scheme", scheme.Name())
		return authn.Identity{}, internalError()
	}
	return authn.Identity{Scheme: scheme.Name(), Subject: proof.Subject}, nil
}

// commit runs a proof's Commit, which returns the record its scheme keeps
// with the identity.
func commit(proof authn.Proof) (jsontext.Value, *Error) {
	if proof.Commit == nil {
		return nil, nil
	}
	record, err := proof.Commit()
	if err != nil {
		return nil, authError(err)
	}
	return record, nil
}

// addProvenLocked adds an identity a scheme proved to the connection's
// account, a guest's making it an account, with the name the proof
// requested, unless another account has the identity or the account has
// another of its scheme. With session, it replies as a sign-in does, with
// a bearer token, which anyOrigin lets resume from any origin; otherwise
// with the account as you. Every check, the allowlist's included, comes
// before anything changes (§3.2.2).
func (s *Server) addProvenLocked(c *client, reply wire.Reply[wire.AuthResult], scheme authn.Scheme, proof authn.Proof, session, anyOrigin bool) *Error {
	id, err := identityOf(scheme, proof)
	if err != nil {
		return err
	}
	label := scheme.Label()
	account := c.user
	switch owner := s.users.ByIdentity(id); {
	case account == nil:
		return &Error{Code: CodeDenied, Message: "Sign in before adding a " + label}
	case owner != nil && owner != account:
		return &Error{Code: CodeDenied, Message: "This " + label + " belongs to an account"}
	case account.Identities[id.Scheme] != "" && account.Identities[id.Scheme] != id.Subject:
		return &Error{Code: CodeDenied, Message: "This account has another " + label + " already"}
	case !s.admittedWithLocked(account, id):
		return notAdmitted()
	}
	if _, manages := scheme.(authn.Credentials); manages && account.Identities[id.Scheme] != "" && !s.config.AddCredentialWithoutReauth {
		// Another passkey for an account that has one: a stolen session
		// token must not add a way back in, nor one it could then sign in
		// with to pass as the owner.
		if err := s.reauthenticatedLocked(c); err != nil {
			return err
		}
	}
	// Only a guest's first identity counts as proof of the account: it is
	// the account's only one.
	wasGuest := !account.Account()
	record, err := commit(proof)
	if err != nil {
		return err
	}
	becomesAccount := !account.Account()
	before := account.views()
	s.users.SetIdentity(account, id, record)
	name := normalizeName(proof.Name)
	renamed := becomesAccount && name != "" && name != account.Name
	if renamed {
		account.Name = name
	}
	s.setGrantLocked(account, id.Scheme, proof.Roles)
	// An account takes the roles its user_id or new identity is granted.
	changed := s.grantRolesLocked(account) || renamed
	// The result carries the account as `you`, so its own connection is
	// told no more.
	if changed {
		s.notifyProfileLocked(account, c, before)
	}
	if session {
		now := time.Now()
		err := s.signInLocked(c, reply, account, now, false, false, anyOrigin)
		if err == nil {
			s.provedLocked(c, scheme, proof, now, wasGuest)
		}
		return err
	}
	s.provedLocked(c, scheme, proof, time.Now(), wasGuest)
	you := account.you()
	_ = reply.Send(wire.AuthResult{You: &you})
	return nil
}

// signInProvenLocked signs the connection in to the account of an identity
// a scheme proved, making one if none has it and the proof may (create),
// with a token that anyOrigin lets resume from any origin. Every check, the
// allowlist's included, comes before anything changes (§3.2.2).
func (s *Server) signInProvenLocked(c *client, reply wire.Reply[wire.AuthResult], scheme authn.Scheme, proof authn.Proof, create, anyOrigin bool) *Error {
	user, created, err := s.provenAccountLocked(scheme, proof, create)
	if err != nil {
		return err
	}
	now := time.Now()
	// A new account joins the default room, as a new guest does, after
	// the result (§3.2.2). Signing in again as the user the connection is
	// signed in as changes no user: it is no sign-in (§4.5), but it is a
	// reauthentication.
	err = s.signInLocked(c, reply, user, now, created || c.user != user, created, anyOrigin)
	if err == nil {
		s.provedLocked(c, scheme, proof, now, true)
	}
	return err
}

// provenAccountLocked returns the account of an identity a scheme proved,
// after its checks and its Commit: the identity's, or, with create, a new
// one for an identity that none has, which takes the user_id the proof
// requested as guests do, or user_<n>; created reports a new one. It
// changes nothing it refuses.
func (s *Server) provenAccountLocked(scheme authn.Scheme, proof authn.Proof, create bool) (user *userState, created bool, err *Error) {
	id, err := identityOf(scheme, proof)
	if err != nil {
		return nil, false, err
	}
	owner := s.users.ByIdentity(id)
	if owner == nil {
		switch {
		case !create:
			return nil, false, &Error{Code: CodeDenied, Message: "This " + scheme.Label() + " belongs to no account"}
		case !s.admitsNewLocked(id):
			return nil, false, notAdmitted()
		}
		record, err := commit(proof)
		if err != nil {
			return nil, false, err
		}
		user := newUserState(s.assignAccountIDLocked(proof.UserID), normalizeName(proof.Name))
		s.users.Add(user)
		s.users.SetIdentity(user, id, record)
		s.setGrantLocked(user, id.Scheme, proof.Roles)
		s.grantRolesLocked(user)
		return user, true, nil
	}
	if err := s.admitLocked(owner); err != nil {
		return nil, false, err
	}
	record, err := commit(proof)
	if err != nil {
		return nil, false, err
	}
	if record != nil {
		s.users.SetIdentity(owner, id, record)
	}
	if before := owner.views(); s.setGrantLocked(owner, id.Scheme, proof.Roles) && s.grantRolesLocked(owner) {
		s.notifyProfileLocked(owner, nil, before)
	}
	return owner, false, nil
}

// setGrantLocked sets the roles an account's identity of scheme grants it,
// and reports whether they changed; grantRolesLocked then applies them.
func (s *Server) setGrantLocked(u *userState, scheme string, roles []string) bool {
	roles = normalizeRoles(roles)
	if slices.Equal(u.Granted[scheme], roles) {
		return false
	}
	if len(roles) == 0 {
		delete(u.Granted, scheme)
	} else {
		if u.Granted == nil {
			u.Granted = make(map[string][]string)
		}
		u.Granted[scheme] = roles
	}
	s.users.Touch(u.id)
	return true
}

// approveLocked approves a proposal with its temporary token (§4.11.3),
// under the scheme that proposed it or token: the code of the connection's
// own proposal, the code of a proposal with state, its state, or the link
// token of a proposal made on any connection.
// Approving a sign-in signs this connection, which must not be signed in,
// in to the identity's account or a new one. Approving an addition, only on
// a connection signed in to the proposing account, adds the identity to
// that account and returns it as you. A token presented on a connection that may
// not approve it is denied, and its proposal stays for one that may.
func (s *Server) approveLocked(c *client, reply wire.Reply[wire.AuthResult], token, state, name, requested string) *Error {
	p := s.proposalLocked(c, token, state, time.Now())
	if p == nil || p.Proof == nil {
		return &Error{Code: CodeDenied, Message: "The code is invalid or expired; ask for a new one"}
	}
	proof := *p.Proof
	if p.Add {
		// Only the proposing account approves an addition: otherwise
		// someone could lead a victim to add the victim's identity to the
		// other person's account.
		if c.user == nil || c.user.id != p.AccountID {
			return &Error{Code: CodeDenied, Message: "Open this link where the account that asked for it is signed in"}
		}
		s.proposals.Drop(p)
		// An addition names nobody: the account keeps its name.
		proof.Name, proof.UserID = "", ""
		return s.addProvenLocked(c, reply, p.Scheme, proof, false, false)
	}
	// A sign-in authenticates a connection that is not signed in; the
	// proposal stays for one that is not.
	if c.user != nil {
		return &Error{Code: CodeDenied, Message: "Sign-in codes work on a connection that is not signed in"}
	}
	own := c.proposal == p
	s.proposals.Drop(p)
	// A proposal's own name and user_id apply only on its connection: a
	// link opened elsewhere is someone reading the email, whose account
	// the proposer must not name, and so may be a code with the state that
	// the link carries.
	if own {
		name, requested = cmp.Or(name, p.Name), cmp.Or(requested, p.UserID)
	}
	proof.Name, proof.UserID = cmp.Or(name, proof.Name), cmp.Or(requested, proof.UserID)
	return s.signInProvenLocked(c, reply, p.Scheme, proof, true, false)
}

// mayApproveLocked reports whether token is one that proposalLocked would
// check against a proposal (proposals.Table.Names).
func (s *Server) mayApproveLocked(c *client, token, state string) bool {
	return s.proposals.Names(c.proposal, token, state)
}

// proposalLocked returns the live proposal that token approves, or nil
// (proposals.Table.Approve), forgetting the connection's own proposal once
// it is no longer live.
func (s *Server) proposalLocked(c *client, token, state string, now time.Time) *proposals.Proposal {
	if own := c.proposal; own != nil && !own.Live(now) {
		s.proposals.Drop(own)
		c.proposal = nil
	}
	return s.proposals.Approve(c.proposal, token, state, now)
}

// schemeConn is the server's side of one auth request's connection, for
// its scheme (authn.Conn). The scheme calls it outside s.mu.
type schemeConn struct {
	s      *Server
	c      *client
	scheme authn.Scheme
	// name and userID are what the request asked for a new account.
	name, userID string
	now          time.Time
}

// ProposeCode proposes what the scheme proved already (§4.11.1).
func (sc *schemeConn) ProposeCode(spec authn.CodeProposal) (authn.Tokens, error) {
	if spec.Proof.Subject == "" {
		return authn.Tokens{}, errors.New("apron: a proposal proves an empty subject")
	}
	s := sc.s
	code, err := proposals.Code()
	if err != nil {
		return authn.Tokens{}, err
	}
	s.mu.Lock()
	defer s.unlock()
	if !s.proposals.Room(spec.LinkPage != "", spec.State != "", sc.now) {
		return authn.Tokens{}, proposalsBusy()
	}
	p, refusal := sc.proposeLocked(spec.State)
	if refusal != nil {
		return authn.Tokens{}, refusal
	}
	proof := spec.Proof
	p.Proof, p.Code = &proof, code
	tokens := authn.Tokens{Code: code, Server: p.Server, Expires: p.Expires, Add: p.Add}
	if token := s.proposals.Offer(p, spec.LinkPage != ""); token != "" {
		// The server's address goes in the link even without a public URL,
		// so a link that opens a client not tied to one server, such as one
		// on link.apron.chat, says where to sign in (§4.11.2).
		tokens.Link = signInLink(spec.LinkPage, token, p.State, p.Server)
	}
	return tokens, nil
}

// proposeLocked replaces the connection's proposal with a new one, which
// takes one of the connection's proposals (proposalsPerConnection).
func (sc *schemeConn) proposeLocked(state string) (*proposals.Proposal, *authn.Error) {
	s, c := sc.s, sc.c
	if c.conn.Closing() {
		return nil, authn.Denied("This connection is closing")
	}
	if c.proposalBudget == nil {
		c.proposalBudget = rate.NewLimiter(rate.Every(proposalRefill), proposalsPerConnection)
	}
	reservation := c.proposalBudget.ReserveN(sc.now, 1)
	if delay := reservation.DelayFrom(sc.now); delay > 0 {
		reservation.CancelAt(sc.now)
		return nil, authn.RetryAfter("Too many sign-ins proposed; try again later", delay)
	}
	p := &proposals.Proposal{
		Scheme: sc.scheme, Add: c.user != nil, State: state, Expires: sc.now.Add(proposals.Lifetime),
		Name: sc.name, UserID: sc.userID, Server: webSocketURL(c.baseURL),
	}
	if p.Add {
		p.AccountID = c.user.id
	}
	if c.proposal != nil {
		s.proposals.Drop(c.proposal)
	}
	c.proposal = p
	return p, nil
}

func proposalsBusy() *authn.Error {
	return authn.RetryAfter("Too many sign-ins in progress; try again shortly", 30*time.Second)
}

// CanProve reports whether proving subject would work: an addition when no
// account has the identity and the connection's has none of its scheme, and
// a sign-in when the identity's account, or a new one for it, may sign in.
func (sc *schemeConn) CanProve(subject string) bool {
	s, c := sc.s, sc.c
	id := authn.Identity{Scheme: sc.scheme.Name(), Subject: subject}
	s.mu.RLock()
	defer s.mu.RUnlock()
	owner := s.users.ByIdentity(id)
	if c.user != nil {
		return owner == nil && c.user.Identities[id.Scheme] == "" && s.admittedWithLocked(c.user, id)
	}
	if owner != nil {
		return s.admittedLocked(owner)
	}
	return s.admitsNewLocked(id)
}

// schemeCore is the server's side of a scheme outside its requests
// (authn.Core), which sees only the scheme's own identities.
type schemeCore struct {
	s      *Server
	scheme authn.Scheme
}

func (sc schemeCore) Logger() *slog.Logger { return sc.s.logger.With("scheme", sc.scheme.Name()) }

// BaseURL is the base of the server's URLs for a request to a scheme's
// route, as for a WebSocket request.
func (sc schemeCore) BaseURL(r *http.Request) string { return sc.s.baseURL(r) }

// ClientKey is the client address of a request to a scheme's route, as
// for a WebSocket request.
func (sc schemeCore) ClientKey(r *http.Request) string { return clientKey(sc.s.clientIP(r)) }

// Title is the server's display name, as the server frame has it.
func (sc schemeCore) Title() string {
	sc.s.mu.RLock()
	defer sc.s.mu.RUnlock()
	title, _ := sc.s.profileValueLocked("title")
	return title
}

// Lookup returns the account that has an identity of the scheme.
func (sc schemeCore) Lookup(subject string) (authn.Account, bool) {
	s := sc.s
	s.mu.RLock()
	defer s.mu.RUnlock()
	u := s.users.ByIdentity(authn.Identity{Scheme: sc.scheme.Name(), Subject: subject})
	if u == nil {
		return authn.Account{}, false
	}
	return authn.Account{UserID: u.id, Name: u.Name, Guest: !u.Account(), Identities: maps.Clone(u.Identities)}, true
}

// Admits reports whether an identity of the scheme could sign in: to its
// account, or, with create, to a new one.
func (sc schemeCore) Admits(subject string, create bool) bool {
	s := sc.s
	id := authn.Identity{Scheme: sc.scheme.Name(), Subject: subject}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if owner := s.users.ByIdentity(id); owner != nil {
		return s.admittedLocked(owner)
	}
	return create && s.admitsNewLocked(id)
}

// Issue signs in outside any connection, as the token endpoint of a
// scheme's page does (§4.12.3), and returns a bearer token for the
// account, which resumes from any origin, as a token from a code does
// (§4.12.4).
func (sc schemeCore) Issue(proof authn.Proof, create bool) (string, error) {
	s := sc.s
	s.mu.Lock()
	defer s.unlock()
	if s.closed {
		return "", authn.RetryAfter("The server is shutting down", retryAfterSeconds*time.Second)
	}
	user, created, err := s.provenAccountLocked(sc.scheme, proof, create)
	if err == nil {
		err = s.admitLocked(user)
	}
	if err != nil {
		return "", &authn.Error{Code: err.Code, Message: err.Message, Data: err.Data}
	}
	token, key, err := s.newSessionLocked(user, "", true, time.Now())
	if err != nil {
		return "", &authn.Error{Code: err.Code, Message: err.Message}
	}
	if session, ok := s.sessions.Get(key); ok {
		session.Credential = credentialKey(sc.scheme.Name(), proof.Credential)
		s.sessions.Put(key, session)
	}
	if created {
		s.joinDefaultRoomLocked(user)
	}
	return token, nil
}

// Go runs fn as the scheme's work, which Shutdown waits for, unless the
// server is closed.
func (sc schemeCore) Go(fn func(ctx context.Context)) bool {
	s := sc.s
	s.mu.Lock()
	closed := s.closed
	if !closed {
		s.schemeWork.Add(1)
	}
	s.mu.Unlock()
	if closed {
		return false
	}
	go func() {
		defer s.schemeWork.Done()
		fn(s.extContext)
	}()
	return true
}

// signInLink builds a link from a page, never from request fields other
// than state, with the token in the fragment so it stays out of server logs
// (§4.11.2): #token=<token>, &state=<state> with the proposal's state,
// unchanged, when it carried one, and &server=<URL> with this server's
// WebSocket URL when it is known, so a client that speaks to several
// servers presents it to the right one. Values are form-encoded, and
// server with `:` and `/` left as they are, as a link spells it
// (PROTOCOL.md Appendix A.4).
func signInLink(page, token, state, server string) string {
	page, _, _ = strings.Cut(page, "#")
	link := page + "#token=" + url.QueryEscape(token)
	if state != "" {
		link += "&state=" + url.QueryEscape(state)
	}
	if server != "" {
		link += "&server=" + linkEscape(server)
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
	return s.ids.Account(requested, s.roomNamedLocked)
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
