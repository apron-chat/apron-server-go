package server

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"maps"
	"slices"
	"time"

	"golang.org/x/time/rate"
)

// Status (§4.11). A user sets a status with `me`: online (the default), ""
// for none, dnd, or invisible; the server stores "" for any other value.
// Others see it as:
//
//	online     online while a connection is attended, idle while connected
//	           with none attended, offline without a connection
//	""         ""
//	dnd        dnd while connected, offline without a connection
//	invisible  offline
//
// and the user sees, in `you`, the value they set. Separately, each
// connection reports whether it is idle, and the user mutes their
// notifications everywhere or in one room and its threads; mutes and a dnd
// status silence pushes (§4.7). Mutes are private: each change goes to every
// connection of the user as a `status` notification, and the mutes in
// effect to a connection after its auth.
const (
	statusOnline    = "online"
	statusIdle      = "idle"
	statusDND       = "dnd"
	statusInvisible = "invisible"
	statusOffline   = "offline"
	statusNone      = ""
	// maxMuteSeconds caps a timed mute at a year; `true` mutes until changed.
	maxMuteSeconds = 365 * 24 * 60 * 60
	// maxPendingStatus bounds the mutes a connection keeps until it signs
	// in; beyond it the oldest are dropped.
	maxPendingStatus = 16
	// A user's derived status changes go to others at once up to
	// statusBurst times, then at most once per statusCoalesce, the latest
	// status winning, so a flapping connection costs its rooms little
	// (§4.11 lets servers delay them).
	statusBurst    = 10
	statusCoalesce = 2 * time.Second
)

// optionalStatuses are the optional statuses the server accepts, which the
// server frame lists as server.status (§3.1, §4.11).
var optionalStatuses = []string{statusDND, statusInvisible}

// settableStatus reports whether the server supports value as a status a
// user sets (§4.11); it stores "" for any other.
func settableStatus(value string) bool {
	switch value {
	case statusOnline, statusNone:
		return true
	}
	return slices.Contains(optionalStatuses, value)
}

// muteState is a mute (§4.11): until a time, forever, or, when zero, none.
type muteState struct {
	forever bool
	until   time.Time
}

// active reports whether the mute is in effect at now.
func (m muteState) active(now time.Time) bool {
	return m.forever || now.Before(m.until)
}

// wire is the mute as a `status` notification carries it: true, the
// seconds left, rounded up, or false when there is none.
func (m muteState) wire(now time.Time) any {
	switch {
	case m.forever:
		return true
	case !m.active(now):
		return false
	}
	return int64((m.until.Sub(now) + time.Second - 1) / time.Second)
}

// statusUpdate is one parsed `status` notification. A nil field was absent.
type statusUpdate struct {
	// roomID, when scoped, is the room the mute is of.
	roomID string
	scoped bool
	idle   *bool
	// mute is present when hasMute: forever, or seconds, 0 ending it.
	hasMute     bool
	muteForever bool
	muteSeconds int64
}

// parseStatus reads a status notification's params (§4.11), ignoring each
// malformed field on its own. A room_id that is not a string ignores the
// whole update, whose mute was not meant for everywhere. room_id scopes
// only the mute: idle is about the connection either way.
func parseStatus(params map[string]jsontext.Value) (statusUpdate, bool) {
	var update statusUpdate
	if _, has := params["room_id"]; has {
		roomID, err := parseString(params, "room_id", true)
		if err != nil {
			return update, false
		}
		update.roomID, update.scoped = roomID, true
	}
	if _, has := params["idle"]; has {
		if value, err := parseBool(params, "idle", true); err == nil {
			update.idle = &value
		}
	}
	if raw, has := params["mute"]; has {
		var seconds int64
		switch raw := bytes.TrimSpace(raw); {
		case bytes.Equal(raw, []byte("true")):
			update.hasMute, update.muteForever = true, true
		case bytes.Equal(raw, []byte("false")):
			update.hasMute = true
		case bytes.Equal(raw, []byte("null")):
		case json.Unmarshal(raw, &seconds) == nil && seconds >= 0:
			update.hasMute, update.muteSeconds = true, min(seconds, maxMuteSeconds)
		}
	}
	return update, true
}

// mute returns the mute an update sets, starting at now.
func (update statusUpdate) mute(now time.Time) muteState {
	if update.muteForever {
		return muteState{forever: true}
	}
	if update.muteSeconds == 0 {
		return muteState{}
	}
	return muteState{until: now.Add(time.Duration(update.muteSeconds) * time.Second)}
}

// status applies a `status` notification (§4.11), which is never answered.
// It is accepted before authentication: idle applies to the connection at
// once, and a mute waits on the connection until it signs in.
func (s *Server) status(c *client, req request) {
	update, ok := parseStatus(req.params)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.unlock()
	if c.user == nil {
		if update.idle != nil {
			c.idle = *update.idle
		}
		if update.hasMute {
			update.idle = nil
			c.pendingStatus = append(c.pendingStatus, update)
			if len(c.pendingStatus) > maxPendingStatus {
				c.pendingStatus = c.pendingStatus[1:]
			}
		}
		return
	}
	s.applyStatusLocked(c, c.user, update, nil)
}

// applyStatusLocked applies an update for user u from connection c, then
// announces what changed. echoExcept, if any, is a connection not sent the
// mute: one signing in, which is sent the mutes in effect after its auth
// result (sendAfterAuthLocked).
func (s *Server) applyStatusLocked(c *client, u *userState, update statusUpdate, echoExcept *client) {
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
		u.mute = mute
		s.touchUser(u.id)
		s.scheduleMuteLocked(u)
		u.sendExcept(echoExcept, muteFrame("", mute, now))
		return
	}
	// A mute of a room the user cannot see changes nothing.
	r := s.visibleRoomLocked(u, update.roomID)
	if r == nil {
		return
	}
	if mute.active(now) {
		u.roomMutes[r.id] = mute
	} else {
		delete(u.roomMutes, r.id)
	}
	s.touchUser(u.id)
	s.scheduleRoomMuteLocked(u, r.id)
	u.sendExcept(echoExcept, muteFrame(r.id, mute, now))
	// What counts toward unread changes with a room's mute (unread.go).
	s.unreadChangedLocked(u, r.id)
}

// muteFrame is the `status` notification that tells the user's connections
// of a mute, everywhere or, with roomID, of that room (§4.11).
func muteFrame(roomID string, mute muteState, now time.Time) jsontext.Value {
	params := map[string]any{"mute": mute.wire(now)}
	if roomID != "" {
		params["room_id"] = roomID
	}
	return notification("status", params)
}

// sendExcept queues a frame to every connection of the user but except.
func (u *userState) sendExcept(except *client, frame jsontext.Value) {
	for c := range u.clients {
		if c != except {
			c.enqueue(frame)
		}
	}
}

// applyPendingStatusLocked applies the mutes c received before it signed in
// as u. Its other connections are told; c is sent the mutes in effect after
// its auth result.
func (s *Server) applyPendingStatusLocked(c *client, u *userState) {
	pending := c.pendingStatus
	c.pendingStatus = nil
	for _, update := range pending {
		s.applyStatusLocked(c, u, update, c)
	}
}

// sendAfterAuthLocked sends a connection that has just authenticated, after
// its auth result (§4.11), one `status` for each of its user's mutes in
// effect, and the status others see of each user who shares a room with
// it. offline (a user without connections, or an invisible one) and "" are
// left out, so it tells neither who is invisible nor who opted out.
func (s *Server) sendAfterAuthLocked(c *client) {
	u := c.user
	if u == nil {
		return
	}
	now := time.Now()
	var frames []any
	if u.mute.active(now) {
		frames = append(frames, muteFrame("", u.mute, now))
	}
	for _, id := range slices.Sorted(maps.Keys(u.roomMutes)) {
		if mute := u.roomMutes[id]; mute.active(now) && s.rooms[id] != nil {
			frames = append(frames, muteFrame(id, mute, now))
		}
	}
	for _, other := range s.sharersLocked(u) {
		if status := other.statusAt(now); status != statusOffline && status != statusNone {
			frames = append(frames, statusFrame(other, status))
		}
	}
	if len(frames) > 0 {
		c.enqueueBatch(frames...)
	}
}

// scheduleMuteLocked ends u's timed mute when its time passes, telling the
// user's connections it is now false (§4.11).
func (s *Server) scheduleMuteLocked(u *userState) {
	if u.muteTimer != nil {
		u.muteTimer.Stop()
		u.muteTimer = nil
	}
	if u.mute.forever || u.mute.until.IsZero() {
		return
	}
	until := u.mute.until
	u.muteTimer = time.AfterFunc(time.Until(until), func() {
		s.mu.Lock()
		defer s.unlock()
		if s.closed || s.users[u.id] != u || u.mute.forever || !u.mute.until.Equal(until) {
			return
		}
		u.mute = muteState{}
		u.muteTimer = nil
		s.touchUser(u.id)
		u.send(muteFrame("", u.mute, time.Now()))
	})
}

// scheduleRoomMuteLocked ends u's timed mute of room id when its time
// passes. Its end is sent like the unscoped mute's, and what counts toward
// u's unread changes with it in the room and its threads, so the counts are
// taken again and a badge push sent (§4.7).
func (s *Server) scheduleRoomMuteLocked(u *userState, id string) {
	if timer := u.roomMuteTimers[id]; timer != nil {
		timer.Stop()
		delete(u.roomMuteTimers, id)
	}
	mute, ok := u.roomMutes[id]
	if !ok || mute.forever || mute.until.IsZero() {
		return
	}
	if u.roomMuteTimers == nil {
		u.roomMuteTimers = make(map[string]*time.Timer)
	}
	until := mute.until
	u.roomMuteTimers[id] = time.AfterFunc(time.Until(until), func() {
		s.mu.Lock()
		defer s.unlock()
		if s.closed || s.users[u.id] != u {
			return
		}
		if mute, ok := u.roomMutes[id]; !ok || mute.forever || !mute.until.Equal(until) {
			return
		}
		s.endRoomMuteLocked(u, id)
		s.unreadChangedLocked(u, "")
	})
}

// endRoomMuteLocked forgets u's mute of room id, which has run out, and
// tells the user's connections it is now false (§4.11). The caller takes
// the unread counts again.
func (s *Server) endRoomMuteLocked(u *userState, id string) {
	if timer := u.roomMuteTimers[id]; timer != nil {
		timer.Stop()
		delete(u.roomMuteTimers, id)
	}
	delete(u.roomMutes, id)
	s.touchUser(u.id)
	u.send(muteFrame(id, muteState{}, time.Now()))
}

// silenced reports whether u's notifications are silenced everywhere: by
// the unscoped mute, or by a dnd status (§4.11).
func (u *userState) silenced(now time.Time) bool {
	return u.mute.active(now) || u.chosen == statusDND
}

// roomMuted reports whether u's own mute of room r, or of a room r is a
// thread of, is in effect: it silences r, mentions too (§4.11). The
// unscoped mute applies besides.
func (u *userState) roomMuted(r *roomState, now time.Time) bool {
	for room := r; room != nil; room = room.parent {
		if mute, ok := u.roomMutes[room.id]; ok && mute.active(now) {
			return true
		}
	}
	return false
}

// attended reports whether any connection of the user has not said it is
// idle (§4.11).
func (u *userState) attended() bool {
	for c := range u.clients {
		if !c.idle {
			return true
		}
	}
	return false
}

// statusAt is the user's status as others see it (§4.11).
func (u *userState) statusAt(time.Time) string {
	switch u.chosen {
	case statusOnline:
		switch {
		case u.attended():
			return statusOnline
		case len(u.clients) > 0:
			return statusIdle
		}
		return statusOffline
	case statusDND:
		// dnd shows only while connected, so it does not tell others
		// that the user is reachable when they are not (§4.11).
		if len(u.clients) > 0 {
			return statusDND
		}
		return statusOffline
	case statusInvisible:
		return statusOffline
	}
	return u.chosen
}

// you is the user's own current object (§3.3): its profile, with the
// status the user set in place of the one others see.
func (u *userState) you() map[string]any {
	profile := u.profile()
	profile["status"] = u.chosen
	return profile
}

// announceStatusLocked sends the connections of those who share a room with
// u its status, as `user` `new`, when it differs from the one last sent: at
// once within the user's burst, else once statusCoalesce has passed, with
// whatever the status is then. The user's own connections are not told:
// their `you` carries the status the user set, which this does not change.
func (s *Server) announceStatusLocked(u *userState) {
	status := u.statusAt(time.Now())
	if status == u.status || u.statusTimer != nil {
		return
	}
	if u.statusLimit == nil {
		u.statusLimit = rate.NewLimiter(rate.Every(statusCoalesce), statusBurst)
	}
	if !u.statusLimit.Allow() {
		u.statusTimer = time.AfterFunc(statusCoalesce, func() {
			s.mu.Lock()
			defer s.unlock()
			u.statusTimer = nil
			if s.closed || s.users[u.id] != u {
				return
			}
			if status := u.statusAt(time.Now()); status != u.status {
				u.status = status
				s.sendStatusLocked(u, status, s.sharersLocked(u))
			}
		})
		return
	}
	u.status = status
	s.sendStatusLocked(u, status, s.sharersLocked(u))
}

// sendStatusLocked tells the connections of users that u's status is
// status.
func (s *Server) sendStatusLocked(u *userState, status string, users []*userState) {
	frame := statusFrame(u, status)
	for _, other := range users {
		other.send(frame)
	}
}

func statusFrame(u *userState, status string) jsontext.Value {
	return notification("user", map[string]any{"new": map[string]any{"user_id": u.id, "status": status}})
}

// announceJoinStatusLocked tells the other members of r the status of the
// user who just joined it, since its membership record carries only a
// recorded user object. Like the snapshot after auth, it leaves out offline
// and "", which would tell who is invisible or opted out.
func (s *Server) announceJoinStatusLocked(u *userState, r *roomState) {
	status := u.statusAt(time.Now())
	if status == statusOffline || status == statusNone {
		return
	}
	users := make([]*userState, 0, len(r.members))
	for id, other := range r.members {
		if id != u.id {
			users = append(users, other)
		}
	}
	s.sendStatusLocked(u, status, users)
}
