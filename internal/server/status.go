package server

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"time"

	"golang.org/x/time/rate"
)

// Status (§4.11): each connection reports whether anyone is attending it,
// and each user whether to appear offline and when to stay quiet. The server
// derives the user's status from them and its push registrations (§4.7), in
// this order:
//
//	offline  the user is invisible (to others only)
//	dnd      the user's unscoped mute is set, attended or not
//	online   a connection is attended
//	idle     none is, but an idle connection or a live push registration
//	         can notify the user
//	offline  none of these
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
	// A user's status changes go to others at once up to statusBurst times,
	// then at most once per statusCoalesce, the latest status winning, so a
	// flapping connection costs its rooms little.
	statusBurst    = 10
	statusCoalesce = 2 * time.Second
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

// wire is the mute as `you` and room records echo it: true, the seconds
// left, rounded up, or 0 when there is none.
func (m muteState) wire(now time.Time) any {
	switch {
	case m.forever:
		return true
	case !m.active(now):
		return 0
	}
	return int64((m.until.Sub(now) + time.Second - 1) / time.Second)
}

// statusUpdate is one parsed `status` notification. A nil field was absent.
type statusUpdate struct {
	roomID string
	scoped bool
	// idle and invisible are unscoped only; a scoped update ignores them.
	idle      *bool
	invisible *bool
	// mute is present when hasMute: forever, or seconds, 0 ending it.
	hasMute     bool
	muteForever bool
	muteSeconds int64
}

// parseStatus reads a status notification's params (§4.11), ignoring each
// malformed field on its own. A room_id that is not a string ignores the
// whole update, whose fields were not meant for everywhere. With room_id,
// only mute applies: idle and invisible are about the connection and the
// user, and are ignored.
func parseStatus(params map[string]jsontext.Value) (statusUpdate, bool) {
	var update statusUpdate
	if _, has := params["room_id"]; has {
		roomID, err := parseString(params, "room_id", true)
		if err != nil {
			return update, false
		}
		update.roomID, update.scoped = roomID, true
	}
	for name, field := range map[string]**bool{"idle": &update.idle, "invisible": &update.invisible} {
		if _, has := params[name]; has && !update.scoped {
			if value, err := parseBool(params, name, true); err == nil {
				*field = &value
			}
		}
	}
	if raw, has := params["mute"]; has {
		var seconds int64
		switch {
		case bytes.Equal(bytes.TrimSpace(raw), []byte("true")):
			update.hasMute, update.muteForever = true, true
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
// once, and the user's fields wait on the connection until it signs in. An
// update scoped to a room the user cannot see changes nothing.
func (s *Server) status(c *client, req request) {
	update, ok := parseStatus(req.params)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.unlock()
	c.statusAware.Store(true)
	if c.user == nil {
		if update.idle != nil {
			c.idle = *update.idle
		}
		c.pendingStatus = append(c.pendingStatus, update)
		if len(c.pendingStatus) > maxPendingStatus {
			c.pendingStatus = c.pendingStatus[1:]
		}
		return
	}
	if update.scoped && s.visibleRoomLocked(c.user, update.roomID) == nil {
		return
	}
	s.applyStatusLocked(c, c.user, update, nil)
}

// applyStatusLocked applies an update for user u from connection c, then
// announces what changed. echoExcept, if any, is a connection that is not
// told: one signing in, whose auth result carries `you`.
func (s *Server) applyStatusLocked(c *client, u *userState, update statusUpdate, echoExcept *client) {
	now := time.Now()
	if update.idle != nil {
		c.idle = *update.idle
	}
	youChanged := false
	switch {
	case update.scoped && update.hasMute:
		r := s.visibleRoomLocked(u, update.roomID)
		if r == nil {
			break
		}
		// 0 removes the room's own mute; the unscoped one applies as ever
		// (§4.11).
		mute := update.mute(now)
		if mute.active(now) {
			u.roomMutes[r.id] = mute
		} else {
			delete(u.roomMutes, r.id)
		}
		s.touchUser(u.id)
		// The user's own room mute is a delivery field of the room records
		// they receive (§3.4), so a change re-sends a joined room to every
		// connection of theirs. A room they have not joined keeps the mute
		// for when they join.
		if u.joined[r.id] != nil {
			record := s.roomParamsLocked(r)
			record["mute"] = mute.wire(now)
			frame := roomUpdate("updated", record)
			for other := range u.clients {
				if other != echoExcept {
					other.enqueue(frame)
				}
			}
		}
		s.unreadChangedLocked(u, r.id)
	case update.hasMute:
		u.mute = update.mute(now)
		s.touchUser(u.id)
		s.scheduleMuteLocked(u)
		youChanged = true
	}
	if update.invisible != nil && *update.invisible != u.invisible {
		u.invisible = *update.invisible
		s.touchUser(u.id)
		youChanged = true
	}
	s.statusChangedLocked(u, echoExcept, youChanged)
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
		// The connection's idle was applied when it arrived.
		update.idle = nil
		s.applyStatusLocked(c, u, update, c)
	}
}

// scheduleMuteLocked ends u's timed mute when its time passes. Clients count
// the seconds down themselves, so the mute is not echoed, but the status
// that changes with it is announced.
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
		s.statusChangedLocked(u, nil, false)
	})
}

// attended reports whether anyone attends the connection: it has not said
// it is idle, which only idle: false ends, and it has not been silent past
// Config.SilentIdleAfter (§4.11).
func (c *client) attended() bool {
	return !c.idle && !c.silent.Load()
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

// ownStatusAt derives the user's status as the user sees it, which
// invisible does not change (§4.11).
func (u *userState) ownStatusAt(now time.Time) string {
	switch {
	case u.mute.active(now):
		return statusDND
	case u.attended():
		return statusOnline
	case len(u.clients) > 0 || u.notifiable(now):
		return statusIdle
	}
	return statusOffline
}

// statusAt derives the user's status as others see it (§4.11).
func (u *userState) statusAt(now time.Time) string {
	if u.invisible {
		return statusOffline
	}
	return u.ownStatusAt(now)
}

// roomMuted reports whether u's own mute of room r, or of a room r is a
// thread of, is in effect: it silences r except mentions (§4.11). The
// unscoped mute, which silences everything, applies besides.
func (u *userState) roomMuted(r *roomState, now time.Time) bool {
	for room := r; room != nil; room = room.parent {
		if mute, ok := u.roomMutes[room.id]; ok && mute.active(now) {
			return true
		}
	}
	return false
}

// withRoomMute adds u's own mute of room r to a record of a room u has
// joined, sent to u (§3.4), when there is one; absent, it is 0.
func (u *userState) withRoomMute(record map[string]any, r *roomState, now time.Time) map[string]any {
	if mute, ok := u.roomMutes[r.id]; ok && mute.active(now) && u.joined[r.id] != nil {
		record["mute"] = mute.wire(now)
	}
	return record
}

// you is the user's own current object (§3.3): its profile, with the
// status the user sees, and the mute and invisibility only the user sees.
func (u *userState) you() map[string]any {
	now := time.Now()
	profile := u.profile()
	profile["status"] = u.ownStatusAt(now)
	if u.mute.active(now) {
		profile["mute"] = u.mute.wire(now)
	}
	if u.invisible {
		profile["invisible"] = true
	}
	return profile
}

// statusChangedLocked announces u's status after something it depends on
// changed: to others (announceStatusLocked), and to u's own connections,
// except echoExcept, as `you` with the user's own status when it changed.
// Only connections that have sent `status` are told of status changes,
// which is how a client shows it implements §4.11; every current user
// object carries the status regardless. youChanged, the user having changed
// mute or invisible, echoes both, with the status, to every connection of
// the user.
func (s *Server) statusChangedLocked(u *userState, echoExcept *client, youChanged bool) {
	now := time.Now()
	s.announceStatusLocked(u)
	own := u.ownStatusAt(now)
	if own == u.ownStatus && !youChanged {
		return
	}
	u.ownStatus = own
	you := map[string]any{"user_id": u.id, "status": own}
	if youChanged {
		you["mute"], you["invisible"] = u.mute.wire(now), u.invisible
	}
	frame := notification("user", map[string]any{"you": you})
	for c := range u.clients {
		if c != echoExcept && (youChanged || c.statusAware.Load()) {
			c.enqueue(frame)
		}
	}
}

// announceStatusLocked sends the status-aware connections of those who
// share a room with u its status, as `user` `new`, when it differs from the
// one last sent: at once within the user's burst, else once statusCoalesce
// has passed, with whatever the status is then.
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

// sendStatusLocked tells the status-aware connections of users that u's
// status is status.
func (s *Server) sendStatusLocked(u *userState, status string, users []*userState) {
	frame := notification("user", map[string]any{"new": map[string]any{"user_id": u.id, "status": status}})
	for _, other := range users {
		for c := range other.clients {
			if c.statusAware.Load() {
				c.enqueue(frame)
			}
		}
	}
}

// announceJoinStatusLocked tells the other members of r that the user who
// just joined it is not offline, since its membership record carries only a
// recorded user object.
func (s *Server) announceJoinStatusLocked(u *userState, r *roomState) {
	status := u.statusAt(time.Now())
	if status == statusOffline {
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

// silenceIdle marks idle a connection that never sent status and has sent
// no frame but liveness pings for Config.SilentIdleAfter (§4.11), and
// announces the change; its next other frame ends it (endSilence).
func (s *Server) silenceIdle(c *client) {
	if c.statusAware.Load() || !c.silent.CompareAndSwap(false, true) {
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
