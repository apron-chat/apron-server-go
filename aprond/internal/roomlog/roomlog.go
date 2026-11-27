// Package roomlog is a server's single append-only log (PROTOCOL.md §2):
// its records, the log_id sequence they share, each room's log, history
// pages over one or more rooms' logs (§4.3), the feed that extensions
// follow, and how records are kept in a store.
//
// It knows nothing of users, connections, or delivery: the server builds a
// record, appends it to the rooms whose logs hold it, and sends Raw as it
// is. Every record has a typed shape here (Membership, Reactions,
// RoomRecord, Message, and the Author and Body they carry) that encodes
// exactly as the maps the server built before them did, keys sorted, so
// stored and sent bytes are unchanged. A message's Body is a apron.Body,
// which keeps what its sender sent and left out.
//
// A Log is not safe for concurrent use: every method is called holding the
// server's lock, which a reader may release while it holds a Record's Raw,
// since Raw is replaced, never modified.
package roomlog

import (
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/server"
	"github.com/apron-chat/apron-server-go/store"
)

// Kind partitions the log (§2.3). Its values are stored.
type Kind int

const (
	KindRoom Kind = iota
	KindMessage
	KindReactions
	KindMembership
)

// Record is one committed change: Raw is the complete protocol object
// (including log_id) as it was at commit time, kept as JSON, which history
// and broadcasts send as is and which costs far less memory than decoded
// values. Only redaction (§4.4) rewrites a record. A record is referenced
// from the log of every room it belongs to, so a move snapshot appears in
// both the source and destination room logs (§4.3.1).
type Record struct {
	ID   int64
	Kind Kind
	Raw  jsontext.Value
	// Rooms are the rooms whose logs hold the record, which Append sets.
	Rooms []string
	// Message is the message_id of a message snapshot.
	Message string
}

// New encodes value as a record. Values hold only JSON-decoded data and
// server-built strings, maps, slices, and the shapes of this package, so
// encoding cannot fail.
func New(id int64, kind Kind, value any) *Record {
	return &Record{ID: id, Kind: kind, Raw: encode(value)}
}

// encode encodes as frames are encoded (apron.Marshal): object keys
// sorted, nil slices and maps as null, and <, >, and & unescaped.
func encode(value any) jsontext.Value {
	payload, err := apron.Marshal(value)
	if err != nil {
		return nil
	}
	return payload
}

// FormatID is a log_id as records spell it.
func FormatID(id int64) string { return strconv.FormatInt(id, 10) }

// Log is the server's log: every record in log_id order, and the sequence
// that issues log_ids.
type Log struct {
	// clock issues the single log_id sequence shared by every record kind
	// and room.
	clock   server.Clock
	records []*Record
	// changed is closed and replaced when a record is added.
	changed chan struct{}
	// dirty are the records changed since Entries last took them.
	dirty map[*Record]struct{}
}

// NewLog returns an empty log.
func NewLog() *Log {
	return &Log{changed: make(chan struct{}), dirty: make(map[*Record]struct{})}
}

// NextID returns the next log_id in the server-wide sequence: the commit
// time in milliseconds, or the previous log_id + 1.
func (l *Log) NextID() int64 { return int64(l.clock.Next()) }

// LastIssued is the last log_id NextID issued, whether or not a record
// holds it, which the server stores so that none is issued again.
func (l *Log) LastIssued() int64 { return int64(l.clock.Last()) }

// RestoreSequence continues the sequence after last, as stored.
func (l *Log) RestoreSequence(last int64) { l.clock.Restore(apron.LogID(last)) }

// Append adds a committed record to the log, and to the log of each room
// it belongs to, whose latest_log_id it becomes.
func (l *Log) Append(record *Record, rooms ...*Timeline) {
	// log_ids rise, so a record almost always goes last.
	i := len(l.records)
	if i > 0 && l.records[i-1].ID > record.ID {
		i, _ = slices.BinarySearchFunc(l.records, record.ID, compareID)
	}
	l.records = slices.Insert(l.records, i, record)
	close(l.changed)
	l.changed = make(chan struct{})
	for _, t := range rooms {
		t.records = append(t.records, record)
		t.latest = record.ID
		if record.Rooms == nil {
			// Most records are one room's: they share its list of one,
			// which a second room's append copies, as its capacity is one.
			record.Rooms = t.only
		} else {
			record.Rooms = append(record.Rooms, t.room)
		}
	}
	l.dirty[record] = struct{}{}
}

// Redact rewrites a message snapshot into a tombstone at its log_id
// (§4.4): without body and ext, and deleted.
func (l *Log) Redact(record *Record) {
	var fields map[string]jsontext.Value
	if json.Unmarshal(record.Raw, &fields) != nil {
		return
	}
	delete(fields, "body")
	delete(fields, "ext")
	fields["deleted"] = jsontext.Value("true")
	record.Raw = encode(fields)
	l.dirty[record] = struct{}{}
}

// Last is the log_id of the latest record, 0 for none.
func (l *Log) Last() int64 {
	if n := len(l.records); n > 0 {
		return l.records[n-1].ID
	}
	return 0
}

// After returns at most n records after the log_id cursor, in order, and a
// channel closed once a record is added.
func (l *Log) After(cursor int64, n int) ([]*Record, <-chan struct{}) {
	i, found := slices.BinarySearchFunc(l.records, cursor, compareID)
	if found {
		i++
	}
	return slices.Clone(l.records[i:min(len(l.records), i+n)]), l.changed
}

func compareID(record *Record, id int64) int {
	return cmp.Compare(record.ID, id)
}

// Timeline is one room's log: the records it holds, in log_id order.
type Timeline struct {
	room string
	// only is the Rooms of a record of this room alone.
	only    []string
	records []*Record
	created int64
	latest  int64
}

// NewTimeline returns the log of a room created at the log_id created,
// whose latest record is latest.
func NewTimeline(room string, created, latest int64) *Timeline {
	return &Timeline{room: room, only: []string{room}, created: created, latest: latest}
}

// Created is the log_id of the room's creation record, where its retained
// history starts (§4.3.3): the server retains every record.
func (t *Timeline) Created() int64 { return t.created }

// Latest is the room's latest_log_id: its latest record's.
func (t *Timeline) Latest() int64 { return t.latest }

// Since returns the records after the log_id since, at most the last limit
// of them.
func (t *Timeline) Since(since int64, limit int) []*Record {
	start, _ := slices.BinarySearchFunc(t.records, since+1, compareID)
	start = max(start, len(t.records)-limit)
	return t.records[start:]
}

// window returns the records of a log within the query's inclusive bounds.
func (t *Timeline) window(q Query) []*Record {
	log := t.records
	if q.HasAfter {
		start, _ := slices.BinarySearchFunc(log, q.After, compareID)
		log = log[start:]
	}
	if q.HasBefore {
		end, found := slices.BinarySearchFunc(log, q.Before, compareID)
		if found {
			end++
		}
		log = log[:end]
	}
	return log
}

// Query selects a history page (§4.3): the records within the inclusive
// bounds that are set, Limit of them from the After end when it is set, and
// else from the Before end, and no more than MaxBytes of them, keeping at
// least one.
type Query struct {
	After, Before       int64
	HasAfter, HasBefore bool
	Limit, MaxBytes     int
}

// Page merges the rooms' logs into one, by log_id, and returns the page of
// it the query selects, in log_id order, and whether more records are past
// its far end. A record in two of the logs, such as a move snapshot, is
// one record and comes once.
func Page(rooms []*Timeline, q Query) (page []*Record, more bool) {
	// The rooms' windows merge by log_id from the end the page starts at,
	// until Limit+1 records.
	windows := make([][]*Record, 0, len(rooms))
	for _, t := range rooms {
		if records := t.window(q); len(records) > 0 {
			windows = append(windows, records)
		}
	}
	matching := make([]*Record, 0, min(q.Limit+1, 64))
	for len(matching) <= q.Limit {
		var next *Record
		for _, records := range windows {
			if len(records) == 0 {
				continue
			}
			head := records[0]
			if !q.HasAfter {
				head = records[len(records)-1]
			}
			if next == nil || (q.HasAfter && head.ID < next.ID) || (!q.HasAfter && head.ID > next.ID) {
				next = head
			}
		}
		if next == nil {
			break
		}
		matching = append(matching, next)
		for i, records := range windows {
			switch {
			case len(records) == 0:
			case q.HasAfter && records[0] == next:
				windows[i] = records[1:]
			case !q.HasAfter && records[len(records)-1] == next:
				windows[i] = records[:len(records)-1]
			}
		}
	}
	if !q.HasAfter {
		slices.Reverse(matching)
	}
	more = len(matching) > q.Limit
	if more {
		if q.HasAfter {
			matching = matching[:q.Limit]
		} else {
			matching = matching[1:]
		}
	}
	// A page also stops at MaxBytes of records, keeping at least one, so
	// that one request cannot render an unbounded reply.
	size := 0
	for i := range matching {
		k := i
		if !q.HasAfter {
			k = len(matching) - 1 - i
		}
		if size += len(matching[k].Raw); size > q.MaxBytes && i > 0 {
			if q.HasAfter {
				matching = matching[:k]
			} else {
				matching = matching[k+1:]
			}
			more = true
			break
		}
	}
	return matching, more
}

// EntryKind is the kind of the store entries that hold records, each named
// by its log_id.
const EntryKind = "record"

// stored is a record as the store keeps it.
type stored struct {
	Kind  Kind           `json:"kind"`
	Raw   jsontext.Value `json:"raw"`
	Rooms []string       `json:"rooms"`
}

// Entry is the store entry that keeps a record.
func Entry(r *Record) store.Entry {
	return store.Entry{Kind: EntryKind, ID: FormatID(r.ID), Value: encode(stored{Kind: r.Kind, Raw: r.Raw, Rooms: r.Rooms})}
}

// Entries returns the entries of the records added or rewritten since it
// was last called.
func (l *Log) Entries() []store.Entry {
	if len(l.dirty) == 0 {
		return nil
	}
	entries := make([]store.Entry, 0, len(l.dirty))
	for r := range l.dirty {
		entries = append(entries, Entry(r))
	}
	clear(l.dirty)
	return entries
}

// Restore rebuilds the log from the store's record entries, by log_id, and
// adds each record to the log of each of its rooms that room returns, in
// log_id order. It returns the records by log_id.
func (l *Log) Restore(entries map[string]jsontext.Value, room func(id string) *Timeline) (map[int64]*Record, error) {
	records := make(map[int64]*Record, len(entries))
	for id, raw := range entries {
		var s stored
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("stored %s %q: %w", EntryKind, id, err)
		}
		logID, _ := strconv.ParseInt(id, 10, 64)
		records[logID] = &Record{ID: logID, Kind: s.Kind, Raw: s.Raw, Rooms: s.Rooms}
	}
	for _, logID := range slices.Sorted(maps.Keys(records)) {
		record := records[logID]
		l.records = append(l.records, record)
		for _, id := range record.Rooms {
			if t := room(id); t != nil {
				t.records = append(t.records, record)
				// A large room's entry may lag behind its latest record
				// (rooms.Table.EntriesDue).
				t.latest = max(t.latest, record.ID)
			}
		}
	}
	return records, nil
}
