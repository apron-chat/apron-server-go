package server

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"maps"
	"time"
)

// Status (§4.11): each connection reports whether anyone is attending it,
// and each user whether to appear offline and when to stay quiet. The server
// derives the user's status from them and its push registrations (§4.7):
//
//	online   a connection is attended
//	dnd      none is, and the user is muted everywhere
//	idle     none is, but an idle connection or a live push registration
//	         can notify the user
//	offline  none of these, or the user is invisible
const (
	statusOnline  = "online"
	statusIdle    = "idle"
	statusDND     = "dnd"
	statusOffline = "offline"
	// maxMuteSeconds caps a timed mute at a year; `true` mutes until changed.
	maxMuteSeconds = 365 * 24 * 60 * 60
	// maxPendingStatus bounds the status updates a connection keeps until it
	// signs in; beyond it the oldest are dropped.
	maxPendingStatus = 16
)

// muteState is a mute (§4.11): until a time, forever, or, when zero, none.
type muteState struct {
	forever bool
	until   time.Time
}

// active reports whether the mute is in effect at now.
func (m muteState) active(now time.Time) bool {
	return m.forever || now.Before(m.until)
}

// wire is the mute as `you` and room records echo it: true, or the seconds
// left, rounded up.
func (m muteState) wire(now time.Time) any {
	if m.forever {
		return true
	}
	return int64((m.until.Sub(now) + time.Second - 1) / time.Second)
}

// statusUpdate is one parsed `status` notification. A nil field was absent.
type statusUpdate struct {
	roomID    string
	scoped    bool
	idle      *bool
	invisible *bool
	// mute is present when hasMute: forever, or seconds, 0 ending it.
	hasMute     bool
	muteForever bool
	muteSeconds int64
}

// parseStatus reads a status notification's params (§4.11). A malformed
// field rejects the whole update.
func parseStatus(params map[string]jsontext.Value) (statusUpdate, *rpcError) {
	var update statusUpdate
	if _, has := params["room_id"]; has {
		roomID, err := parseString(params, "room_id", true)
		if err != nil {
			return update, err
		}
		update.roomID, update.scoped = roomID, true
	}
	for name, field := range map[string]**bool{"idle": &update.idle, "invisible": &update.invisible} {
		if _, has := params[name]; has {
			value, err := parseBool(params, name, true)
			if err != nil {
				return update, err
			}
			*field = &value
		}
	}
	if raw, has := params["mute"]; has {
		update.hasMute = true
		var seconds int64
		switch {
		case bytes.Equal(bytes.TrimSpace(raw), []byte("true")):
			update.muteForever = true
		case json.Unmarshal(raw, &seconds) == nil && seconds >= 0:
			update.muteSeconds = min(seconds, maxMuteSeconds)
		default:
			return update, invalidParams("mute must be true or a non-negative whole number of seconds")
		}
	}
	return update, nil
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

// status applies a `status` notification (§4.11). It is accepted before
// authentication: the connection's own fields apply at once, and the user's
// wait on the connection until it signs in. A malformed update, or one
// scoped to a room the user cannot see, changes nothing; as a request, with
// an id, it is answered with invalid_params, and otherwise with {}.
func (s *Server) status(c *client, req request) {
	update, err := parseStatus(req.params)
	s.mu.Lock()
	if err == nil {
		c.statusAware = true
		if c.user == nil {
			s.applyConnectionStatusLocked(c, update)
			c.pendingStatus = append(c.pendingStatus, update)
			if len(c.pendingStatus) > maxPendingStatus {
				c.pendingStatus = c.pendingStatus[1:]
			}
		} else if update.scoped && s.visibleRoomLocked(c.user, update.roomID) == nil {
			err = invalidParams("Unknown room %q", update.roomID)
		} else {
			s.applyStatusLocked(c, c.user, update, nil)
		}
	}
	if req.hasID {
		if err != nil {
			c.sendError(req, err)
		} else {
			c.sendResult(req, map[string]any{})
		}
	}
	s.unlock()
}

// applyConnectionStatusLocked applies an update's idle to the connection:
// unscoped, whether anyone attends it; scoped, whether it attends that room,
// which `idle: false` also makes it attended.
func (s *Server) applyConnectionStatusLocked(c *client, update statusUpdate) {
	if update.idle == nil {
		return
	}
	switch {
	case !update.scoped:
		c.idle = *update.idle
	case !*update.idle:
		c.idle, c.room, c.roomScoped = false, update.roomID, true
	case c.room == update.roomID:
		c.room, c.roomScoped = "", true
	}
}

// applyStatusLocked applies an update for user u from connection c, then
// announces what changed. echoExcept, if any, is a connection that is not
// told: one signing in, whose auth result carries `you`.
func (s *Server) applyStatusLocked(c *client, u *userState, update statusUpdate, echoExcept *client) {
	now := time.Now()
	s.applyConnectionStatusLocked(c, update)
	muteChanged := false
	switch {
	case update.scoped && update.hasMute:
		r := s.visibleRoomLocked(u, update.roomID)
		if r == nil {
			break
		}
		mute := update.mute(now)
		if mute.active(now) {
			u.roomMutes[r.id] = mute
		} else {
			delete(u.roomMutes, r.id)
		}
		s.touchUser(u.id)
		// The caller's own room mute is a field of the room records they
		// receive (§3.4), so a change re-sends the room to their connections.
		record := s.roomParamsLocked(r)
		record["mute"] = 0
		if mute.active(now) {
			record["mute"] = mute.wire(now)
		}
		frame := roomUpdate("updated", record)
		for other := range u.clients {
			if other != echoExcept && other.statusAware {
				other.enqueue(frame)
			}
		}
	case update.hasMute:
		u.mute = update.mute(now)
		s.touchUser(u.id)
		s.scheduleMuteLocked(u)
		muteChanged = true
	}
	// invisible is about the user; a room has no invisibility.
	if update.invisible != nil && !update.scoped && *update.invisible != u.invisible {
		u.invisible = *update.invisible
		s.touchUser(u.id)
	}
	s.statusChangedLocked(u, echoExcept, muteChanged)
	if update.hasMute {
		// A mute changes which pushes the user gets, and so its unread count.
		s.badgeLocked(u, now)
	}
}

// applyPendingStatusLocked applies the status updates c received before it
// signed in as u, without telling c, whose auth result carries `you`.
func (s *Server) applyPendingStatusLocked(c *client, u *userState) {
	pending := c.pendingStatus
	c.pendingStatus = nil
	for _, update := range pending {
		if update.scoped && s.visibleRoomLocked(u, update.roomID) == nil {
			continue
		}
		// The connection's own fields were applied when they arrived.
		update.idle = nil
		s.applyStatusLocked(c, u, update, c)
	}
}

// scheduleMuteLocked ends u's timed mute when its time passes, announcing the
// change as if the user had ended it.
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
		s.statusChangedLocked(u, nil, true)
	})
}

// attended reports whether anyone attends the connection: it has not said it
// is idle, and it has not been silent past Config's silence (§4.11).
func (c *client) attended() bool {
	return !c.idle && !c.silent.Load()
}

// attends reports whether the connection attends room r: it is attended, and
// it has not said it attends another room.
func (c *client) attends(r *roomState) bool {
	return c.attended() && (!c.roomScoped || c.room == r.id)
}

// attended reports whether any connection of the user is attended (§4.11).
func (u *userState) attended() bool {
	for c := range u.clients {
		if c.attended() {
			return true
		}
	}
	return false
}

// statusAt derives the user's status as others see it (§4.11).
func (u *userState) statusAt(now time.Time) string {
	switch {
	case u.invisible:
		return statusOffline
	case u.attended():
		return statusOnline
	case u.mute.active(now):
		return statusDND
	case len(u.clients) > 0 || u.livePush(now):
		return statusIdle
	}
	return statusOffline
}

// roomMuted reports whether u has muted room r, by its own mute or, without
// one, the user's (§4.11).
func (u *userState) roomMuted(roomID string, now time.Time) bool {
	if mute, ok := u.roomMutes[roomID]; ok && mute.active(now) {
		return true
	}
	return u.mute.active(now)
}

// withRoomMute adds u's own mute of room r to a room record sent to u
// (§3.4), when there is one.
func (u *userState) withRoomMute(record map[string]any, r *roomState, now time.Time) map[string]any {
	if mute, ok := u.roomMutes[r.id]; ok && mute.active(now) {
		record["mute"] = mute.wire(now)
	}
	return record
}

// you is the user's own current object (§3.3): its profile and the
// remaining mute (§4.11), which only the user sees.
func (u *userState) you() map[string]any {
	profile := u.profile()
	if now := time.Now(); u.mute.active(now) {
		profile["mute"] = u.mute.wire(now)
	}
	return profile
}

// statusChangedLocked announces u's status when it changed since it was
// last announced: as `user` `new` to the status-aware connections of those
// who share a room with u, and as `you` to u's own, except echoExcept.
// muteChanged also sends `you`, with the remaining mute or 0, to u's own.
// Only connections that have sent `status` are told of changes, which is
// how a client shows it implements §4.11; every current user object carries
// the status regardless.
func (s *Server) statusChangedLocked(u *userState, echoExcept *client, muteChanged bool) {
	now := time.Now()
	status := u.statusAt(now)
	changed := status != u.status
	if !changed && !muteChanged {
		return
	}
	u.status = status
	if changed {
		s.sendStatusLocked(u, status, s.sharersLocked(u))
	}
	you := map[string]any{"user_id": u.id, "status": status}
	if muteChanged {
		you["mute"] = 0
		if u.mute.active(now) {
			you["mute"] = u.mute.wire(now)
		}
	}
	frame := notification("user", map[string]any{"you": you})
	for c := range u.clients {
		if c != echoExcept && c.statusAware {
			c.enqueue(frame)
		}
	}
}

// sendStatusLocked tells the status-aware connections of users that u's
// status is status.
func (s *Server) sendStatusLocked(u *userState, status string, users []*userState) {
	frame := notification("user", map[string]any{"new": map[string]any{"user_id": u.id, "status": status}})
	for _, other := range users {
		for c := range other.clients {
			if c.statusAware {
				c.enqueue(frame)
			}
		}
	}
}

// announceJoinStatusLocked tells the other members of r that the user who
// just joined it is not offline, since its membership record carries only a
// recorded user object.
func (s *Server) announceJoinStatusLocked(u *userState, r *roomState) {
	if u.status == "" || u.status == statusOffline {
		return
	}
	others := maps.Clone(r.members)
	delete(others, u.id)
	users := make([]*userState, 0, len(others))
	for _, other := range others {
		users = append(users, other)
	}
	s.sendStatusLocked(u, u.status, users)
}

// silenceIdle marks a connection idle once it has sent no frame for the
// silence that closes a client that pings (§1), and announces the change;
// it is attended again at its next frame (handleWebSocket).
func (s *Server) silenceIdle(c *client) {
	if !c.silent.CompareAndSwap(false, true) {
		return
	}
	s.mu.Lock()
	if u := c.user; u != nil {
		s.statusChangedLocked(u, nil, false)
	}
	s.unlock()
}

// endSilence makes a silent connection attended again after a frame.
func (s *Server) endSilence(c *client) {
	if !c.silent.CompareAndSwap(true, false) {
		return
	}
	s.mu.Lock()
	if u := c.user; u != nil {
		s.statusChangedLocked(u, nil, false)
	}
	s.unlock()
}
