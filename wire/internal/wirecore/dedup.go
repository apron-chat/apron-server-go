package wirecore

import "sync/atomic"

// Dedup keeps the outcomes of the latest requests (§1.2) by a key, such as
// a user_id and a request id, oldest forgotten first. Its methods are
// called holding the caller's lock; the zero Dedup keeps none.
type Dedup[K comparable, V any] struct {
	// Size bounds the requests kept; negative keeps none, and Claim then
	// claims nothing.
	Size    int
	entries map[K]*DedupEntry[V]
	order   []K
}

// DedupEntry is one request's outcome: Value, once Done is closed.
type DedupEntry[V any] struct {
	Fingerprint [32]byte
	Value       V
	// Later is set once the request is to be answered after its handler
	// returns, so that its duplicates wait for it without holding up
	// their own connections.
	Later atomic.Bool
	done  chan struct{}
}

// Done is closed once the request finished, and Value is its outcome.
func (e *DedupEntry[V]) Done() <-chan struct{} { return e.done }

// Claim returns a new entry for key, which the caller runs and finishes,
// or the prior entry of a request with the same key, which the caller
// waits for (without the lock) unless its fingerprint differs. With Size
// negative, both are nil.
func (d *Dedup[K, V]) Claim(key K, fingerprint [32]byte) (entry, prior *DedupEntry[V]) {
	if d.Size < 0 {
		return nil, nil
	}
	if prior := d.entries[key]; prior != nil {
		return nil, prior
	}
	if d.entries == nil {
		d.entries = make(map[K]*DedupEntry[V])
	}
	entry = &DedupEntry[V]{Fingerprint: fingerprint, done: make(chan struct{})}
	d.entries[key] = entry
	d.order = append(d.order, key)
	for len(d.order) > d.Size && d.evict() {
	}
	return entry, nil
}

// evict forgets the oldest request that finished, and reports whether
// there was one. A request still running stays, in its place, so that its
// duplicates find it, and it is forgotten once it is the oldest finished.
func (d *Dedup[K, V]) evict() bool {
	for i, key := range d.order {
		if e := d.entries[key]; e != nil && !isClosed(e.done) {
			continue
		}
		delete(d.entries, key)
		if i == 0 {
			d.order = d.order[1:]
		} else {
			d.order = append(d.order[:i], d.order[i+1:]...)
		}
		return true
	}
	return false
}

// Finish records the outcome of a claimed request; forget drops it once
// the requests waiting on it have it, so that a retry runs again.
func (d *Dedup[K, V]) Finish(key K, entry *DedupEntry[V], value V, forget bool) {
	entry.Value = value
	if forget {
		d.Forget(key, entry)
	}
	close(entry.done)
}

// Forget drops a claimed request, running or finished, so that a request
// with its key claimed after it runs again; the requests already waiting
// on it still get its outcome, once it finishes.
func (d *Dedup[K, V]) Forget(key K, entry *DedupEntry[V]) {
	if d.entries[key] == entry {
		delete(d.entries, key)
	}
}

// Kept reports whether a request with key is kept: running, or finished
// and not yet forgotten.
func (d *Dedup[K, V]) Kept(key K) bool { return d.entries[key] != nil }

func isClosed(done chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}
