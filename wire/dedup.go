package wire

import (
	"encoding/json/jsontext"
	"sync"
)

type dedupKey struct{ userID, id string }

type dedupEntry struct {
	fingerprint [32]byte
	done        chan struct{}
	result      jsontext.Value
	err         *Error
}

// dedupCache keeps the outcomes of the latest requests (§1.2), oldest
// forgotten first. Guarded by the Server's mu.
type dedupCache struct {
	size    int
	entries map[dedupKey]*dedupEntry
	order   []dedupKey
}

// claim returns a new entry for key, which the caller runs and finishes, or
// the prior entry of a request with the same key. With dedup off, both are
// nil.
func (d *dedupCache) claim(mu *sync.Mutex, key dedupKey, fingerprint [32]byte) (entry, prior *dedupEntry) {
	if d.size < 0 {
		return nil, nil
	}
	mu.Lock()
	defer mu.Unlock()
	if prior := d.entries[key]; prior != nil {
		return nil, prior
	}
	entry = &dedupEntry{fingerprint: fingerprint, done: make(chan struct{})}
	d.entries[key] = entry
	d.order = append(d.order, key)
	for len(d.order) > d.size {
		oldest := d.order[0]
		d.order = d.order[1:]
		if e := d.entries[oldest]; e != nil && isClosed(e.done) {
			delete(d.entries, oldest)
		}
	}
	return entry, nil
}

// finish records the outcome of a claimed request; forget drops it once
// the requests waiting on it have it, so that a retry runs again.
func (d *dedupCache) finish(mu *sync.Mutex, key dedupKey, entry *dedupEntry, result jsontext.Value, err *Error, forget bool) {
	mu.Lock()
	defer mu.Unlock()
	entry.result, entry.err = result, err
	if forget && d.entries[key] == entry {
		delete(d.entries, key)
	}
	close(entry.done)
}

func isClosed(done chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}
