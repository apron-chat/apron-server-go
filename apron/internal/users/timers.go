package users

import (
	"time"

	"github.com/apron-chat/apron-server-go/apron/internal/presence"
)

// A Table keeps two kinds of timers for its users: the end of each timed
// mute (§4.5), and the publication of a shown status held back to
// coalesce a flapping user's changes (presence.Coalesce). When one fires,
// the Table calls the server back (Timers) from the timer's goroutine,
// without the server's lock; the server takes it, and asks the Table
// whether the timer is still current (EndMute, ReleaseStatus) before
// acting on it. Deleting a user cancels its timers, and Stop every one.

// Timers are what a Table calls when a user's timer fires.
type Timers[U Holder] struct {
	// MuteEnded is called when u's timed mute of roomID, "" for its mute
	// everywhere, set until until, runs out.
	MuteEnded func(u U, roomID string, until time.Time)
	// StatusDue is called when u's held shown status may be published.
	StatusDue func(u U)
}

// timers are a Table's timers by user_id: mutes by room_id, "" for the
// mute everywhere, and held each user's held status.
type timers[U Holder] struct {
	Timers[U]
	mutes   map[string]map[string]*time.Timer
	held    map[string]*time.Timer
	stopped bool
}

// SetMute sets u's mute of roomID, "" for its mute everywhere, to m; a
// zero m unmutes a room. It cancels the end of the mute it replaces, and
// schedules the end of a timed one (Timers.MuteEnded).
func (t *Table[U]) SetMute(u U, roomID string, m Mute) {
	kept := u.Kept()
	switch {
	case roomID == "":
		kept.Mute = m
	case m == Mute{}:
		delete(kept.RoomMutes, roomID)
	default:
		kept.RoomMutes[roomID] = m
	}
	t.Touch(u.UserID())
	t.scheduleMute(u, roomID, m)
}

// ScheduleMutes schedules the end of each of u's timed mutes, as Restore
// left them.
func (t *Table[U]) ScheduleMutes(u U) {
	kept := u.Kept()
	t.scheduleMute(u, "", kept.Mute)
	for roomID, m := range kept.RoomMutes {
		t.scheduleMute(u, roomID, m)
	}
}

// scheduleMute schedules the end of u's mute m of roomID, canceling the
// one scheduled before.
func (t *Table[U]) scheduleMute(u U, roomID string, m Mute) {
	id := u.UserID()
	mutes := t.timers.mutes[id]
	if timer := mutes[roomID]; timer != nil {
		timer.Stop()
		delete(mutes, roomID)
	}
	if t.timers.stopped || m.Forever || m.Until.IsZero() {
		return
	}
	if mutes == nil {
		mutes = make(map[string]*time.Timer)
		t.timers.mutes[id] = mutes
	}
	until := m.Until
	mutes[roomID] = time.AfterFunc(time.Until(until), func() { t.timers.MuteEnded(u, roomID, until) })
}

// EndMute ends u's mute of roomID, "" for its mute everywhere, if u is
// still the table's user and the mute is still the one set until until,
// and reports whether it did. The server calls it holding its lock when
// Timers.MuteEnded is called.
func (t *Table[U]) EndMute(u U, roomID string, until time.Time) bool {
	if t.byID[u.UserID()] != u {
		return false
	}
	kept := u.Kept()
	m, ok := kept.Mute, true
	if roomID != "" {
		m, ok = kept.RoomMutes[roomID]
	}
	if !ok || m.Forever || !m.Until.Equal(until) {
		return false
	}
	t.SetMute(u, roomID, Mute{})
	return true
}

// PublishStatus reports whether u's shown status as noted is to be
// published at once: when it is not the one published, none is held, and
// the change is within the user's burst (presence.State.PublishNow).
// Beyond the burst it holds the change for presence.Coalesce, then calls
// Timers.StatusDue.
func (t *Table[U]) PublishStatus(u U) bool {
	id, status := u.UserID(), &u.Kept().Presence
	if !status.Pending() || t.timers.held[id] != nil {
		return false
	}
	if status.PublishNow() {
		return true
	}
	if !t.timers.stopped {
		t.timers.held[id] = time.AfterFunc(presence.Coalesce, func() { t.timers.StatusDue(u) })
	}
	return false
}

// ReleaseStatus forgets u's held status, and reports whether u is still
// the table's user and had one held. The server calls it holding its lock
// when Timers.StatusDue is called, and publishes the status if so.
func (t *Table[U]) ReleaseStatus(u U) bool {
	id := u.UserID()
	if t.timers.held[id] == nil || t.byID[id] != u {
		return false
	}
	delete(t.timers.held, id)
	return true
}

// Pending counts the timers pending for the user id.
func (t *Table[U]) Pending(id string) int {
	n := len(t.timers.mutes[id])
	if t.timers.held[id] != nil {
		n++
	}
	return n
}

// cancel stops the timers of the user id.
func (t *Table[U]) cancel(id string) {
	for _, timer := range t.timers.mutes[id] {
		timer.Stop()
	}
	delete(t.timers.mutes, id)
	if timer := t.timers.held[id]; timer != nil {
		timer.Stop()
		delete(t.timers.held, id)
	}
}

// Stop stops every user's timers, and schedules none after, as the server
// shuts down.
func (t *Table[U]) Stop() {
	t.timers.stopped = true
	for id := range t.timers.mutes {
		t.cancel(id)
	}
	for id := range t.timers.held {
		t.cancel(id)
	}
}
