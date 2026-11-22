package wire

import (
	"container/list"
	"encoding/json/v2"
	"strconv"
	"sync"
	"time"
)

// LogID is a log_id (§2.1): the position of one change in the server's log,
// the time of its commit in milliseconds since the Unix epoch. A message_id
// is the log_id of the message's creation (§2.2), so it is a LogID too.
//
// Its JSON is a decimal string, such as "1724803200042". The zero LogID is
// no log_id, and encodes as null: a field that is left out when it has
// none is tagged omitzero, and one the protocol requires, such as a
// history result's history_log_id, is null. A null decodes as 0. A LogID
// is decoded only from a string, as ParseLogID reads it.
type LogID int64

// ParseLogID reads a log_id or message_id (§2.1): a decimal string of a
// non-negative number, without a sign or leading zeros, that fits in an
// int64. "0" is 0, which is a valid bound but no record's log_id.
func ParseLogID(s string) (LogID, error) {
	if s == "" || s[0] == '0' && len(s) > 1 {
		return 0, InvalidParams("Not a log_id: " + strconv.Quote(s))
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, InvalidParams("Not a log_id: " + strconv.Quote(s))
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, InvalidParams("Not a log_id: " + strconv.Quote(s))
	}
	return LogID(n), nil
}

// String returns the log_id as a decimal string, "0" for none.
func (id LogID) String() string { return strconv.FormatInt(int64(id), 10) }

// MarshalJSON writes the log_id as a decimal string, or null for 0.
func (id LogID) MarshalJSON() ([]byte, error) {
	if id == 0 {
		return []byte("null"), nil
	}
	return strconv.AppendQuote(nil, id.String()), nil
}

// UnmarshalJSON reads a log_id from a decimal string, as ParseLogID does,
// or 0 from null.
func (id *LogID) UnmarshalJSON(data []byte) error {
	if isNull(data) {
		*id = 0
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return InvalidParams("A log_id is a decimal string")
	}
	parsed, err := ParseLogID(s)
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// Clock issues log_ids (§2.1): the time in milliseconds, or the previous
// log_id plus one, so they strictly increase. The zero Clock is ready; one
// whose backend keeps its log across restarts is restored first.
type Clock struct {
	mu   sync.Mutex
	last LogID
}

// Next returns the next log_id.
func (c *Clock) Next() LogID { return c.At(time.Now()) }

// At returns the next log_id for a change made at t: t in milliseconds, or
// the previous log_id plus one when that is not later, so log_ids still
// strictly increase (§2.1). A host that timestamps its own changes uses it
// to keep log_ids close to when they happened.
func (c *Clock) At(t time.Time) LogID {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = max(LogID(t.UnixMilli()), c.last+1)
	return c.last
}

// Last returns the latest log_id issued, or 0 before any.
func (c *Clock) Last() LogID {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// Restore resumes the sequence after last, the latest log_id a backend
// kept, such as one it stored with its log: the log_ids issued after are
// greater. It never moves the clock back: a last below the latest log_id
// issued changes nothing.
func (c *Clock) Restore(last LogID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = max(c.last, last)
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
	byKey map[K]LogID
	byID  map[LogID]K
	order *list.List
}

// ID returns the message_id of key, made from at the first time.
func (ids *IDs[K]) ID(key K, at time.Time) LogID {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	if id, ok := ids.byKey[key]; ok {
		return id
	}
	if ids.byKey == nil {
		ids.byKey, ids.byID, ids.order = make(map[K]LogID), make(map[LogID]K), list.New()
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
func (ids *IDs[K]) Lookup(key K) (LogID, bool) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	id, ok := ids.byKey[key]
	return id, ok
}

// Key returns the key that has message_id id, such as to check a reply_to
// (§3.5.4).
func (ids *IDs[K]) Key(id LogID) (K, bool) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	key, ok := ids.byID[id]
	return key, ok
}
