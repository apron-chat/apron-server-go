package apron

import (
	"cmp"
	"maps"
	"slices"
)

// Replaces reports whether the snapshot m replaces kept, a snapshot of the
// same message, under replay (§2.3): it has a greater log_id, or the same
// one and is a tombstone, which wins at an equal log_id (§4.4), as a
// redacted snapshot does.
func (m Message) Replaces(kept Message) bool {
	return m.LogID > kept.LogID || m.LogID == kept.LogID && m.Deleted && !kept.Deleted
}

// Replay is the state that a log's records leave (§2.3): for each key, the
// record with the greatest log_id, whatever the source (live, history, or
// embedded) and the order they are applied in. Its keys are a room
// record's room_id, a message snapshot's message_id, a reaction set's
// (message_id, user_id) (§4.7), and a membership's (room_id, user_id)
// (§4.2.2): a reactions record or a membership record, compacted or not,
// replaces the entry of each user it carries.
//
// A client applies the records of history pages and of live deliveries,
// in any order, and ends in the same state (§4.3.5). The zero Replay is
// empty and ready to use. It is not safe for concurrent use.
type Replay struct {
	rooms       map[string]Room
	messages    map[LogID]Message
	reactions   map[reactionKey]reactionEntry
	memberships map[memberKey]memberEntry
	latest      LogID
}

type reactionKey struct {
	messageID LogID
	userID    string
}

// reactionEntry is one user's set, with the record that carried it.
type reactionEntry struct {
	logID  LogID
	roomID string
	set    Reaction
}

type memberKey struct {
	roomID, userID string
}

// memberEntry is one user's membership, with the record that carried it.
type memberEntry struct {
	logID  LogID
	member Member
}

// ApplyRoom applies a room record (§4.2), and reports whether it replaced
// what the Replay kept. A room record of a room_list or room_update
// result, a RoomView's Room, is one too: its log_id is the record's, and
// its delivery fields are not. One without a log_id, as from a server
// without capability history, is older than every other.
func (r *Replay) ApplyRoom(room Room) bool {
	if !ValidID(room.RoomID) {
		return false
	}
	if kept, ok := r.rooms[room.RoomID]; ok && room.LogID <= kept.LogID {
		return false
	}
	if r.rooms == nil {
		r.rooms = map[string]Room{}
	}
	r.rooms[room.RoomID] = room
	r.saw(room.LogID)
	return true
}

// ApplyMessage applies a message snapshot (§3.5.3), and reports whether
// it replaced what the Replay kept. A transient notice, without
// message_id, is never installed.
func (r *Replay) ApplyMessage(m Message) bool {
	if m.MessageID == 0 || m.LogID == 0 {
		return false
	}
	if kept, ok := r.messages[m.MessageID]; ok && !m.Replaces(kept) {
		return false
	}
	if r.messages == nil {
		r.messages = map[LogID]Message{}
	}
	r.messages[m.MessageID] = m
	r.saw(m.LogID)
	return true
}

// ApplyReactions applies a reactions record (§4.7): each of its sets
// replaces its user's on the message, unless the Replay keeps a later one.
// It reports whether any did.
func (r *Replay) ApplyReactions(rec Reactions) bool {
	if rec.MessageID == 0 || rec.LogID == 0 {
		return false
	}
	changed := false
	for _, set := range rec.Reactions {
		key := reactionKey{rec.MessageID, set.From.UserID}
		if kept, ok := r.reactions[key]; key.userID == "" || ok && rec.LogID <= kept.logID {
			continue
		}
		if r.reactions == nil {
			r.reactions = map[reactionKey]reactionEntry{}
		}
		r.reactions[key] = reactionEntry{rec.LogID, rec.RoomID, set}
		changed = true
	}
	if changed {
		r.saw(rec.LogID)
	}
	return changed
}

// ApplyMembership applies a membership record (§4.2.2): each of its
// entries replaces its user's membership of the room, unless the Replay
// keeps a later one. It reports whether any did.
func (r *Replay) ApplyMembership(rec Membership) bool {
	if rec.LogID == 0 || !ValidID(rec.RoomID) {
		return false
	}
	changed := false
	for _, m := range rec.Members {
		key := memberKey{rec.RoomID, m.User.UserID}
		if kept, ok := r.memberships[key]; key.userID == "" || ok && rec.LogID <= kept.logID {
			continue
		}
		if r.memberships == nil {
			r.memberships = map[memberKey]memberEntry{}
		}
		r.memberships[key] = memberEntry{rec.LogID, m}
		changed = true
	}
	if changed {
		r.saw(rec.LogID)
	}
	return changed
}

// ApplyHistory applies every record of a history result (§4.3.5), in any
// order, and returns how many replaced what the Replay kept.
func (r *Replay) ApplyHistory(h HistoryResult) int {
	n := 0
	count := func(applied bool) {
		if applied {
			n++
		}
	}
	for _, room := range h.Rooms {
		count(r.ApplyRoom(room))
	}
	for _, m := range h.Messages {
		count(r.ApplyMessage(m))
	}
	for _, rec := range h.Reactions {
		count(r.ApplyReactions(rec))
	}
	for _, rec := range h.Memberships {
		count(r.ApplyMembership(rec))
	}
	return n
}

// Apply applies one logged record, whichever kind it is, as the methods
// above do.
func (r *Replay) Apply(rec Record) bool {
	switch {
	case rec.Room != nil:
		return r.ApplyRoom(*rec.Room)
	case rec.Message != nil:
		return r.ApplyMessage(*rec.Message)
	case rec.Reactions != nil:
		return r.ApplyReactions(*rec.Reactions)
	case rec.Membership != nil:
		return r.ApplyMembership(*rec.Membership)
	}
	return false
}

func (r *Replay) saw(id LogID) { r.latest = max(r.latest, id) }

// Latest is the greatest log_id of the records the Replay keeps, or 0.
func (r *Replay) Latest() LogID { return r.latest }

// Room returns the room record kept for roomID.
func (r *Replay) Room(roomID string) (Room, bool) {
	room, ok := r.rooms[roomID]
	return room, ok
}

// Rooms returns the room records kept, in the order of their room_ids.
func (r *Replay) Rooms() []Room {
	rooms := make([]Room, 0, len(r.rooms))
	for _, id := range slices.Sorted(maps.Keys(r.rooms)) {
		rooms = append(rooms, r.rooms[id])
	}
	return rooms
}

// Message returns the snapshot kept of a message, which may be a
// tombstone.
func (r *Replay) Message(messageID LogID) (Message, bool) {
	m, ok := r.messages[messageID]
	return m, ok
}

// Messages returns the messages whose kept snapshot is in roomID, or in
// any room for "", tombstones too, in the order of their message_ids,
// which is the order they were posted in.
func (r *Replay) Messages(roomID string) []Message {
	// The list is made once, of its size: a room's whole history may be
	// millions of messages.
	n := 0
	for _, m := range r.messages {
		if roomID == "" || m.RoomID == roomID {
			n++
		}
	}
	if n == 0 {
		return nil
	}
	list := make([]Message, 0, n)
	for _, m := range r.messages {
		if roomID == "" || m.RoomID == roomID {
			list = append(list, m)
		}
	}
	slices.SortFunc(list, func(a, b Message) int { return cmp.Compare(a.MessageID, b.MessageID) })
	return list
}

// Reactions returns the sets of emoji kept on a message that are not
// empty, one for each user, in the order they were set. A client hides
// those of a tombstone (§4.4).
func (r *Replay) Reactions(messageID LogID) []Reaction {
	var entries []reactionEntry
	for key, entry := range r.reactions {
		if key.messageID == messageID && len(entry.set.Emojis) > 0 {
			entries = append(entries, entry)
		}
	}
	slices.SortFunc(entries, func(a, b reactionEntry) int {
		return cmp.Or(cmp.Compare(a.logID, b.logID), cmp.Compare(a.set.From.UserID, b.set.From.UserID))
	})
	sets := make([]Reaction, len(entries))
	for i, entry := range entries {
		sets[i] = entry.set
	}
	return sets
}

// Members returns the users the kept memberships of roomID have joined,
// as their membership records logged them, in the order of their
// user_ids.
func (r *Replay) Members(roomID string) []User {
	var users []User
	for key, entry := range r.memberships {
		if key.roomID == roomID && entry.member.Joined {
			users = append(users, entry.member.User)
		}
	}
	slices.SortFunc(users, func(a, b User) int { return cmp.Compare(a.UserID, b.UserID) })
	return users
}

// Records returns what the Replay keeps as logged records, in ascending
// log_id order: each room record and snapshot it keeps, and its reaction
// sets and memberships folded into one record for each log_id that
// carried them, of a message or a room. Applied to an empty Replay, they
// leave the same state: they are a compacted log (§4.3.4).
func (r *Replay) Records() []Record {
	var records []Record
	if n := len(r.rooms) + len(r.messages); n > 0 {
		records = make([]Record, 0, n)
	}
	for _, room := range r.rooms {
		records = append(records, Record{LogID: room.LogID, Room: &room})
	}
	for _, m := range r.messages {
		records = append(records, Record{LogID: m.LogID, Message: &m})
	}
	type reactionsKey struct {
		logID, messageID LogID
		roomID           string
	}
	reactions := map[reactionsKey]*Reactions{}
	for key, entry := range r.reactions {
		k := reactionsKey{entry.logID, key.messageID, entry.roomID}
		if reactions[k] == nil {
			reactions[k] = &Reactions{LogID: entry.logID, MessageID: key.messageID, RoomID: entry.roomID}
		}
		reactions[k].Reactions = append(reactions[k].Reactions, entry.set)
	}
	for _, rec := range reactions {
		slices.SortFunc(rec.Reactions, func(a, b Reaction) int { return cmp.Compare(a.From.UserID, b.From.UserID) })
		records = append(records, Record{LogID: rec.LogID, Reactions: rec})
	}
	type membershipKey struct {
		logID  LogID
		roomID string
	}
	memberships := map[membershipKey]*Membership{}
	for key, entry := range r.memberships {
		k := membershipKey{entry.logID, key.roomID}
		if memberships[k] == nil {
			memberships[k] = &Membership{LogID: entry.logID, RoomID: key.roomID}
		}
		memberships[k].Members = append(memberships[k].Members, entry.member)
	}
	for _, rec := range memberships {
		slices.SortFunc(rec.Members, func(a, b Member) int { return cmp.Compare(a.User.UserID, b.User.UserID) })
		records = append(records, Record{LogID: rec.LogID, Membership: rec})
	}
	slices.SortFunc(records, func(a, b Record) int {
		return cmp.Or(cmp.Compare(a.LogID, b.LogID), kind(a)-kind(b), cmp.Compare(key(a), key(b)))
	})
	return records
}

// kind orders the records at one log_id, which a server logs one at a
// time, but compacted records of several keys can share.
func kind(r Record) int {
	switch {
	case r.Room != nil:
		return 0
	case r.Message != nil:
		return 1
	case r.Reactions != nil:
		return 2
	}
	return 3
}

// key is a record's key, to order the records of one kind at one log_id.
func key(r Record) string {
	switch {
	case r.Room != nil:
		return r.Room.RoomID
	case r.Message != nil:
		return r.Message.MessageID.String()
	case r.Reactions != nil:
		return r.Reactions.MessageID.String() + " " + r.Reactions.RoomID
	case r.Membership != nil:
		return r.Membership.RoomID
	}
	return ""
}
