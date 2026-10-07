package server

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"time"

	"golang.org/x/time/rate"
)

// Status (§4.5). A user sets a status with `me`: online (the default), ""
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
// status silence pushes (§4.9). Both are set with the `status` request.
// Mutes are private: each change goes to every connection of the user as a
// `status` notification, and the mutes in effect to a connection after a
// sign-in. A connection starts attended, and only its own `idle: true`
// makes it idle: a connection that never sends idle is never taken as idle.
const (
	statusOnline    = "online"
	statusIdle      = "idle"
	statusDND       = "dnd"
	statusInvisible = "invisible"
	statusOffline   = "offline"
	statusNone      = ""
	// maxMuteSeconds caps a timed mute at a year; `true` mutes until changed.
	maxMuteSeconds = 365 * 24 * 60 * 60
	// maxRoomMutes bounds the rooms a user mutes at once; muting another
	// is denied until one is unmuted.
	maxRoomMutes = 1000
	// Each user may send statusRequestBurst `status` requests at once,
	// refilled one every statusRequestRefill; beyond them a request is
	// retry_after (§4.5).
	statusRequestBurst  = 20
	statusRequestRefill = time.Second
	// A user's derived status changes go to others at once up to
	// statusBurst times, then at most once per statusCoalesce, the latest
	// status winning, so a flapping connection costs its rooms little
	// (§4.5 lets servers delay them).
	statusBurst    = 10
	statusCoalesce = 2 * time.Second
)

// optionalStatuses are the optional statuses the server accepts, which the
// server frame lists as server.status (§3.1, §4.5).
var optionalStatuses = []string{statusDND, statusInvisible}

// settableStatus reports whether the server supports value as a status a
// user sets (§4.5); it stores "" for any other.
func settableStatus(value string) bool {
	switch value {
	case statusOnline, statusNone:
		return true
	}
	return slices.Contains(optionalStatuses, value)
}

// muteState is a mute (§4.5): until a time, forever, or, when zero, none.
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

// parseStatus reads a status request's params (§4.5). Any field of the
// wrong type is invalid_params, and then nothing changes: room_id a string,
// idle a boolean, and mute true, false, or a positive whole number of
// seconds, a longer one shortened to maxMuteSeconds. room_id scopes only
// the mute, so room_id without mute is invalid_params: idle is about the
// connection either way.
func parseStatus(params map[string]jsontext.Value) (statusUpdate, *rpcError) {
	var update statusUpdate
	if _, has := params["room_id"]; has {
		roomID, err := parseString(params, "room_id", true)
		if err != nil {
			return update, err
		}
		update.roomID, update.scoped = roomID, true
	}
	if _, has := params["idle"]; has {
		value, err := parseBool(params, "idle", true)
		if err != nil {
			return update, err
		}
		update.idle = &value
	}
	if raw, has := params["mute"]; has {
		var seconds int64
		switch raw := bytes.TrimSpace(raw); {
		case bytes.Equal(raw, []byte("true")):
			update.hasMute, update.muteForever = true, true
		case bytes.Equal(raw, []byte("false")):
			update.hasMute = true
		case !bytes.Equal(raw, []byte("null")) && json.Unmarshal(raw, &seconds) == nil && seconds > 0:
			update.hasMute, update.muteSeconds = true, min(seconds, maxMuteSeconds)
		default:
			return update, invalidParams("mute must be true, false, or a positive whole number of seconds")
		}
	}
	if update.scoped && !update.hasMute {
		return update, invalidParams("room_id scopes a mute; send it with mute")
	}
	return update, nil
}

// mute returns the mute an update sets, starting at now.
func (update statusUpdate) mute(now time.Time) muteState {
	if update.muteForever {
		return muteState{forever: true}
	}
	if update.muteSeconds == 0 {
		// mute: false
		return muteState{}
	}
	return muteState{until: now.Add(time.Duration(update.muteSeconds) * time.Second)}
}

// status applies a `status` request (§4.5) and answers {} once it has: the
// mute's echo to the user's connections, the sender's included, precedes
// the result. Like other requests it needs a signed-in connection, and a
// `status` without an id is a notification no client sends, which
// processFrame ignores. On an error nothing changes: invalid params, a mute
// of a room the user cannot see, a mute of another room beyond
// maxRoomMutes, or more requests than the user's limit (admitStatusLocked).
func (s *Server) status(c *client, req request) (any, bool, *rpcError) {
	update, err := parseStatus(req.params)
	if err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.unlock()
	u := c.user
	if update.hasMute && update.scoped && s.visibleRoomLocked(u, update.roomID) == nil {
		return nil, false, invalidParams("Unknown room %q", update.roomID)
	}
	if now := time.Now(); update.hasMute && update.scoped && update.mute(now).active(now) {
		if _, muted := u.roomMutes[update.roomID]; !muted && len(u.roomMutes) >= maxRoomMutes {
			return nil, false, &rpcError{Code: codeDenied, Message: fmt.Sprintf("At most %d rooms can be muted; unmute one first", maxRoomMutes)}
		}
	}
	if err := admitStatusLocked(u); err != nil {
		return nil, false, err
	}
	s.applyStatusLocked(c, u, update)
	return map[string]any{}, false, nil
}

// admitStatusLocked applies the user's limit on `status` requests, idle and
// mute alike, and on `me` requests that change the status (§4.5 lets
// servers limit them): a burst of statusRequestBurst,
// refilled one every statusRequestRefill, across all of the user's
// connections. Beyond it the request is retry_after and changes nothing, so
// a client flipping idle or its mutes in a loop cannot make the server
// announce, store, and recount for it without end.
func admitStatusLocked(u *userState) *rpcError {
	if u.statusRequests == nil {
		u.statusRequests = rate.NewLimiter(rate.Every(statusRequestRefill), statusRequestBurst)
	}
	now := time.Now()
	reservation := u.statusRequests.ReserveN(now, 1)
	if delay := reservation.DelayFrom(now); delay > 0 {
		reservation.CancelAt(now)
		return retryAfter("Too many status changes; slow down", delay)
	}
	return nil
}

// applyStatusLocked applies an update for user u from connection c, then
// announces what changed. A scoped mute's room is visible to u.
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
		u.mute = mute
		s.touchUser(u.id)
		s.scheduleMuteLocked(u)
		u.send(muteFrame("", mute, now))
		return
	}
	r := s.visibleRoomLocked(u, update.roomID)
	if mute.active(now) {
		u.roomMutes[r.id] = mute
	} else {
		delete(u.roomMutes, r.id)
	}
	s.touchUser(u.id)
	s.scheduleRoomMuteLocked(u, r.id)
	u.send(muteFrame(r.id, mute, now))
	// What counts toward unread changes with a room's mute, in the room and
	// its threads, at any depth (unread.go), so every count is taken again,
	// as when the mute runs out.
	s.unreadChangedLocked(u, "")
}

// muteFrame is the `status` notification that tells the user's connections
// of a mute, everywhere or, with roomID, of that room (§4.5).
func muteFrame(roomID string, mute muteState, now time.Time) jsontext.Value {
	params := map[string]any{"mute": mute.wire(now)}
	if roomID != "" {
		params["room_id"] = roomID
	}
	return notification("status", params)
}

// sendAfterAuthLocked sends a connection that has just signed in, after
// its auth result (§4.5), one `status` for each of its user's mutes in
// effect, of rooms the user can see, and one `user` with the status others
// see of each user who shares a room with it. offline (a user without
// connections, or an invisible one) and "" are left out, so it tells
// neither who is invisible nor who opted out. A server may limit the
// statuses to the users it would list in `members` (§4.5), and this one
// does: in a room of more than Config.MaxListedMembers, only the members
// that the room lists (§4.3.1) count.
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
		if mute := u.roomMutes[id]; mute.active(now) && s.visibleRoomLocked(u, id) != nil {
			frames = append(frames, muteFrame(id, mute, now))
		}
	}
	listed := make(map[string]*userState)
	for _, r := range u.joined {
		ids, _ := s.listedMemberIDsLocked(r)
		for _, id := range ids {
			if id != u.id {
				listed[id] = r.members[id]
			}
		}
	}
	var shown []*userState
	for _, id := range slices.Sorted(maps.Keys(listed)) {
		if other := listed[id]; other.shownStatus() != statusOffline && other.shownStatus() != statusNone {
			shown = append(shown, other)
		}
	}
	for _, other := range shown {
		frames = append(frames, statusFrame(other, other.shownStatus()))
	}
	if len(frames) > 0 {
		c.enqueueBatch(frames...)
	}
}

// scheduleMuteLocked ends u's timed mute when its time passes, telling the
// user's connections it is now false (§4.5).
func (s *Server) scheduleMuteLocked(u *userState) {
	if u.muteTimer != nil {
		u.muteTimer.Stop()
		u.muteTimer = nil
	}
	if s.closed || u.mute.forever || u.mute.until.IsZero() {
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
// taken again and a badge push sent (§4.9).
func (s *Server) scheduleRoomMuteLocked(u *userState, id string) {
	if timer := u.roomMuteTimers[id]; timer != nil {
		timer.Stop()
		delete(u.roomMuteTimers, id)
	}
	mute, ok := u.roomMutes[id]
	if s.closed || !ok || mute.forever || mute.until.IsZero() {
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
// tells the user's connections it is now false (§4.5). The caller takes
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
// the unscoped mute, or by a dnd status (§4.5).
func (u *userState) silenced(now time.Time) bool {
	return u.mute.active(now) || u.chosen == statusDND
}

// roomMuted reports whether u's own mute of room r, or of a room r is a
// thread of, is in effect: it silences r, mentions too (§4.5). The
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
// idle (§4.5).
func (u *userState) attended() bool {
	for c := range u.clients {
		if !c.idle {
			return true
		}
	}
	return false
}

// shownStatus is the user's status as others see it (§4.5).
func (u *userState) shownStatus() string {
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
		// that the user is reachable when they are not (§4.5).
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
	status := u.shownStatus()
	if s.closed || status == u.status || u.statusTimer != nil {
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
			if status := u.shownStatus(); status != u.status {
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
	status := u.shownStatus()
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
