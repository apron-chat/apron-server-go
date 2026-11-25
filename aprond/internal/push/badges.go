package push

import (
	"slices"
	"sync"
	"time"
)

// BadgeDelay is how long a user's badge pushes wait, so that one push
// carries the count after a burst of changes (§4.9).
const BadgeDelay = 2 * time.Second

// badges are the users' pending badge pushes: a Table schedules one when
// a user's unread count may have changed, and calls the server back
// (NewTable's due) once it has waited, from the timer's goroutine; the
// server takes its lock, and sends it if TakeBadge says it is still
// pending.
type badges struct {
	// delay is how long a badge push waits, pending the ones waiting by
	// user_id, and work the waits that have not finished, a callback
	// included.
	delay   time.Duration
	pending map[string]*PendingBadge
	work    sync.WaitGroup
	due     func(userID string, b *PendingBadge)
	stopped bool
}

// PendingBadge is a badge push waiting to be sent.
type PendingBadge struct{ timer *time.Timer }

// SetBadgeDelay replaces BadgeDelay for the badge pushes scheduled from
// now on, such as for a test.
func (t *Table) SetBadgeDelay(d time.Duration) { t.badges.delay = d }

// ScheduleBadge schedules a badge push of userID's, when it has a
// registration that takes them (Registration.TakesBadges), and none is
// pending already: the changes until it is sent ride along. None is
// scheduled once the table is stopped.
func (t *Table) ScheduleBadge(userID string) {
	b := &t.badges
	if b.stopped || b.pending[userID] != nil || !slices.ContainsFunc(t.Of(userID), (*Registration).TakesBadges) {
		return
	}
	badge := new(PendingBadge)
	b.work.Add(1)
	badge.timer = time.AfterFunc(b.delay, func() {
		defer b.work.Done()
		b.due(userID, badge)
	})
	b.pending[userID] = badge
}

// TakeBadge forgets the badge push of userID's, and reports whether it was
// still the one pending. The server calls it holding its lock when the
// badge push is due, and sends it if so.
func (t *Table) TakeBadge(userID string, badge *PendingBadge) bool {
	if t.badges.pending[userID] != badge {
		return false
	}
	delete(t.badges.pending, userID)
	return true
}

// BadgePending reports whether a badge push of userID's is pending.
func (t *Table) BadgePending(userID string) bool { return t.badges.pending[userID] != nil }

// cancelBadge cancels userID's pending badge push.
func (t *Table) cancelBadge(userID string) {
	if badge := t.badges.pending[userID]; badge != nil {
		if badge.timer.Stop() {
			t.badges.work.Done()
		}
		delete(t.badges.pending, userID)
	}
}

// Stop cancels every pending badge push, and schedules none after, as the
// server shuts down.
func (t *Table) Stop() {
	t.badges.stopped = true
	for userID := range t.badges.pending {
		t.cancelBadge(userID)
	}
}

// Wait waits for the badge pushes that are due to be sent, or handed to
// the server to send. Called without the server's lock, which they take;
// the server schedules them only holding it.
func (t *Table) Wait() { t.badges.work.Wait() }
