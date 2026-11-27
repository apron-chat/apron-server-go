package core

import "sync/atomic"

// DedupEntryBytes is what a kept request costs besides its key and its
// outcome, for a Dedup's byte bound: its entry, its place in the order, and
// its map slot, roughly.
const DedupEntryBytes = 160

// Dedup keeps the outcomes of the latest requests (§1.2) by a key, such as
// a user_id and a request id, oldest forgotten first. Its methods are
// called holding the caller's lock; the zero Dedup keeps none.
type Dedup[K comparable, V any] struct {
	// Size bounds the requests kept; negative keeps none, and Claim then
	// claims nothing.
	Size int
	// MaxBytes, if positive, bounds the bytes the requests kept cost, as
	// Claim and Finish count them: a request's key and outcome, and
	// DedupEntryBytes.
	MaxBytes int
	entries  map[K]*dedupNode[K, V]
	// oldest and newest are the ends of the order the requests are kept
	// in, of len of them.
	oldest, newest *dedupNode[K, V]
	len            int
	bytes          int
}

// dedupNode is a kept request, in its place in the order: one allocation,
// its entry and its links.
type dedupNode[K comparable, V any] struct {
	key          K
	entry        DedupEntry[V]
	bytes        int
	older, newer *dedupNode[K, V]
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
// waits for (without the lock) unless its fingerprint differs. keyBytes is
// what key costs, for MaxBytes. With Size negative, both are nil.
func (d *Dedup[K, V]) Claim(key K, fingerprint [32]byte, keyBytes int) (entry, prior *DedupEntry[V]) {
	if d.Size < 0 {
		return nil, nil
	}
	if node := d.entries[key]; node != nil {
		return nil, &node.entry
	}
	if d.entries == nil {
		d.entries = make(map[K]*dedupNode[K, V])
	}
	node := &dedupNode[K, V]{key: key, entry: DedupEntry[V]{Fingerprint: fingerprint, done: make(chan struct{})}, bytes: keyBytes + DedupEntryBytes}
	d.entries[key] = node
	node.older = d.newest
	if d.newest != nil {
		d.newest.newer = node
	} else {
		d.oldest = node
	}
	d.newest = node
	d.len++
	d.bytes += node.bytes
	d.trim()
	return &node.entry, nil
}

// over reports whether the requests kept are over Size or MaxBytes.
func (d *Dedup[K, V]) over() bool {
	return d.len > d.Size || d.MaxBytes > 0 && d.bytes > d.MaxBytes
}

// trim forgets the oldest requests that finished while the requests kept
// are over Size or MaxBytes.
func (d *Dedup[K, V]) trim() {
	for d.over() && d.EvictOldest() {
	}
}

// EvictOldest forgets the oldest request that finished, and reports
// whether there was one. A request still running stays, in its place, so
// that its duplicates find it, and it is forgotten once it is the oldest
// finished.
func (d *Dedup[K, V]) EvictOldest() bool {
	for node := d.oldest; node != nil; node = node.newer {
		if !isClosed(node.entry.done) {
			continue
		}
		d.remove(node)
		return true
	}
	return false
}

// remove forgets a kept request.
func (d *Dedup[K, V]) remove(node *dedupNode[K, V]) {
	if node.older != nil {
		node.older.newer = node.newer
	} else {
		d.oldest = node.newer
	}
	if node.newer != nil {
		node.newer.older = node.older
	} else {
		d.newest = node.older
	}
	node.older, node.newer = nil, nil
	d.len--
	delete(d.entries, node.key)
	d.bytes -= node.bytes
}

// Finish records the outcome of a claimed request; forget drops it once
// the requests waiting on it have it, so that a retry runs again. Else
// valueBytes is what the outcome costs, for MaxBytes: one that does not
// fit is not kept.
func (d *Dedup[K, V]) Finish(key K, entry *DedupEntry[V], value V, valueBytes int, forget bool) {
	entry.Value = value
	if forget {
		d.Forget(key, entry)
	} else if node := d.entries[key]; node != nil && &node.entry == entry {
		node.bytes += valueBytes
		d.bytes += valueBytes
	}
	close(entry.done)
	d.trim()
}

// Forget drops a claimed request, running or finished, so that a request
// with its key claimed after it runs again; the requests already waiting
// on it still get its outcome, once it finishes.
func (d *Dedup[K, V]) Forget(key K, entry *DedupEntry[V]) {
	if node := d.entries[key]; node != nil && &node.entry == entry {
		d.remove(node)
	}
}

// Kept reports whether a request with key is kept: running, or finished
// and not yet forgotten.
func (d *Dedup[K, V]) Kept(key K) bool { return d.entries[key] != nil }

// Len is how many requests are kept, and Bytes what they cost.
func (d *Dedup[K, V]) Len() int   { return d.len }
func (d *Dedup[K, V]) Bytes() int { return d.bytes }

func isClosed(done chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}
