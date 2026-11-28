package server

import (
	"container/list"
	"encoding/json/jsontext"
	"sync"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/internal/core"
)

// dedupOutcome is a request's encoded result, or its error.
type dedupOutcome struct {
	result jsontext.Value
	err    *apron.Error
}

type dedupEntry = core.DedupEntry[dedupOutcome]

// dedupKey is a request's key within its user's requests: its id, and the
// deduplication scope of the connection it came on (Conn.SetDedupScope).
type dedupKey struct {
	scope, id string
}

// bytes is what the key costs, for the byte bounds.
func (k dedupKey) bytes() int { return len(k.scope) + len(k.id) }

// dedup keeps the outcomes of each user's latest requests (§1.2), so that
// one user's requests never push out another's: perUser of each, and at
// most perUserBytes of each, for the latest users users, and at most
// totalBytes of them all, the least recently active users' oldest
// forgotten first. A user's are kept until they are pushed out so, or the
// backend forgets the user (Server.ForgetUser): not when the user's last
// connection ends, as a client retries across reconnects (§1.2).
type dedup struct {
	perUser, users           int
	perUserBytes, totalBytes int

	mu     sync.Mutex
	caches map[string]*list.Element // of *userDedup
	order  *list.List               // most recent last
	bytes  int                      // of every cache
}

type userDedup struct {
	userID string
	cache  core.Dedup[dedupKey, dedupOutcome]
}

func newDedup(perUser, users, perUserBytes, totalBytes int) *dedup {
	return &dedup{
		perUser: perUser, users: users, perUserBytes: perUserBytes, totalBytes: totalBytes,
		caches: map[string]*list.Element{}, order: list.New(),
	}
}

// change runs f on a user's cache, and counts what it changed of the
// bytes kept, which it then brings within totalBytes. A cache left empty
// is dropped.
func (d *dedup) change(element *list.Element, f func(cache *core.Dedup[dedupKey, dedupOutcome])) {
	cache := &element.Value.(*userDedup).cache
	before := cache.Bytes()
	f(cache)
	d.bytes += cache.Bytes() - before
	if cache.Len() == 0 {
		d.drop(element)
	}
	d.trim()
}

// trim forgets the oldest finished requests of the least recently active
// users while all that is kept is over totalBytes, and the caches it
// empties.
func (d *dedup) trim() {
	for element := d.order.Front(); element != nil && d.bytes > d.totalBytes; {
		next := element.Next()
		u := element.Value.(*userDedup)
		for d.bytes > d.totalBytes {
			before := u.cache.Bytes()
			if !u.cache.EvictOldest() {
				break
			}
			d.bytes += u.cache.Bytes() - before
		}
		if u.cache.Len() == 0 {
			d.drop(element)
		}
		element = next
	}
}

// drop forgets a user's cache; the requests waiting on its entries still
// get their outcomes (finish).
func (d *dedup) drop(element *list.Element) {
	u := d.order.Remove(element).(*userDedup)
	delete(d.caches, u.userID)
	d.bytes -= u.cache.Bytes()
}

func (d *dedup) claim(userID string, key dedupKey, method string, params []byte) (entry, prior *dedupEntry) {
	if d.perUser < 0 {
		return nil, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	element := d.caches[userID]
	if element == nil {
		element = d.order.PushBack(&userDedup{userID: userID, cache: core.Dedup[dedupKey, dedupOutcome]{Size: d.perUser, MaxBytes: d.perUserBytes}})
		d.caches[userID] = element
		for d.order.Len() > d.users {
			d.drop(d.order.Front())
		}
	} else {
		d.order.MoveToBack(element)
	}
	d.change(element, func(cache *core.Dedup[dedupKey, dedupOutcome]) {
		entry, prior = cache.Claim(key, method, params, key.bytes())
	})
	return entry, prior
}

// forget drops a request whose outcome is not kept as its reply is queued,
// before the client can see the reply, so that a retry the reply prompts
// runs again, even while the original's handler is still returning.
func (d *dedup) forget(userID string, key dedupKey, entry *dedupEntry) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if element := d.caches[userID]; element != nil {
		d.change(element, func(cache *core.Dedup[dedupKey, dedupOutcome]) { cache.Forget(key, entry) })
	}
}

func (d *dedup) finish(userID string, key dedupKey, entry *dedupEntry, outcome dedupOutcome, forget bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if element := d.caches[userID]; element != nil {
		d.change(element, func(cache *core.Dedup[dedupKey, dedupOutcome]) {
			cache.Finish(key, entry, outcome, len(outcome.result), forget)
		})
		return
	}
	// The user's cache was dropped meanwhile; its waiters still get the
	// outcome.
	var orphan core.Dedup[dedupKey, dedupOutcome]
	orphan.Finish(key, entry, outcome, 0, true)
}

// forgetUser drops a user's cache.
func (d *dedup) forgetUser(userID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if element := d.caches[userID]; element != nil {
		d.drop(element)
	}
}

// usage is how many requests are kept, of how many users, and what they
// cost, for tests.
func (d *dedup) usage() (users, requests, bytes int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for element := d.order.Front(); element != nil; element = element.Next() {
		requests += element.Value.(*userDedup).cache.Len()
	}
	return d.order.Len(), requests, d.bytes
}
