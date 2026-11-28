// Package rooms keeps a server's rooms (PROTOCOL.md §4.2): each room's
// current record and log, its place among threads, its members and their
// read cursors (§4.6), and the rules of who may see a room (§4.2.4) and of
// whom a record naming one room would show it to; and its messages by
// message_id (Message): each one's room, snapshots, and reaction sets
// (§4.7). It logs the records it makes in the server's log (roomlog):
// rooms, memberships, message snapshots, and reactions. It tracks the rooms
// and messages it changes, renders them as store entries, and restores
// them.
//
// A Table knows its members as values of the type U, such as the server's
// user state, through the methods of Member, and keeps each message as a
// value of the type M, a Message with whatever else the server keeps of it
// (MessageHolder). It knows nothing of connections, delivery, or what a
// message says: the server builds snapshots, and sends what a change
// returns. It is not safe for concurrent use: every method is called
// holding the server's lock.
package rooms

import (
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"iter"
	"maps"
	"math"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/aprond/internal/roomlog"
	"github.com/apron-chat/apron-server-go/store"
)

// Member is a user as rooms know them.
type Member interface {
	comparable
	// MemberID is the user's user_id.
	MemberID() string
	// MemberScope is the rooms the user is limited to, nil for none.
	MemberScope() *Scope
	// MemberFrom is the user as records carry it: user_id and name.
	MemberFrom() roomlog.Author
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
	// encoded is the latest record as EncodedRecord last encoded it, for
	// the record of its log_id: replaced, never changed, so that readers
	// under a read lock share it.
	encoded atomic.Pointer[encodedRecord]
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
	// listed is the latest ListedMemberIDs, nil once it may have changed.
	listed atomic.Pointer[listing]
	// membersRecord is the record of every member that Retain logged
	// last, 0 for none since the server started.
	membersRecord int64
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

// encodedRecord is a room record as encode made it, of the record of
// logID.
type encodedRecord struct {
	logID int64
	raw   jsontext.Value
}

// EncodedRecord returns r's latest room record as encode makes it, kept
// from the last call while the record is the same, for a listing that
// sends it as it is (apron.RoomView.Raw). The caller does not change it.
// It may be called while the caller holds a read lock, as others do.
func (r *Room[U]) EncodedRecord(encode func(roomlog.RoomRecord) jsontext.Value) jsontext.Value {
	if e := r.encoded.Load(); e != nil && e.logID == r.recordLogID {
		return e.raw
	}
	raw := encode(r.record)
	r.encoded.Store(&encodedRecord{r.recordLogID, raw})
	return raw
}

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
// positive, the most recently active, and then truncated is set. The
// caller does not modify them: they are kept until the members, or, when
// truncated, their activity, change, as every sign-in lists them.
func (r *Room[U]) ListedMemberIDs(limit int) (ids []string, truncated bool) {
	if l := r.listed.Load(); l != nil && l.limit == limit {
		return l.ids, l.truncated
	}
	ids = slices.Collect(maps.Keys(r.members))
	if limit > 0 && len(ids) > limit {
		slices.SortFunc(ids, func(a, b string) int {
			return cmp.Or(cmp.Compare(r.active[b], r.active[a]), cmp.Compare(a, b))
		})
		ids, truncated = ids[:limit], true
	}
	slices.Sort(ids)
	// Listings run holding the server's lock for reading, so several may
	// keep one at once: each is the same.
	r.listed.Store(&listing{limit: limit, ids: slices.Clip(ids), truncated: truncated})
	return ids, truncated
}

// listing is a ListedMemberIDs result kept.
type listing struct {
	limit     int
	ids       []string
	truncated bool
}

// Listing selects what room_list lists of some rooms to a user (§4.2.1).
type Listing struct {
	// Joined and NotJoined are the arrays the listing wants.
	Joined, NotJoined bool
	// Since, with HasSince, lists only the rooms whose latest_log_id is
	// greater, and those the user left after it.
	Since    int64
	HasSince bool
	// Threads lists unjoined threads, as a listing of one room or of a
	// room's threads does; others leave them to their parent's listing.
	Threads bool
	// Limit bounds the unjoined rooms listed, the most recently active;
	// the joined are never truncated.
	Limit int
}

// List sorts rooms for u as l selects them: the rooms u joined, the
// unjoined rooms u may see, and, with Since, the rooms u left after it, as
// leftAt reports the log_id of u's latest leave of a room; each most
// recently active first. A left room may be among the unjoined too.
func List[U Member](u U, rooms []*Room[U], l Listing, leftAt func(id string) int64) (joined, notJoined, left []*Room[U]) {
	id := u.MemberID()
	for _, r := range rooms {
		if l.HasSince && r.log.Latest() <= l.Since {
			continue
		}
		if _, member := r.members[id]; member {
			if l.Joined {
				joined = append(joined, r)
			}
			continue
		}
		if l.HasSince && l.Joined && leftAt(r.id) > l.Since {
			left = append(left, r)
		}
		// A private room is listed only to its members.
		if l.NotJoined && (l.Threads || r.parent == nil) && r.VisibleTo(u) {
			notJoined = append(notJoined, r)
		}
	}
	byActivity := func(a, b *Room[U]) int {
		return cmp.Or(cmp.Compare(b.log.Latest(), a.log.Latest()), cmp.Compare(b.log.Created(), a.log.Created()))
	}
	slices.SortFunc(joined, byActivity)
	slices.SortFunc(notJoined, byActivity)
	slices.SortFunc(left, byActivity)
	if l.Limit > 0 && len(notJoined) > l.Limit {
		notJoined = notJoined[:l.Limit]
	}
	return joined, notJoined, left
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

const (
	maxThreadTitleRunes = 60
	defaultThreadTitle  = "Thread"
)

// ThreadTitle is the title the server gives a thread made without one, so
// that clients unaware of parent_room_id render it (§4.2.4): the first
// line of its description, cut at maxThreadTitleRunes, or
// defaultThreadTitle.
func ThreadTitle(description string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(description), "\n")
	line = strings.TrimSpace(line)
	if runes := []rune(line); len(runes) > maxThreadTitleRunes {
		line = strings.TrimSpace(string(runes[:maxThreadTitleRunes])) + "…"
	}
	if line == "" {
		return defaultThreadTitle
	}
	return line
}

// Table is the server's rooms by room_id, and its messages by message_id.
type Table[U Member, M MessageHolder] struct {
	log   *roomlog.Log
	rooms map[string]*Room[U]
	// messages are every message, and hold, wraps a new or restored one in
	// the server's M.
	messages map[string]M
	hold     func(*Message) M
	// dirty are the rooms, and dirtyMessages the messages, changed since
	// Entries last took them.
	dirty         map[string]struct{}
	dirtyMessages map[string]*Message
	// forgottenMessages are the messages Forget let go of since Entries
	// last took them, whose entries are deleted.
	forgottenMessages []string
	// later are the large rooms changed since Entries last took them only
	// in what their entry may lag behind (touchLater), since laterSince.
	// laterMessages are the messages with many reactions changed since
	// then only in their reactions (touchMessageLater).
	later         map[string]struct{}
	laterMessages map[string]*Message
	laterSince    time.Time
	// restoring are the stored members of each room, from Restore until
	// RestoreMembers.
	restoring map[string][]string
}

// NewTable returns an empty table whose records go to log, which keeps
// each message as hold wraps it.
func NewTable[U Member, M MessageHolder](log *roomlog.Log, hold func(*Message) M) *Table[U, M] {
	return &Table[U, M]{
		log: log, rooms: make(map[string]*Room[U]), messages: make(map[string]M), hold: hold,
		dirty: make(map[string]struct{}), dirtyMessages: make(map[string]*Message),
		later: make(map[string]struct{}), laterMessages: make(map[string]*Message),
	}
}

// Get returns the room named id, or nil.
func (t *Table[U, M]) Get(id string) *Room[U] { return t.rooms[id] }

// Visible returns the room named id if u may see it, or nil.
func (t *Table[U, M]) Visible(id string, u U) *Room[U] {
	if r := t.rooms[id]; r != nil && r.VisibleTo(u) {
		return r
	}
	return nil
}

// All lists the rooms, oldest first.
func (t *Table[U, M]) All() []*Room[U] {
	return slices.SortedFunc(maps.Values(t.rooms), func(a, b *Room[U]) int {
		return cmp.Compare(a.log.Created(), b.log.Created())
	})
}

// Touch marks r as changed, for a change the table does not make itself.
func (t *Table[U, M]) Touch(r *Room[U]) { t.dirty[r.id] = struct{}{} }

// LaterMembers is the size, in members and read cursors, from which a
// room's entry is rendered at most once every EntryDelay for what it may
// lag behind (EntriesDue): the entry lists every member, their activity,
// and their read cursors, so rendering it for each message in a large room
// costs as much as the room is large, and the server's lock is held while
// it is rendered.
const LaterMembers = 64

// EntryDelay is how long a large room's entry may lag behind its
// latest_log_id, its members' activity, and their read cursors.
const EntryDelay = 200 * time.Millisecond

// touchLater marks r as changed in its latest_log_id, a member's activity
// (§4.2.1), or a read cursor (§4.6): what its entry may lag behind for a
// while when the room is large, since the records that changed them are
// stored at once. A room that lags so is restored with the latest_log_id
// of its latest stored record (roomlog.Log.Restore), and with the activity
// and cursors its entry last kept, as after a restart a moment earlier.
func (t *Table[U, M]) touchLater(r *Room[U]) {
	if len(r.members)+len(r.reads) < LaterMembers {
		t.Touch(r)
		return
	}
	if _, now := t.dirty[r.id]; !now {
		t.later[r.id] = struct{}{}
	}
}

// Create logs the creation record of a room holding the client fields of
// fields: title, description, and ext. The room is named id, or by its
// creation log_id when id is "", is a thread of parent, if any, is private
// if asked, and was made by the user creator, if any.
func (t *Table[U, M]) Create(id string, parent *Room[U], private bool, creator string, fields roomlog.RoomRecord) *Room[U] {
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
func (t *Table[U, M]) Update(r *Room[U], fields roomlog.RoomRecord) {
	t.commit(r, t.log.NextID(), fields)
}

func (t *Table[U, M]) commit(r *Room[U], logID int64, fields roomlog.RoomRecord) {
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
func (t *Table[U, M]) Append(record *roomlog.Record, rooms ...*Room[U]) {
	t.appendRecord(record, t.Touch, rooms)
}

// appendLater is Append for a record that changes nothing of its rooms'
// entries but their latest_log_id, such as a message snapshot or a
// reactions record (touchLater).
func (t *Table[U, M]) appendLater(record *roomlog.Record, rooms ...*Room[U]) {
	t.appendRecord(record, t.touchLater, rooms)
}

func (t *Table[U, M]) appendRecord(record *roomlog.Record, touch func(*Room[U]), rooms []*Room[U]) {
	logs := make([]*roomlog.Timeline, len(rooms))
	for i, r := range rooms {
		logs[i] = r.log
		touch(r)
	}
	t.log.Append(record, logs...)
}

// Join makes u, recorded as from, a member of r, and logs the membership
// record (§4.2.2), which it returns. It does nothing and reports false
// when u is a member already, or r is outside u's scope.
func (t *Table[U, M]) Join(r *Room[U], u U, from roomlog.Author) (*roomlog.Record, bool) {
	id := u.MemberID()
	if _, member := r.members[id]; member || !r.InScope(u.MemberScope()) {
		return nil, false
	}
	r.members[id] = u
	r.listed.Store(nil)
	record := t.logMembership(r, from, true)
	r.active[id] = record.ID
	return record, true
}

// Leave takes u, recorded as from, out of r, and logs the membership
// record (§4.2.2), which it returns. It does nothing and reports false
// when u is not a member.
func (t *Table[U, M]) Leave(r *Room[U], u U, from roomlog.Author) (*roomlog.Record, bool) {
	id := u.MemberID()
	if _, member := r.members[id]; !member {
		return nil, false
	}
	record := t.logMembership(r, from, false)
	delete(r.members, id)
	r.listed.Store(nil)
	delete(r.active, id)
	return record, true
}

// logMembership logs one membership record of the user from in r,
// advancing the room's latest_log_id. The record carries the user as a
// recorded object: user_id and name.
func (t *Table[U, M]) logMembership(r *Room[U], from roomlog.Author, joined bool) *roomlog.Record {
	record := roomlog.NewMembership(t.log.NextID(), r.id, from, joined)
	// Restoring applies memberships over the room's entry, which may lag
	// behind them (RestoreMembers).
	t.appendLater(record, r)
	return record
}

// Posted notes the first snapshot of a message the user id posted in r at
// logID, which makes a member more recently active (§4.2.1).
func (t *Table[U, M]) Posted(r *Room[U], id string, logID int64) {
	if _, member := r.members[id]; member {
		r.active[id] = logID
		if l := r.listed.Load(); l != nil && l.truncated {
			r.listed.Store(nil)
		}
		t.touchLater(r)
	}
}

// Read moves the user's read cursor in r to cursor, if it is past the one
// kept (§4.6), and reports whether it moved.
func (t *Table[U, M]) Read(r *Room[U], id string, cursor Cursor) bool {
	if kept, ok := r.reads[id]; ok && cursor.ID <= kept.ID {
		return false
	}
	r.reads[id] = cursor
	t.touchLater(r)
	return true
}

// ForgetRead forgets the user's read cursor in r.
func (t *Table[U, M]) ForgetRead(r *Room[U], id string) {
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
	// Retained is where the room's retained history starts once a part of
	// its log was discarded (roomlog.Timeline.Discard).
	Retained int64 `json:"retained_id,omitzero"`
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
	s.Retained = r.log.Retained()
	if r.parent != nil {
		s.Parent = r.parent.id
	}
	if len(r.reads) > 0 {
		s.Reads = make(map[string]storedCursor, len(r.reads))
		for id, cursor := range r.reads {
			s.Reads[id] = storedCursor{From: cursor.From, MessageID: cursor.MessageID, ID: cursor.ID}
		}
	}
	value, _ := apron.Marshal(s)
	return store.Entry{Kind: EntryKind, ID: r.id, Value: value}
}

// Entries returns the entries of the rooms and messages changed since it
// was last called.
func (t *Table[U, M]) Entries() []store.Entry {
	return t.entries(true)
}

// EntriesDue is Entries but for the large rooms that changed only in what
// their entries may lag behind (touchLater), which it leaves out until
// EntryDelay has passed since the first of them changed, as of now.
func (t *Table[U, M]) EntriesDue(now time.Time) []store.Entry {
	lagging := t.lagging()
	if lagging && t.laterSince.IsZero() {
		t.laterSince = now
	}
	return t.entries(lagging && now.Sub(t.laterSince) >= EntryDelay)
}

func (t *Table[U, M]) lagging() bool { return len(t.later) > 0 || len(t.laterMessages) > 0 }

// Lagging reports when the entries EntriesDue leaves out are due, and
// whether there are any.
func (t *Table[U, M]) Lagging() (time.Time, bool) {
	if !t.lagging() {
		return time.Time{}, false
	}
	return t.laterSince.Add(EntryDelay), true
}

func (t *Table[U, M]) entries(withLater bool) []store.Entry {
	if withLater {
		for id := range t.later {
			t.dirty[id] = struct{}{}
		}
		maps.Copy(t.dirtyMessages, t.laterMessages)
		clear(t.later)
		clear(t.laterMessages)
		t.laterSince = time.Time{}
	} else {
		for id := range t.dirty {
			delete(t.later, id)
		}
		for id := range t.dirtyMessages {
			delete(t.laterMessages, id)
		}
		if !t.lagging() {
			t.laterSince = time.Time{}
		}
	}
	if len(t.dirty) == 0 && len(t.dirtyMessages) == 0 && len(t.forgottenMessages) == 0 {
		return nil
	}
	entries := make([]store.Entry, 0, len(t.dirty)+len(t.dirtyMessages)+len(t.forgottenMessages))
	for _, id := range t.forgottenMessages {
		entries = append(entries, store.Entry{Kind: MessageEntryKind, ID: id})
	}
	t.forgottenMessages = t.forgottenMessages[:0]
	for id := range t.dirty {
		if r := t.rooms[id]; r != nil {
			entries = append(entries, Entry(r))
		}
	}
	for _, m := range t.dirtyMessages {
		entries = append(entries, MessageEntry(m))
	}
	clear(t.dirty)
	clear(t.dirtyMessages)
	return entries
}

// Restore rebuilds the rooms from the store's room entries, without their
// members, whom RestoreMembers adds once the users are restored. The
// rooms' logs are empty until the server's log is restored into them.
func (t *Table[U, M]) Restore(entries map[string]jsontext.Value) error {
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
			log:     restoredTimeline(id, s),
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
//
// A large room's entry may lag behind its memberships (touchLater), so
// its membership records are applied over the members its entry lists, in
// order, each join or leave replacing its user's: each user ends as their
// latest record left them, which an entry that is up to date lists
// already.
func (t *Table[U, M]) RestoreMembers(member func(r *Room[U], id string) (U, bool)) error {
	for _, r := range t.All() {
		ids := make(map[string]bool, len(t.restoring[r.id]))
		for _, id := range t.restoring[r.id] {
			ids[id] = true
		}
		for _, record := range r.log.Since(0, math.MaxInt) {
			if record.Kind != roomlog.KindMembership {
				continue
			}
			var m roomlog.Membership
			if err := json.Unmarshal(record.Raw, &m); err != nil {
				return fmt.Errorf("stored membership record %d: %w", record.ID, err)
			}
			for _, change := range m.Members {
				id := change.User.UserID
				switch {
				case change.Joined && !ids[id]:
					ids[id] = true
					r.active[id] = max(r.active[id], record.ID)
				case !change.Joined && ids[id]:
					delete(ids, id)
					delete(r.active, id)
				}
			}
		}
		for _, id := range slices.Sorted(maps.Keys(ids)) {
			if u, ok := member(r, id); ok {
				r.members[id] = u
			}
		}
		r.listed.Store(nil)
	}
	t.restoring = nil
	return nil
}

// restoredTimeline is the log of a room as its entry keeps it, without
// its records.
func restoredTimeline(id string, s stored) *roomlog.Timeline {
	t := roomlog.NewTimeline(id, s.CreatedID, s.LatestID)
	t.RestoreRetained(s.Retained)
	return t
}

// Retain discards the records of r's log before the log_id before, but
// never its latest record (§4.3.3): r's history_log_id moves to before,
// or to its latest record. When the part discarded holds memberships, one
// membership record of every current member, each joined, is logged after
// it, as it must be before such a part goes. It returns the records
// discarded, which Forget takes out of the log once no room holds them.
func (t *Table[U, M]) Retain(r *Room[U], before int64) []*roomlog.Record {
	before = min(before, r.log.Latest())
	discarded := r.log.Discard(before)
	if len(discarded) == 0 {
		return nil
	}
	t.Touch(r)
	// A record of the members logged by an earlier Retain that the log
	// still holds serves for this part too: from it on, the log has every
	// change of who is in the room, as when retention discards in passes.
	if len(r.members) > 0 && r.membersRecord < r.log.HistoryID() && slices.ContainsFunc(discarded, func(record *roomlog.Record) bool { return record.Kind == roomlog.KindMembership }) {
		ids := slices.Sorted(maps.Keys(r.members))
		users := make([]roomlog.Author, len(ids))
		for i, id := range ids {
			users[i] = r.members[id].MemberFrom()
		}
		record := roomlog.NewMembers(t.log.NextID(), r.id, users)
		t.Append(record, r)
		r.membersRecord = record.ID
	}
	return discarded
}

// Forget takes the records discarded from rooms' logs (Retain) that no
// room holds any more out of the server's log and the store, and with
// them the messages none of whose snapshots is held any more, which it
// returns: the server lets go of what it keeps of them.
func (t *Table[U, M]) Forget(discarded []*roomlog.Record) []M {
	gone := make(map[*roomlog.Record]bool)
	for _, record := range discarded {
		held := false
		for _, id := range record.Rooms {
			if r := t.rooms[id]; r != nil && r.log.HistoryID() <= record.ID {
				held = true
				break
			}
		}
		if !held {
			gone[record] = true
		}
	}
	t.log.Forget(gone)
	var forgotten []M
	for record := range gone {
		if record.Kind != roomlog.KindMessage {
			continue
		}
		holder, ok := t.messages[record.Message]
		if !ok {
			continue
		}
		m := holder.Kept()
		goneOf := 0
		for _, r := range m.records {
			if gone[r] {
				goneOf++
			}
		}
		switch {
		case goneOf == len(m.records):
			// The message keeps its snapshots, for what the server lets
			// go of with it, such as its uploads.
			delete(t.messages, m.id)
			delete(t.dirtyMessages, m.id)
			delete(t.laterMessages, m.id)
			t.forgottenMessages = append(t.forgottenMessages, m.id)
			forgotten = append(forgotten, holder)
		case goneOf > 0:
			m.records = slices.DeleteFunc(m.records, func(r *roomlog.Record) bool { return gone[r] })
			t.dirtyMessages[m.id] = m
		}
	}
	return forgotten
}
