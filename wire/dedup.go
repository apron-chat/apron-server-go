package wire

import (
	"container/list"
	"encoding/json/jsontext"
	"sync"

	"github.com/apron-chat/apron-server-go/internal/wirecore"
)

// dedupOutcome is a request's encoded result, or its error.
type dedupOutcome struct {
	result jsontext.Value
	err    *Error
}

type dedupEntry = wirecore.DedupEntry[dedupOutcome]

// dedup keeps the outcomes of each user's latest requests (§1.2), so that
// one user's requests never push out another's: perUser of each, for the
// latest users users.
type dedup struct {
	perUser int
	users   int

	mu     sync.Mutex
	caches map[string]*list.Element // of *userDedup
	order  *list.List               // most recent last
}

type userDedup struct {
	userID string
	cache  wirecore.Dedup[string, dedupOutcome]
}

func newDedup(perUser, users int) *dedup {
	return &dedup{perUser: perUser, users: users, caches: map[string]*list.Element{}, order: list.New()}
}

func (d *dedup) claim(userID, id string, fingerprint [32]byte) (entry, prior *dedupEntry) {
	if d.perUser < 0 {
		return nil, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	element := d.caches[userID]
	if element == nil {
		element = d.order.PushBack(&userDedup{userID: userID, cache: wirecore.Dedup[string, dedupOutcome]{Size: d.perUser}})
		d.caches[userID] = element
		for d.order.Len() > d.users {
			oldest := d.order.Remove(d.order.Front()).(*userDedup)
			delete(d.caches, oldest.userID)
		}
	} else {
		d.order.MoveToBack(element)
	}
	return element.Value.(*userDedup).cache.Claim(id, fingerprint)
}

func (d *dedup) finish(userID, id string, entry *dedupEntry, outcome dedupOutcome, forget bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if element := d.caches[userID]; element != nil {
		element.Value.(*userDedup).cache.Finish(id, entry, outcome, forget)
		return
	}
	// The user's cache was dropped meanwhile; its waiters still get the
	// outcome.
	var orphan wirecore.Dedup[string, dedupOutcome]
	orphan.Finish(id, entry, outcome, true)
}
