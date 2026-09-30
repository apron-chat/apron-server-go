package server

import (
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// maxListedRooms caps `not_joined` in a room_list result; `joined` is never
// truncated (§4.3.1).
const maxListedRooms = 200

// roomState is a room's current record, its log, and its members. Every room,
// including threads (rooms with parent_room_id), is visible to every
// authenticated user, except that a private room and its threads are visible
// only to their members (§4.3.4); members are the users who have joined it
// and receive its deliveries (§3.4, §4.3.2).
type roomState struct {
	id       string
	parent   *roomState
	children []*roomState
	// private is fixed at creation and kept in record as "private": true.
	private bool
	// record holds the latest logged room record.
	record      map[string]any
	recordLogID int64
	createdID   int64
	latestID    int64
	log         []*logRecord
	members     map[string]*userState
	// active maps each member to the log_id of their latest join or new
	// message in the room, which ranks members when a listing truncates
	// them (§4.3.1).
	active map[string]int64
	// creator is the user_id that created the room, empty for the seeded
	// room; only the creator may /kick (§4.8).
	creator string
	// reads holds each user's latest read cursor (§4.4).
	reads map[string]readCursor
}

type readCursor struct {
	from      map[string]any
	messageID string
	id        int64
}

// visibleTo reports whether u may see the room (§4.3.4): unless u is a
// member of the room and of every private room it is a thread of, at any
// depth, it is invisible, and answered like an unknown one. So a thread of a
// private room is hidden from everyone outside that room, members of the
// thread included.
func (r *roomState) visibleTo(u *userState) bool {
	for room := r; room != nil; room = room.parent {
		if room.private && room.members[u.id] == nil {
			return false
		}
	}
	return true
}

// audience is who can see the room: everyone, or, for a private room or a
// thread of one, the users in every private room on the way up.
func (r *roomState) audience() (everyone bool, users map[string]*userState) {
	for room := r; room != nil; room = room.parent {
		if !room.private {
			continue
		}
		if users == nil {
			users = maps.Clone(room.members)
			continue
		}
		for id := range users {
			if room.members[id] == nil {
				delete(users, id)
			}
		}
	}
	return users == nil, users
}

// revealsTo reports whether a record in r, such as a move snapshot naming
// another room, would reach someone who cannot see to.
func (r *roomState) revealsTo(to *roomState) bool {
	toEveryone, toUsers := to.audience()
	if toEveryone {
		return false
	}
	everyone, users := r.audience()
	if everyone {
		return true
	}
	for id := range users {
		if toUsers[id] == nil {
			return true
		}
	}
	return false
}

// visibleRoomLocked returns the room named id if u may see it, or nil.
func (s *Server) visibleRoomLocked(u *userState, id string) *roomState {
	if r := s.rooms[id]; r != nil && r.visibleTo(u) {
		return r
	}
	return nil
}

// visibleMessageLocked returns the message named id if u may see the room
// it is in, or nil.
func (s *Server) visibleMessageLocked(u *userState, id string) *messageState {
	if m := s.messages[id]; m != nil && s.rooms[m.roomID].visibleTo(u) {
		return m
	}
	return nil
}

// mayRemove reports whether u may remove others from r, with /kick or
// room_leave's user_id (§4.3.2, §4.8): the room's creator, or a user with
// the admin or moderator role.
func (r *roomState) mayRemove(u *userState) bool {
	return r.creator == u.id || u.hasRole("admin") || u.hasRole("moderator")
}

// deliveryFields returns the room's log head and the inclusive lower bound of
// retained history. The Go server retains every record, so history always
// starts at the room's creation record. Callers hold s.mu.
func (r *roomState) deliveryFields() map[string]any {
	return map[string]any{
		"latest_log_id":  formatID(r.latestID),
		"history_log_id": formatID(r.createdID),
	}
}

// title is the room's display title, falling back to its room_id (§3.4).
func (r *roomState) title() string {
	if title, _ := r.record["title"].(string); title != "" {
		return title
	}
	return r.id
}

// cursorFramesLocked renders the read cursors kept for a room that the
// server sends after listing it (§4.4): every member's cursor for a room u
// has joined, which delivers read receipts, and otherwise only u's own.
func (s *Server) cursorFramesLocked(u *userState, r *roomState) []any {
	var frames []any
	for _, userID := range slices.Sorted(maps.Keys(r.reads)) {
		if u.joined[r.id] == nil && userID != u.id {
			continue
		}
		cursor := r.reads[userID]
		frames = append(frames, map[string]any{"method": "activity", "params": map[string]any{
			"room_id": r.id, "from": cloneObject(cursor.from), "read_message_id": cursor.messageID,
		}})
	}
	return frames
}

// roomParamsLocked renders a room's current record with delivery fields.
// Only the top level is new: nested values are shared with records that are
// replaced rather than modified, so the result may be encoded after s.mu is
// released.
func (s *Server) roomParamsLocked(r *roomState) map[string]any {
	params := maps.Clone(r.record)
	maps.Copy(params, r.deliveryFields())
	return params
}

// deliverLocked sends a frame to every connection of every member of the
// given rooms, once per connection. The frame is encoded once for all of them.
func (s *Server) deliverLocked(frame any, rooms ...*roomState) {
	frame = render(frame)
	seen := make(map[string]bool)
	for _, r := range rooms {
		for id, member := range r.members {
			if !seen[id] {
				seen[id] = true
				member.send(frame)
			}
		}
	}
}

// render encodes a frame once for sending to many connections.
func render(frame any) jsontext.Value {
	if payload, rendered := frame.(jsontext.Value); rendered {
		return payload
	}
	return encodeJSON(frame)
}

// roomUpdate renders a room_update notification (§4.3.3) with one field.
func roomUpdate(field string, records ...any) jsontext.Value {
	return notification("room_update", map[string]any{field: records})
}

// logMembershipLocked logs one membership record (§4.3.2) of u in r,
// advancing the room's latest_log_id, and returns the record for delivery
// in a room_update's memberships (§4.3.3). The record carries u as a
// recorded object: user_id and name.
func (s *Server) logMembershipLocked(u *userState, r *roomState, joined bool) jsontext.Value {
	logID := s.nextIDLocked()
	record := newLogRecord(logID, kindMembership, map[string]any{
		"log_id":  formatID(logID),
		"room_id": r.id,
		"members": []any{map[string]any{"user": u.from(), "joined": joined}},
	})
	s.appendLocked(record, r)
	if !joined {
		u.leftAt[r.id] = logID
	}
	return record.raw
}

// deliverMembershipLocked sends a membership record to the room's members
// but u, as room_update memberships (§4.3.3); u's connections receive it
// with the room_update that tells them of the change.
func (s *Server) deliverMembershipLocked(membership jsontext.Value, r *roomState, u *userState) {
	frame := roomUpdate("memberships", membership)
	for id, member := range r.members {
		if id != u.id {
			member.send(frame)
		}
	}
}

// addMemberLocked joins u to r, logs the membership, and delivers it to the
// room's other members (§4.3.2). It returns the membership record, nil if
// u was a member already.
func (s *Server) addMemberLocked(u *userState, r *roomState) jsontext.Value {
	if u.joined[r.id] != nil {
		return nil
	}
	u.joined[r.id] = r
	r.members[u.id] = u
	s.touchRoom(r)
	s.touchUser(u.id)
	delete(u.leftAt, r.id)
	membership := s.logMembershipLocked(u, r, true)
	r.active[u.id] = r.latestID
	s.deliverMembershipLocked(membership, r, u)
	return membership
}

// joinDefaultRoomLocked joins a new identity to the default room, so its
// room list is not empty. Its connections receive the membership alone, as
// room_update memberships before the auth result: the client lists its rooms
// with room_list (§4.3.1).
func (s *Server) joinDefaultRoomLocked(u *userState) {
	if membership := s.addMemberLocked(u, s.rooms[defaultRoomID]); membership != nil {
		u.send(roomUpdate("memberships", membership))
	}
}

// joinLocked joins u to r: the room's other members receive the membership,
// and u's connections one room_update with the room, its members, and the
// membership (§4.3.2). It reports whether u was not a member before.
func (s *Server) joinLocked(u *userState, r *roomState) bool {
	membership := s.addMemberLocked(u, r)
	if membership == nil {
		return false
	}
	u.send(s.joinedUpdateLocked(r, membership))
	return true
}

// joinedUpdateLocked renders room_update joined for r: its record with its
// members, as bare user objects, their current objects in `users`, and the
// membership that joined the user, if any (§4.3.3).
func (s *Server) joinedUpdateLocked(r *roomState, membership jsontext.Value) jsontext.Value {
	record := s.roomParamsLocked(r)
	listed := s.addMembersLocked(record, r)
	params := map[string]any{"joined": []any{record}, "users": profiles(listed)}
	if membership != nil {
		params["memberships"] = []any{membership}
	}
	return notification("room_update", params)
}

// addMembersLocked adds a room's members to its record as bare user
// objects, ordered by user_id, and returns them. A room with more than
// Config.MaxListedMembers lists only its most recently active members and
// adds member_count, the total (§4.3.1).
func (s *Server) addMembersLocked(record map[string]any, r *roomState) map[string]*userState {
	ids := slices.Collect(maps.Keys(r.members))
	listed := r.members
	if limit := s.config.MaxListedMembers; limit > 0 && len(ids) > limit {
		slices.SortFunc(ids, func(a, b string) int {
			return cmp.Or(cmp.Compare(r.active[b], r.active[a]), cmp.Compare(a, b))
		})
		ids = ids[:limit]
		listed = make(map[string]*userState, limit)
		for _, id := range ids {
			listed[id] = r.members[id]
		}
		record["member_count"] = len(r.members)
	}
	slices.Sort(ids)
	refs := make([]any, len(ids))
	for i, id := range ids {
		refs[i] = map[string]any{"user_id": id}
	}
	record["members"] = refs
	return listed
}

// leaveLocked removes u from r (§4.3.2): the room's other members receive
// the logged membership, and u's connections one room_update with left and
// the membership. It reports whether u was a member.
func (s *Server) leaveLocked(u *userState, r *roomState) bool {
	if u.joined[r.id] == nil {
		return false
	}
	membership := s.logMembershipLocked(u, r, false)
	s.deliverMembershipLocked(membership, r, u)
	delete(u.joined, r.id)
	s.touchRoom(r)
	s.touchUser(u.id)
	delete(r.members, u.id)
	delete(r.active, u.id)
	u.send(notification("room_update", map[string]any{"left": []any{map[string]any{"room_id": r.id}}, "memberships": []any{membership}}))
	if r.private {
		s.hideThreadsLocked(u, r)
	}
	return true
}

// hideThreadsLocked follows u leaving the private room r: u can no longer see
// the threads it could see there, at any depth, so u leaves those it joined,
// which stops their deliveries, and u's connections receive room_update left
// for the others, which they may know from room_update updated (§4.3.3). A
// private thread u was not in was never visible to u, so it and its threads
// are passed over, and one u was in is left, which hides its own threads.
func (s *Server) hideThreadsLocked(u *userState, r *roomState) {
	var hidden []any
	var walk func(*roomState)
	walk = func(room *roomState) {
		for _, thread := range room.children {
			switch {
			case thread.private && thread.members[u.id] == nil:
			case thread.private:
				s.leaveLocked(u, thread)
			default:
				if !s.leaveLocked(u, thread) {
					hidden = append(hidden, map[string]any{"room_id": thread.id})
				}
				walk(thread)
			}
		}
	}
	walk(r)
	if len(hidden) > 0 {
		u.send(roomUpdate("left", hidden...))
	}
}

// commitRoomLocked logs a room record holding fields, the client fields other
// than parent_room_id and private. An unknown roomID creates the room, private
// if asked; an empty one names it by its creation log_id.
func (s *Server) commitRoomLocked(roomID string, parent *roomState, private bool, fields map[string]any) *roomState {
	logID := s.nextIDLocked()
	r := s.rooms[roomID]
	if r == nil {
		if roomID == "" {
			roomID = formatID(logID)
		}
		r = &roomState{
			id: roomID, parent: parent, private: private, createdID: logID,
			members: make(map[string]*userState), active: make(map[string]int64), reads: make(map[string]readCursor),
		}
		s.rooms[roomID] = r
		if parent != nil {
			parent.children = append(parent.children, r)
		}
	}
	record := map[string]any{"room_id": r.id, "log_id": formatID(logID)}
	if r.recordLogID != 0 {
		record["prev_log_id"] = formatID(r.recordLogID)
	}
	if r.parent != nil {
		record["parent_room_id"] = r.parent.id
	}
	if r.private {
		record["private"] = true
	}
	maps.Copy(record, fields)
	r.record = record
	r.recordLogID = logID
	s.appendLocked(newLogRecord(logID, kindRoom, record), r)
	return r
}

// announceRoomLocked sends a room's current record as room_update updated to
// its members, the parent's members for a thread that is not private, and
// editor, if any.
func (s *Server) announceRoomLocked(r *roomState, editor *userState) {
	audience := maps.Clone(r.members)
	if r.parent != nil && !r.private {
		maps.Copy(audience, r.parent.members)
	}
	if editor != nil {
		audience[editor.id] = editor
	}
	frame := roomUpdate("updated", s.roomParamsLocked(r))
	for _, member := range audience {
		member.send(frame)
	}
}

// setRoom creates a room (no room_id) or replaces an existing room's client
// fields (§4.3.4). parent_room_id and private are fixed at creation and
// ignored on updates, so an omitted private is kept; a thread created
// without private takes its parent's. Any authenticated user may create rooms and threads and
// update any room they can see.
//
// A new room joins only its creator, logging the creator's membership: the
// creator's connections receive room_update joined, then the membership,
// then the result. A new thread that is not private goes to the parent's
// other members as room_update updated, without joining them. An edit goes
// as updated to the room's members, to the parent's members for a thread
// that is not private, and to the editor.
func (s *Server) setRoom(c *client, req request) (any, bool, *rpcError) {
	_, updating := req.params["room_id"]
	var roomID, parentID string
	var err *rpcError
	if updating {
		roomID, err = parseString(req.params, "room_id", true)
		if err != nil {
			return nil, false, err
		}
	} else if _, present := req.params["parent_room_id"]; present {
		parentID, err = parseString(req.params, "parent_room_id", true)
		if err != nil {
			return nil, false, err
		}
		if parentID == "" {
			return nil, false, invalidParams("parent_room_id must be a non-empty string")
		}
	}
	title, err := parseString(req.params, "title", false)
	if err != nil {
		return nil, false, err
	}
	description, err := parseString(req.params, "description", false)
	if err != nil {
		return nil, false, err
	}
	if len(description) > maxDescriptionBytes {
		return nil, false, invalidParams("description is at most %d bytes", maxDescriptionBytes)
	}
	ext, err := parseObject(req.params, "ext", false)
	if err != nil {
		return nil, false, err
	}
	private, err := parseBool(req.params, "private", false)
	if err != nil {
		return nil, false, err
	}

	s.mu.Lock()
	defer s.unlock()
	u := c.user
	var parent *roomState
	if updating {
		existing := s.visibleRoomLocked(u, roomID)
		if existing == nil {
			return nil, false, invalidParams("Unknown room %q", roomID)
		}
		parent = existing.parent
	} else if parentID != "" {
		if parent = s.visibleRoomLocked(u, parentID); parent == nil {
			return nil, false, invalidParams("Unknown parent room %q", parentID)
		}
	}
	if err := s.admitPostLocked(u); err != nil {
		return nil, false, err
	}
	fields := make(map[string]any)
	if title == "" && parent != nil {
		// Servers title threads so clients unaware of parent_room_id render them.
		title = threadTitle(description)
	}
	if title != "" {
		fields["title"] = title
	}
	if description != "" {
		fields["description"] = description
	}
	if ext != nil {
		fields["ext"] = ext
	}
	if _, given := req.params["private"]; !given && !updating && parent != nil {
		// A thread created without private takes its parent's (§4.3.4).
		private = parent.private
	}
	r := s.commitRoomLocked(roomID, parent, private, fields)
	if !updating {
		r.creator = u.id
	}
	s.touchRoom(r)
	// Room updates precede the result, so the room is known when it arrives.
	if updating {
		s.announceRoomLocked(r, u)
	} else {
		// The room record and the membership are both logged before the
		// room_update, whose latest_log_id is then the membership's.
		u.joined[r.id] = r
		r.members[u.id] = u
		s.touchUser(u.id)
		membership := s.logMembershipLocked(u, r, true)
		r.active[u.id] = r.latestID
		u.send(s.joinedUpdateLocked(r, membership))
		// A private thread's record goes only to its own members.
		if parent != nil && !r.private {
			frame := roomUpdate("updated", s.roomParamsLocked(r))
			for id, member := range parent.members {
				if id != u.id {
					member.send(frame)
				}
			}
		}
	}
	result := map[string]any{"room_id": r.id}
	if req.hasID {
		c.sendResult(req, result)
	}
	return result, true, nil
}

const (
	maxThreadTitleRunes = 60
	defaultThreadTitle  = "Thread"
	// maxDescriptionBytes bounds a room's description, which every room
	// record carries.
	maxDescriptionBytes = 16 << 10
)

// threadTitle derives a default thread title from the first line of its
// description, or defaultThreadTitle.
func threadTitle(description string) string {
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

// listRooms answers room_list (§4.3.1): the rooms matching its filters,
// joined ones in `joined` (never truncated) and visible unjoined ones in
// `not_joined` (the most recently active maxListedRooms), each most recently
// active first. `filter` leaves either array out; one it asks for is present
// even when empty. With `members: true` every room carries its complete
// members as bare user objects, whose complete objects are in `users`.
//
// With `latest_log_id`, only rooms whose latest_log_id is greater are
// listed, and a result with `joined` carries `left`: the rooms among the
// candidates the user left after that position. The server logs every
// membership, so a left room's latest_log_id is at least its leave; a left
// room that is top-level, or a thread listed with parent_room_id or
// room_id, is in `not_joined` too when the filter asks for it.
//
// The result is followed by the read cursors kept for the listed rooms
// (§4.4).
func (s *Server) listRooms(c *client, req request) (any, bool, *rpcError) {
	filter, err := parseString(req.params, "filter", false)
	if err != nil {
		return nil, false, err
	}
	switch filter {
	case "":
		filter = "all"
	case "all", "joined", "not_joined":
	default:
		return nil, false, invalidParams("filter must be joined, not_joined, or all")
	}
	wantJoined, wantOthers := filter != "not_joined", filter != "joined"
	withMembers, err := parseBool(req.params, "members", false)
	if err != nil {
		return nil, false, err
	}
	parentID, err := parseString(req.params, "parent_room_id", false)
	if err != nil {
		return nil, false, err
	}
	_, hasParent := req.params["parent_room_id"]
	roomID, err := parseString(req.params, "room_id", false)
	if err != nil {
		return nil, false, err
	}
	_, hasRoom := req.params["room_id"]
	since, hasSince, err := parseBound(req.params, "latest_log_id")
	if err != nil {
		return nil, false, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	u := c.user
	var candidates []*roomState
	switch {
	case hasRoom:
		r := s.visibleRoomLocked(u, roomID)
		if r == nil {
			return nil, false, invalidParams("Unknown room %q", roomID)
		}
		candidates = []*roomState{r}
	case hasParent:
		parent := s.visibleRoomLocked(u, parentID)
		if parent == nil {
			return nil, false, invalidParams("Unknown parent room %q", parentID)
		}
		candidates = parent.children
	default:
		candidates = slices.Collect(maps.Values(s.rooms))
	}
	var joined, others, left []*roomState
	for _, r := range candidates {
		if hasSince && r.latestID <= since {
			continue
		}
		if u.joined[r.id] != nil {
			if wantJoined {
				joined = append(joined, r)
			}
			continue
		}
		if hasSince && wantJoined && u.leftAt[r.id] > since {
			left = append(left, r)
		}
		// Without parent_room_id or room_id, unjoined threads are left to
		// their parent's listing. Private rooms are listed only to their
		// members.
		if wantOthers && (hasRoom || hasParent || r.parent == nil) && r.visibleTo(u) {
			others = append(others, r)
		}
	}
	byActivity := func(a, b *roomState) int {
		return cmp.Or(cmp.Compare(b.latestID, a.latestID), cmp.Compare(b.createdID, a.createdID))
	}
	slices.SortFunc(joined, byActivity)
	slices.SortFunc(others, byActivity)
	slices.SortFunc(left, byActivity)
	if len(others) > maxListedRooms {
		others = others[:maxListedRooms]
	}

	users := make(map[string]*userState)
	renderRooms := func(rooms []*roomState) []any {
		entries := make([]any, len(rooms))
		for i, r := range rooms {
			entry := s.roomParamsLocked(r)
			if withMembers {
				maps.Copy(users, s.addMembersLocked(entry, r))
			}
			entries[i] = entry
		}
		return entries
	}
	result := map[string]any{}
	if wantJoined {
		result["joined"] = renderRooms(joined)
	}
	if wantOthers {
		result["not_joined"] = renderRooms(others)
	}
	if hasSince && wantJoined {
		gone := make([]any, len(left))
		for i, r := range left {
			gone[i] = map[string]any{"room_id": r.id}
		}
		result["left"] = gone
	}
	if withMembers {
		result["users"] = profiles(users)
	}
	frames := []any{response(req.id, result)}
	for _, r := range slices.Concat(joined, others) {
		frames = append(frames, s.cursorFramesLocked(u, r)...)
	}
	if !req.hasID {
		frames = frames[1:]
	}
	c.enqueueBatch(frames...)
	return result, true, nil
}

// profiles lists users' complete objects ordered by user_id, for a result's
// `users` (§3.3).
func profiles(users map[string]*userState) []any {
	list := make([]any, 0, len(users))
	for _, id := range slices.Sorted(maps.Keys(users)) {
		list = append(list, users[id].profile())
	}
	return list
}

// joinRoom joins a visible room (§4.3.2): the logged membership goes to the
// room's members, the joining user's connections included, then room_update
// joined to the joining user's connections, then the result. Joining a room
// already joined logs nothing and re-sends room_update joined to the calling
// connection only.
//
// With the user_id of another user, a member of the room adds that user,
// which is how people are brought into a private room; adding a member
// changes nothing.
func (s *Server) joinRoom(c *client, req request) (any, bool, *rpcError) {
	roomID, targetID, hasTarget, err := parseMembershipParams(req.params)
	if err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.unlock()
	u := c.user
	r := s.visibleRoomLocked(u, roomID)
	if r == nil {
		return nil, false, invalidParams("Unknown room %q", roomID)
	}
	target := u
	if hasTarget && targetID != u.id {
		if r.members[u.id] == nil {
			return nil, false, &rpcError{Code: codeDenied, Message: fmt.Sprintf("Join %s before adding others to it", r.title())}
		}
		if target = s.users[targetID]; target == nil {
			return nil, false, invalidParams("Unknown user %q", targetID)
		}
		// Adding someone to a thread of a private room takes adding them to
		// that room first.
		if r.parent != nil && !r.parent.visibleTo(target) {
			return nil, false, &rpcError{Code: codeDenied, Message: fmt.Sprintf("Add %s to the private room %s is in first", targetID, r.title())}
		}
	}
	if !s.joinLocked(target, r) && target == u {
		c.enqueue(s.joinedUpdateLocked(r, nil))
	}
	result := map[string]any{}
	if req.hasID {
		c.sendResult(req, result)
	}
	return result, true, nil
}

// leaveRoom leaves a room (§4.3.2): the logged membership goes to the room's
// members, the leaving user's connections included, then room_update left to
// the leaving user's connections, then the result. The room stays visible in
// room_list unless it is private. Leaving a room not joined changes nothing.
//
// With the user_id of another user, it removes that user, which the room's
// creator and users with the admin or moderator role may do.
func (s *Server) leaveRoom(c *client, req request) (any, bool, *rpcError) {
	roomID, targetID, hasTarget, err := parseMembershipParams(req.params)
	if err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.unlock()
	u := c.user
	r := s.visibleRoomLocked(u, roomID)
	if r == nil {
		return nil, false, invalidParams("Unknown room %q", roomID)
	}
	target := u
	if hasTarget && targetID != u.id {
		if !r.mayRemove(u) {
			return nil, false, &rpcError{Code: codeDenied, Message: fmt.Sprintf("Only the creator of %s or a moderator can remove people from it", r.title())}
		}
		if target = s.users[targetID]; target == nil {
			return nil, false, invalidParams("Unknown user %q", targetID)
		}
	}
	s.leaveLocked(target, r)
	result := map[string]any{}
	if req.hasID {
		c.sendResult(req, result)
	}
	return result, true, nil
}

// parseMembershipParams reads room_join and room_leave params: room_id and
// an optional user_id (§4.3.2).
func parseMembershipParams(params map[string]jsontext.Value) (roomID, userID string, hasUser bool, err *rpcError) {
	if roomID, err = parseString(params, "room_id", true); err != nil {
		return "", "", false, err
	}
	if userID, err = parseString(params, "user_id", false); err != nil {
		return "", "", false, err
	}
	_, hasUser = params["user_id"]
	if hasUser && userID == "" {
		return "", "", false, invalidParams("user_id must be a non-empty string")
	}
	return roomID, userID, hasUser, nil
}

// history returns a window of one room's log (§4.1), the default room's
// without room_id. limit counts records of every kind; the slice is
// partitioned into rooms, messages, reactions, and membership, each omitted
// when empty. The server retains all records and does not compact, and it
// discards no prefix, so it never appends a full-member record. History needs
// no membership, only a room the user can see.
//
// A move snapshot is in both rooms' logs and names the source room in
// prev_room_id, so a window bounded to one log_id (after == before) in the
// room a snapshot names walks prev_log_id back through a message's edits
// (§2).
func (s *Server) history(c *client, req request) (any, bool, *rpcError) {
	roomID, err := parseString(req.params, "room_id", false)
	if err != nil {
		return nil, false, err
	}
	if _, has := req.params["room_id"]; !has {
		roomID = defaultRoomID
	}
	after, hasAfter, err := parseBound(req.params, "after")
	if err != nil {
		return nil, false, err
	}
	before, hasBefore, err := parseBound(req.params, "before")
	if err != nil {
		return nil, false, err
	}
	limit, err := parseLimit(req.params, s.config.HistoryPageSize)
	if err != nil {
		return nil, false, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	r := s.visibleRoomLocked(c.user, roomID)
	if r == nil {
		return nil, false, invalidParams("Unknown room %q", roomID)
	}
	matching := window(r.log, after, hasAfter, before, hasBefore)
	more := len(matching) > limit
	if more {
		if hasAfter {
			matching = matching[:limit]
		} else {
			matching = matching[len(matching)-limit:]
		}
	}
	// A page also stops at maxHistoryReplyBytes of records, keeping at least
	// one, so one request cannot hold the lock to render an unbounded reply.
	size := 0
	for i := range matching {
		k := i
		if !hasAfter {
			k = len(matching) - 1 - i
		}
		if size += len(matching[k].raw); size > maxHistoryReplyBytes && i > 0 {
			if hasAfter {
				matching = matching[:k]
			} else {
				matching = matching[k+1:]
			}
			more = true
			break
		}
	}
	result := jsontext.Value(renderHistory(r, matching, more, size))
	// The records are already JSON: the reply is assembled from them without
	// decoding or re-encoding.
	if req.hasID {
		c.enqueue(rawResponse(req.id, result))
	}
	return result, true, nil
}

// renderHistory assembles a history result (§4.1) from a window of records.
func renderHistory(r *roomState, matching []*logRecord, more bool, size int) []byte {
	buf := make([]byte, 0, size+len(matching)+256)
	buf = append(buf, `{"more":`...)
	buf = strconv.AppendBool(buf, more)
	for _, group := range []struct {
		key  string
		kind recordKind
	}{{"rooms", kindRoom}, {"messages", kindMessage}, {"reactions", kindReactions}, {"memberships", kindMembership}} {
		first := true
		for _, record := range matching {
			if record.kind != group.kind {
				continue
			}
			if first {
				buf = append(buf, `,"`...)
				buf = append(buf, group.key...)
				buf = append(buf, `":[`...)
				first = false
			} else {
				buf = append(buf, ',')
			}
			buf = append(buf, record.raw...)
		}
		if !first {
			buf = append(buf, ']')
		}
	}
	field := func(key string, id int64) {
		buf = append(buf, `,"`...)
		buf = append(buf, key...)
		buf = append(buf, `":"`...)
		buf = strconv.AppendInt(buf, id, 10)
		buf = append(buf, '"')
	}
	field("latest_log_id", r.latestID)
	field("history_log_id", r.createdID)
	if len(matching) > 0 {
		field("first_log_id", matching[0].id)
		field("last_log_id", matching[len(matching)-1].id)
	}
	return append(buf, '}')
}

// window returns the records of a log, which is in log_id order, within the
// inclusive bounds.
func window(log []*logRecord, after int64, hasAfter bool, before int64, hasBefore bool) []*logRecord {
	if hasAfter {
		start, _ := slices.BinarySearchFunc(log, after, compareLogID)
		log = log[start:]
	}
	if hasBefore {
		end, found := slices.BinarySearchFunc(log, before, compareLogID)
		if found {
			end++
		}
		log = log[:end]
	}
	return log
}

func compareLogID(record *logRecord, id int64) int {
	return cmp.Compare(record.id, id)
}

func parseBound(params map[string]jsontext.Value, name string) (int64, bool, *rpcError) {
	raw, ok := params[name]
	if !ok {
		return 0, false, nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return 0, false, invalidParams("%s must be a decimal string", name)
	}
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil || number < 0 {
		return 0, false, invalidParams("%s must be a non-negative decimal string", name)
	}
	return number, true, nil
}

func parseLimit(params map[string]jsontext.Value, defaultLimit int) (int, *rpcError) {
	raw, ok := params["limit"]
	if !ok {
		return defaultLimit, nil
	}
	var value int
	if json.Unmarshal(raw, &value) != nil || value <= 0 {
		return 0, invalidParams("limit must be a positive integer")
	}
	return min(value, maxHistoryPageSize), nil
}

// activity applies a connection's activity (§4.4). typing and a read cursor
// in a room are relayed to the room's members; a read cursor must name a
// message and only advances, and the server keeps the latest per user and
// sends it after the room is listed. A frame whose fields change nothing
// relays nothing. away is kept per connection for push decisions and never
// delivered; typing and a read cursor end it.
func (s *Server) activity(c *client, req request) (any, bool, *rpcError) {
	roomID, err := parseString(req.params, "room_id", false)
	if err != nil {
		return nil, false, err
	}
	if _, has := req.params["room_id"]; !has {
		roomID = defaultRoomID
	}
	var typing any
	if raw, ok := req.params["typing"]; ok {
		var value int
		if json.Unmarshal(raw, &value) != nil || value < 0 {
			return nil, false, invalidParams("typing must be a non-negative integer")
		}
		typing = value
	}
	readID, err := parseString(req.params, "read_message_id", false)
	if err != nil {
		return nil, false, err
	}
	_, hasRead := req.params["read_message_id"]
	away, err := parseBool(req.params, "away", false)
	if err != nil {
		return nil, false, err
	}
	_, hasAway := req.params["away"]
	s.mu.Lock()
	defer s.unlock()
	inRoom := typing != nil || hasRead
	r := s.visibleRoomLocked(c.user, roomID)
	if inRoom && r == nil {
		return nil, false, invalidParams("Unknown room %q", roomID)
	}
	if hasRead && s.visibleMessageLocked(c.user, readID) == nil {
		return nil, false, invalidParams("Unknown read_message_id %q", readID)
	}
	result := map[string]any{}
	// The relays this request causes precede its result (§1).
	defer func() {
		if req.hasID {
			c.sendResult(req, result)
		}
	}()
	if inRoom {
		c.away = false
	}
	if hasAway {
		c.away = away
	}
	if !inRoom {
		return result, true, nil
	}
	u := c.user
	params := map[string]any{"room_id": roomID, "from": u.from()}
	if typing != nil {
		params["typing"] = typing
	}
	if hasRead {
		id, _ := strconv.ParseInt(readID, 10, 64)
		if cursor, ok := r.reads[u.id]; !ok || id > cursor.id {
			r.reads[u.id] = readCursor{from: u.from(), messageID: readID, id: id}
			s.touchRoom(r)
			if u.joined[r.id] == nil {
				// A cursor for a room the user has not joined still syncs
				// across their own connections.
				u.send(map[string]any{"method": "activity", "params": map[string]any{"room_id": roomID, "from": u.from(), "read_message_id": readID}})
			}
			params["read_message_id"] = readID
		}
	}
	if len(params) > 2 {
		s.deliverLocked(map[string]any{"method": "activity", "params": params}, r)
	}
	return result, true, nil
}
