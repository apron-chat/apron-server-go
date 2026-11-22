package wire

import (
	"container/list"
	"strconv"
	"sync"
	"time"
)

// At returns the next log_id for a change made at t: t in milliseconds, or
// the previous log_id plus one when that is not later, so log_ids still
// strictly increase (§2.1). A host that timestamps its own changes uses it
// to keep log_ids close to when they happened.
func (c *Clock) At(t time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = max(t.UnixMilli(), c.last+1)
	return strconv.FormatInt(c.last, 10)
}

// Last returns the latest log_id issued, or "" before any.
func (c *Clock) Last() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == 0 {
		return ""
	}
	return strconv.FormatInt(c.last, 10)
}

// IDs gives message_ids to the messages of a host that has none of its own
// (§2.2), such as a chat system the server is a gateway to. The host
// chooses a key that tells its messages apart, such as the message itself
// when it is comparable, or its author, time, and text; IDs remembers the
// latest Size of them.
//
// A message_id is the log_id of the message's creation, so it cannot be a
// pure function of the message: log_ids are unique and strictly increase
// across everything the server logs. ID derives it from the message's own
// time with Clock.At, the first time it sees the key, and returns the same
// one after. Give IDs the messages in the order the host committed them,
// and their log_ids follow that order.
type IDs[K comparable] struct {
	// Clock issues the log_ids; share it with the server's other records.
	Clock *Clock
	// Size bounds how many keys are kept; 4096 when zero.
	Size int

	mu    sync.Mutex
	byKey map[K]string
	byID  map[string]K
	order *list.List
}

// ID returns the message_id of key, made from at the first time.
func (ids *IDs[K]) ID(key K, at time.Time) string {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	if id, ok := ids.byKey[key]; ok {
		return id
	}
	if ids.byKey == nil {
		ids.byKey, ids.byID, ids.order = make(map[K]string), make(map[string]K), list.New()
	}
	id := ids.Clock.At(at)
	ids.byKey[key], ids.byID[id] = id, key
	ids.order.PushBack(key)
	size := ids.Size
	if size <= 0 {
		size = 4096
	}
	for ids.order.Len() > size {
		oldest := ids.order.Remove(ids.order.Front()).(K)
		delete(ids.byID, ids.byKey[oldest])
		delete(ids.byKey, oldest)
	}
	return id
}

// Lookup returns the message_id of key, if it has one.
func (ids *IDs[K]) Lookup(key K) (string, bool) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	id, ok := ids.byKey[key]
	return id, ok
}

// Key returns the key that has message_id id, such as to check a reply_to
// (§3.5.4).
func (ids *IDs[K]) Key(id string) (K, bool) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	key, ok := ids.byID[id]
	return key, ok
}
