// Package presence keeps a user's status (PROTOCOL.md §4.5): the status
// the user chose, and the shown status others see, which the chosen status
// and the user's presence give (§4.5.1):
//
//	chosen     presence online  presence idle  presence offline
//	online     online           idle           offline
//	dnd        dnd              dnd            offline
//	invisible  offline          offline        offline
//	""         ""               ""             ""
//
// The presence is online while a connection is attended, idle while
// connected with none attended, and offline without a connection. Others
// see the shown status as last published: a State notes each change of it
// (Note), and publishes it when the server sends it (Publish), so that a
// change held back to coalesce a flapping user's changes shows nowhere
// until it is sent. Seen is when the published shown status last became
// offline: it is set only then, so an invisible user's connections never
// move it, nor does a flap that coalescing hides (§4.5.2). A State also
// limits the user's status requests, and the changes it publishes at once.
//
// A State is not safe for concurrent use: the server keeps one with each
// user, under its lock.
package presence

import (
	"slices"
	"time"

	"golang.org/x/time/rate"
)

// The statuses (§4.5). A user may choose Online (the default), None, or
// one of Optional; Idle and Offline are only ever shown.
const (
	Online    = "online"
	Idle      = "idle"
	DND       = "dnd"
	Invisible = "invisible"
	Offline   = "offline"
	None      = ""
)

// Optional are the optional statuses a user may choose, which the server
// frame lists as server.status (§3.1, §4.5).
var Optional = []string{DND, Invisible}

// Settable reports whether value is a status a user may choose (§4.5); a
// server stores None for any other.
func Settable(value string) bool {
	switch value {
	case Online, None:
		return true
	}
	return slices.Contains(Optional, value)
}

const (
	// Each user may send RequestBurst status changes at once, refilled one
	// every RequestRefill; beyond them a request is refused for a while
	// (Admit).
	RequestBurst  = 20
	RequestRefill = time.Second
	// A user's shown status changes go to others at once up to Burst
	// times, then at most once per Coalesce, the latest status winning, so
	// a flapping connection costs its rooms little (§4.5 lets servers delay
	// them). A change that is undone before it is sent is never sent, and
	// moves no seen.
	Burst    = 10
	Coalesce = 2 * time.Second
)

// State is a user's status.
type State struct {
	// Chosen is the chosen status (§4.5.1).
	Chosen string
	// Status is the shown status as last published, the one every object
	// others are sent carries, and Seen the Unix milliseconds when it last
	// became offline, 0 if unknown (§4.5.2).
	Status string
	Seen   int64
	// shown is the shown status as last noted, and offlineAt the Unix
	// milliseconds when it last became offline.
	shown     string
	offlineAt int64
	// requests limits the user's status requests, and published the
	// changes published at once.
	requests  *rate.Limiter
	published *rate.Limiter
}

// New returns the status of a user that has not chosen one, and is not
// connected.
func New() State {
	return State{Chosen: Online, Status: Offline, shown: Offline}
}

// Shown is the shown status the chosen status and a presence give: attended
// while a connection is attended, connected while there is any (§4.5.1).
func (st *State) Shown(attended, connected bool) string {
	switch st.Chosen {
	case Online:
		switch {
		case attended:
			return Online
		case connected:
			return Idle
		}
		return Offline
	case DND:
		// dnd shows only while connected, so it does not tell others that
		// the user is reachable when they are not (§4.5).
		if connected {
			return DND
		}
		return Offline
	case Invisible:
		return Offline
	}
	return st.Chosen
}

// Note notes the shown status after a change to the chosen status or the
// presence, and when it has just become offline, at now, the time it did.
// Others do not see it until it is published.
func (st *State) Note(attended, connected bool, now time.Time) {
	shown := st.Shown(attended, connected)
	if shown == st.shown {
		return
	}
	st.shown = shown
	if shown == Offline {
		st.offlineAt = now.UnixMilli()
	}
}

// Pending reports whether the shown status noted is not the one published.
func (st *State) Pending() bool { return st.shown != st.Status }

// Publish makes the shown status noted the one others see, and reports
// whether that changed, and whether the change is one the store keeps:
// one to or from offline, whose seen, and whether the published status is
// offline, the store has. When the published status becomes offline, Seen
// is the time the shown status became offline, which is after it was last
// published otherwise, or now: only that change sets Seen, so a user
// whose chosen invisible keeps them shown offline can connect and leave
// without moving it, and neither does a flap held back and undone before
// it is published, which nobody saw.
func (st *State) Publish(now time.Time) (changed, stored bool) {
	if st.shown == st.Status {
		return false, false
	}
	wasOffline := st.Status == Offline
	st.Status = st.shown
	if st.Status == Offline {
		st.Seen = st.offlineAt
		if st.Seen == 0 {
			st.Seen = now.UnixMilli()
		}
		return true, true
	}
	return true, wasOffline
}

// Restore takes the status of a user restored from the store, which is not
// connected: its shown status, published, is what its chosen status gives
// offline. One stored as shown other than offline was connected when the
// server stopped without closing its connections, as in a crash, which
// stores nothing; when it is shown offline now, its Seen is now, as for a
// disconnect (§4.5.2), and Restore reports that the store needs the
// change: no earlier time is known not to be before others last saw the
// user, and nobody could see it in between.
func (st *State) Restore(storedShown bool, now time.Time) (stored bool) {
	st.shown = st.Shown(false, false)
	st.Status = st.shown
	if storedShown && st.shown == Offline {
		st.Seen = now.UnixMilli()
		return true
	}
	return false
}

// Admit applies the limit on status requests (RequestBurst), and returns
// zero when it admits one at now, or how long until it would.
func (st *State) Admit(now time.Time) time.Duration {
	if st.requests == nil {
		st.requests = rate.NewLimiter(rate.Every(RequestRefill), RequestBurst)
	}
	reservation := st.requests.ReserveN(now, 1)
	if delay := reservation.DelayFrom(now); delay > 0 {
		reservation.CancelAt(now)
		return delay
	}
	return 0
}

// SetRequestLimit replaces the limit on status requests (Admit), such as
// for a test.
func (st *State) SetRequestLimit(l *rate.Limiter) { st.requests = l }

// SetPublishLimit replaces the limit on changes published at once
// (PublishNow), such as for a test.
func (st *State) SetPublishLimit(l *rate.Limiter) { st.published = l }

// PublishNow reports whether a change may be published at once, within the
// user's burst (Burst); otherwise it waits for Coalesce.
func (st *State) PublishNow() bool {
	if st.published == nil {
		st.published = rate.NewLimiter(rate.Every(Coalesce), Burst)
	}
	return st.published.Allow()
}
