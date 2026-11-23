package apron

import (
	"cmp"
	"encoding/json/jsontext"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/apron-chat/apron-server-go/apron/internal/accounts"
	"github.com/apron-chat/apron-server-go/apron/internal/roomlog"
	"github.com/apron-chat/apron-server-go/internal/authn"
	"github.com/apron-chat/apron-server-go/wire"
	"golang.org/x/text/secure/precis"
	"golang.org/x/time/rate"
)

const (
	maxNameRunes = 64
	// maxAvatarDataURLBytes bounds an avatar given inline as a data: URL;
	// larger images go through a /avatar upload (§4.8.6).
	maxAvatarDataURLBytes = 64 << 10
	maxDedupEntries       = 1024
	// maxProfileExtBytes bounds a profile's ext, which is sent to everyone
	// who shares a room with the user.
	maxProfileExtBytes = 16 << 10
)

// userState is everything the server keeps for one user_id across its
// connections: the profile (§3.3), joined rooms (§4.2.2), request
// deduplication (§1.2), and push registrations (§4.9). Guest users
// are retired when their last connection closes; accounts (passkey users)
// persist.
type userState struct {
	id     string
	name   string
	avatar string
	ext    extObject
	// identities are what prove the account, by scheme (auth.go): its
	// email address, its passkeys' user handle, its external sign-in. A
	// guest has none. records are what each scheme keeps with the account,
	// such as passkey credentials.
	identities map[string]string
	records    map[string]jsontext.Value
	// roles are the server roles (§3.3) the account holds, sorted; guests
	// have none. They are derived from Config.Roles, granted, and bot
	// (grantRolesLocked), and not stored themselves.
	roles []string
	// granted are the roles the account's identities grant it, by scheme,
	// as of each identity's latest proof (authn.Proof.Roles), such as an
	// external sign-in's (§4.12).
	granted map[string][]string
	// created is when the user was made; zero for users stored before it
	// was kept.
	created time.Time
	// botOf names the Extension whose bot the account is (Registrar.Bot),
	// which holds the "bot" role (§3.3.4).
	botOf string
	// avatarEmbed is the hosted /avatar upload behind avatar, if any.
	avatarEmbed *embedState
	// writes are the user's writes that have not finished (embedState.author).
	writes map[*embedState]struct{}
	// retired is set on a guest retired when its last connection closed,
	// whose writes go on as they could when it left (writeAllowedLocked).
	retired bool
	// enrolled is what the invitation that made the account left with it,
	// nil if none did (invitations.go).
	enrolled *enrollment
	// scope limits the rooms the account may see, nil for none (scope.go).
	scope *roomScope
	// consents are the https redirect URIs of apps the account allowed on
	// the external sign-in page, with when, in Unix milliseconds
	// (consents.go).
	consents map[string]int64

	clients map[*client]struct{}
	joined  map[string]*roomState
	// leftAt maps each room the user left, and has not rejoined, to the
	// log_id of the leave, for room_list's `left` (§4.2.1).
	leftAt map[string]int64
	// posts limits the user's new messages, room_set requests, and /avatar
	// commands to MessagesPerMinute; nil when unlimited.
	posts *rate.Limiter
	// actions limits the user's other changes that others receive
	// (admitActionLocked).
	actions *rate.Limiter

	// chosen is the chosen status, set with `me` (§4.5.1), and mute and
	// roomMutes their mutes; muteTimer ends a timed mute, and
	// roomMuteTimers each timed room mute. shown is the shown status as
	// last noted (noteShownLocked), and offlineAt the Unix milliseconds
	// when it last became offline. status is the shown status published to
	// others (publishLocked), the one every object they are sent carries,
	// and seen the Unix milliseconds when it last became offline, 0 if
	// unknown (§4.5.2); statusLimit and statusTimer coalesce a flapping
	// user's changes. statusRequests limits the user's `status` requests.
	chosen         string
	mute           muteState
	roomMutes      map[string]muteState
	muteTimer      *time.Timer
	roomMuteTimers map[string]*time.Timer
	shown          string
	offlineAt      int64
	seen           int64
	status         string
	statusLimit    *rate.Limiter
	statusTimer    *time.Timer
	statusRequests *rate.Limiter
	// pushes are the user's push registrations by url (§4.9), and pings the
	// rooms they have not joined where a message mentioned or replied to
	// them, from the first such message, for unread counts. unread holds
	// the counts known per room while the user has registrations, and
	// badgeTimer a pending badge push (unread.go).
	pushes     map[string]*pushRegistration
	pings      map[string]int64
	unread     map[string]int
	badgeTimer *time.Timer
	// pushDay is the UTC day pushesToday counts the pushes delivered to the
	// user on, against maxPushesPerUserDay.
	pushDay     string
	pushesToday int
}

func newUserState(id, name string) *userState {
	return &userState{
		id:        id,
		name:      name,
		clients:   make(map[*client]struct{}),
		joined:    make(map[string]*roomState),
		leftAt:    make(map[string]int64),
		roomMutes: make(map[string]muteState),
		chosen:    statusOnline,
		shown:     statusOffline,
		status:    statusOffline,
		pushes:    make(map[string]*pushRegistration),
		pings:     make(map[string]int64),
		created:   time.Now(),
	}
}

// from is the recorded user object carried in logged records (a message's
// or reaction's from, a membership's user): user_id and name. Avatars and
// ext travel only in current objects (§3.3, §4.8.6).
func (u *userState) from() roomlog.Author {
	return roomlog.Author{UserID: u.id, Name: u.name}
}

// account reports whether the user signed in with a credential (a passkey,
// an email address, or an external identity) or is an Extension's bot, so
// it persists across connections and restarts, rather than being a guest.
func (u *userState) account() bool {
	return len(u.identities) > 0 || u.botOf != ""
}

// hasRole reports whether the user holds a server role.
func (u *userState) hasRole(role string) bool {
	return slices.Contains(u.roles, role)
}

// profile is the complete current user object for user and users (§3.3),
// with the published shown status, and seen when it is offline (§4.5.2);
// you has the chosen status instead.
func (u *userState) profile() wire.CurrentUser {
	return u.current(u.status, true)
}

// current is the user's complete current object with status, and seen
// when the status is offline and withSeen is set. The status is always
// sent, "" too, and so are an account's roles, [] when it has none, so
// that a role taken away clears the one a client kept (§3.3); guests hold
// none.
func (u *userState) current(status string, withSeen bool) wire.CurrentUser {
	object := wire.CurrentUser{User: wire.User{
		UserID: u.id, Name: u.name, Avatar: u.avatar, Status: status,
		Roles: slices.Clone(u.roles), Ext: maps.Clone(u.ext),
	}}
	if withSeen && status == statusOffline && u.seen > 0 {
		object.Seen = strconv.FormatInt(u.seen, 10)
	}
	if status == "" {
		object.Cleared = append(object.Cleared, "status")
	}
	if len(u.roles) == 0 && u.account() {
		object.Cleared = append(object.Cleared, "roles")
	}
	return object
}

// normalizeName prepares a requested display name with the PRECIS
// Nickname profile (RFC 8266): it maps compatibility characters, folds
// runs of spaces, and trims. Invisible characters, such as controls and
// bidirectional overrides, are dropped first rather than refused, and a
// name that still cannot be a nickname becomes empty. The result is capped
// at maxNameRunes. The server may alter names (§3.3) and `you.name` is the
// answer.
func normalizeName(name string) string {
	name = prepareName(name)
	if utf8.RuneCountInString(name) > maxNameRunes {
		name = strings.TrimSpace(string([]rune(name)[:maxNameRunes]))
	}
	return name
}

// prepareName is normalizeName without the cap.
func prepareName(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs) {
			return -1
		}
		return r
	}, name)
	name, err := precis.Nickname.String(name)
	if err != nil {
		return ""
	}
	return name
}

var avatarDataURL = regexp.MustCompile(`^data:image/(png|jpeg|gif|webp);base64,[A-Za-z0-9+/]+={0,2}$`)

// validAvatar accepts https: URLs and small image data: URLs (§4.8.6).
func validAvatar(value string) bool {
	if strings.HasPrefix(value, "data:") {
		return len(value) <= maxAvatarDataURLBytes && avatarDataURL.MatchString(value)
	}
	return strings.HasPrefix(value, "https://") && len(value) <= 2048 && !strings.ContainsAny(value, " \t\r\n\"'<>")
}

// requestable reports whether a user may request id (accounts.Requestable).
func requestable(id string) bool { return accounts.Requestable(id) }

// assignUserIDLocked honors a requested user_id when it is requestable and
// was never assigned, ignoring case, nor names a room; otherwise it assigns
// the next unused guest_<n> (§3.2, accounts.IDs.Guest).
func (s *Server) assignUserIDLocked(requested string) string {
	return s.ids.Guest(requested, s.roomNamedLocked)
}

// roomNamedLocked reports whether id names a room, ignoring case, so a
// user_id never passes for a room_id to a reader that folds case. Rooms
// created by clients have numeric IDs; only the seeded room has letters.
func (s *Server) roomNamedLocked(id string) bool {
	return s.rooms.Get(id) != nil || strings.EqualFold(id, defaultRoomID)
}

// switchUserLocked replies with user as `you`, and the token that resumes
// the sign-in, if any (§3.2, §3.3), then makes user the connection's
// identity. Every
// notification a sign-in causes on its connection follows the result
// (§3.2): the departure of the identity it leaves, the join of a new
// identity to the default room when joinDefault is set, and then what
// follows a sign-in (§4.5). A sign-in signs the connection in as a user it
// was not signed in as: an auth that adds a passkey or address, or that
// signs in again as the same user, is not one, and is sent nothing after
// its result. No room_update is sent for rooms the identity had already
// joined: the client lists them with room_list.
func (s *Server) switchUserLocked(c *client, reply wire.Reply[wire.AuthResult], user *userState, token string, signIn, joinDefault bool) {
	you := user.you()
	_ = reply.Send(wire.AuthResult{You: &you, Token: token})
	// A connection that got only what signing in needs gets the whole
	// server frame now (§3.1).
	if !s.revealedLocked(c) {
		c.conn.Announce(s.serverParams(c.baseURL, true))
	}
	s.attachLocked(c, user)
	if joinDefault {
		s.joinDefaultRoomLocked(user)
	}
	if signIn {
		s.sendAfterAuthLocked(c)
	}
}

// attachLocked makes user the connection's identity and announces the
// status others see of it and of the identity the connection leaves
// (§4.5). A guest identity left without connections is retired, logging
// its leaves, as when its last connection closes: a sign-in to an existing
// account is announced as the guest's departure, never as a `user` change
// with `old` (§3.3), and the account appears to others through its own
// status, so an invisible one, or one without a status, shows nothing new.
// A guest that becomes a new account does so in place, keeping its user_id
// (a passkey registration or an email addition), so this server has no
// user_id change to announce with `old`.
func (s *Server) attachLocked(c *client, user *userState) {
	previous := c.user
	if previous == user {
		return
	}
	if previous != nil {
		delete(previous.clients, c)
	}
	// A pending email proposal was made by the previous identity: it is
	// dropped, so the connection cannot approve it as the next one
	// (§4.11.3).
	if c.proposal != nil {
		s.proposals.Drop(c.proposal)
		c.proposal = nil
	}
	// So is what the schemes kept for it, such as a pending passkey
	// ceremony.
	c.authStates = nil
	c.user = user
	c.conn.SetUser(wire.User{UserID: user.id})
	user.clients[c] = struct{}{}
	// The previous identity's departure goes first, then the user's own
	// status.
	switch {
	case previous == nil:
	case len(previous.clients) == 0 && !previous.account():
		s.retireLocked(previous)
	default:
		s.announceStatusLocked(previous)
	}
	s.announceStatusLocked(user)
}

// detachLocked forgets a closed connection, retiring its guest identity.
func (s *Server) detachLocked(c *client) {
	user := c.user
	if user == nil {
		return
	}
	delete(user.clients, c)
	c.user = nil
	c.conn.SignOut()
	if len(user.clients) == 0 && !user.account() {
		s.retireLocked(user)
		return
	}
	s.announceStatusLocked(user)
}

// retireLocked removes a guest identity for good. It leaves every room it
// joined, logging each leave as a membership for the remaining members
// (§4.2.2). Its user_id is never reissued, so its records stay consistent
// (§3.3).
func (s *Server) retireLocked(u *userState) {
	u.retired = true
	rooms := slices.SortedFunc(maps.Values(u.joined), func(a, b *roomState) int {
		return cmp.Compare(a.Log().Created(), b.Log().Created())
	})
	// Its registrations go first, so its leaves send it no badge pushes.
	for _, registration := range u.pushes {
		s.removePushLocked(registration)
	}
	for _, r := range rooms {
		s.leaveLocked(u, r)
		s.rooms.ForgetRead(r, u.id)
	}
	s.stopTimersLocked(u)
	s.setAvatarEmbedLocked(u, nil)
	delete(s.users, u.id)
	s.touchUser(u.id)
}

// stopTimersLocked stops u's timers: the ends of its timed mutes, its
// coalesced status, and its pending badge pushes.
func (s *Server) stopTimersLocked(u *userState) {
	if u.muteTimer != nil {
		u.muteTimer.Stop()
		u.muteTimer = nil
	}
	if u.statusTimer != nil {
		u.statusTimer.Stop()
		u.statusTimer = nil
	}
	for id, timer := range u.roomMuteTimers {
		timer.Stop()
		delete(u.roomMuteTimers, id)
	}
	s.stopBadgeLocked(u)
}

// send queues a frame to every connection of the user.
func (u *userState) send(frames ...any) {
	for c := range u.clients {
		c.enqueueBatch(frames...)
	}
}

// sharersLocked lists the other users who share a joined room with u.
func (s *Server) sharersLocked(u *userState) []*userState {
	seen := make(map[string]*userState)
	for _, r := range u.joined {
		for id, member := range r.Members() {
			if id != u.id {
				seen[id] = member
			}
		}
	}
	ids := slices.Sorted(maps.Keys(seen))
	users := make([]*userState, len(ids))
	for i, id := range ids {
		users[i] = seen[id]
	}
	return users
}

// userViews are a user as its two audiences see it (wire.UserChange): its
// own connections, which see the chosen status (you), and everyone else,
// who see the shown status (shownView).
type userViews struct{ you, shown wire.User }

// views are u as each audience sees it now, for a change to be told from.
func (u *userState) views() userViews {
	return userViews{you: u.you().User, shown: u.shownView()}
}

// shownView is u as others see it: its profile, with the published shown
// status, and its seen whenever it is known, as a client keeps it. As seen
// changes only when the shown status becomes offline (§4.5.2), the change
// of one view to another carries seen only then, with that status, and
// never clears it: a user object carries seen only with a shown offline.
func (u *userState) shownView() wire.User {
	shown := u.profile().User
	if u.seen > 0 {
		shown.Seen = strconv.FormatInt(u.seen, 10)
	}
	return shown
}

// notifyProfileLocked tells of a change to u since before, as package wire
// does (wire.Server.PublishUserChange): `you` to the user's connections but
// except, whose result carries the user, and `new` to everyone who shares
// a room with them (§3.3.3), each with the fields of their view that
// changed, a cleared one as its empty value (§3.3.1), and only when their
// view changed. The change goes to every sharer, so it publishes the shown
// status, one held back to coalesce included.
func (s *Server) notifyProfileLocked(u *userState, except *client, before userViews) {
	s.publishLocked(u)
	s.publishUserLocked(u, except, before)
}

// publishUserLocked tells of a change to u since before (notifyProfileLocked),
// without publishing its shown status.
func (s *Server) publishUserLocked(u *userState, except *client, before userViews) {
	sharers := make(map[*userState]bool)
	for _, other := range s.sharersLocked(u) {
		sharers[other] = true
	}
	now := u.views()
	change := wire.UserChange{Old: before.you, New: now.you, OldShown: before.shown, NewShown: now.shown}
	if except != nil {
		change.Except = except.conn
	}
	s.wire.PublishUserChange(change, func(wc *wire.Conn) bool {
		c, ok := wire.DataOf[*client](wc)
		return ok && c.user != nil && sharers[c.user]
	})
}

// withRemoved clears each removed field of a profile, which sends it as
// its empty value.
func withRemoved(profile wire.CurrentUser, removed []string) wire.CurrentUser {
	for _, field := range removed {
		if !slices.Contains(profile.Cleared, field) {
			profile.Cleared = append(profile.Cleared, field)
		}
	}
	return profile
}

// updateProfile applies a `me` request (§3.3): a given field replaces the
// current value, an omitted one stays, and an empty value ("" or {}) removes
// the field, which the result and notifications carry as that empty value.
// Names are trimmed and capped; avatars must be https: URLs or small image
// data: URLs, or the current avatar unchanged. status is the chosen status
// (§4.5.1), "" for a value the server does not support, and is kept
// across connections and restarts; a change to it counts against the
// user's limit on status changes (admitStatusLocked), beyond which the
// request is retry_after and changes nothing. roles are not settable, and
// like other unknown fields are ignored.
func (s *Server) updateProfile(c *client, p wire.MeParams, reply wire.Reply[wire.CurrentUser]) *Error {
	var name, avatar, status string
	hasName, hasAvatar, hasStatus := p.Name != nil, p.Avatar != nil, p.Status != nil
	if hasName {
		name = *p.Name
	}
	if hasAvatar {
		avatar = *p.Avatar
	}
	if hasStatus {
		status = *p.Status
	}
	extWrite, hasExt := p.Ext, p.Ext != nil

	s.mu.Lock()
	defer s.unlock()
	u := c.user
	if hasAvatar && avatar != "" && avatar != u.avatar && !validAvatar(avatar) {
		if strings.HasPrefix(avatar, "data:") && len(avatar) > maxAvatarDataURLBytes {
			return &Error{Code: CodeTooLarge, Message: fmt.Sprintf("An avatar data: URL is at most %d bytes; upload larger images with /avatar", maxAvatarDataURLBytes)}
		}
		return invalidParams("avatar must be an https: URL or a data:image URL of at most %d bytes; upload larger images with /avatar", maxAvatarDataURLBytes)
	}
	if hasStatus && !settableStatus(status) {
		status = statusNone
	}
	// ext merges into the kept one (§4.12), and the limit applies to the
	// result.
	ext := u.ext
	if hasExt {
		ext = mergeExt(u.ext, extWrite)
		if encodedSize(ext) > maxProfileExtBytes {
			return &Error{Code: CodeTooLarge, Message: fmt.Sprintf("Your profile's ext would be over %d bytes", maxProfileExtBytes)}
		}
	}
	// A status change counts against the user's limit on status changes,
	// as a `status` request does (§4.5), so `me` flipping it in a loop
	// cannot make the server announce it to every sharer without end.
	if hasStatus && status != u.chosen {
		if err := admitStatusLocked(u); err != nil {
			return err
		}
	}
	// A change of name, avatar, or ext goes to everyone who shares a room
	// with the user.
	if (hasName && normalizeName(name) != u.name) || (hasAvatar && avatar != u.avatar) || (hasExt && !jsonEqual(ext, u.ext)) {
		if err := admitActionLocked(u); err != nil {
			return err
		}
	}
	before := u.views()
	s.touchUser(u.id)
	var removed []string
	if hasName {
		u.name = normalizeName(name)
		if u.name == "" {
			removed = append(removed, "name")
		}
	}
	if hasAvatar && avatar != u.avatar {
		s.setAvatarEmbedLocked(u, nil)
		u.avatar = avatar
	}
	if hasAvatar && avatar == "" {
		removed = append(removed, "avatar")
	}
	u.ext = ext
	if hasStatus {
		// A change of the chosen status is sent at once, with the profile.
		u.chosen = status
		s.publishLocked(u)
	}
	s.notifyProfileLocked(u, c, before)
	// The result is the user's complete object, which Send makes the
	// connection's user too.
	_ = reply.Send(withRemoved(u.you(), removed))
	return nil
}

func jsonEqual(a, b any) bool {
	left, right := encodeJSON(a), encodeJSON(b)
	return left != nil && right != nil && string(left) == string(right)
}

// grantRolesLocked sets the roles an account holds: those Config.Roles
// grants it, by its user_id or an identity, those its identities grant it
// (authn.Proof.Roles), and "bot" for a bot, and reports whether they
// changed. Guests hold no roles, so a granted user_id taken by a guest
// grants nothing until it is an account; assignUserIDLocked never hands out
// a granted user_id that was never used.
func (s *Server) grantRolesLocked(u *userState) bool {
	var roles []string
	if u.account() {
		holders := []Principal{UserID(u.id)}
		for scheme, subject := range u.identities {
			holders = append(holders, principalOf(authn.Identity{Scheme: scheme, Subject: subject}))
		}
		for role, granted := range s.roleHolders {
			if slices.ContainsFunc(granted, func(p Principal) bool { return slices.Contains(holders, p) }) {
				roles = append(roles, role)
			}
		}
		for _, granted := range u.granted {
			roles = append(roles, granted...)
		}
		if u.botOf != "" {
			roles = append(roles, "bot")
		}
		slices.Sort(roles)
		roles = slices.Compact(roles)
	}
	if slices.Equal(roles, u.roles) {
		return false
	}
	u.roles = roles
	return true
}
