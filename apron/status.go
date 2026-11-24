package apron

import (
	"encoding/json/jsontext"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/apron-chat/apron-server-go/apron/internal/presence"
	"github.com/apron-chat/apron-server-go/apron/internal/users"
	"github.com/apron-chat/apron-server-go/wire"
)

// Status (§4.5). The chosen status is set with `me`: online (the default),
// "" for none, dnd, or invisible; the server stores "" for any other value.
// Others see the shown status, which the chosen status and the presence
// give (§4.5.1), as last published: a presence.State keeps both, and when
// seen (§4.5.2) moves. The server notes each change of the presence, and
// publishes the shown status to those who share a room with the user, at
// once or, for a flapping user, coalesced (announceStatusLocked), so
// everyone sees the same changes; the user sees, in `you`, the chosen
// status. Separately, each connection reports whether it is idle, and the
// user mutes their notifications everywhere or in one room and its
// threads; mutes and a dnd status silence pushes (§4.9). Both are set with
// the `status` request. Mutes are private: each change goes to every
// connection of the user as a `status` notification, and the mutes in
// effect to a connection after a sign-in. A connection starts attended, and
// only its own `idle` with the seconds since attention stopped makes it
// idle, until its `idle: false` (§4.5.3): a connection that never sends
// idle is never taken as idle.
const (
	statusOnline    = presence.Online
	statusIdle      = presence.Idle
	statusDND       = presence.DND
	statusInvisible = presence.Invisible
	statusOffline   = presence.Offline
	statusNone      = presence.None
	// maxMuteSeconds caps a timed mute at a year; `true` mutes until changed.
	maxMuteSeconds = 365 * 24 * 60 * 60
	// maxRoomMutes bounds the rooms a user mutes at once; muting another
	// is denied until one is unmuted.
	maxRoomMutes = 1000
	// Each user may send statusRequestBurst `status` requests at once,
	// refilled one every statusRequestRefill; beyond them a request is
	// retry_after (§4.5).
	statusRequestBurst  = presence.RequestBurst
	statusRequestRefill = presence.RequestRefill
	// A user's shown status changes go to others at once up to
	// statusBurst times, then at most once per statusCoalesce (presence).
	statusBurst    = presence.Burst
	statusCoalesce = presence.Coalesce
)

// optionalStatuses are the optional statuses the server accepts, which the
// server frame lists as server.status (§3.1, §4.5).
var optionalStatuses = presence.Optional

// settableStatus reports whether the server supports value as a status a
// user sets (§4.5); it stores "" for any other.
func settableStatus(value string) bool { return presence.Settable(value) }

// muteState is a mute (§4.5): until a time, forever, or, when zero, none.
type muteState = users.Mute

// muteValue is a mute as a `status` notification carries it (wire.Mute):
// true, the seconds left, rounded up, or false when there is none.
func muteValue(m muteState, now time.Time) wire.Mute {
	switch {
	case m.Forever:
		return wire.Mute{On: true}
	case !m.Active(now):
		return wire.Mute{}
	}
	return wire.Mute{On: true, Seconds: int64((m.Until.Sub(now) + time.Second - 1) / time.Second)}
}

// statusUpdate is one parsed `status` request. A nil field was absent.
type statusUpdate struct {
	// roomID, when scoped, is the room the mute is of.
	roomID string
	scoped bool
	idle   *bool
	// mute is present when hasMute: forever, a positive number of seconds,
	// or, when neither, false, which ends it.
	hasMute     bool
	muteForever bool
	muteSeconds int64
}

// statusUpdateOf is a status request (§4.5.3), as package wire reads it,
// with a mute longer than maxMuteSeconds shortened to it.
func statusUpdateOf(p wire.StatusParams) statusUpdate {
	update := statusUpdate{roomID: p.RoomID, scoped: p.RoomID != ""}
	if p.Idle != nil {
		idle := !p.Idle.Attended
		update.idle = &idle
	}
	if p.Mute != nil {
		update.hasMute = true
		switch {
		case p.Mute.On && p.Mute.Seconds == 0:
			update.muteForever = true
		case p.Mute.On:
			update.muteSeconds = min(p.Mute.Seconds, maxMuteSeconds)
		}
	}
	return update
}

// mute returns the mute an update sets, starting at now.
func (update statusUpdate) mute(now time.Time) muteState {
	if update.muteForever {
		return muteState{Forever: true}
	}
	if update.muteSeconds == 0 {
		// mute: false
		return muteState{}
	}
	return muteState{Until: now.Add(time.Duration(update.muteSeconds) * time.Second)}
}

// status applies a `status` request (§4.5) and answers {} once it has: the
// mute's echo to the user's connections, the sender's included, precedes
// the result. Like other requests it needs a signed-in connection, and a
// `status` without an id is a notification no client sends, which
// package wire ignores. On an error nothing changes: invalid params, a mute
// of a room the user cannot see, a mute of another room beyond
// maxRoomMutes, or more requests than the user's limit (admitStatusLocked).
func (s *Server) status(c *client, p wire.StatusParams, reply wire.Reply[wire.OK]) *Error {
	update := statusUpdateOf(p)
	s.mu.Lock()
	defer s.unlock()
	u := c.user
	// A room mute the user has stays clearable after the room is no longer
	// visible to them, such as a private room they left, so it does not keep
	// counting toward maxRoomMutes, or come back if they are added again.
	_, hasRoomMute := u.RoomMutes[update.roomID]
	clearing := hasRoomMute && update.muteSeconds == 0 && !update.muteForever
	if update.hasMute && update.scoped && !clearing && s.visibleRoomLocked(u, update.roomID) == nil {
		return invalidParams("Unknown room %q", update.roomID)
	}
	if now := time.Now(); update.hasMute && update.scoped && update.mute(now).Active(now) {
		if _, muted := u.RoomMutes[update.roomID]; !muted && len(u.RoomMutes) >= maxRoomMutes {
			return &Error{Code: CodeDenied, Message: fmt.Sprintf("At most %d rooms can be muted; unmute one first", maxRoomMutes)}
		}
	}
	if err := admitStatusLocked(u); err != nil {
		return err
	}
	s.applyStatusLocked(c, u, update)
	_ = reply.Send(wire.OK{})
	return nil
}

// admitStatusLocked applies the user's limit on `status` requests, idle and
// mute alike, and on `me` requests that change the status (§4.5 lets
// servers limit them): a burst of statusRequestBurst,
// refilled one every statusRequestRefill, across all of the user's
// connections. Beyond it the request is retry_after and changes nothing, so
// a client flipping idle or its mutes in a loop cannot make the server
// announce, store, and recount for it without end.
func admitStatusLocked(u *userState) *Error {
	if delay := u.Presence.Admit(time.Now()); delay > 0 {
		return RetryAfter("Too many status changes; slow down", delay)
	}
	return nil
}

// applyStatusLocked applies an update for user u from connection c, then
// announces what changed. A scoped mute's room is visible to u, unless
// the update clears a mute u has.
func (s *Server) applyStatusLocked(c *client, u *userState, update statusUpdate) {
	if update.idle != nil && c.idle != *update.idle {
		c.idle = *update.idle
		s.announceStatusLocked(u)
	}
	if !update.hasMute {
		return
	}
	now := time.Now()
	mute := update.mute(now)
	if !update.scoped {
		s.users.SetMute(u, "", mute)
		u.send(muteFrame("", mute, now))
		return
	}
	id := update.roomID
	if !mute.Active(now) {
		// A room's mute that ends is forgotten.
		mute = muteState{}
	}
	s.users.SetMute(u, id, mute)
	u.send(muteFrame(id, mute, now))
	// What counts toward unread changes with a room's mute, in the room and
	// its threads, at any depth (unread.go), so every count is taken again,
	// as when the mute runs out.
	s.unreadChangedLocked(u, "")
}

// muteFrame is the `status` notification that tells the user's connections
// of a mute, everywhere or, with roomID, of that room (§4.5). Its params
// are a map, not a wire.StatusUpdate, which would send room_id first: the
// server has always sent its keys sorted.
func muteFrame(roomID string, mute muteState, now time.Time) jsontext.Value {
	return encodeNotification(wire.StatusUpdate{RoomID: roomID, Mute: muteValue(mute, now)})
}

// sendAfterAuthLocked sends a connection that has just signed in, after
// its auth result (§4.5), one `status` for each of its user's mutes in
// effect, of rooms the user can see, and one `user` with the shown status
// of each user who shares a room with it. offline (a user without
// connections, or an invisible one) and "" are left out, so it tells
// neither who is invisible nor who opted out. A server may limit the
// statuses to the users it would list in `members` (§4.5), and this one
// does: in a room of more than Config.MaxListedMembers, only the members
// that the room lists (§4.2.1) count.
func (s *Server) sendAfterAuthLocked(c *client) {
	u := c.user
	if u == nil {
		return
	}
	now := time.Now()
	var frames []any
	if u.Mute.Active(now) {
		frames = append(frames, muteFrame("", u.Mute, now))
	}
	for _, id := range slices.Sorted(maps.Keys(u.RoomMutes)) {
		if mute := u.RoomMutes[id]; mute.Active(now) && s.visibleRoomLocked(u, id) != nil {
			frames = append(frames, muteFrame(id, mute, now))
		}
	}
	listed := make(map[string]*userState)
	for _, r := range u.joined {
		ids, _ := r.ListedMemberIDs(s.config.MaxListedMembers)
		for _, id := range ids {
			if id != u.id {
				listed[id] = r.Member(id)
			}
		}
	}
	var shown []*userState
	for _, id := range slices.Sorted(maps.Keys(listed)) {
		if other := listed[id]; other.Presence.Status != statusOffline && other.Presence.Status != statusNone {
			shown = append(shown, other)
		}
	}
	for _, other := range shown {
		frames = append(frames, statusFrame(other))
	}
	if len(frames) > 0 {
		c.enqueueBatch(frames...)
	}
}

// muteEnded ends u's timed mute of the room roomID, or everywhere when
// roomID is "", set until until, when its time passes (users.Timers),
// unless it changed since, telling the user's connections it is now false
// (§4.5). What counts toward u's unread changes with a room's mute in the
// room and its threads, so the counts are taken again and a badge push
// sent (§4.9).
func (s *Server) muteEnded(u *userState, roomID string, until time.Time) {
	s.mu.Lock()
	defer s.unlock()
	if s.closed || !s.users.EndMute(u, roomID, until) {
		return
	}
	u.send(muteFrame(roomID, muteState{}, time.Now()))
	if roomID != "" {
		s.unreadChangedLocked(u, "")
	}
}

// endRoomMuteLocked forgets u's mute of room id, which has run out, and
// tells the user's connections it is now false (§4.5). The caller takes
// the unread counts again.
func (s *Server) endRoomMuteLocked(u *userState, id string) {
	s.users.SetMute(u, id, muteState{})
	u.send(muteFrame(id, muteState{}, time.Now()))
}

// silenced reports whether u's notifications are silenced everywhere: by
// the unscoped mute, or by a dnd status (§4.5).
func (u *userState) silenced(now time.Time) bool {
	return u.Mute.Active(now) || u.Presence.Chosen == statusDND
}

// roomMuted reports whether u's own mute of room r, or of a room r is a
// thread of, is in effect: it silences r but for mentions of u (§4.5). The
// unscoped mute applies besides.
func (u *userState) roomMuted(r *roomState, now time.Time) bool {
	for room := r; room != nil; room = room.Parent() {
		if mute, ok := u.RoomMutes[room.ID()]; ok && mute.Active(now) {
			return true
		}
	}
	return false
}

// attended reports whether any connection of the user has not said it is
// idle (§4.5).
func (u *userState) attended() bool {
	for c := range u.clients {
		if !c.idle {
			return true
		}
	}
	return false
}

// shownStatus is the user's shown status, from the chosen status and the
// presence (§4.5.1).
func (u *userState) shownStatus() string {
	return u.Presence.Shown(u.attended(), len(u.clients) > 0)
}

// you is the user's own current object (§3.3): its profile, with the
// chosen status in place of the shown one, and never seen (§4.5.2).
func (u *userState) you() wire.CurrentUser {
	return u.current(u.Presence.Chosen, false)
}

// noteShownLocked notes u's shown status after a change to its chosen
// status or its connections, and when it has just become offline, the
// time it did. Others do not see it until it is published.
func (s *Server) noteShownLocked(u *userState) {
	u.Presence.Note(u.attended(), len(u.clients) > 0, time.Now())
}

// publishLocked notes u's shown status and makes it the one others see
// (§4.5.2, presence.State.Publish), reporting whether that changed; the
// caller sends it to those who share a room with u. The store keeps whether
// the published status is offline, and its seen, so a change to or out of
// offline is stored: after a crash, which closes no connection,
// restoreLocked gives the users who were shown otherwise a seen then.
func (s *Server) publishLocked(u *userState) bool {
	s.noteShownLocked(u)
	changed, stored := u.Presence.Publish(time.Now())
	if stored {
		s.users.Touch(u.id)
	}
	return changed
}

// shownObject is the bare current object of u that others see: user_id and
// the published shown status, "" too, with seen when it is offline, if it
// is known (§4.5.2).
func (u *userState) shownObject() wire.CurrentUser {
	status, seen := u.Presence.Status, u.Presence.Seen
	object := wire.CurrentUser{User: wire.User{UserID: u.id, Status: status}}
	switch {
	case status == statusNone:
		object.Cleared = []string{"status"}
	case status == statusOffline && seen > 0:
		object.Seen = strconv.FormatInt(seen, 10)
	}
	return object
}

// announceStatusLocked notes u's shown status (noteShownLocked), then
// publishes it and sends it to the connections of those who share a room
// with u, as `user` `new`, when it differs from the one last published: at
// once within the user's burst, else once statusCoalesce has passed, with
// whatever the status is then. Until then others see the one published
// before, and a status back to it by then is not sent, nor seen moved: a
// flap nobody saw is not one. The user's own connections are not told:
// their `you` carries the chosen status, which this does not change. A
// shutdown sends nothing, but still publishes, noting the seen of each
// user whose connections it closes.
func (s *Server) announceStatusLocked(u *userState) {
	s.noteShownLocked(u)
	if s.closed {
		s.publishLocked(u)
		return
	}
	if !s.users.PublishStatus(u) {
		return
	}
	before := u.views()
	s.publishLocked(u)
	s.publishUserLocked(u, nil, before)
}

// statusDue publishes u's shown status held back by announceStatusLocked
// (users.Timers), unless the server closed, or u is gone, since.
func (s *Server) statusDue(u *userState) {
	s.mu.Lock()
	defer s.unlock()
	if !s.users.ReleaseStatus(u) || s.closed {
		return
	}
	before := u.views()
	if s.publishLocked(u) {
		s.publishUserLocked(u, nil, before)
	}
}

// sendStatusLocked tells the connections of users u's published shown
// status, such as those of a room u just joined, who share it with u for
// the first time.
func (s *Server) sendStatusLocked(u *userState, users []*userState) {
	frame := statusFrame(u)
	for _, other := range users {
		other.send(frame)
	}
}

// statusFrame is the `user` notification of u's published shown status:
// `{new: {user_id, status}}`, with seen when it is offline (§4.5.2).
func statusFrame(u *userState) jsontext.Value {
	shown := u.shownObject()
	return notification("user", wire.UserUpdate{New: &shown})
}

// announceJoinStatusLocked tells the other members of r the published
// status of the user who just joined it, since its membership record
// carries only a recorded user object; a change still held back reaches
// them with the user's other sharers. Like the snapshot after auth, it
// leaves out offline and "", which would tell who is invisible or opted
// out.
func (s *Server) announceJoinStatusLocked(u *userState, r *roomState) {
	if status := u.Presence.Status; status == statusOffline || status == statusNone {
		return
	}
	users := make([]*userState, 0, r.MemberCount())
	for id, other := range r.Members() {
		if id != u.id {
			users = append(users, other)
		}
	}
	s.sendStatusLocked(u, users)
}
