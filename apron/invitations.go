package apron

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/apron-chat/apron-server-go/internal/authn"
)

// Invitations. An invitation makes one account: a connection that is not
// signed in presents its token with a sign-in that can make an account
// without a guest to add to, such as a passkey registration
// (`"invitation"` in the auth params), and the account is made, admitted,
// given what the invitation grants, and the invitation used up, in one
// change, which is stored before the reply (fenceLocked). A sign-in that
// fails, or is abandoned, leaves the invitation until it expires.

const (
	// DefaultInvitationLifetime is how long an invitation lasts unless
	// InvitationOptions.Lifetime says otherwise.
	DefaultInvitationLifetime = 7 * 24 * time.Hour
	// maxInvitations bounds the invitations outstanding at once.
	maxInvitations = 1000
	// invitationTokenPrefix starts every invitation token, so that one is
	// recognizable where it leaks.
	invitationTokenPrefix = "apron_invite_"
	entryInvitation       = "invitation"
	// invitationGrant is the key of the roles an invitation grants in an
	// account's grants, beside those its identities' schemes grant.
	invitationGrant    = "invitation"
	maxInvitationRooms = 32
)

// invitation is an outstanding invitation, kept by the SHA-256 of its
// token. Guarded by s.mu.
type invitation struct {
	id string
	// by names the extension that made it, empty for the program that
	// embeds the server.
	by               string
	created, expires time.Time
	name, userID     string
	rooms, roles     []string
	data             jsontext.Value
}

// enrollment is what an invitation left with the account it made: kept
// with the account, so that it is stored in the same change as the
// account. admitted reports that the invitation still admits the account
// to a server whose sign-in an extension restricts.
type enrollment struct {
	invitation string
	by         string
	at         time.Time
	data       jsontext.Value
	admitted   bool
}

type storedInvitation struct {
	ID      string         `json:"id"`
	By      string         `json:"by,omitzero"`
	Created time.Time      `json:"created"`
	Expires time.Time      `json:"expires"`
	Name    string         `json:"name,omitzero"`
	UserID  string         `json:"user_id,omitzero"`
	Rooms   []string       `json:"rooms,omitzero"`
	Roles   []string       `json:"roles,omitzero"`
	Data    jsontext.Value `json:"data,omitzero"`
}

type storedEnrollment struct {
	Invitation string         `json:"invitation"`
	By         string         `json:"by,omitzero"`
	At         time.Time      `json:"at"`
	Data       jsontext.Value `json:"data,omitzero"`
	Admitted   bool           `json:"admitted,omitzero"`
}

func (e *enrollment) stored() *storedEnrollment {
	if e == nil {
		return nil
	}
	return &storedEnrollment{Invitation: e.invitation, By: e.by, At: e.at, Data: e.data, Admitted: e.admitted}
}

func (e *storedEnrollment) restored() *enrollment {
	if e == nil {
		return nil
	}
	return &enrollment{invitation: e.Invitation, by: e.By, at: e.At, data: e.Data, admitted: e.Admitted}
}

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
	if len(options.Rooms) > maxInvitationRooms {
		return Invitation{}, fmt.Errorf("apron: an invitation joins at most %d rooms", maxInvitationRooms)
	}
	var data jsontext.Value
	if options.Data != nil {
		raw, err := json.Marshal(options.Data, jsonOptions)
		if err != nil {
			return Invitation{}, fmt.Errorf("apron: invitation data: %w", err)
		}
		data = raw
	}
	token := invitationTokenPrefix + rand.Text()
	now := time.Now()
	inv := &invitation{
		id: "inv_" + strings.ToLower(rand.Text()[:16]), by: by, created: now, expires: now.Add(lifetime),
		name: normalizeName(options.Name), userID: options.UserID,
		rooms: slices.Clone(options.Rooms), roles: normalizeRoles(options.Roles), data: data,
	}
	s.mu.Lock()
	if s.storeClosed {
		s.unlock()
		return Invitation{}, ErrServerClosed
	}
	s.pruneInvitationsLocked(now)
	if len(s.invitations) >= maxInvitations {
		s.unlock()
		return Invitation{}, fmt.Errorf("apron: at most %d invitations may be outstanding", maxInvitations)
	}
	key := sha256.Sum256([]byte(token))
	s.invitations[key] = inv
	s.dirty.invitations[key] = true
	f := s.newFenceLocked()
	s.unlock()
	select {
	case <-f.done:
		if f.err != nil {
			return Invitation{}, f.err
		}
	case <-ctx.Done():
		return Invitation{}, ctx.Err()
	}
	described := inv.describe()
	described.Token = token
	return described, nil
}

func (inv *invitation) describe() Invitation {
	return Invitation{
		ID: inv.id, By: inv.by, Created: inv.created, Expires: inv.expires,
		Name: inv.name, UserID: inv.userID, Rooms: slices.Clone(inv.rooms), Roles: slices.Clone(inv.roles),
	}
}

// Invitations lists the outstanding invitations, oldest first, without
// their tokens.
func (s *Server) Invitations() []Invitation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	var list []Invitation
	for _, inv := range s.invitations {
		if now.Before(inv.expires) {
			list = append(list, inv.describe())
		}
	}
	slices.SortFunc(list, func(a, b Invitation) int {
		return cmp.Or(a.Created.Compare(b.Created), cmp.Compare(a.ID, b.ID))
	})
	return list
}

// Invitations lists the outstanding invitations; see Server.Invitations.
func (h *Host) Invitations() []Invitation { return h.s.Invitations() }

// RevokeInvitation forgets an outstanding invitation by its ID, once that
// is stored, and reports whether there was one. A registration that
// presented it fails as it finishes.
func (s *Server) RevokeInvitation(ctx context.Context, id string) (bool, error) {
	s.mu.Lock()
	found := false
	for key, inv := range s.invitations {
		if inv.id == id {
			delete(s.invitations, key)
			s.dirty.invitations[key] = true
			found = true
		}
	}
	f := s.newFenceLocked()
	s.unlock()
	select {
	case <-f.done:
		return found, f.err
	case <-ctx.Done():
		return found, ctx.Err()
	}
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
	u := s.users[userID]
	if u == nil || u.enrolled == nil {
		return Enrollment{}, false
	}
	e := u.enrolled
	return Enrollment{Invitation: e.invitation, By: e.by, At: e.at, Data: slices.Clone(e.data), Admitted: e.admitted}, true
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
	if u := s.users[userID]; u != nil && u.enrolled != nil {
		s.revokeEnrollmentLocked(u)
	}
	f := s.newFenceLocked()
	s.unlock()
	select {
	case <-f.done:
		return f.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) revokeEnrollmentLocked(u *userState) {
	u.enrolled.admitted = false
	s.touchUser(u.id)
	if s.setGrantLocked(u, invitationGrant, nil) && s.grantRolesLocked(u) {
		s.notifyProfileLocked(u, nil, nil)
	}
	if s.allowlist != nil && !s.admittedLocked(u) {
		s.signOutLocked(u)
	}
}

// pruneInvitationsLocked forgets the invitations that expired.
func (s *Server) pruneInvitationsLocked(now time.Time) {
	for key, inv := range s.invitations {
		if !now.Before(inv.expires) {
			delete(s.invitations, key)
			s.dirty.invitations[key] = true
		}
	}
}

// presentedInvitationLocked returns the invitation an auth request presents
// with "invitation", if any, and refuses one that is unknown, used, or
// expired, or presented on a connection signed in already.
func (s *Server) presentedInvitationLocked(c *client, req request) (*invitation, *Error) {
	if _, has := req.params["invitation"]; !has {
		return nil, nil
	}
	token, err := parseString(req.params, "invitation", true)
	if err != nil {
		return nil, err
	}
	if c.user != nil {
		return nil, &Error{Code: CodeDenied, Message: "An invitation makes a new account; sign out first"}
	}
	inv := s.invitations[sha256.Sum256([]byte(token))]
	if inv == nil || !time.Now().Before(inv.expires) {
		return nil, &Error{Code: CodeDenied, Message: "This invitation is invalid, used, or expired"}
	}
	return inv, nil
}

// enrollLocked makes a new account for an identity a scheme proved with an
// invitation, which it uses up, then signs the connection in to it. The
// invitation, the identity, and the allowlist are checked again first,
// since the scheme ran outside s.mu, and nothing changes unless all pass.
// The reply waits until the change is stored.
func (s *Server) enrollLocked(c *client, req request, scheme authn.Scheme, proof authn.Proof, invitationID string) (any, *Error) {
	inv, err := s.presentedInvitationLocked(c, req)
	if err != nil {
		return nil, err
	}
	if inv == nil || inv.id != invitationID {
		return nil, &Error{Code: CodeDenied, Message: "This invitation is invalid, used, or expired"}
	}
	id, err := identityOf(scheme, proof)
	if err != nil {
		return nil, err
	}
	if s.accounts[id] != nil {
		return nil, &Error{Code: CodeDenied, Message: "This " + scheme.Label() + " belongs to an account"}
	}
	record, err := commit(proof)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	user := newUserState(s.assignAccountIDLocked(cmp.Or(proof.UserID, inv.userID)), normalizeName(cmp.Or(proof.Name, inv.name)))
	user.enrolled = &enrollment{invitation: inv.id, by: inv.by, at: now, data: inv.data, admitted: true}
	s.users[user.id] = user
	s.setIdentityLocked(user, id, record)
	s.setGrantLocked(user, id.Scheme, proof.Roles)
	s.setGrantLocked(user, invitationGrant, inv.roles)
	s.grantRolesLocked(user)
	key := sha256.Sum256([]byte(mustString(req.params, "invitation")))
	delete(s.invitations, key)
	s.dirty.invitations[key] = true
	// Nothing the connection is sent from here on goes before the account
	// and the used invitation are stored.
	c.fenceLocked()
	result, err := s.signInLocked(c, req, user, now, true, true)
	if err != nil {
		return nil, err
	}
	s.provedLocked(c, scheme, proof, now)
	for _, roomID := range inv.rooms {
		r := s.rooms[roomID]
		if r == nil || (r.parent != nil && !r.parent.visibleTo(user)) {
			continue
		}
		s.joinLocked(user, r)
	}
	return result, nil
}

// mustString is a string param that parsing found already.
func mustString(params map[string]jsontext.Value, name string) string {
	value, _ := parseString(params, name, true)
	return value
}

// invitationEntriesLocked renders the invitations a dirty set names.
func (s *Server) invitationEntriesLocked(d dirtySet, put func(kind, id string, value any), del func(kind, id string)) {
	for key := range d.invitations {
		if inv := s.invitations[key]; inv != nil {
			put(entryInvitation, hex.EncodeToString(key[:]), storedInvitation{
				ID: inv.id, By: inv.by, Created: inv.created, Expires: inv.expires,
				Name: inv.name, UserID: inv.userID, Rooms: inv.rooms, Roles: inv.roles, Data: inv.data,
			})
		} else {
			del(entryInvitation, hex.EncodeToString(key[:]))
		}
	}
}

// restoreInvitationsLocked keeps the stored invitations that have not
// expired, and forgets the others.
func (s *Server) restoreInvitationsLocked(entries map[string]jsontext.Value, now time.Time) error {
	for id, raw := range entries {
		key, err := hex.DecodeString(id)
		if err != nil || len(key) != 32 {
			continue
		}
		var stored storedInvitation
		if err := json.Unmarshal(raw, &stored, decodeOptions); err != nil {
			return fmt.Errorf("stored invitation %s: %w", id, err)
		}
		if !now.Before(stored.Expires) {
			s.dirty.invitations[[32]byte(key)] = true
			continue
		}
		s.invitations[[32]byte(key)] = &invitation{
			id: stored.ID, by: stored.By, created: stored.Created, expires: stored.Expires,
			name: stored.Name, userID: stored.UserID, rooms: stored.Rooms, roles: stored.Roles, data: stored.Data,
		}
	}
	return nil
}

// enrolledNamedLocked ends the invitation's admission of the accounts a
// principal names, by user_id or identity, as when Allowlist.Disallow
// takes it off the list.
func (s *Server) enrolledNamedLocked(p Principal) {
	for _, u := range s.users {
		if u.enrolled == nil || !u.enrolled.admitted {
			continue
		}
		if p == UserID(u.id) || slices.ContainsFunc(slices.Collect(maps.Keys(u.identities)), func(scheme string) bool {
			return principalOf(authn.Identity{Scheme: scheme, Subject: u.identities[scheme]}) == p
		}) {
			u.enrolled.admitted = false
			s.touchUser(u.id)
		}
	}
}
