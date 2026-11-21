package apron

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/coder/websocket"

	"github.com/apron-chat/apron-server-go/internal/auth/passkey"
	"github.com/apron-chat/apron-server-go/internal/authn"
)

// Access modes (SERVER.md, Access modes). A server is public, the
// default, where anyone may sign in as a guest and see every room that is
// not private; private, where nobody signs themselves up; or lobby, where newcomers see one room, the Lobby, and
// its threads until an admin or moderator welcomes them in.
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
	// welcomes them in with /welcome.
	AccessLobby AccessMode = "lobby"
	// AccessPrivate offers no guests, and nobody signs themselves up: a
	// new account needs an invitation (Server.Invite), or an identity the
	// operator names, on an extension's allowlist (Registrar.RestrictSignIn)
	// or granted a role (Config.Roles). /welcome lets in the waiting
	// accounts lobby mode left.
	AccessPrivate AccessMode = "private"
)

// DefaultLobbyRoom is the Lobby's room_id unless Access.LobbyRoom names
// another.
const DefaultLobbyRoom = "lobby"

// lobbyTitle is the title of a Lobby the server creates.
const lobbyTitle = "Public Lobby"

// Access is the server's access mode (Config.Access).
type Access struct {
	// Mode is public, lobby, or private; empty is public.
	Mode AccessMode
	// LobbyRoom is the Lobby's room_id in lobby mode, DefaultLobbyRoom when
	// empty. The server creates it, titled "Public Lobby", unless a room
	// with that room_id exists, which then becomes the Lobby as it is.
	LobbyRoom string
}

// normalized checks the access configuration and fills in its defaults.
// DisableGuests goes with public mode, where it is what it always was (no
// guests, and sign-up open to every scheme), and with private mode, which
// has no guests anyway; lobby mode is for guests.
func (a Access) normalized(disableGuests bool) (Access, error) {
	switch a.Mode {
	case "":
		a.Mode = AccessPublic
	case AccessPublic, AccessPrivate:
	case AccessLobby:
		if disableGuests {
			return a, errors.New("Config.DisableGuests (--disable-guests) offers no guests, which lobby mode is for: use one or the other")
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
	// private reports private mode, where /welcome admits the waiting
	// accounts lobby mode left.
	private bool
}

// Lobby states, which show as a role beside the user's name.
const (
	lobbyWaiting  = "waiting"
	lobbyWelcomed = "welcomed"
)

// serverRole reports a role only the server gives, computed in lobby mode,
// which no invitation or identity may grant as a stored role.
func serverRole(role string) bool { return role == lobbyWaiting || role == lobbyWelcomed }

// lobbyState is how lobby mode limits u: "waiting" for a guest, or an
// account that signed itself up, until someone welcomes it in; "welcomed"
// for a guest that was welcomed, which becomes a member once it adds a way
// to sign in; and "" for everyone else, everyone in the other modes
// included. Admins, moderators, and bots are never limited, so a server
// that starts in lobby mode can grant its first admin with Config.Roles.
func (u *userState) lobbyState() string {
	if u.access == nil || u.access.lobby == "" || u.botOf != "" || u.moderates() {
		return ""
	}
	switch {
	case !u.account() && u.welcomed:
		return lobbyWelcomed
	case !u.account(), u.waiting:
		return lobbyWaiting
	}
	return ""
}

// restricted reports a user limited to the Lobby.
func (u *userState) restricted() bool { return u.lobbyState() != "" }

// moderates reports a user with the admin or moderator role.
func (u *userState) moderates() bool { return u.hasRole("admin") || u.hasRole("moderator") }

// mayWelcome reports whether u may run /welcome and /toggle: an admin or
// moderator in lobby mode, or /welcome in private mode.
func (u *userState) mayWelcome() bool {
	return u.access != nil && (u.access.lobby != "" || u.access.private) && u.botOf == "" && u.moderates()
}

// vouchedLocked reports whether the operator names an account: an
// extension's allowlist, or Config.Roles granting it a role.
func (s *Server) vouchedLocked(u *userState) bool {
	if s.allowlist != nil && s.listedLocked(u) {
		return true
	}
	holders := []Principal{UserID(u.id)}
	for scheme, subject := range u.identities {
		holders = append(holders, principalOf(authn.Identity{Scheme: scheme, Subject: subject}))
	}
	for _, granted := range s.roleHolders {
		if slices.ContainsFunc(granted, func(p Principal) bool { return slices.Contains(holders, p) }) {
			return true
		}
	}
	return false
}

// admitVouchedLocked lets in, in private mode, the waiting accounts lobby
// mode left that the operator names (vouchedLocked).
func (s *Server) admitVouchedLocked() {
	for _, u := range s.users {
		if u.leftWaiting() && s.vouchedLocked(u) {
			s.admitWaitingLocked(u)
		}
	}
}

// admitWaitingLocked makes a waiting account left from lobby mode a
// member, in private mode: its mark is cleared, and it joins the default
// room.
func (s *Server) admitWaitingLocked(u *userState) {
	u.waiting = false
	s.touchUser(u.id)
	s.admitToRoomsLocked(u)
}

// leftWaiting reports a waiting account in private mode: one that signed
// itself up in lobby mode, and may not sign in until it is welcomed.
func (u *userState) leftWaiting() bool {
	return u.access != nil && u.access.private && u.waiting && u.account() && u.botOf == "" && !u.moderates()
}

// allows reports whether r is in what u may see apart from private rooms:
// its scope (scope.go), and for a restricted user the Lobby and its threads.
func (u *userState) allows(r *roomState) bool {
	if !u.scope.allows(r) {
		return false
	}
	return !u.restricted() || inLobby(r, u.access.lobby)
}

// inLobby reports whether r is the Lobby, named lobby, or one of its
// threads at any depth.
func inLobby(r *roomState, lobby string) bool {
	for room := r; room != nil && lobby != ""; room = room.parent {
		if room.id == lobby {
			return true
		}
	}
	return false
}

// revealsLocked reports whether a record in r, such as a move snapshot or
// a reply, would name to someone a room they cannot see: to (revealsTo)
// as private rooms go, and in lobby mode a room outside the Lobby named in
// it, which restricted users see.
func (s *Server) revealsLocked(r, to *roomState) bool {
	lobby := s.access.lobby
	return inLobby(r, lobby) && !inLobby(to, lobby) || r.revealsTo(to)
}

// crossesLobbyLocked reports whether a and b lie on either side of the
// Lobby's edge in lobby mode: a move between them would take a message out
// of restricted users' sight, or show them where it was.
func (s *Server) crossesLobbyLocked(a, b *roomState) bool {
	lobby := s.access.lobby
	return inLobby(a, lobby) != inLobby(b, lobby)
}

// displayRoles are the roles u's user objects carry: those it holds, and
// in lobby mode "waiting" for a waiting account and "welcomed" for a
// welcomed guest, which no grant gives and nothing counts as a role.
func (u *userState) displayRoles() []string {
	state := u.lobbyState()
	if state == "" || state == lobbyWaiting && !u.account() {
		return u.roles
	}
	roles := append(slices.Clone(u.roles), state)
	slices.Sort(roles)
	return slices.Compact(roles)
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
//
// It then joins the Lobby, the one room a restricted user sees, so that it
// is never in no room at all, as when the Lobby's room_id changed between
// restarts, or it left the Lobby while the server was public.
func (s *Server) confineLocked(u *userState) {
	if !u.restricted() {
		return
	}
	s.applyScopeLocked(u)
	if lobby := s.lobbyRoomLocked(); lobby != nil && u.account() {
		s.joinLocked(u, lobby)
	}
}

// noSignUpLocked says why a passkey registration makes a new account only
// with an invitation, on a server without guests: the Lobby is closed, the
// server is private, an allowlist names who may sign in, or there are no
// guests to start from (DisableGuests). Empty where guests are allowed.
func (s *Server) noSignUpLocked() string {
	switch {
	case s.guestsAllowedLocked():
		return ""
	case s.access.lobby != "" && s.access.closed:
		return "The Public Lobby is closed; ask for an invite link"
	case s.access.private:
		return inviteOnly().Message
	case s.allowlist != nil:
		return "Only those this server lets in may make an account; ask for an invite link"
	}
	return "Make an account another way, or present an invitation, before adding a passkey"
}

// restrictedRefusal refuses a restricted user what is not for the Lobby.
func restrictedRefusal(what string) *Error {
	return &Error{Code: CodeDenied, Message: "In the Public Lobby you may not " + what + " until an admin or moderator welcomes you in"}
}

// signUpHintLocked says how a welcomed guest finishes joining: with the
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

// welcomeLocked lets target in on by's word (/welcome, Server.Admit): a
// connected guest becomes welcomed, and a waiting account a member at
// once. It returns what to tell whoever welcomed it.
func (s *Server) welcomeLocked(target *userState, by string) string {
	welcomer := "An admin"
	if by != "" {
		welcomer = "@" + by
	}
	if target.leftWaiting() {
		// Private mode has no Lobby: the account, signed out at start,
		// signs in as a member from now on, in general.
		s.admitWaitingLocked(target)
		return fmt.Sprintf("@%s is in: they may sign in as a member now.", target.id)
	}
	switch target.lobbyState() {
	case "":
		return fmt.Sprintf("@%s is already a member.", target.id)
	case lobbyWelcomed:
		return fmt.Sprintf("@%s is welcomed already; they become a member once they sign up, unless they leave first.", target.id)
	}
	lobby := s.lobbyRoomLocked()
	if !target.account() {
		// The guest stays in the Lobby, now welcomed; its next sign-up
		// makes it a member (addProvenLocked).
		target.welcomed = true
		s.notifyProfileLocked(target, nil, nil)
		hint := s.signUpHintLocked()
		target.send(privateNotice(lobby.id, fmt.Sprintf("%s welcomed you. %s to finish joining; until you do, closing this page ends your visit.", welcomer, hint)))
		return fmt.Sprintf("@%s is welcomed: they become a member once they sign up, unless they leave first.", target.id)
	}
	u := target
	u.waiting = false
	s.touchUser(u.id)
	s.notifyProfileLocked(u, nil, nil)
	s.viewChangedLocked(u)
	s.admitToRoomsLocked(u)
	u.send(privateNotice(lobby.id, welcomer+" welcomed you in."))
	return fmt.Sprintf("@%s is in: they are a member now.", target.id)
}

// admitToRoomsLocked shows a restricted user that became a member the
// rooms it may see now: it joins the default room, and its connections
// get one room_update with the default room in joined, its members and
// the membership, and in updated the records of the other rooms room_list
// would list it as not joined, which clients add to what they know.
func (s *Server) admitToRoomsLocked(u *userState) {
	r := s.defaultRoomLocked(u)
	params := map[string]any{}
	if membership := s.addMemberLocked(u, r); membership != nil {
		record := s.roomParamsLocked(r)
		params["joined"] = []any{record}
		params["users"] = profiles(s.addMembersLocked(record, r))
		params["memberships"] = []any{membership}
	}
	var others []*roomState
	for _, room := range s.rooms {
		if room.parent == nil && u.joined[room.id] == nil && room.visibleTo(u) {
			others = append(others, room)
		}
	}
	slices.SortFunc(others, func(a, b *roomState) int {
		return cmp.Or(cmp.Compare(b.latestID, a.latestID), cmp.Compare(b.createdID, a.createdID))
	})
	if len(others) > maxListedRooms {
		others = others[:maxListedRooms]
	}
	if len(others) > 0 {
		updated := make([]any, len(others))
		for i, room := range others {
			updated[i] = s.roomParamsLocked(room)
		}
		params["updated"] = updated
	}
	if len(params) > 0 {
		u.send(notification("room_update", params))
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

// Admit lets a user in, as /welcome does: in lobby mode a waiting account
// becomes a member at once, and a connected guest becomes welcomed, a
// member once it signs up; in private mode a waiting account left from
// lobby mode signs in as a member from then on. It returns once that is
// stored.
// It is an error in another mode, or for a user that does not exist; a
// member is left as it is.
func (s *Server) Admit(ctx context.Context, userID string) error {
	s.mu.Lock()
	if s.access.lobby == "" && !s.access.private {
		s.unlock()
		return errors.New("apron: the server is in public mode")
	}
	u := s.users[userID]
	if u == nil || u.retired {
		s.unlock()
		return errors.New("apron: no such user")
	}
	s.welcomeLocked(u, "")
	f := s.newFenceLocked()
	s.unlock()
	return waitFence(ctx, f)
}

// Admit lets a user in from the Lobby; see Server.Admit.
func (h *Host) Admit(ctx context.Context, userID string) error { return h.s.Admit(ctx, userID) }

// Waiting reports whether the user is waiting to be welcomed in: in lobby
// mode a guest or an account that signed itself up, limited to the Lobby,
// and in private mode such an account left from lobby mode, which may not
// sign in. It is false in public mode.
func (s *Server) Waiting(userID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u := s.users[userID]
	return u != nil && (u.lobbyState() == lobbyWaiting || u.leftWaiting())
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

// welcomeCommand is /welcome <user_id>, for admins and moderators in lobby
// mode: it lets a connected guest or a waiting account in (welcomeLocked),
// and tells the sender what happened in a ~private notice before the
// result.
func (s *Server) welcomeCommand(c *client, r *roomState, body map[string]any, args, replyTo string) (map[string]any, *Error) {
	if !c.user.mayWelcome() {
		return nil, &Error{Code: CodeDenied, Message: "Only admins and moderators can welcome people in"}
	}
	// Whom: the one user the command mentions, whatever its text says (a
	// client's mention text may be a display name); else the user_id typed
	// as the one word after the command, an autocompleted @ included; else
	// the author of the message it replies to. Several mentions name no one.
	var id string
	switch targets := mentions(body); {
	case len(targets) > 1:
		return nil, invalidParams("/welcome lets in one person at a time; mention just one")
	case len(targets) == 1:
		id = targets[0]
	default:
		if words := strings.Fields(args); len(words) == 1 {
			id = strings.TrimPrefix(words[0], "@")
		} else if replyTo != "" {
			if m := s.visibleMessageLocked(c.user, replyTo); m != nil {
				id = m.owner
			}
		}
	}
	if id == "" {
		return nil, invalidParams("Usage: /welcome <user_id>, or /welcome in reply to their message")
	}
	target := s.users[id]
	switch {
	case (target == nil || target.retired) && s.usedIDs[strings.ToLower(id)]:
		// Accounts are never retired: whoever had the user_id was a guest,
		// who is gone, and so is anything a welcome could give it.
		return nil, invalidParams("No guest %s is connected; send them an invite link instead", id)
	case target == nil || target.retired:
		return nil, invalidParams("Unknown user %q", id)
	}
	text := s.welcomeLocked(target, c.user.id)
	c.enqueue(privateNotice(r.id, text))
	return map[string]any{}, nil
}

// toggleCommand is /toggle, for admins and moderators in lobby mode:
// alone it shows the toggles, and /toggle lobby closes the Lobby to
// newcomers or opens it again.
func (s *Server) toggleCommand(c *client, r *roomState, _ map[string]any, args, _ string) (map[string]any, *Error) {
	if !c.user.mayWelcome() {
		return nil, &Error{Code: CodeDenied, Message: "Only admins and moderators can open and close the Public Lobby"}
	}
	state := func() string {
		if s.access.closed {
			return "closed"
		}
		return "open"
	}
	words := strings.Fields(strings.ToLower(args))
	if len(words) == 0 {
		c.enqueue(privateNotice(r.id, "- `lobby`: "+state()+"; `/toggle lobby` "+map[bool]string{true: "opens", false: "closes"}[s.access.closed]+" it, and `/toggle lobby open` or `close` says which"))
		return map[string]any{}, nil
	}
	if words[0] != "lobby" || len(words) > 2 {
		return nil, invalidParams("Usage: /toggle lobby [open|close]")
	}
	closed := !s.access.closed
	if len(words) == 2 {
		switch words[1] {
		case "open":
			closed = false
		case "close", "closed":
			closed = true
		default:
			return nil, invalidParams("Usage: /toggle lobby [open|close]")
		}
	}
	if closed == s.access.closed {
		c.enqueue(privateNotice(r.id, "The Public Lobby is "+state()+" already."))
		return map[string]any{}, nil
	}
	s.setLobbyClosedLocked(closed)
	text := "The Public Lobby is open again: anyone may come in as a guest or sign up, and see what it holds, what was posted while it was closed included."
	if s.access.closed {
		text = "The Public Lobby is closed: no new guests or sign-ups, and its guests were let go. Waiting accounts still see it, and can still be welcomed. Whatever is posted in it is public again once it opens."
	}
	c.enqueue(privateNotice(r.id, text))
	return map[string]any{}, nil
}

// lobbyClosedError is the error about the connection as a whole (§1.1)
// that each guest gets when the Lobby closes: denied, which clients do not
// reconnect after.
var lobbyClosedError = &Error{Code: CodeDenied, Message: "The Public Lobby is closed. Ask for an invite link."}

// setLobbyClosedLocked closes the Lobby to newcomers, or opens it: closing
// sends each guest a notice and the error lobbyClosedError, without id,
// and closes its connection with 1008 (policy violation); either way every
// connection gets a new server frame.
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
				c.closeWithStatus(websocket.StatusPolicyViolation, "The Public Lobby is closed", privateNotice(lobby.id, "The Public Lobby is closed"), errorResponse(nil, lobbyClosedError))
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
