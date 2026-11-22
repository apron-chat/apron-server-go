package wirecore

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
	done        chan struct{}
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
	for len(d.order) > d.Size {
		oldest := d.order[0]
		d.order = d.order[1:]
		// A request still running stays, so that its duplicates find it.
		if e := d.entries[oldest]; e != nil && isClosed(e.done) {
			delete(d.entries, oldest)
		}
	}
	return entry, nil
}

// Finish records the outcome of a claimed request; forget drops it once
// the requests waiting on it have it, so that a retry runs again.
func (d *Dedup[K, V]) Finish(key K, entry *DedupEntry[V], value V, forget bool) {
	entry.Value = value
	if forget && d.entries[key] == entry {
		delete(d.entries, key)
	}
	close(entry.done)
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
