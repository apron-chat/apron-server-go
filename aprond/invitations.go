package aprond

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/server"
	"github.com/apron-chat/apron-server-go/aprond/internal/invitations"
	"github.com/apron-chat/apron-server-go/internal/authn"
)

// Invitations. An invitation makes one account: a connection that is not
// signed in presents its token with a sign-in that can make an account
// without a guest to add to, such as a passkey registration
// (`"invitation"` in the auth params), and the account is made, admitted,
// given what the invitation grants, and the invitation used up, in one
// change, which is stored before the reply (fenceLocked). A sign-in that
// fails, or is abandoned, leaves the invitation until it expires. An
// invitations.Table keeps the outstanding invitations; the server keeps
// what using one makes, and what its enrollment admits.

const (
	// DefaultInvitationLifetime is how long an invitation lasts unless
	// InvitationOptions.Lifetime says otherwise.
	DefaultInvitationLifetime = 7 * 24 * time.Hour
	// invitationGrant is the key of the roles an invitation grants in an
	// account's grants, beside those its identities' schemes grant.
	invitationGrant = "invitation"
)

// invitation is an outstanding invitation, which an invitations.Table
// keeps by the SHA-256 of its token. Guarded by s.mu.
type invitation = invitations.Invitation

// InvitationOptions describe an invitation to make one account.
type InvitationOptions struct {
	// Lifetime is how long the invitation lasts; zero is
	// DefaultInvitationLifetime.
	Lifetime time.Duration
	// Name is the account's display name, unless the sign-in asks for
	// another; UserID the user_id it asks for, which it gets as a guest's
	// requested one would (§3.3), or user_<n>.
	Name, UserID string
	// Rooms are rooms the account joins, after the default room, in order.
	// It joins a private one as a member its creator added would, and
	// skips a room that is gone, or a thread it cannot see.
	Rooms []string
	// Roles are server roles the invitation grants the account, beside
	// those Config.Roles grants it, such as RoleAdmin for a server's first
	// account. They are kept with the account until RevokeEnrollment.
	Roles []string
	// Data is kept with the account the invitation makes, in the same
	// change, for whoever made the invitation to read with Enrollment, such
	// as an extension's grant to its owner. It is encoded as JSON now.
	Data any
}

// Invitation describes an invitation.
type Invitation struct {
	// ID names the invitation, to revoke it; it is not its token.
	ID string
	// Token is what a client presents to use it: Invite returns it once,
	// and the server keeps only its hash, so Invitations lists none.
	Token string
	// By names the extension that made it, empty for the program that
	// embeds the server.
	By               string
	Created, Expires time.Time
	Name, UserID     string
	Rooms, Roles     []string
}

// Enrollment is what an invitation left with the account it made.
type Enrollment struct {
	// Invitation is the ID of the invitation, and By who made it.
	Invitation, By string
	At             time.Time
	// Data is the invitation's InvitationOptions.Data, as JSON.
	Data jsontext.Value
	// Admitted reports that the invitation still admits the account to a
	// server whose sign-in an extension restricts (Registrar.RestrictSignIn),
	// whether or not the allowlist names it. Allowlist.Disallow, naming the
	// account by user_id or an identity, ends that, as does
	// RevokeEnrollment.
	Admitted bool
}

// Invite makes an invitation to make one account, and returns it with its
// token, once it is stored: a client presents the token with a sign-in
// that makes an account, such as a passkey registration on a connection
// that is not signed in, `{"scheme": "webauthn", "action": "register",
// "step": "begin", "invitation": "<token>"}`, and the finish step with the
// same token. The account is made, admitted, given what the invitation
// grants, and the invitation used up, in one change that is stored before
// the reply.
func (s *Server) Invite(ctx context.Context, options InvitationOptions) (Invitation, error) {
	return s.invite(ctx, "", options)
}

// Invite makes an invitation as the extension; see Server.Invite.
func (h *Host) Invite(ctx context.Context, options InvitationOptions) (Invitation, error) {
	return h.s.invite(ctx, h.name, options)
}

func (s *Server) invite(ctx context.Context, by string, options InvitationOptions) (Invitation, error) {
	lifetime := cmp.Or(options.Lifetime, DefaultInvitationLifetime)
	if lifetime < 0 {
		return Invitation{}, errors.New("apron: an invitation's lifetime is positive")
	}
	if options.UserID != "" && !requestable(options.UserID) {
		return Invitation{}, fmt.Errorf("apron: %q is no user_id an account can ask for", options.UserID)
	}
	if len(options.Rooms) > invitations.MaxRooms {
		return Invitation{}, fmt.Errorf("apron: an invitation joins at most %d rooms", invitations.MaxRooms)
	}
	var data jsontext.Value
	if options.Data != nil {
		raw, err := apron.Marshal(options.Data)
		if err != nil {
			return Invitation{}, fmt.Errorf("apron: invitation data: %w", err)
		}
		data = raw
	}
	token := invitations.NewToken()
	now := time.Now()
	inv := &invitation{
		ID: invitations.NewID(), By: by, Created: now, Expires: now.Add(lifetime),
		Name: normalizeName(options.Name), UserID: options.UserID,
		Rooms: slices.Clone(options.Rooms), Roles: normalizeRoles(options.Roles), Data: data,
	}
	s.mu.Lock()
	if s.store.Stopped() {
		s.unlock()
		return Invitation{}, ErrServerClosed
	}
	if !s.invitations.Add(token, inv, now) {
		s.unlock()
		return Invitation{}, fmt.Errorf("apron: at most %d invitations may be outstanding", invitations.Max)
	}
	f := s.newFenceLocked()
	s.unlock()
	if err := f.Wait(ctx); err != nil {
		return Invitation{}, err
	}
	described := describeInvitation(inv)
	described.Token = token
	return described, nil
}

func describeInvitation(inv *invitation) Invitation {
	return Invitation{
		ID: inv.ID, By: inv.By, Created: inv.Created, Expires: inv.Expires,
		Name: inv.Name, UserID: inv.UserID, Rooms: slices.Clone(inv.Rooms), Roles: slices.Clone(inv.Roles),
	}
}

// Invitations lists the outstanding invitations, oldest first, without
// their tokens.
func (s *Server) Invitations() []Invitation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var list []Invitation
	for _, inv := range s.invitations.List(time.Now()) {
		list = append(list, describeInvitation(inv))
	}
	return list
}

// Invitations lists the outstanding invitations; see Server.Invitations.
func (h *Host) Invitations() []Invitation { return h.s.Invitations() }

// RevokeInvitation forgets an outstanding invitation by its ID, once that
// is stored, and reports whether there was one. A registration that
// presented it fails as it finishes.
func (s *Server) RevokeInvitation(ctx context.Context, id string) (bool, error) {
	s.mu.Lock()
	found := s.invitations.Revoke(id)
	f := s.newFenceLocked()
	s.unlock()
	return found, f.Wait(ctx)
}

// RevokeInvitation forgets an invitation; see Server.RevokeInvitation.
func (h *Host) RevokeInvitation(ctx context.Context, id string) (bool, error) {
	return h.s.RevokeInvitation(ctx, id)
}

// Enrollment returns what an invitation left with the account it made, if
// an invitation made it.
func (s *Server) Enrollment(userID string) (Enrollment, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u := s.users.Get(userID)
	if u == nil || u.Enrolled == nil {
		return Enrollment{}, false
	}
	e := u.Enrolled
	return Enrollment{Invitation: e.Invitation, By: e.By, At: e.At, Data: slices.Clone(e.Data), Admitted: e.Admitted}, true
}

// Enrollment returns what an invitation left with an account; see
// Server.Enrollment.
func (h *Host) Enrollment(userID string) (Enrollment, bool) { return h.s.Enrollment(userID) }

// RevokeEnrollment takes back what an invitation granted an account: the
// roles it granted, and its admission to a restricted server, which signs
// the account out everywhere unless the allowlist admits it. The account,
// and the enrollment's Data, stay. It returns once that is stored.
func (s *Server) RevokeEnrollment(ctx context.Context, userID string) error {
	s.mu.Lock()
	if u := s.users.Get(userID); u != nil && u.Enrolled != nil {
		s.revokeEnrollmentLocked(u)
	}
	f := s.newFenceLocked()
	s.unlock()
	return f.Wait(ctx)
}

func (s *Server) revokeEnrollmentLocked(u *userState) {
	u.Enrolled.Admitted = false
	s.users.Touch(u.id)
	if before := u.views(); s.setGrantLocked(u, invitationGrant, nil) && s.grantRolesLocked(u) {
		s.notifyProfileLocked(u, nil, before)
	}
	if s.allowlist != nil && !s.admittedLocked(u) {
		s.signOutLocked(u)
	}
}

// presentedInvitationLocked returns the invitation an auth request presents
// with "invitation", if any, and refuses one that is unknown, used, or
// expired, or presented on a connection signed in already.
func (s *Server) presentedInvitationLocked(c *client, reply server.Reply[apron.AuthResult]) (*invitation, *Error) {
	d := authParams(reply).Decoder()
	token := server.IfPresent(d, "invitation", d.String)
	if err := paramError(d.Err()); err != nil || token == nil {
		return nil, err
	}
	if c.user != nil {
		return nil, &Error{Code: CodeDenied, Message: "An invitation makes a new account; sign out first"}
	}
	inv := s.invitations.Presented(*token, time.Now())
	if inv == nil {
		return nil, &Error{Code: CodeDenied, Message: "This invitation is invalid, used, or expired"}
	}
	return inv, nil
}

// enrollLocked makes a new account for an identity a scheme proved with an
// invitation, which it uses up, then signs the connection in to it. The
// invitation, the identity, and the allowlist are checked again first,
// since the scheme ran outside s.mu, and nothing changes unless all pass.
// The reply waits until the change is stored.
func (s *Server) enrollLocked(c *client, reply server.Reply[apron.AuthResult], scheme authn.Scheme, proof authn.Proof, invitationID string, anyOrigin bool) *Error {
	inv, err := s.presentedInvitationLocked(c, reply)
	if err != nil {
		return err
	}
	if inv == nil || inv.ID != invitationID {
		return &Error{Code: CodeDenied, Message: "This invitation is invalid, used, or expired"}
	}
	id, err := identityOf(scheme, proof)
	if err != nil {
		return err
	}
	if s.users.ByIdentity(id) != nil {
		return &Error{Code: CodeDenied, Message: "This " + scheme.Label() + " belongs to an account"}
	}
	record, err := commit(proof)
	if err != nil {
		return err
	}
	now := time.Now()
	user := s.enrollAccountLocked(inv, mustString(authParams(reply), "invitation"), id, record, proof, now)
	// Nothing the connection is sent from here on goes before the account
	// and the used invitation are stored.
	c.fenceLocked()
	if err := s.signInLocked(c, reply, user, now, true, true, anyOrigin); err != nil {
		return err
	}
	s.provedLocked(c, scheme, proof, now, true)
	s.joinInvitedLocked(user, inv)
	return nil
}

// enrollAccountLocked makes the account that the invitation of token makes
// for an identity, with the record its scheme keeps and the name, user_id,
// and roles its proof asked for, and uses the invitation up.
func (s *Server) enrollAccountLocked(inv *invitation, token string, id authn.Identity, record jsontext.Value, proof authn.Proof, now time.Time) *userState {
	user := newUserState(s.assignAccountIDLocked(cmp.Or(proof.UserID, inv.UserID)), normalizeName(cmp.Or(proof.Name, inv.Name)))
	user.Enrolled = inv.Enroll(now)
	s.users.Add(user)
	s.users.SetIdentity(user, id, record)
	s.setGrantLocked(user, id.Scheme, proof.Roles)
	s.setGrantLocked(user, invitationGrant, inv.Roles)
	s.grantRolesLocked(user)
	s.invitations.Use(token)
	return user
}

// joinInvitedLocked joins an account to the rooms its invitation names, in
// order, skipping a room that is gone, or a thread it cannot see.
func (s *Server) joinInvitedLocked(user *userState, inv *invitation) {
	for _, roomID := range inv.Rooms {
		r := s.rooms.Get(roomID)
		if r == nil || (r.Parent() != nil && !r.Parent().VisibleTo(user)) {
			continue
		}
		s.joinLocked(user, r)
	}
}

// mustString is a string param that parsing found already.
func mustString(params server.Fields, name string) string {
	value, _ := parseString(params, name, required)
	return value
}

// enrolledNamedLocked ends the invitation's admission of the accounts a
// principal names, by user_id or identity, as when Allowlist.Disallow
// takes it off the list.
func (s *Server) enrolledNamedLocked(p Principal) {
	for _, u := range s.users.All() {
		if u.Enrolled == nil || !u.Enrolled.Admitted {
			continue
		}
		if p == UserID(u.id) || slices.ContainsFunc(slices.Collect(maps.Keys(u.Identities)), func(scheme string) bool {
			return principalOf(authn.Identity{Scheme: scheme, Subject: u.Identities[scheme]}) == p
		}) {
			u.Enrolled.Admitted = false
			s.users.Touch(u.id)
		}
	}
}
