package apron

import (
	"context"
	"encoding/json/jsontext"
	"slices"
	"strconv"
	"time"
)

// EventKind is the kind of record an Event carries (PROTOCOL.md §2.3).
type EventKind string

const (
	// EventRoom is a room's record: its creation or a room_set (§4.2.4).
	EventRoom EventKind = "room"
	// EventMessage is a message snapshot (§3.5): new, edited, moved, or a
	// deletion's tombstone (§4.4). DecodeMessage reads it.
	EventMessage EventKind = "message"
	// EventReactions is a message's reactions (§4.7).
	EventReactions EventKind = "reactions"
	// EventMembership is a join or a leave (§4.2.2).
	EventMembership EventKind = "membership"
)

var eventKinds = map[recordKind]EventKind{
	kindRoom:       EventRoom,
	kindMessage:    EventMessage,
	kindReactions:  EventReactions,
	kindMembership: EventMembership,
}

// Event is one logged record: the server's log is the stream of what
// changes in its rooms, which extensions follow with OnEvent and
// OnEventFrom.
type Event struct {
	// Kind says what the record is.
	Kind EventKind
	// LogID orders the events (§2.1): each is greater than the last.
	LogID int64
	// Rooms are the rooms whose logs hold the record: one, or both rooms
	// of a move (§4.3.1).
	Rooms []string
	// Record is the record as clients receive it, in history and as it is
	// broadcast, which PROTOCOL.md defines. A deletion redacts the
	// message's earlier records (§4.4): an event read after it carries the
	// redacted record.
	Record jsontext.Value
}

func eventOf(r *logRecord) Event {
	return Event{Kind: eventKinds[r.kind], LogID: r.id, Rooms: slices.Clone(r.rooms), Record: slices.Clone(r.raw)}
}

// LastLogID is the log_id of the latest record, 0 for none.
func (h *Host) LastLogID() int64 {
	h.s.mu.RLock()
	defer h.s.mu.RUnlock()
	if n := len(h.s.records); n > 0 {
		return h.s.records[n-1].id
	}
	return 0
}

// OnEvent calls fn with each record logged from now on, in order, on one
// goroutine of its own. fn delays the events after it, not the server; a
// slow fn falls behind and catches up. Events logged before a restart are
// not sent again: an extension that must see every event uses
// OnEventFrom.
func (h *Host) OnEvent(fn func(ctx context.Context, e Event)) {
	h.follow(h.LastLogID(), func(ctx context.Context, e Event) error {
		fn(ctx, e)
		return nil
	})
}

// OnEventFrom calls fn with each record logged after the log_id after, in
// order, then each one logged later, on one goroutine of its own: 0 sends
// the whole log first. When fn returns an error, it is logged, and fn is
// called with the same event again, after a delay that grows to a minute,
// until it succeeds or the server shuts down. An extension that keeps the
// LogID of the last event it handled (KV) and passes it here at the next
// start sees every event at least once, as a bridge or a search index
// needs.
func (h *Host) OnEventFrom(after int64, fn func(ctx context.Context, e Event) error) {
	h.follow(after, fn)
}

// eventBatch bounds the events a follower takes from the log at once.
const eventBatch = 256

func (h *Host) follow(after int64, deliver func(ctx context.Context, e Event) error) {
	s := h.s
	h.Go(func(ctx context.Context) {
		cursor := after
		for {
			s.mu.RLock()
			i, found := slices.BinarySearchFunc(s.records, cursor, compareLogID)
			if found {
				i++
			}
			batch := make([]Event, 0, min(eventBatch, len(s.records)-i))
			for _, r := range s.records[i:min(len(s.records), i+eventBatch)] {
				batch = append(batch, eventOf(r))
			}
			changed := s.recordsChanged
			s.mu.RUnlock()
			if len(batch) == 0 {
				select {
				case <-changed:
					continue
				case <-ctx.Done():
					return
				}
			}
			for _, e := range batch {
				for delay := time.Second; ; delay = min(2*delay, time.Minute) {
					err := deliver(ctx, e)
					if err == nil {
						break
					}
					h.logger.Error("handling an event failed; retrying", "log_id", e.LogID, "error", err, "retry_in", delay)
					select {
					case <-time.After(delay):
					case <-ctx.Done():
						return
					}
				}
				cursor = e.LogID
			}
		}
	})
}

// Message is a message record, decoded (PROTOCOL.md §3.5).
type Message struct {
	ID     string
	LogID  int64
	RoomID string
	// PrevLogID is the message's previous snapshot, 0 for a new message;
	// PrevRoomID is set when the snapshot moved it from that room.
	PrevLogID  int64
	PrevRoomID string
	// From is the author: a user, or a system identity such as ~room.
	From Sender
	// Deleted marks a deletion's tombstone, which has no body.
	Deleted bool
	// Text and Format are the body's: Format is "plain" or "markdown".
	Text, Format string
	// ReplyTo is the message_id it replies to, if any.
	ReplyTo string
	// Mentions are the user_ids its body mentions (§3.5.5).
	Mentions []string
	// Body is the whole body, and Ext the message's ext (§4.12).
	Body map[string]any
	Ext  map[string]jsontext.Value
}

// Sender is who a record is from.
type Sender struct {
	UserID string
	Name   string
}

// New reports a message's first snapshot.
func (m Message) New() bool { return m.PrevLogID == 0 }

// MentionsUser reports whether the message mentions userID.
func (m Message) MentionsUser(userID string) bool { return slices.Contains(m.Mentions, userID) }

// DecodeMessage decodes the record of an EventMessage.
func DecodeMessage(e Event) (Message, bool) {
	if e.Kind != EventMessage {
		return Message{}, false
	}
	return decodeMessageRecord(e.Record)
}

// decodeMessageRecord decodes a message record, as history and broadcasts
// carry it.
func decodeMessageRecord(raw []byte) (Message, bool) {
	record := decodeObject(raw)
	if record == nil {
		return Message{}, false
	}
	str := func(object map[string]any, name string) string {
		value, _ := object[name].(string)
		return value
	}
	id := func(value string) int64 {
		n, _ := strconv.ParseInt(value, 10, 64)
		return n
	}
	m := Message{
		ID: str(record, "message_id"), LogID: id(str(record, "log_id")), RoomID: str(record, "room_id"),
		PrevLogID: id(str(record, "prev_log_id")), PrevRoomID: str(record, "prev_room_id"),
		Deleted: record["deleted"] == true,
	}
	if m.ID == "" {
		return Message{}, false
	}
	if from, ok := record["from"].(map[string]any); ok {
		m.From = Sender{UserID: str(from, "user_id"), Name: str(from, "name")}
	}
	if reply, ok := record["reply_to"].(map[string]any); ok {
		m.ReplyTo = str(reply, "message_id")
	}
	if body, ok := record["body"].(map[string]any); ok {
		m.Body = body
		m.Text, m.Format = str(body, "text"), str(body, "format")
		if m.Format == "" {
			m.Format = "plain"
		}
		m.Mentions = mentions(body)
	}
	m.Ext = extOf(record["ext"])
	return m, true
}
