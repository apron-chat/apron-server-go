package server

import (
	"container/list"
	"sync"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
)

// Clock issues log_ids (§2.1): the time in milliseconds, or the previous
// log_id plus one, so they strictly increase. The zero Clock is ready; one
// whose backend keeps its log across restarts is restored first.
//
// A Clock never issues a log_id past MaxLogID: one whose sequence has
// reached it panics rather than issue an invalid log_id, as the server can
// log nothing more.
type Clock struct {
	mu   sync.Mutex
	last apron.LogID
}

// Next returns the next log_id.
func (c *Clock) Next() apron.LogID { return c.At(time.Now()) }

// At returns the next log_id for a change made at t: t in milliseconds, or
// the previous log_id plus one when that is not later, so log_ids still
// strictly increase (§2.1). A host that timestamps its own changes uses it
// to keep log_ids close to when they happened. A t past MaxLogID, after
// the year 287,000, counts as not later.
func (c *Clock) At(t time.Time) apron.LogID {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last >= apron.MaxLogID {
		panic("apron: Clock has issued the last log_id below 2^53")
	}
	next := c.last + 1
	if ms := apron.LogID(t.UnixMilli()); ms > next && ms <= apron.MaxLogID {
		next = ms
	}
	c.last = next
	return c.last
}

// Last returns the latest log_id issued, or 0 before any.
func (c *Clock) Last() apron.LogID {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// Restore resumes the sequence after last, the latest log_id a backend
// kept, such as one it stored with its log: the log_ids issued after are
// greater. It never moves the clock back: a last below the latest log_id
// issued changes nothing. It panics when last is past MaxLogID, which no
// log_id is.
func (c *Clock) Restore(last apron.LogID) {
	if last > apron.MaxLogID {
		panic("apron: restoring a Clock past 2^53: " + last.String())
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = max(c.last, last)
}

// MessageIDs gives message_ids to the messages of a host that has none of
// its own (§2.2), such as a chat system the server is a gateway to. The
// host chooses a key that tells its messages apart, such as the message
// itself when it is comparable, or its author, time, and text; MessageIDs
// remembers the latest Size of them.
//
// A message_id is the log_id of the message's creation, so it cannot be a
// pure function of the message: log_ids are unique and strictly increase
// across everything the server logs. ID derives it from the message's own
// time with Clock.At, the first time it sees the key, and returns the same
// one after. Give MessageIDs the messages in the order the host committed
// them, and their log_ids follow that order.
type MessageIDs[K comparable] struct {
	// Clock issues the log_ids; share it with the server's other records.
	Clock *Clock
	// Size bounds how many keys are kept; 4096 when zero.
	Size int

	mu    sync.Mutex
	byKey map[K]apron.LogID
	byID  map[apron.LogID]K
	order *list.List
}

// ID returns the message_id of key, made from at the first time.
func (ids *MessageIDs[K]) ID(key K, at time.Time) apron.LogID {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	if id, ok := ids.byKey[key]; ok {
		return id
	}
	if ids.byKey == nil {
		ids.byKey, ids.byID, ids.order = make(map[K]apron.LogID), make(map[apron.LogID]K), list.New()
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
func (ids *MessageIDs[K]) Lookup(key K) (apron.LogID, bool) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	id, ok := ids.byKey[key]
	return id, ok
}

// Key returns the key that has message_id id, such as to check a reply_to
// (§3.5.4).
func (ids *MessageIDs[K]) Key(id apron.LogID) (K, bool) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	key, ok := ids.byID[id]
	return key, ok
}
