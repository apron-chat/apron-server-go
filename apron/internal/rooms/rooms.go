// Package rooms keeps a server's rooms (PROTOCOL.md §4.2): each room's
// current record and log, its place among threads, its members and their
// read cursors (§4.6), and the rules of who may see a room (§4.2.4) and of
// whom a record naming one room would show it to; and each message's place
// in them (Message): its room, its snapshots, and its reaction sets (§4.7).
// It logs the records it makes in the server's log (roomlog): rooms,
// memberships, message snapshots, and reactions. It tracks the rooms it
// changes, and renders rooms and messages as store entries.
//
// A Table knows its members as values of the type U, such as the server's
// user state, through the methods of Member. It knows nothing of
// connections, delivery, or what a message says: the server builds
// snapshots, and sends what a change returns. It is not safe for
// concurrent use: every method is called holding the server's lock.
package rooms

import (
	"cmp"
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

// Member is a user as rooms know them.
type Member interface {
	comparable
	// MemberID is the user's user_id.
	MemberID() string
	// MemberScope is the rooms the user is limited to, nil for none.
	MemberScope() *Scope
}

// Room is a room's current record, its log, and its members. Every room,
// including threads (rooms with parent_room_id), is visible to every user,
// except that a private room and its threads are visible only to their
// members (§4.2.4), and a scoped user sees only its scope's rooms; members
// are the users who have joined it and receive its deliveries (§3.4,
// §4.2.2).
type Room[U Member] struct {
	id       string
	parent   *Room[U]
	children []*Room[U]
	// private is fixed at creation and kept in record as "private": true.
	private bool
	// record holds the latest logged room record.
	record      roomlog.RoomRecord
	recordLogID int64
	// log is the room's log, from its creation record.
	log     *roomlog.Timeline
	members map[string]U
	// active maps each member to the log_id of their latest join or new
	// message in the room, which ranks members when a listing truncates
	// them (§4.2.1).
	active map[string]int64
	// creator is the user_id that created the room, empty for the seeded
	// room; only the creator may /kick (§4.1).
	creator string
	// reads holds each user's latest read cursor (§4.6).
	reads map[string]Cursor
}

// Cursor is a user's read cursor in a room (§4.6): the message they read
// up to, and its log_id.
type Cursor struct {
	From      roomlog.Author
	MessageID string
	ID        int64
}

// ID is the room's room_id.
func (r *Room[U]) ID() string { return r.id }

// Parent is the room r is a thread of, nil for a top-level room.
func (r *Room[U]) Parent() *Room[U] { return r.parent }

// Children are r's threads, oldest first. The caller does not modify them.
func (r *Room[U]) Children() []*Room[U] { return r.children }

// Private reports whether r is private (§4.2.4), which is fixed when it is
// made.
func (r *Room[U]) Private() bool { return r.private }

// Record is r's latest room record.
func (r *Room[U]) Record() roomlog.RoomRecord { return r.record }

// Log is r's log.
func (r *Room[U]) Log() *roomlog.Timeline { return r.log }

// Creator is the user_id of the user who made r, "" for a seeded room.
func (r *Room[U]) Creator() string { return r.creator }

// Title is the room's display title, falling back to its room_id (§3.4).
func (r *Room[U]) Title() string {
	if r.record.Title != "" {
		return r.record.Title
	}
	return r.id
}

// Depth counts the rooms r is a thread of, at any depth.
func (r *Room[U]) Depth() int {
	n := 0
	for room := r.parent; room != nil; room = room.parent {
		n++
	}
	return n
}

// Member returns the member of r with the user_id id, or the zero U.
func (r *Room[U]) Member(id string) U { return r.members[id] }

// Members lists r's members by user_id, in no order.
func (r *Room[U]) Members() iter.Seq2[string, U] { return maps.All(r.members) }

// MemberCount counts r's members.
func (r *Room[U]) MemberCount() int { return len(r.members) }

// Active is the log_id of the member's latest join or new message in r.
func (r *Room[U]) Active(id string) int64 { return r.active[id] }

// Read returns the read cursor kept for the user id in r.
func (r *Room[U]) Read(id string) (Cursor, bool) {
	cursor, ok := r.reads[id]
	return cursor, ok
}

// Reads lists the read cursors kept in r, by user_id in order.
func (r *Room[U]) Reads() iter.Seq2[string, Cursor] {
	return func(yield func(string, Cursor) bool) {
		for _, id := range slices.Sorted(maps.Keys(r.reads)) {
			if !yield(id, r.reads[id]) {
				return
			}
		}
	}
}

// VisibleTo reports whether u may see r (§4.2.4): r must be in u's scope,
// and u a member of every private room on the way up from r, r included,
// or r is invisible, and answered like an unknown room. So a thread of a
// private room is hidden from everyone outside that room, members of the
// thread included.
func (r *Room[U]) VisibleTo(u U) bool {
	if !r.InScope(u.MemberScope()) {
		return false
	}
	id := u.MemberID()
	for room := r; room != nil; room = room.parent {
		if _, member := room.members[id]; room.private && !member {
			return false
		}
	}
	return true
}

// Audience is who can see r: everyone, or, for a private room or a thread
// of one, the users in every private room on the way up.
func (r *Room[U]) Audience() (everyone bool, users map[string]U) {
	for room := r; room != nil; room = room.parent {
		if !room.private {
			continue
		}
		if users == nil {
			users = maps.Clone(room.members)
			continue
		}
		for id := range users {
			if _, member := room.members[id]; !member {
				delete(users, id)
			}
		}
	}
	return users == nil, users
}

// RevealsTo reports whether a record in r, such as a move snapshot or a
// reply naming a message in to, would reach someone who cannot see to.
func (r *Room[U]) RevealsTo(to *Room[U]) bool {
	toEveryone, toUsers := to.Audience()
	if toEveryone {
		return false
	}
	everyone, users := r.Audience()
	if everyone {
		return true
	}
	for id := range users {
		if _, ok := toUsers[id]; !ok {
			return true
		}
	}
	return false
}

// ListedMemberIDs returns the user_ids of the members that r lists
// (§4.2.1), ordered by user_id: all of them, or, past limit when it is
// positive, the most recently active, and then truncated is set.
func (r *Room[U]) ListedMemberIDs(limit int) (ids []string, truncated bool) {
	ids = slices.Collect(maps.Keys(r.members))
	if limit > 0 && len(ids) > limit {
		slices.SortFunc(ids, func(a, b string) int {
			return cmp.Or(cmp.Compare(r.active[b], r.active[a]), cmp.Compare(a, b))
		})
		ids, truncated = ids[:limit], true
	}
	slices.Sort(ids)
	return ids, truncated
}

// Scope limits a user, such as a bot, to some rooms: those it lists, and
// their threads if it takes threads in.
type Scope struct {
	rooms   map[string]bool
	threads bool
}

// NewScope returns the scope of the rooms ids, and their threads with
// threads.
func NewScope(ids []string, threads bool) *Scope {
	sc := &Scope{rooms: make(map[string]bool, len(ids)), threads: threads}
	for _, id := range ids {
		sc.rooms[id] = true
	}
	return sc
}

// Rooms lists the room_ids the scope names, in order, nil for no scope.
func (sc *Scope) Rooms() []string {
	if sc == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(sc.rooms))
}

// Threads reports whether the scope takes in the threads of its rooms.
func (sc *Scope) Threads() bool { return sc != nil && sc.threads }

// InScope reports whether r is in the scope sc: listed, or a thread of a
// listed room when the scope takes threads in. Every room is in no scope
// (nil).
func (r *Room[U]) InScope(sc *Scope) bool {
	if sc == nil {
		return true
	}
	for room := r; room != nil; room = room.parent {
		if sc.rooms[room.id] {
			return true
		}
		if !sc.threads {
			return false
		}
	}
	return false
}

// Table is the server's rooms by room_id.
type Table[U Member] struct {
	log   *roomlog.Log
	rooms map[string]*Room[U]
	// dirty are the rooms changed since Entries last took them.
	dirty map[string]struct{}
	// restoring are the stored members of each room, from Restore until
	// RestoreMembers.
	restoring map[string][]string
}

// NewTable returns an empty table whose records go to log.
func NewTable[U Member](log *roomlog.Log) *Table[U] {
	return &Table[U]{log: log, rooms: make(map[string]*Room[U]), dirty: make(map[string]struct{})}
}

// Get returns the room named id, or nil.
func (t *Table[U]) Get(id string) *Room[U] { return t.rooms[id] }

// Visible returns the room named id if u may see it, or nil.
func (t *Table[U]) Visible(id string, u U) *Room[U] {
	if r := t.rooms[id]; r != nil && r.VisibleTo(u) {
		return r
	}
	return nil
}

// All lists the rooms, oldest first.
func (t *Table[U]) All() []*Room[U] {
	return slices.SortedFunc(maps.Values(t.rooms), func(a, b *Room[U]) int {
		return cmp.Compare(a.log.Created(), b.log.Created())
	})
}

// Touch marks r as changed, for a change the table does not make itself.
func (t *Table[U]) Touch(r *Room[U]) { t.dirty[r.id] = struct{}{} }

// Create logs the creation record of a room holding the client fields of
// fields: title, description, and ext. The room is named id, or by its
// creation log_id when id is "", is a thread of parent, if any, is private
// if asked, and was made by the user creator, if any.
func (t *Table[U]) Create(id string, parent *Room[U], private bool, creator string, fields roomlog.RoomRecord) *Room[U] {
	logID := t.log.NextID()
	if id == "" {
		id = roomlog.FormatID(logID)
	}
	r := &Room[U]{
		id: id, parent: parent, private: private, creator: creator, log: roomlog.NewTimeline(id, logID, 0),
		members: make(map[string]U), active: make(map[string]int64), reads: make(map[string]Cursor),
	}
	t.rooms[id] = r
	if parent != nil {
		parent.children = append(parent.children, r)
	}
	t.commit(r, logID, fields)
	return r
}

// Update logs a room record of r holding the client fields of fields,
// which replace its own (§4.2.4).
func (t *Table[U]) Update(r *Room[U], fields roomlog.RoomRecord) {
	t.commit(r, t.log.NextID(), fields)
}

func (t *Table[U]) commit(r *Room[U], logID int64, fields roomlog.RoomRecord) {
	record := roomlog.RoomRecord{
		RoomID: r.id, LogID: roomlog.FormatID(logID), Private: r.private,
		Title: fields.Title, Description: fields.Description, Ext: fields.Ext,
	}
	if r.recordLogID != 0 {
		record.PrevLogID = roomlog.FormatID(r.recordLogID)
	}
	if r.parent != nil {
		record.ParentRoomID = r.parent.id
	}
	r.record = record
	r.recordLogID = logID
	t.Append(roomlog.New(logID, roomlog.KindRoom, record), r)
}

// Append adds a committed record to the log of each room it belongs to,
// whose latest_log_id it becomes.
func (t *Table[U]) Append(record *roomlog.Record, rooms ...*Room[U]) {
	logs := make([]*roomlog.Timeline, len(rooms))
	for i, r := range rooms {
		logs[i] = r.log
		t.Touch(r)
	}
	t.log.Append(record, logs...)
}

// Join makes u, recorded as from, a member of r, and logs the membership
// record (§4.2.2), which it returns. It does nothing and reports false
// when u is a member already, or r is outside u's scope.
func (t *Table[U]) Join(r *Room[U], u U, from roomlog.Author) (*roomlog.Record, bool) {
	id := u.MemberID()
	if _, member := r.members[id]; member || !r.InScope(u.MemberScope()) {
		return nil, false
	}
	r.members[id] = u
	record := t.logMembership(r, from, true)
	r.active[id] = record.ID
	return record, true
}

// Leave takes u, recorded as from, out of r, and logs the membership
// record (§4.2.2), which it returns. It does nothing and reports false
// when u is not a member.
func (t *Table[U]) Leave(r *Room[U], u U, from roomlog.Author) (*roomlog.Record, bool) {
	id := u.MemberID()
	if _, member := r.members[id]; !member {
		return nil, false
	}
	record := t.logMembership(r, from, false)
	delete(r.members, id)
	delete(r.active, id)
	return record, true
}

// logMembership logs one membership record of the user from in r,
// advancing the room's latest_log_id. The record carries the user as a
// recorded object: user_id and name.
func (t *Table[U]) logMembership(r *Room[U], from roomlog.Author, joined bool) *roomlog.Record {
	record := roomlog.NewMembership(t.log.NextID(), r.id, from, joined)
	t.Append(record, r)
	return record
}

// Posted notes the first snapshot of a message the user id posted in r at
// logID, which makes a member more recently active (§4.2.1).
func (t *Table[U]) Posted(r *Room[U], id string, logID int64) {
	if _, member := r.members[id]; member {
		r.active[id] = logID
		t.Touch(r)
	}
}

// Read moves the user's read cursor in r to cursor, if it is past the one
// kept (§4.6), and reports whether it moved.
func (t *Table[U]) Read(r *Room[U], id string, cursor Cursor) bool {
	if kept, ok := r.reads[id]; ok && cursor.ID <= kept.ID {
		return false
	}
	r.reads[id] = cursor
	t.Touch(r)
	return true
}

// ForgetRead forgets the user's read cursor in r.
func (t *Table[U]) ForgetRead(r *Room[U], id string) {
	if _, ok := r.reads[id]; ok {
		delete(r.reads, id)
		t.Touch(r)
	}
}

// EntryKind is the kind of the store entries that hold rooms, each named
// by its room_id.
const EntryKind = "room"

// stored is a room as the store keeps it.
type stored struct {
	Parent      string                  `json:"parent,omitzero"`
	Private     bool                    `json:"private,omitzero"`
	Active      map[string]int64        `json:"active,omitzero"`
	Record      roomlog.RoomRecord      `json:"record"`
	RecordLogID int64                   `json:"record_log_id"`
	CreatedID   int64                   `json:"created_id"`
	LatestID    int64                   `json:"latest_id"`
	Members     []string                `json:"members"`
	Creator     string                  `json:"creator,omitzero"`
	Reads       map[string]storedCursor `json:"reads,omitzero"`
}

type storedCursor struct {
	From      roomlog.Author `json:"from"`
	MessageID string         `json:"message_id"`
	ID        int64          `json:"id"`
}

// Entry is the store entry that keeps r.
func Entry[U Member](r *Room[U]) store.Entry {
	s := stored{
		Record:      r.record,
		RecordLogID: r.recordLogID,
		CreatedID:   r.log.Created(),
		LatestID:    r.log.Latest(),
		Members:     slices.Sorted(maps.Keys(r.members)),
		Creator:     r.creator,
		Private:     r.private,
		Active:      r.active,
	}
	if r.parent != nil {
		s.Parent = r.parent.id
	}
	if len(r.reads) > 0 {
		s.Reads = make(map[string]storedCursor, len(r.reads))
		for id, cursor := range r.reads {
			s.Reads[id] = storedCursor{From: cursor.From, MessageID: cursor.MessageID, ID: cursor.ID}
		}
	}
	value, _ := wire.Marshal(s)
	return store.Entry{Kind: EntryKind, ID: r.id, Value: value}
}

// Entries returns the entries of the rooms changed since it was last
// called.
func (t *Table[U]) Entries() []store.Entry {
	if len(t.dirty) == 0 {
		return nil
	}
	entries := make([]store.Entry, 0, len(t.dirty))
	for id := range t.dirty {
		if r := t.rooms[id]; r != nil {
			entries = append(entries, Entry(r))
		}
	}
	clear(t.dirty)
	return entries
}

// Restore rebuilds the rooms from the store's room entries, without their
// members, whom RestoreMembers adds once the users are restored. The
// rooms' logs are empty until the server's log is restored into them.
func (t *Table[U]) Restore(entries map[string]jsontext.Value) error {
	t.restoring = make(map[string][]string, len(entries))
	parents := make(map[string]string)
	for id, raw := range entries {
		var s stored
		if err := json.Unmarshal(raw, &s); err != nil {
			return fmt.Errorf("stored %s %q: %w", EntryKind, id, err)
		}
		if s.Active == nil {
			s.Active = make(map[string]int64)
		}
		r := &Room[U]{
			id: id, record: s.Record, recordLogID: s.RecordLogID,
			log:     roomlog.NewTimeline(id, s.CreatedID, s.LatestID),
			creator: s.Creator, private: s.Private, active: s.Active,
			members: make(map[string]U), reads: make(map[string]Cursor, len(s.Reads)),
		}
		for userID, cursor := range s.Reads {
			r.reads[userID] = Cursor{From: cursor.From, MessageID: cursor.MessageID, ID: cursor.ID}
		}
		t.rooms[id] = r
		t.restoring[id] = s.Members
		parents[id] = s.Parent
	}
	// Threads are listed oldest first, as they were made.
	for _, r := range t.All() {
		if parent := t.rooms[parents[r.id]]; parent != nil {
			r.parent = parent
			parent.children = append(parent.children, r)
		}
	}
	return nil
}

// RestoreMembers adds each room's stored members, oldest room first, as
// member returns them; one it reports false for, such as a user who is
// gone, is left out.
func (t *Table[U]) RestoreMembers(member func(r *Room[U], id string) (U, bool)) {
	for _, r := range t.All() {
		for _, id := range t.restoring[r.id] {
			if u, ok := member(r, id); ok {
				r.members[id] = u
			}
		}
	}
	t.restoring = nil
}
