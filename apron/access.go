package apron

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/coder/websocket"

	"github.com/apron-chat/apron-server-go/internal/auth/passkey"
)

// Access modes (SERVER.md, Access modes). A server is public, the
// default, where anyone may sign in as a guest and see every room that is
// not private; private, where only accounts sign in (the old
// DisableGuests); or lobby, where newcomers see one room, the Lobby, and
// its threads until an admin or moderator invites them in.
//
// In lobby mode the users limited to the Lobby, restricted users, are
// guests and waiting accounts: accounts that signed themselves up there,
// which the store marks so (userState.waiting). The limit reuses the room
// scope machinery (scope.go): userState.allows adds the Lobby to a user's
// own scope, so every check of who may see a room, roomState.visibleTo,
// holds for them. Nothing but the waiting marker and whether the Lobby is
// closed is stored: a guest's invitation lives as long as the guest, and
// the roles and permissions that show where a user stands are computed.

// AccessMode is who may see what before being admitted (Config.Access).
type AccessMode string

const (
	// AccessPublic lets anyone sign in as a guest, and see every room that
	// is not private. It is the default.
	AccessPublic AccessMode = "public"
	// AccessLobby lets anyone sign in as a guest, or sign up, and see the
	// Lobby (Access.LobbyRoom) and its threads, until an admin or moderator
	// invites them in with /invite.
	AccessLobby AccessMode = "lobby"
	// AccessPrivate offers no guests: only accounts sign in.
	AccessPrivate AccessMode = "private"
)

// DefaultLobbyRoom is the Lobby's room_id unless Access.LobbyRoom names
// another.
const DefaultLobbyRoom = "lobby"

// lobbyTitle is the title of a Lobby the server creates.
const lobbyTitle = "Public Lobby"

// Access is the server's access mode (Config.Access).
type Access struct {
	// Mode is public, lobby, or private; empty is public, unless
	// Config.DisableGuests, which makes it private.
	Mode AccessMode
	// LobbyRoom is the Lobby's room_id in lobby mode, DefaultLobbyRoom when
	// empty. The server creates it, titled "Public Lobby", unless a room
	// with that room_id exists, which then becomes the Lobby as it is.
	LobbyRoom string
}

// normalized checks the access configuration and fills in its defaults,
// with DisableGuests, the deprecated way to say private.
func (a Access) normalized(disableGuests bool) (Access, error) {
	switch a.Mode {
	case "":
		a.Mode = AccessPublic
		if disableGuests {
			a.Mode = AccessPrivate
		}
	case AccessPublic, AccessPrivate:
		if disableGuests {
			a.Mode = AccessPrivate
		}
	case AccessLobby:
		if disableGuests {
			return a, errors.New("Config.DisableGuests (--disable-guests) is access mode private, which cannot be combined with access mode lobby")
		}
	default:
		return a, fmt.Errorf("unknown access mode %q: use public, lobby, or private", a.Mode)
	}
	if a.LobbyRoom == "" {
		a.LobbyRoom = DefaultLobbyRoom
	}
	if !validID(a.LobbyRoom) {
		return a, fmt.Errorf("the lobby room %q is no room_id: it may not be empty, or contain whitespace or control characters", a.LobbyRoom)
	}
	return a, nil
}

// accessState is the server's access mode as its users see it: every
// userState points at its server's (userState.access).
type accessState struct {
	// lobby is the Lobby's room_id in lobby mode, and empty in the other
	// modes. It does not change once the server is open.
	lobby string
	// closed reports that the Lobby is closed to newcomers (/toggle lobby).
	// Guarded by s.mu.
	closed bool
}

// Lobby states, which ext.permissions.lobby carries and which show as a
// role beside the user's name.
const (
	lobbyWaiting = "waiting"
	lobbyInvited = "invited"
)

// lobbyState is how lobby mode limits u: "waiting" for a guest, or an
// account that signed itself up, until someone invites it in; "invited"
// for a guest that was invited, which becomes a member once it adds a way
// to sign in; and "" for everyone else, everyone in the other modes
// included. Admins, moderators, and bots are never limited, so a server
// that starts in lobby mode can grant its first admin with Config.Roles.
func (u *userState) lobbyState() string {
	if u.access == nil || u.access.lobby == "" || u.botOf != "" || u.moderates() {
		return ""
	}
	switch {
	case !u.account() && u.invited:
		return lobbyInvited
	case !u.account(), u.waiting:
		return lobbyWaiting
	}
	return ""
}

// restricted reports a user limited to the Lobby.
func (u *userState) restricted() bool { return u.lobbyState() != "" }

// moderates reports a user with the admin or moderator role.
func (u *userState) moderates() bool { return u.hasRole("admin") || u.hasRole("moderator") }

// mayInvite reports whether u may run /invite: an admin or moderator in
// lobby mode.
func (u *userState) mayInvite() bool {
	return u.access != nil && u.access.lobby != "" && u.botOf == "" && u.moderates()
}

// allows reports whether r is in what u may see apart from private rooms:
// its scope (scope.go), and for a restricted user the Lobby and its threads.
func (u *userState) allows(r *roomState) bool {
	if !u.scope.allows(r) {
		return false
	}
	if !u.restricted() {
		return true
	}
	for room := r; room != nil; room = room.parent {
		if room.id == u.access.lobby {
			return true
		}
	}
	return false
}

// displayRoles are the roles u's user objects carry: those it holds, and
// in lobby mode "waiting" for a waiting account and "invited" for an
// invited guest, which no grant gives and nothing counts as a role.
func (u *userState) displayRoles() []string {
	state := u.lobbyState()
	if state == "" || state == lobbyWaiting && !u.account() {
		return u.roles
	}
	roles := append(slices.Clone(u.roles), state)
	slices.Sort(roles)
	return slices.Compact(roles)
}

// permissions is you.ext.permissions in lobby mode (ext:permissions): lobby
// for a restricted user, invite for one who may invite; nil when neither,
// or in the other modes.
func (u *userState) permissions() jsontext.Value {
	if u.access == nil || u.access.lobby == "" {
		return nil
	}
	permissions := map[string]any{}
	if state := u.lobbyState(); state != "" {
		permissions["lobby"] = state
	}
	if u.mayInvite() {
		permissions["invite"] = true
	}
	if len(permissions) == 0 {
		return nil
	}
	return encodeJSON(permissions)
}

// newUserLocked is a new user of this server, which sees its access mode.
func (s *Server) newUserLocked(id, name string) *userState {
	u := newUserState(id, name)
	u.access = &s.access
	return u
}

// lobbyRoomLocked is the Lobby in lobby mode, nil otherwise.
func (s *Server) lobbyRoomLocked() *roomState {
	if s.access.lobby == "" {
		return nil
	}
	return s.rooms[s.access.lobby]
}

// defaultRoomLocked is where u starts, and what requests without room_id
// address: the Lobby for a restricted user, else the default room.
func (s *Server) defaultRoomLocked(u *userState) *roomState {
	if u != nil && u.restricted() {
		if r := s.lobbyRoomLocked(); r != nil {
			return r
		}
	}
	return s.rooms[defaultRoomID]
}

// defaultRoomFor is the room_id requests on c address without one.
func (s *Server) defaultRoomFor(c *client) string {
	if s.access.lobby == "" {
		return defaultRoomID
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.defaultRoomLocked(c.user).id
}

// openLobbyLocked creates the Lobby in lobby mode, or adopts the room with
// its room_id as it is, and limits the waiting accounts to it.
func (s *Server) openLobbyLocked() error {
	if s.access.lobby == "" {
		return nil
	}
	r := s.rooms[s.access.lobby]
	switch {
	case r == nil:
		// A logged room record, as for the default room, so its
		// history_log_id is never null.
		s.commitRoomLocked(s.access.lobby, nil, false, map[string]any{"title": lobbyTitle})
	case r.private || r.parent != nil:
		return fmt.Errorf("the lobby room %q is a private room or a thread, which cannot be the Lobby; name another with --access.lobby-room", r.id)
	}
	for _, u := range s.users {
		s.confineLocked(u)
	}
	return nil
}

// confineLocked takes a restricted user out of the rooms it may no longer
// see, as a scope does (scope.go), such as a waiting account that joined
// other rooms while the server was public.
func (s *Server) confineLocked(u *userState) {
	if u.restricted() {
		s.applyScopeLocked(u)
	}
}

// restrictedRefusal refuses a restricted user what is not for the Lobby.
func restrictedRefusal(what string) *Error {
	return &Error{Code: CodeDenied, Message: "In the Public Lobby you may not " + what + " until an admin or moderator invites you in"}
}

// signUpHintLocked says how an invited guest finishes joining: with the
// ways this server lets a guest add a sign-in.
func (s *Server) signUpHintLocked() string {
	switch {
	case s.schemeLocked(passkey.Name) != nil:
		return "Add a passkey"
	case len(s.schemes) > 0:
		return "Add a way to sign in"
	}
	return "Ask for an invite link"
}

// inviteLocked lets target in on by's word (/invite, Server.Admit): a
// connected guest becomes invited, and a waiting account a member at once.
// It returns what to tell the inviter.
func (s *Server) inviteLocked(target *userState, by string) (string, *Error) {
	inviter := "An admin"
	if by != "" {
		inviter = "@" + by
	}
	switch target.lobbyState() {
	case "":
		return fmt.Sprintf("@%s is already a member.", target.id), nil
	case lobbyInvited:
		return fmt.Sprintf("@%s is invited already; they become a member once they sign up, unless they leave first.", target.id), nil
	}
	lobby := s.lobbyRoomLocked()
	if !target.account() {
		// The guest stays in the Lobby, now invited; its next sign-up makes
		// it a member (addProvenLocked).
		target.invited = true
		s.notifyAccessLocked(target, nil, true)
		hint := s.signUpHintLocked()
		target.send(privateNotice(lobby.id, fmt.Sprintf("%s invited you. %s to finish joining; until you do, closing this page ends your visit.", inviter, hint)))
		return fmt.Sprintf("@%s is invited: they become a member once they sign up, unless they leave first.", target.id), nil
	}
	s.admitWaitingLocked(target, inviter)
	return fmt.Sprintf("@%s is in: they are a member now.", target.id), nil
}

// admitWaitingLocked makes a waiting account a member: its marker is
// cleared, its connections get `you` without the waiting role and
// ext.permissions.lobby, then its join to the default room and a notice
// saying who invited it, and those who share the Lobby with it `user` new.
func (s *Server) admitWaitingLocked(u *userState, inviter string) {
	u.waiting = false
	s.touchUser(u.id)
	s.notifyAccessLocked(u, nil, true)
	s.viewChangedLocked(u)
	s.joinDefaultRoomLocked(u)
	lobby := s.lobbyRoomLocked()
	u.send(privateNotice(lobby.id, inviter+" invited you in."))
}

// notifyAccessLocked sends u's changed standing, its roles and permissions,
// as a profile change (notifyProfileLocked): `you` to its connections other
// than except, and `new` to those who share a room with it. hadPermissions
// says its `you` carried ext.permissions before, which a `you` without
// them clears.
func (s *Server) notifyAccessLocked(u *userState, except *client, hadPermissions bool) {
	s.publishLocked(u)
	you := u.you()
	if hadPermissions && u.permissions() == nil {
		you = withCleared(you, extObject{"permissions": jsontext.Value("{}")})
	}
	frame := notification("user", map[string]any{"you": you})
	for c := range u.clients {
		if c != except {
			c.enqueue(frame)
		}
	}
	others := notification("user", map[string]any{"new": u.profile()})
	for _, other := range s.sharersLocked(u) {
		other.send(others)
	}
}

// viewChangedLocked sends u's connections a new server frame when the one
// for what u may see now differs from the Lobby's, as when a restricted
// user becomes a member (§3.1): its description may be the Lobby's.
func (s *Server) viewChangedLocked(u *userState) {
	if s.access.lobby == "" {
		return
	}
	lobby, _ := s.profileValueForLocked("description", true)
	member, _ := s.profileValueForLocked("description", false)
	if lobby == member {
		return
	}
	for c := range u.clients {
		c.enqueue(s.serverFrameLocked(c))
	}
}

// Admit lets a user in from the Lobby in lobby mode, as /invite does: a
// waiting account becomes a member at once, and a connected guest becomes
// invited, a member once it signs up. It returns once that is stored.
// It is an error in another mode, or for a user that does not exist; a
// member is left as it is.
func (s *Server) Admit(ctx context.Context, userID string) error {
	s.mu.Lock()
	if s.access.lobby == "" {
		s.unlock()
		return errors.New("apron: the server is not in lobby mode")
	}
	u := s.users[userID]
	if u == nil || u.retired {
		s.unlock()
		return errors.New("apron: no such user")
	}
	if _, err := s.inviteLocked(u, ""); err != nil {
		s.unlock()
		return err
	}
	f := s.newFenceLocked()
	s.unlock()
	return waitFence(ctx, f)
}

// Admit lets a user in from the Lobby; see Server.Admit.
func (h *Host) Admit(ctx context.Context, userID string) error { return h.s.Admit(ctx, userID) }

// Waiting reports whether the user is limited to the Lobby, waiting to be
// invited in: a guest or an account that signed itself up in lobby mode.
// It is false in the other modes.
func (s *Server) Waiting(userID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u := s.users[userID]
	return u != nil && u.lobbyState() == lobbyWaiting
}

// Waiting reports whether the user is waiting in the Lobby; see
// Server.Waiting.
func (h *Host) Waiting(userID string) bool { return h.s.Waiting(userID) }

// LobbyOpen reports whether the Lobby is open to newcomers, in lobby mode.
func (s *Server) LobbyOpen() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.access.lobby != "" && !s.access.closed
}

// inviteCommand is /invite <user_id>, for admins and moderators in lobby
// mode: it lets a connected guest or a waiting account in (inviteLocked),
// and tells the sender what happened in a ~private notice before the
// result.
func (s *Server) inviteCommand(c *client, r *roomState, body map[string]any, args, _ string) (map[string]any, *Error) {
	if !c.user.mayInvite() {
		return nil, &Error{Code: CodeDenied, Message: "Only admins and moderators can invite people in"}
	}
	id, _, _ := strings.Cut(args, " ")
	id = strings.TrimPrefix(id, "@")
	if id == "" {
		if targets := mentions(body); len(targets) == 1 {
			id = targets[0]
		}
	}
	if id == "" {
		return nil, invalidParams("Usage: /invite <user_id>")
	}
	target := s.users[id]
	if target == nil || target.retired {
		return nil, invalidParams("Unknown user %q", id)
	}
	text, err := s.inviteLocked(target, c.user.id)
	if err != nil {
		return nil, err
	}
	c.enqueue(privateNotice(r.id, text))
	return map[string]any{}, nil
}

// toggleCommand is /toggle, for admins and moderators in lobby mode:
// alone it shows the toggles, and /toggle lobby closes the Lobby to
// newcomers or opens it again.
func (s *Server) toggleCommand(c *client, r *roomState, _ map[string]any, args, _ string) (map[string]any, *Error) {
	if !c.user.mayInvite() {
		return nil, &Error{Code: CodeDenied, Message: "Only admins and moderators can open and close the Public Lobby"}
	}
	state := func() string {
		if s.access.closed {
			return "closed"
		}
		return "open"
	}
	switch strings.ToLower(strings.TrimSpace(args)) {
	case "":
		c.enqueue(privateNotice(r.id, "- `lobby`: "+state()+"; `/toggle lobby` "+map[bool]string{true: "opens", false: "closes"}[s.access.closed]+" it"))
		return map[string]any{}, nil
	case "lobby":
	default:
		return nil, invalidParams("Usage: /toggle lobby")
	}
	s.setLobbyClosedLocked(!s.access.closed)
	text := "The Public Lobby is open again: anyone may come in as a guest or sign up, and see what it holds, what was posted while it was closed included."
	if s.access.closed {
		text = "The Public Lobby is closed: no new guests or sign-ups, and its guests were let go. Waiting accounts still see it, and can still be invited. Whatever is posted in it is public again once it opens."
	}
	c.enqueue(privateNotice(r.id, text))
	return map[string]any{}, nil
}

// setLobbyClosedLocked closes the Lobby to newcomers, or opens it: closing
// sends each guest a notice and closes its connection with 1008 (policy
// violation); either way every connection gets a new server frame.
func (s *Server) setLobbyClosedLocked(closed bool) {
	if s.access.closed == closed {
		return
	}
	s.access.closed = closed
	s.dirty.lobby = true
	if closed {
		lobby := s.lobbyRoomLocked()
		for c := range s.clients {
			if u := c.user; u != nil && !u.account() {
				c.closeWithStatus(websocket.StatusPolicyViolation, "The Public Lobby is closed", privateNotice(lobby.id, "The Public Lobby is closed"))
			}
		}
	}
	s.announceServerLocked()
}

// storedLobby is the Lobby's state, as /toggle lobby leaves it.
type storedLobby struct {
	Closed bool `json:"closed,omitzero"`
}

// restoreLobbyLocked restores whether the Lobby is closed.
func (s *Server) restoreLobbyLocked(raw jsontext.Value) error {
	if raw == nil {
		return nil
	}
	var stored storedLobby
	if err := json.Unmarshal(raw, &stored); err != nil {
		return fmt.Errorf("stored lobby: %w", err)
	}
	s.access.closed = stored.Closed
	return nil
}
