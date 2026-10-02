package server

import (
	"cmp"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/secure/precis"
	"golang.org/x/time/rate"
)

const (
	maxNameRunes = 64
	// maxAvatarDataURLBytes bounds an avatar given inline as a data: URL;
	// larger images go through a /avatar upload (§4.6.6).
	maxAvatarDataURLBytes = 64 << 10
	maxDedupEntries       = 1024
	// maxProfileExtBytes bounds a profile's ext, which is sent to everyone
	// who shares a room with the user.
	maxProfileExtBytes = 16 << 10
)

// userState is everything the server keeps for one user_id across its
// connections: the profile (§3.3), joined rooms (§4.3.2), request
// deduplication (§1.2), and push registrations (§4.7). Guest users
// are retired when their last connection closes; accounts (passkey users)
// persist.
type userState struct {
	id     string
	name   string
	avatar string
	ext    map[string]any
	// email is the account's sign-in address (§4.10), lowercased; empty for
	// guests and passkey users without one.
	email string
	// roles are the server roles (§3.3) Config.Roles grants the account,
	// sorted; guests have none. They are derived from the configuration, not
	// stored.
	roles []string
	// avatarEmbed is the hosted /avatar upload behind avatar, if any.
	avatarEmbed *embedState

	// fromValue caches from(); it is cleared when the name changes.
	fromValue map[string]any

	clients map[*client]struct{}
	joined  map[string]*roomState
	// leftAt maps each room the user left, and has not rejoined, to the
	// log_id of the leave, for room_list's `left` (§4.3.1).
	leftAt  map[string]int64
	dedup   dedupCache
	passkey *passkeyUser
	// posts limits the user's new messages, room_set requests, and /avatar
	// commands to MessagesPerMinute; nil when unlimited.
	posts *rate.Limiter

	// invisible, mute, and roomMutes are the user's status fields (§4.11);
	// muteTimer ends a timed mute. status is the status last announced.
	invisible bool
	mute      muteState
	roomMutes map[string]muteState
	muteTimer *time.Timer
	status    string
	// pushes are the user's push registrations by url (§4.7), and pings the
	// rooms they have not joined where a message mentioned or replied to
	// them, from the first such message, for unread counts.
	pushes map[string]*pushRegistration
	pings  map[string]int64
}

func newUserState(id, name string) *userState {
	return &userState{
		id:        id,
		name:      name,
		clients:   make(map[*client]struct{}),
		joined:    make(map[string]*roomState),
		leftAt:    make(map[string]int64),
		roomMutes: make(map[string]muteState),
		status:    statusOffline,
		pushes:    make(map[string]*pushRegistration),
		pings:     make(map[string]int64),
	}
}

// from is the recorded user object carried in logged records (a message's
// or reaction's from, a membership's user): user_id and name. Avatars and
// ext travel only in current objects (§3.3, §4.6.6). Every
// record by the user shares the returned map until the name changes, so it
// must not be modified.
func (u *userState) from() map[string]any {
	if u.fromValue == nil {
		u.fromValue = map[string]any{"user_id": u.id}
		if u.name != "" {
			u.fromValue["name"] = u.name
		}
	}
	return u.fromValue
}

// account reports whether the user signed in with a credential, a passkey
// or an email address, so it persists across connections and restarts,
// rather than being a guest.
func (u *userState) account() bool {
	return u.passkey != nil || u.email != ""
}

// hasRole reports whether the user holds a server role.
func (u *userState) hasRole(role string) bool {
	return slices.Contains(u.roles, role)
}

// profile is the complete current user object for user and users (§3.3),
// with the user's status (§4.11); you adds what only the user sees.
func (u *userState) profile() map[string]any {
	value := maps.Clone(u.from())
	value["status"] = u.statusAt(time.Now())
	if u.avatar != "" {
		value["avatar"] = u.avatar
	}
	// An account's roles are always sent, [] when it has none, so a role
	// taken away clears the one a client kept (§3.3); guests hold none.
	if len(u.roles) > 0 || u.account() {
		value["roles"] = append([]string{}, u.roles...)
	}
	if len(u.ext) > 0 {
		value["ext"] = cloneObject(u.ext)
	}
	return value
}

type dedupEntry struct {
	// fingerprint is a hash of the request's method and canonical params.
	fingerprint [32]byte
	done        chan struct{}
	result      any
	err         *rpcError
}

// dedupCache keeps the most recent request IDs of one user. Guarded by s.mu.
type dedupCache struct {
	entries map[string]*dedupEntry
	order   []string
}

func (d *dedupCache) get(id string) *dedupEntry {
	return d.entries[id]
}

func (d *dedupCache) put(id string, entry *dedupEntry) {
	if d.entries == nil {
		d.entries = make(map[string]*dedupEntry)
	}
	d.entries[id] = entry
	d.order = append(d.order, id)
	for len(d.order) > maxDedupEntries {
		oldest := d.order[0]
		d.order = d.order[1:]
		if e := d.entries[oldest]; e != nil && isClosed(e.done) {
			delete(d.entries, oldest)
		}
	}
}

func (d *dedupCache) remove(id string, entry *dedupEntry) {
	if d.entries[id] == entry {
		delete(d.entries, id)
	}
}

func isClosed(done chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// normalizeName prepares a requested display name with the PRECIS
// Nickname profile (RFC 8266): it maps compatibility characters, folds
// runs of spaces, and trims. Invisible characters, such as controls and
// bidirectional overrides, are dropped first rather than refused, and a
// name that still cannot be a nickname becomes empty. The result is capped
// at maxNameRunes. The server may alter names (§3.3) and `you.name` is the
// answer.
func normalizeName(name string) string {
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
	if utf8.RuneCountInString(name) > maxNameRunes {
		name = strings.TrimSpace(string([]rune(name)[:maxNameRunes]))
	}
	return name
}

var avatarDataURL = regexp.MustCompile(`^data:image/(png|jpeg|gif|webp);base64,[A-Za-z0-9+/]+={0,2}$`)

// validAvatar accepts https: URLs and small image data: URLs (§4.6.6).
func validAvatar(value string) bool {
	if strings.HasPrefix(value, "data:") {
		return len(value) <= maxAvatarDataURLBytes && avatarDataURL.MatchString(value)
	}
	return strings.HasPrefix(value, "https://") && len(value) <= 2048 && !strings.ContainsAny(value, " \t\r\n\"'<>")
}

// requestableUserID is the shape of a user_id a guest may request: a
// mentionable ID (Appendix A.3) that starts with a letter, so it never looks
// like a system identity or a log_id-derived room_id.
var requestableUserID = regexp.MustCompile(`^[A-Za-z](?:[A-Za-z0-9_.-]{0,62}[A-Za-z0-9_])?$`)

// guestIDPrefix starts every counter-assigned guest ID. The guest_
// namespace, ignoring case, belongs to the counter: requests in it are never
// honored, so guest numbers are taken only in sequence and the latest one
// counts the guests the server has admitted.
const guestIDPrefix = "guest_"

// requestable reports whether a user may request id: it has the requestable
// shape and lies outside the counters' guest_ and user_ namespaces.
func requestable(id string) bool {
	for _, prefix := range []string{guestIDPrefix, accountIDPrefix} {
		if len(id) >= len(prefix) && strings.EqualFold(id[:len(prefix)], prefix) {
			return false
		}
	}
	return requestableUserID.MatchString(id)
}

// assignUserIDLocked honors a requested user_id when it is requestable and
// was never assigned, ignoring case, nor names a room; otherwise it assigns
// the next unused guest_<n> (§3.2). Each call that does not honor a request
// takes exactly one counter value: with guest_ requests refused, only the
// counter assigns guest_<n>, so the skip over taken IDs is a safeguard.
func (s *Server) assignUserIDLocked(requested string) string {
	if requestable(requested) && s.claimUserIDLocked(requested) {
		return requested
	}
	for {
		s.guestNumber++
		if id := fmt.Sprintf("%s%d", guestIDPrefix, s.guestNumber); s.claimUserIDLocked(id) {
			return id
		}
	}
}

// claimUserIDLocked marks id used and reports whether it was free: never
// assigned, ignoring case, not a room's name, and not granted a role.
func (s *Server) claimUserIDLocked(id string) bool {
	key := strings.ToLower(id)
	if s.usedIDs[key] || s.roomNamedLocked(id) || s.grantedIDs[key] {
		return false
	}
	s.usedIDs[key] = true
	s.touchUsedID(key)
	return true
}

// roomNamedLocked reports whether id names a room, ignoring case, so a
// user_id never passes for a room_id to a reader that folds case. Rooms
// created by clients have numeric IDs; only the seeded room has letters.
func (s *Server) roomNamedLocked(id string) bool {
	return s.rooms[id] != nil || strings.EqualFold(id, defaultRoomID)
}

func (s *Server) authenticate(c *client, req request) (any, *rpcError) {
	s.mu.Lock()
	defer s.unlock()
	scheme, err := parseString(req.params, "scheme", true)
	if err != nil {
		return nil, err
	}
	switch scheme {
	case "webauthn":
		return s.authenticatePasskey(c, req)
	case "email":
		if s.config.EmailSender == nil {
			return nil, &rpcError{Code: codeUnsupported, Message: "Email sign-in is disabled"}
		}
		return s.authenticateEmail(c, req)
	case "token":
		if s.config.WebAuthn == nil && s.config.EmailSender == nil {
			return nil, &rpcError{Code: codeUnsupported, Message: "This server has no sign-in sessions; use guest"}
		}
		return s.authenticateToken(c, req)
	case "guest":
	default:
		return nil, &rpcError{Code: codeUnsupported, Message: fmt.Sprintf("Unsupported authentication scheme %q", scheme)}
	}
	name, err := parseString(req.params, "name", false)
	if err != nil {
		return nil, err
	}
	requested, err := parseString(req.params, "user_id", false)
	if err != nil {
		return nil, err
	}
	if c.user != nil {
		result := map[string]any{"you": c.user.you()}
		if req.hasID {
			c.sendResult(req, result)
		}
		return result, nil
	}
	user := newUserState(s.assignUserIDLocked(requested), normalizeName(name))
	s.users[user.id] = user
	s.touchUser(user.id)
	s.attachLocked(c, user)
	// A new guest joins the default room, so their room list is not empty,
	// and the membership reaches this connection before the result (§1).
	s.joinDefaultRoomLocked(user)
	result := map[string]any{"you": user.you()}
	if req.hasID {
		c.sendResult(req, result)
	}
	return result, nil
}

// switchUserLocked makes user the connection's identity and replies with
// extra fields beside `you` (§3.2, §3.3). No room_update is sent for the new
// identity's rooms: the client lists them with room_list.
func (s *Server) switchUserLocked(c *client, req request, user *userState, extra map[string]any) map[string]any {
	s.attachLocked(c, user)
	result := map[string]any{"you": user.you()}
	maps.Copy(result, extra)
	if req.hasID {
		c.sendResult(req, result)
	}
	return result
}

// attachLocked makes user the connection's identity, applying the status
// the connection sent before signing in (§4.11) and announcing the user's
// new status. A guest identity left without connections is retired, logging
// its leaves, and then others who shared a room with it learn of the
// user_id change through a `user` notification with `new` and `old` (§3.3).
func (s *Server) attachLocked(c *client, user *userState) {
	previous := c.user
	if previous == user {
		return
	}
	if previous != nil {
		delete(previous.clients, c)
	}
	// A pending email proposal was made by the previous identity: an
	// addition must not be approved as the next one (§4.10).
	if c.proposal != nil {
		s.dropProposalLocked(c.proposal)
	}
	c.user = user
	user.clients[c] = struct{}{}
	s.applyPendingStatusLocked(c, user)
	s.statusChangedLocked(user, c, false)
	if previous == nil || len(previous.clients) > 0 || previous.account() {
		if previous != nil {
			s.statusChangedLocked(previous, nil, false)
		}
		return
	}
	sharers := s.sharersLocked(previous)
	frame := notification("user", map[string]any{"new": user.profile(), "old": previous.profile()})
	s.retireLocked(previous)
	for _, other := range sharers {
		if other != user {
			other.send(frame)
		}
	}
}

// detachLocked forgets a closed connection, retiring its guest identity.
func (s *Server) detachLocked(c *client) {
	user := c.user
	if user == nil {
		return
	}
	delete(user.clients, c)
	c.user = nil
	if len(user.clients) == 0 && !user.account() {
		s.retireLocked(user)
		return
	}
	s.statusChangedLocked(user, nil, false)
}

// retireLocked removes a guest identity for good. It leaves every room it
// joined, logging each leave as a membership for the remaining members
// (§4.3.2). Its user_id is never reissued, so its records stay consistent
// (§3.3).
func (s *Server) retireLocked(u *userState) {
	rooms := slices.SortedFunc(maps.Values(u.joined), func(a, b *roomState) int {
		return cmp.Compare(a.createdID, b.createdID)
	})
	for _, r := range rooms {
		s.leaveLocked(u, r)
		delete(r.reads, u.id)
	}
	for _, registration := range u.pushes {
		s.removePushLocked(registration)
	}
	if u.muteTimer != nil {
		u.muteTimer.Stop()
	}
	s.setAvatarEmbedLocked(u, nil)
	delete(s.users, u.id)
	s.touchUser(u.id)
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
		for id, member := range r.members {
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

// notifyProfileLocked sends a `user` notification after a profile change:
// `you` to the user's connections other than except, and `new` to everyone
// who shares a room with them (§3.3). removed lists fields the change
// removed, announced as empty values.
func (s *Server) notifyProfileLocked(u *userState, except *client, removed ...string) {
	profile := withRemoved(u.profile(), removed)
	you := notification("user", map[string]any{"you": withRemoved(u.you(), removed)})
	for c := range u.clients {
		if c != except {
			c.enqueue(you)
		}
	}
	others := notification("user", map[string]any{"new": profile})
	for _, other := range s.sharersLocked(u) {
		other.send(others)
	}
}

// withRemoved adds each removed field to a profile as its empty value.
func withRemoved(profile map[string]any, removed []string) map[string]any {
	for _, field := range removed {
		if field == "ext" {
			profile[field] = map[string]any{}
		} else {
			profile[field] = ""
		}
	}
	return profile
}

// updateProfile applies a `me` request (§3.3): a given field replaces the
// current value, an omitted one stays, and an empty value ("" or {}) removes
// the field, which the result and notifications carry as that empty value.
// Names are trimmed and capped; avatars must be https: URLs or small image
// data: URLs, or the current avatar unchanged. roles are not settable, and
// like other unknown fields are ignored.
func (s *Server) updateProfile(c *client, req request) (any, bool, *rpcError) {
	name, err := parseString(req.params, "name", false)
	if err != nil {
		return nil, false, err
	}
	avatar, err := parseString(req.params, "avatar", false)
	if err != nil {
		return nil, false, err
	}
	ext, err := parseObject(req.params, "ext", false)
	if err != nil {
		return nil, false, err
	}
	if ext != nil && len(req.params["ext"]) > maxProfileExtBytes {
		return nil, false, invalidParams("ext is at most %d bytes", maxProfileExtBytes)
	}
	_, hasName := req.params["name"]
	_, hasAvatar := req.params["avatar"]

	s.mu.Lock()
	defer s.unlock()
	u := c.user
	if hasAvatar && avatar != "" && avatar != u.avatar && !validAvatar(avatar) {
		return nil, false, invalidParams("avatar must be an https: URL or a data:image URL of at most %d bytes; upload larger images with /avatar", maxAvatarDataURLBytes)
	}
	before := u.profile()
	s.touchUser(u.id)
	var removed []string
	if hasName {
		u.name = normalizeName(name)
		u.fromValue = nil
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
	if ext != nil {
		u.ext = ext
		if len(ext) == 0 {
			u.ext = nil
			removed = append(removed, "ext")
		}
	}
	result := map[string]any{"you": withRemoved(u.you(), removed)}
	if !jsonEqual(before, u.profile()) {
		s.notifyProfileLocked(u, c, removed...)
	}
	if req.hasID {
		c.sendResult(req, result)
	}
	return result, true, nil
}

func jsonEqual(a, b any) bool {
	left, right := encodeJSON(a), encodeJSON(b)
	return left != nil && right != nil && string(left) == string(right)
}

// grantRolesLocked sets the roles Config.Roles grants an account, by its
// user_id or email address, and reports whether they changed. Guests hold no
// roles, so a granted user_id taken by a guest grants nothing until it is an
// account; assignUserIDLocked never hands out a granted user_id that was
// never used.
func (s *Server) grantRolesLocked(u *userState) bool {
	var roles []string
	if u.account() {
		for role, holders := range s.config.Roles {
			if slices.ContainsFunc(holders, func(holder string) bool {
				return strings.EqualFold(holder, u.id) || (u.email != "" && strings.EqualFold(holder, u.email))
			}) {
				roles = append(roles, strings.ToLower(role))
			}
		}
		slices.Sort(roles)
	}
	if slices.Equal(roles, u.roles) {
		return false
	}
	u.roles = roles
	return true
}
