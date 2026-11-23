package rooms

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"iter"
	"maps"
	"slices"

	"github.com/apron-chat/apron-server-go/apron/internal/roomlog"
	"github.com/apron-chat/apron-server-go/store"
	"github.com/apron-chat/apron-server-go/wire"
)

// Message is a message as rooms keep it (§3.5, §4.4): the room it is in,
// who sent it, every snapshot it logged, the last of which is current, and
// its reaction sets (§4.7), of which only non-empty ones are kept.
type Message struct {
	id string
	// from is the author identity of every snapshot; owner is the user_id
	// who may edit it.
	from  roomlog.Author
	owner string
	// room is the room_id of the room it is in, and logID its current
	// snapshot's log_id; both are empty before its first snapshot.
	room      string
	logID     int64
	reactions map[string]roomlog.Reaction
	// records are every logged snapshot, for redaction; the last is current.
	records []*roomlog.Record
}

// MessageHolder is the type of the values a Table keeps for messages: a
// message's state, which holds its Message. A *Message is one by itself,
// and so is a struct embedding one.
type MessageHolder interface {
	comparable
	// Kept is the message as the table keeps it.
	Kept() *Message
}

// Kept is m itself, which makes a *Message a MessageHolder.
func (m *Message) Kept() *Message { return m }

func newMessage(id string, from roomlog.Author, owner string) *Message {
	return &Message{id: id, from: from, owner: owner, reactions: make(map[string]roomlog.Reaction)}
}

// NewMessage keeps a new message named id, which owner sends as from,
// before its first snapshot, which stores it.
func (t *Table[U, M]) NewMessage(id string, from roomlog.Author, owner string) M {
	m := t.hold(newMessage(id, from, owner))
	t.messages[id] = m
	return m
}

// Message returns the message named id, or the zero M.
func (t *Table[U, M]) Message(id string) M { return t.messages[id] }

// VisibleMessage returns the message named id if u may see the room it is
// in, or the zero M.
func (t *Table[U, M]) VisibleMessage(id string, u U) M {
	if m, ok := t.messages[id]; ok && t.rooms[m.Kept().room].VisibleTo(u) {
		return m
	}
	var none M
	return none
}

// Messages lists the messages by message_id, in no order.
func (t *Table[U, M]) Messages() iter.Seq2[string, M] { return maps.All(t.messages) }

// ID is the message's message_id.
func (m *Message) ID() string { return m.id }

// From is the author of every snapshot of the message.
func (m *Message) From() roomlog.Author { return m.from }

// Owner is the user_id of the user who may edit the message.
func (m *Message) Owner() string { return m.owner }

// RoomID is the room_id of the room the message is in.
func (m *Message) RoomID() string { return m.room }

// Records are every snapshot the message logged, the current one last. The
// caller does not modify them.
func (m *Message) Records() []*roomlog.Record { return m.records }

// Current decodes the message's current snapshot, a copy the caller owns.
func (m *Message) Current() *roomlog.Message {
	if snapshot := roomlog.DecodeMessage(m.records[len(m.records)-1].Raw); snapshot != nil {
		return snapshot
	}
	return &roomlog.Message{MessageID: m.id}
}

// Arrival is the log_id at which the message arrived in the room id: its
// first snapshot in the room's log, a move snapshot for a message moved
// there, and 0 if it never was there.
func (m *Message) Arrival(id string) int64 {
	for _, record := range m.records {
		if slices.Contains(record.Rooms, id) {
			return record.ID
		}
	}
	return 0
}

// Emojis is the reaction set of the user id, which the caller does not
// modify.
func (m *Message) Emojis(id string) []string { return m.reactions[id].Emojis }

// SameEmojis reports whether the reaction set of the user id holds exactly
// emojis, which hold no entry twice, in any order.
func (m *Message) SameEmojis(id string, emojis []string) bool {
	kept := m.reactions[id].Emojis
	return len(kept) == len(emojis) && !slices.ContainsFunc(emojis, func(e string) bool { return !slices.Contains(kept, e) })
}

// Reactions lists every non-empty reaction set, ordered by user_id.
func (m *Message) Reactions() []roomlog.Reaction {
	reactions := make([]roomlog.Reaction, 0, len(m.reactions))
	for _, id := range slices.Sorted(maps.Keys(m.reactions)) {
		set := m.reactions[id]
		reactions = append(reactions, roomlog.Reaction{From: set.From, Emojis: slices.Clone(set.Emojis)})
	}
	return reactions
}

// React replaces the reaction set of the user from with emojis, removing it
// when emojis is empty, and logs the change as a reactions record in the
// message's room, which it returns with the room.
func (t *Table[U, M]) React(m *Message, from roomlog.Author, emojis []string) (*roomlog.Record, *Room[U]) {
	t.dirtyMessages[m.id] = m
	if len(emojis) == 0 {
		delete(m.reactions, from.UserID)
	} else {
		m.reactions[from.UserID] = roomlog.Reaction{From: from, Emojis: emojis}
	}
	return t.LogReactions(m, []roomlog.Reaction{{From: from, Emojis: slices.Clone(emojis)}})
}

// LogReactions logs a reactions record of the given sets in the message's
// room, which it returns with the room. Reaction sets carry no prev_log_id
// (§4.3.1).
func (t *Table[U, M]) LogReactions(m *Message, reactions []roomlog.Reaction) (*roomlog.Record, *Room[U]) {
	r := t.rooms[m.room]
	record := roomlog.NewReactions(t.log.NextID(), m.id, m.room, reactions)
	t.Append(record, r)
	return record, r
}

// Snapshot logs a snapshot of m at logID, in the room it names and, for a
// move, the room m was in too, and returns its record with the rooms whose
// logs hold it, and whether it moved m. It sets the snapshot's prev_log_id
// to m's current one, if any, and its prev_room_id, for a move, to the room
// m was in, since the previous snapshot is in that room's log (§4.3.1);
// but a move out of a room that some who see the destination cannot see,
// such as a private one, does not name it to them. A new message makes
// its sender more recently active in its room (§4.2.1).
func (t *Table[U, M]) Snapshot(m *Message, snapshot *roomlog.Message, logID int64) (record *roomlog.Record, rooms []*Room[U], moved bool) {
	snapshot.PrevLogID, snapshot.PrevRoomID = "", ""
	if m.logID != 0 {
		snapshot.PrevLogID = roomlog.FormatID(m.logID)
	}
	destination := t.rooms[snapshot.RoomID]
	rooms = []*Room[U]{destination}
	moved = m.room != "" && m.room != destination.id
	if moved {
		source := t.rooms[m.room]
		rooms = []*Room[U]{source, destination}
		if !destination.RevealsTo(source) {
			snapshot.PrevRoomID = m.room
		}
	}
	if len(m.records) == 0 {
		t.Posted(destination, m.owner, logID)
	}
	m.logID = logID
	m.room = destination.id
	t.dirtyMessages[m.id] = m
	record = roomlog.NewMessage(logID, snapshot)
	m.records = append(m.records, record)
	t.Append(record, rooms...)
	return record, rooms, moved
}

// Redact rewrites every snapshot of a deleted message into a tombstone at
// its original log_id (§4.4). At an equal log_id a tombstone wins (§2.3),
// so a client that kept one of them as it was replaces it with the
// redacted one it reads later.
func (t *Table[U, M]) Redact(m *Message) {
	for _, record := range m.records {
		t.log.Redact(record)
	}
}

// MessageEntryKind is the kind of the store entries that hold messages,
// each named by its message_id. The records of their snapshots are the
// log's (roomlog).
const MessageEntryKind = "message"

// storedMessage is a message as the store keeps it.
type storedMessage struct {
	From      roomlog.Author            `json:"from"`
	LogID     int64                     `json:"log_id"`
	Owner     string                    `json:"owner"`
	RoomID    string                    `json:"room_id"`
	Reactions map[string]storedReaction `json:"reactions,omitzero"`
	Records   []int64                   `json:"records"`
}

type storedReaction struct {
	From   roomlog.Author `json:"from"`
	Emojis []string       `json:"emojis"`
}

// MessageEntry is the store entry that keeps m.
func MessageEntry(m *Message) store.Entry {
	s := storedMessage{From: m.from, LogID: m.logID, Owner: m.owner, RoomID: m.room}
	for _, record := range m.records {
		s.Records = append(s.Records, record.ID)
	}
	if len(m.reactions) > 0 {
		s.Reactions = make(map[string]storedReaction, len(m.reactions))
		for id, set := range m.reactions {
			s.Reactions[id] = storedReaction{From: set.From, Emojis: set.Emojis}
		}
	}
	value, _ := wire.Marshal(s)
	return store.Entry{Kind: MessageEntryKind, ID: m.id, Value: value}
}

// RestoreMessages rebuilds the messages from the store's message entries,
// with their snapshots among records, the log's records by log_id, once
// the rooms and the log are restored. A message none of whose records is
// kept cannot be served, and is left out.
func (t *Table[U, M]) RestoreMessages(entries map[string]jsontext.Value, records map[int64]*roomlog.Record) error {
	for id, raw := range entries {
		m, err := restoreMessage(id, raw, records)
		if err != nil {
			return err
		}
		if m != nil {
			t.messages[id] = t.hold(m)
		}
	}
	return nil
}

// restoreMessage rebuilds the message named id from its store entry, with
// the snapshots among records. It returns nil for a message none of whose
// records is kept.
func restoreMessage(id string, raw jsontext.Value, records map[int64]*roomlog.Record) (*Message, error) {
	var s storedMessage
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("stored %s %q: %w", MessageEntryKind, id, err)
	}
	m := newMessage(id, s.From, s.Owner)
	m.logID, m.room = s.LogID, s.RoomID
	for userID, set := range s.Reactions {
		m.reactions[userID] = roomlog.Reaction{From: set.From, Emojis: set.Emojis}
	}
	for _, logID := range s.Records {
		if record := records[logID]; record != nil {
			record.Message = id
			m.records = append(m.records, record)
		}
	}
	if len(m.records) == 0 {
		return nil, nil
	}
	return m, nil
}
