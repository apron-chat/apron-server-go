package aprond

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/server"
	"github.com/apron-chat/apron-server-go/aprond/internal/roomlog"
	"github.com/apron-chat/apron-server-go/aprond/internal/rooms"
)

// maxListedRooms caps `not_joined` in a room_list result; `joined` is never
// truncated (§4.2.1).
const maxListedRooms = 200

// roomState is a room: its current record, its log, and its members, with
// the rules of who may see it (rooms.Room). The rooms module keeps them;
// this file serves the rooms requests and delivers what their changes
// return.
type roomState = rooms.Room[*userState]

// MemberID is u's user_id, as rooms know it (rooms.Member).
func (u *userState) MemberID() string { return u.id }

// MemberScope is the rooms u is limited to, nil for none (rooms.Member).
func (u *userState) MemberScope() *rooms.Scope { return u.Scope }

// privateRooms counts the top-level private rooms u is in, which
// Config.MaxPrivateRooms bounds. Their threads are not counted: only their
// members can be added to those.
func (u *userState) privateRooms() int {
	n := 0
	for _, r := range u.joined {
		if r.Private() && r.Parent() == nil {
			n++
		}
	}
	return n
}

// privateRoomsFullLocked reports whether u is in as many top-level private
// rooms as Config.MaxPrivateRooms allows, so may not be in another.
func (s *Server) privateRoomsFullLocked(u *userState) bool {
	return s.config.MaxPrivateRooms > 0 && u.privateRooms() >= s.config.MaxPrivateRooms
}

// visibleRoomLocked returns the room named id if u may see it, or nil.
func (s *Server) visibleRoomLocked(u *userState, id string) *roomState {
	return s.rooms.Visible(id, u)
}

// visibleMessageLocked returns the message named id if u may see the room
// it is in, or nil.
func (s *Server) visibleMessageLocked(u *userState, id string) *messageState {
	return s.rooms.VisibleMessage(id, u)
}

// mayRemove reports whether u may remove others from r, with /kick or
// room_leave's user_id (§4.2.2, §4.1): the room's creator, or a user with
// the admin or moderator role.
func mayRemove(r *roomState, u *userState) bool {
	return r.Creator() == u.id || u.hasRole("admin") || u.hasRole("moderator")
}

// cursorFramesLocked renders the read cursors kept for a room that the
// server sends after listing it (§4.6): every member's cursor for a room u
// has joined, which delivers read receipts, and otherwise only u's own.
func (s *Server) cursorFramesLocked(u *userState, r *roomState) []apron.Frame {
	var frames []apron.Frame
	for userID, cursor := range r.Reads() {
		if u.joined[r.ID()] == nil && userID != u.id {
			continue
		}
		frames = append(frames, apron.Frame(encodeNotification(apron.Activity{
			RoomID: r.ID(), From: cursor.From.User(), ReadMessageID: apron.LogID(cursor.ID),
		})))
	}
	return frames
}

// roomViewOfLocked is a room's current record with its delivery fields
// (§4.2): its log head, and the inclusive lower bound of retained history.
// The Go server retains every record, so history always starts at the
// room's creation record. The view shares nothing with the room, so it may
// be encoded after s.mu is released.
func (s *Server) roomViewOfLocked(r *roomState) apron.RoomView {
	created := apron.LogID(r.Log().Created())
	return apron.RoomView{Room: decodeRecord[apron.Room](encodeJSON(r.Record())), LatestLogID: apron.LogID(r.Log().Latest()), HistoryLogID: &created}
}

// decodeRecord decodes a logged record, which the server encoded, as the
// wire type of its kind, which keeps any member it has no field for
// (Unknown), so that it encodes as the record. A record that is not one is
// the zero value, as none is.
func decodeRecord[T any](raw jsontext.Value) T {
	var record T
	_ = json.Unmarshal(raw, &record)
	return record
}

// deliverLocked sends a frame to every connection of every member of the
// given rooms, once per connection. The frame is encoded once for all of them.
func (s *Server) deliverLocked(frame any, rooms ...*roomState) {
	frame = render(frame)
	seen := make(map[string]bool)
	for _, r := range rooms {
		for id, member := range r.Members() {
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

// encodeNotification renders a typed notification once, such as a
// room_update (§4.2.3) or an activity (§4.6), for sending to many
// connections.
func encodeNotification(n apron.Notification) jsontext.Value {
	return notification(n.Method(), n)
}

// deliverMembershipLocked sends a membership record to the room's members
// but u, as room_update memberships (§4.2.3); u's connections receive it
// with the room_update that tells them of the change.
func (s *Server) deliverMembershipLocked(membership apron.Membership, r *roomState, u *userState) {
	frame := encodeNotification(apron.RoomUpdate{Memberships: []apron.Membership{membership}})
	for id, member := range r.Members() {
		if id != u.id {
			member.send(frame)
		}
	}
}

// joinedLocked makes u a member of r and logs the membership
// (rooms.Table.Join), which it returns for delivery in a room_update's
// memberships (§4.2.3). It reports false, and changes nothing, if u was a
// member already or r is outside u's scope.
func (s *Server) joinedLocked(u *userState, r *roomState) (apron.Membership, bool) {
	record, added := s.rooms.Join(r, u, u.from())
	if !added {
		return apron.Membership{}, false
	}
	u.joined[r.ID()] = r
	delete(u.LeftAt, r.ID())
	s.users.Touch(u.id)
	return decodeRecord[apron.Membership](record.Raw), true
}

// addMemberLocked joins u to r (joinedLocked) and delivers the membership
// to the room's other members (§4.2.2); u's status goes to those who did
// not share a room with u, and u's unread counts r.
func (s *Server) addMemberLocked(u *userState, r *roomState) (apron.Membership, bool) {
	membership, added := s.joinedLocked(u, r)
	if !added {
		return apron.Membership{}, false
	}
	s.deliverMembershipLocked(membership, r, u)
	s.announceJoinStatusLocked(u, r)
	s.unreadChangedLocked(u, r.ID())
	return membership, true
}

// joinDefaultRoomLocked joins a new identity to the default room, so its
// room list is not empty. Like any join, its connections receive
// room_update joined with the membership (§4.2.3), after the auth result
// (§3.2).
func (s *Server) joinDefaultRoomLocked(u *userState) {
	s.joinLocked(u, s.rooms.Get(defaultRoomID))
}

// joinLocked joins u to r: the room's other members receive the membership,
// and u's connections one room_update with the room, its members, and the
// membership (§4.2.2). It reports whether u was not a member before.
func (s *Server) joinLocked(u *userState, r *roomState) bool {
	membership, added := s.addMemberLocked(u, r)
	if !added {
		return false
	}
	u.send(s.joinedUpdateLocked(r, membership))
	return true
}

// joinedUpdateLocked renders room_update joined for r: its record with its
// members, as bare user objects with their status, their complete objects
// in `users`, and the memberships that joined the user, if any (§4.2.3).
func (s *Server) joinedUpdateLocked(r *roomState, memberships ...apron.Membership) jsontext.Value {
	view := s.roomViewOfLocked(r)
	listed := s.addMembersLocked(&view, r)
	return encodeNotification(apron.RoomUpdate{Joined: []apron.RoomView{view}, Users: profiles(listed), Memberships: memberships})
}

// addMembersLocked adds a room's members to its view as bare user
// objects, ordered by user_id, and returns them. Each carries the shown
// status, offline and "" included, and seen with offline, as every current
// user object a listing carries does (§4.5.2); their complete objects go
// in `users`. A room with more than Config.MaxListedMembers lists only its
// most recently active members and adds member_count, the total (§4.2.1).
func (s *Server) addMembersLocked(view *apron.RoomView, r *roomState) map[string]*userState {
	ids, truncated := r.ListedMemberIDs(s.config.MaxListedMembers)
	if truncated {
		view.MemberCount = r.MemberCount()
	}
	listed := make(map[string]*userState, len(ids))
	view.Members = make([]apron.CurrentUser, len(ids))
	for i, id := range ids {
		member := r.Member(id)
		listed[id] = member
		view.Members[i] = member.shownObject()
	}
	return listed
}

// leaveLocked removes u from r (§4.2.2): the room's other members receive
// the logged membership, and u's connections one room_update with left and
// the membership. It reports whether u was a member.
func (s *Server) leaveLocked(u *userState, r *roomState) bool {
	record, left := s.rooms.Leave(r, u, u.from())
	if !left {
		return false
	}
	membership := decodeRecord[apron.Membership](record.Raw)
	s.deliverMembershipLocked(membership, r, u)
	delete(u.joined, r.ID())
	u.LeftAt[r.ID()] = record.ID
	s.users.Touch(u.id)
	u.send(encodeNotification(apron.RoomUpdate{Left: []apron.RoomRef{{RoomID: r.ID()}}, Memberships: []apron.Membership{membership}}))
	// The room no longer counts toward the user's unread (§4.9), and
	// neither do the threads of a private room, joined or not, which the
	// user can no longer see: their counts are all taken again.
	if r.Private() {
		s.hideThreadsLocked(u, r)
		s.unreadChangedLocked(u, "")
		// Writes into what u can no longer see end.
		s.endRefusedWritesLocked(u)
	} else {
		s.unreadChangedLocked(u, r.ID())
	}
	return true
}

// hideThreadsLocked follows u leaving the private room r: u can no longer see
// the threads it could see there, at any depth, so u leaves those it joined,
// which stops their deliveries, and u's connections receive room_update left
// for the others, which they may know from room_update updated (§4.2.3). A
// private thread u was not in was never visible to u, so it and its threads
// are passed over, and one u was in is left, which hides its own threads.
func (s *Server) hideThreadsLocked(u *userState, r *roomState) {
	var hidden []apron.RoomRef
	var walk func(*roomState)
	walk = func(room *roomState) {
		for _, thread := range room.Children() {
			switch {
			case thread.Private() && thread.Member(u.id) == nil:
			case thread.Private():
				s.leaveLocked(u, thread)
			default:
				if !s.leaveLocked(u, thread) {
					hidden = append(hidden, apron.RoomRef{RoomID: thread.ID()})
				}
				walk(thread)
			}
		}
	}
	walk(r)
	if len(hidden) > 0 {
		u.send(encodeNotification(apron.RoomUpdate{Left: hidden}))
	}
}

// announceRoomLocked sends a room's current record as room_update updated to
// its members, the parent's members for a thread that is not private, and
// editor, if any.
func (s *Server) announceRoomLocked(r *roomState, editor *userState) {
	audience := maps.Collect(r.Members())
	if r.Parent() != nil && !r.Private() {
		maps.Insert(audience, r.Parent().Members())
	}
	if editor != nil {
		audience[editor.id] = editor
	}
	frame := encodeNotification(apron.RoomUpdate{Updated: []apron.RoomView{s.roomViewOfLocked(r)}})
	for _, member := range audience {
		// A thread's record goes to its parent's members who may see it.
		if r.VisibleTo(member) {
			member.send(frame)
		}
	}
}

// setRoom creates a room (no room_id) or merges into an existing room's
// client fields (§4.2.4). parent_room_id and private are fixed at creation
// and ignored on updates, so an omitted private is kept; a thread created
// without private takes its parent's. Any authenticated user may create rooms and threads and
// update any room they can see.
//
// A new room joins only its creator, logging the creator's membership: the
// creator's connections receive room_update joined, then the membership,
// then the result. A new thread that is not private goes to the parent's
// other members as room_update updated, without joining them. An edit goes
// as updated to the room's members, to the parent's members for a thread
// that is not private, and to the editor.
func (s *Server) setRoom(c *client, p apron.RoomSetParams, reply server.Reply[apron.RoomSetResult]) *Error {
	updating := p.RoomID != ""
	roomID, parentID := p.RoomID, ""
	if !updating {
		parentID = p.ParentRoomID
	}
	var title, description string
	if p.Title != nil {
		title = *p.Title
	}
	if p.Description != nil {
		description = *p.Description
	}
	if len(title) > maxTitleBytes {
		return &Error{Code: CodeTooLarge, Message: fmt.Sprintf("title is at most %d bytes", maxTitleBytes)}
	}
	if len(description) > maxDescriptionBytes {
		return &Error{Code: CodeTooLarge, Message: fmt.Sprintf("description is at most %d bytes", maxDescriptionBytes)}
	}
	extWrite := p.Ext
	private := p.Private != nil && *p.Private

	s.mu.Lock()
	defer s.unlock()
	u := c.user
	var parent *roomState
	var keptExt extObject
	if updating {
		existing := s.visibleRoomLocked(u, roomID)
		if existing == nil {
			return invalidParams("Unknown room %q", roomID)
		}
		parent = existing.Parent()
		keptExt = existing.Record().Ext
		// An edit merges into the room's client fields (§4.2.4): a field it
		// leaves out keeps its value, and "" clears it.
		if p.Title == nil {
			title = existing.Record().Title
		}
		if p.Description == nil {
			description = existing.Record().Description
		}
	} else if parentID != "" {
		if parent = s.visibleRoomLocked(u, parentID); parent == nil {
			return invalidParams("Unknown parent room %q", parentID)
		}
	} else if private && s.privateRoomsFullLocked(u) {
		return &Error{Code: CodeDenied, Message: fmt.Sprintf("You are in %d private rooms, the most at once; leave one to start another", s.config.MaxPrivateRooms)}
	}
	if !updating {
		if err := mayCreateRoomLocked(u, parent); err != nil {
			return err
		}
	}
	// ext merges into the room's (§4.12), and the limit applies to the
	// result; so do the other client fields, one level deep (§4.2.4).
	ext := mergeExt(keptExt, extWrite)
	if encodedSize(ext) > maxRoomExtBytes {
		return &Error{Code: CodeTooLarge, Message: fmt.Sprintf("The room's ext would be over %d bytes", maxRoomExtBytes)}
	}
	if err := s.admitPostLocked(u); err != nil {
		return err
	}
	if title == "" && parent != nil {
		// Servers title threads so clients unaware of parent_room_id render them.
		title = rooms.ThreadTitle(description)
	}
	// Empty client fields are left out of the record.
	fields := roomlog.RoomRecord{Title: title, Description: description}
	if len(ext) > 0 {
		fields.Ext = ext
	}
	if p.Private == nil && !updating && parent != nil {
		// A thread created without private takes its parent's (§4.2.4).
		private = parent.Private()
	}
	// The server description may be the default room's.
	serverDescription, _ := s.profileValueLocked("description")
	// Room updates precede the result, so the room is known when it arrives.
	if updating {
		r := s.rooms.Get(roomID)
		s.rooms.Update(r, fields)
		s.announceRoomLocked(r, u)
		if description, _ := s.profileValueLocked("description"); description != serverDescription {
			s.announceServerLocked()
		}
		_ = reply.Send(apron.RoomSetResult{RoomID: r.ID()})
		return nil
	}
	// The room record and the membership are both logged before the
	// room_update, whose latest_log_id is then the membership's.
	r := s.rooms.Create("", parent, private, u.id, fields)
	membership, _ := s.joinedLocked(u, r)
	u.send(s.joinedUpdateLocked(r, membership))
	// A private thread's record goes only to its own members.
	if parent != nil && !r.Private() {
		frame := encodeNotification(apron.RoomUpdate{Updated: []apron.RoomView{s.roomViewOfLocked(r)}})
		for id, member := range parent.Members() {
			if id != u.id && r.VisibleTo(member) {
				member.send(frame)
			}
		}
	}
	_ = reply.Send(apron.RoomSetResult{RoomID: r.ID()})
	return nil
}

const (
	// maxTitleBytes bounds a room's title, maxDescriptionBytes its
	// description, and maxRoomExtBytes its merged ext, which every room
	// record carries.
	maxTitleBytes       = 1 << 10
	maxDescriptionBytes = 16 << 10
	maxRoomExtBytes     = 16 << 10
)

// listRooms answers room_list (§4.2.1): the rooms matching its filters,
// joined ones in `joined` (never truncated) and visible unjoined ones in
// `not_joined` (the most recently active maxListedRooms), each most recently
// active first. `filter` leaves either array out; one it asks for is present
// even when empty. With `members: true` every room carries its complete
// members as bare user objects, whose complete objects are in `users`;
// `user_id` then lists only that user, if they are a member.
//
// With `latest_log_id`, only rooms whose latest_log_id is greater are
// listed, and a result with `joined` carries `left`: the rooms among the
// candidates the user left after that position. The server logs every
// membership, so a left room's latest_log_id is at least its leave; a left
// room that is top-level, or a thread listed with parent_room_id or
// room_id, is in `not_joined` too when the filter asks for it.
//
// The result is followed by the read cursors kept for the listed rooms
// (§4.6).
func (s *Server) listRooms(c *client, p apron.RoomListParams, reply server.Reply[apron.RoomListResult]) *Error {
	wantJoined, wantOthers := p.Filter != apron.RoomsNotJoined, p.Filter != apron.RoomsJoined
	withMembers := p.Members
	parentID, hasParent := p.ParentRoomID, p.ParentRoomID != ""
	roomID, hasRoom := p.RoomID, p.RoomID != ""
	onlyUser, hasUser := p.UserID, p.UserID != ""
	var since int64
	hasSince := p.LatestLogID != nil
	if hasSince {
		since = int64(*p.LatestLogID)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	u := c.user
	var candidates []*roomState
	switch {
	case hasRoom:
		r := s.visibleRoomLocked(u, roomID)
		if r == nil {
			return invalidParams("Unknown room %q", roomID)
		}
		candidates = []*roomState{r}
	case hasParent:
		parent := s.visibleRoomLocked(u, parentID)
		if parent == nil {
			return invalidParams("Unknown parent room %q", parentID)
		}
		candidates = parent.Children()
	default:
		candidates = s.rooms.All()
	}
	joined, others, left := rooms.List(u, candidates, rooms.Listing{
		Joined: wantJoined, NotJoined: wantOthers, Since: since, HasSince: hasSince,
		Threads: hasRoom || hasParent, Limit: maxListedRooms,
	}, func(id string) int64 { return u.LeftAt[id] })

	users := make(map[string]*userState)
	views := func(rooms []*roomState) []apron.RoomView {
		views := make([]apron.RoomView, len(rooms))
		for i, r := range rooms {
			view := s.roomViewOfLocked(r)
			switch {
			case withMembers && hasUser:
				view.Members = []apron.CurrentUser{}
				if m := r.Member(onlyUser); m != nil {
					view.Members = append(view.Members, m.shownObject())
					users[onlyUser] = m
				}
			case withMembers:
				maps.Copy(users, s.addMembersLocked(&view, r))
			}
			views[i] = view
		}
		return views
	}
	// wire sends the arrays the filter asks for, empty too, and no others.
	result := apron.RoomListResult{Joined: views(joined), NotJoined: views(others)}
	if hasSince && wantJoined {
		result.Left = make([]apron.RoomRef, len(left))
		for i, r := range left {
			result.Left[i] = apron.RoomRef{RoomID: r.ID()}
		}
	}
	if withMembers {
		result.Users = profiles(users)
	}
	var cursors []apron.Frame
	for _, r := range slices.Concat(joined, others) {
		cursors = append(cursors, s.cursorFramesLocked(u, r)...)
	}
	_ = reply.Send(result, cursors...)
	return nil
}

// profiles lists users' complete objects ordered by user_id, for a result's
// `users` (§3.3).
func profiles(users map[string]*userState) []apron.CurrentUser {
	list := make([]apron.CurrentUser, 0, len(users))
	for _, id := range slices.Sorted(maps.Keys(users)) {
		list = append(list, users[id].profile())
	}
	return list
}

// joinRoom joins a visible room (§4.2.2): the logged membership goes to the
// room's members, the joining user's connections included, then room_update
// joined to the joining user's connections, then the result. Joining a room
// already joined logs nothing and re-sends room_update joined to the calling
// connection only.
//
// With the user_id of another user, a member of the room adds that user,
// which is how people are brought into a private room; adding a member
// changes nothing.
func (s *Server) joinRoom(c *client, p apron.RoomJoinParams, reply server.Reply[apron.OK]) *Error {
	roomID, targetID := p.RoomID, p.UserID
	hasTarget := targetID != ""
	s.mu.Lock()
	defer s.unlock()
	u := c.user
	r := s.visibleRoomLocked(u, roomID)
	if r == nil {
		return invalidParams("Unknown room %q", roomID)
	}
	target := u
	if hasTarget && targetID != u.id {
		if r.Member(u.id) == nil {
			return &Error{Code: CodeDenied, Message: fmt.Sprintf("Join %s before adding others to it", r.Title())}
		}
		if target = s.users.Get(targetID); target == nil {
			return invalidParams("Unknown user %q", targetID)
		}
		// Adding someone to a thread of a private room takes adding them to
		// that room first.
		if r.Parent() != nil && !r.Parent().VisibleTo(target) {
			return &Error{Code: CodeDenied, Message: fmt.Sprintf("Add %s to the private room %s is in first", targetID, r.Title())}
		}
		if !r.InScope(target.Scope) {
			return &Error{Code: CodeDenied, Message: fmt.Sprintf("%s is limited to other rooms", targetID)}
		}
		if r.Private() && r.Parent() == nil && r.Member(target.id) == nil && s.privateRoomsFullLocked(target) {
			return &Error{Code: CodeDenied, Message: fmt.Sprintf("%s is in as many private rooms as anyone may be, so cannot be added to another", targetID)}
		}
	}
	if r.Member(target.id) == nil {
		if err := admitActionLocked(u); err != nil {
			return err
		}
	}
	if !s.joinLocked(target, r) && target == u {
		c.enqueue(s.joinedUpdateLocked(r))
	}
	_ = reply.Send(apron.OK{})
	return nil
}

// leaveRoom leaves a room (§4.2.2): the logged membership goes to the room's
// members, the leaving user's connections included, then room_update left to
// the leaving user's connections, then the result. The room stays visible in
// room_list unless it is private. Leaving a room not joined changes nothing.
//
// With the user_id of another user, it removes that user, which the room's
// creator and users with the admin or moderator role may do.
func (s *Server) leaveRoom(c *client, p apron.RoomLeaveParams, reply server.Reply[apron.OK]) *Error {
	roomID, targetID := p.RoomID, p.UserID
	hasTarget := targetID != ""
	s.mu.Lock()
	defer s.unlock()
	u := c.user
	r := s.visibleRoomLocked(u, roomID)
	if r == nil {
		return invalidParams("Unknown room %q", roomID)
	}
	target := u
	if hasTarget && targetID != u.id {
		if !mayRemove(r, u) {
			return &Error{Code: CodeDenied, Message: fmt.Sprintf("Only the creator of %s or a moderator can remove people from it", r.Title())}
		}
		if target = s.users.Get(targetID); target == nil {
			return invalidParams("Unknown user %q", targetID)
		}
	}
	if r.Member(target.id) != nil {
		if err := admitActionLocked(u); err != nil {
			return err
		}
	}
	s.leaveLocked(target, r)
	_ = reply.Send(apron.OK{})
	return nil
}

// history returns a window of the log (§4.3): with room_id, that room's
// log, which needs no membership, only a room the user can see; without it,
// the log of every room the user joined, whose deliveries their connections
// receive, as one log, so a client catches up on all its rooms with one
// request. limit counts records of every kind; the slice is partitioned into
// rooms, messages, reactions, and membership, each omitted when empty. The
// server retains all records and does not compact, and it discards no
// prefix, so it never appends a full-member record.
//
// A move snapshot is in both rooms' logs and names the source room in
// prev_room_id, so a window bounded to one log_id (after == before) in the
// room a snapshot names walks prev_log_id back through a message's edits
// (§4.3.1). Across rooms, it comes once.
func (s *Server) history(c *client, p apron.HistoryParams, reply server.Reply[apron.HistoryResult]) *Error {
	roomID, hasRoom := p.RoomID, p.RoomID != ""
	var after, before int64
	hasAfter, hasBefore := p.After != nil, p.Before != nil
	if hasAfter {
		after = int64(*p.After)
	}
	if hasBefore {
		before = int64(*p.Before)
	}
	limit := s.config.HistoryPageSize
	if p.Limit > 0 {
		limit = min(p.Limit, maxHistoryPageSize)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	var rooms []*roomState
	if hasRoom {
		r := s.visibleRoomLocked(c.user, roomID)
		if r == nil {
			return invalidParams("Unknown room %q", roomID)
		}
		rooms = []*roomState{r}
	} else {
		for _, r := range c.user.joined {
			if r.VisibleTo(c.user) {
				rooms = append(rooms, r)
			}
		}
	}
	logs := make([]*roomlog.Timeline, len(rooms))
	var latest int64
	for i, r := range rooms {
		logs[i] = r.Log()
		latest = max(latest, r.Log().Latest())
	}
	matching, more := roomlog.Page(logs, roomlog.Query{
		After: after, HasAfter: hasAfter, Before: before, HasBefore: hasBefore,
		Limit: limit, MaxBytes: maxHistoryReplyBytes,
	})
	// With room_id, the room's log starts at its creation. Without it, the
	// log of the joined rooms starts where the server's does, at the default
	// room's creation, whoever is in which room, so the lower bound never
	// moves (§4.3.3); for a user in no rooms it is empty, ending just before.
	created := s.rooms.Get(defaultRoomID).Log().Created()
	if hasRoom {
		created = rooms[0].Log().Created()
	}
	if len(rooms) == 0 {
		latest = created - 1
	}
	// The log of no rooms has no history: its history_log_id is null.
	result := apron.HistoryResult{More: more, LatestLogID: apron.LogID(latest)}
	if len(rooms) > 0 {
		result.HistoryLogID = apron.LogID(created)
	}
	if len(matching) > 0 {
		result.FirstLogID, result.LastLogID = apron.LogID(matching[0].ID), apron.LogID(matching[len(matching)-1].ID)
	}
	for _, record := range matching {
		switch record.Kind {
		case roomlog.KindRoom:
			result.Rooms = append(result.Rooms, decodeRecord[apron.Room](record.Raw))
		case roomlog.KindMessage:
			result.Messages = append(result.Messages, decodeRecord[apron.Message](record.Raw))
		case roomlog.KindReactions:
			result.Reactions = append(result.Reactions, decodeRecord[apron.Reactions](record.Raw))
		case roomlog.KindMembership:
			result.Memberships = append(result.Memberships, decodeRecord[apron.Membership](record.Raw))
		}
	}
	_ = reply.Send(result)
	return nil
}

// activity applies a connection's activity (§4.6). typing and a read cursor
// in a room are relayed to the room's members; a read cursor must name a
// message in the room and only advances, and the server keeps the latest per
// user and sends it after the room is listed. A frame whose fields change
// nothing relays nothing, and typing past the user's limit on actions
// (admitActionLocked) is dropped.
func (s *Server) activity(c *client, p apron.ActivityParams) *Error {
	roomID := p.RoomID
	if roomID == "" {
		roomID = defaultRoomID
	}
	typing := p.Typing
	readID, hasRead := "", p.ReadMessageID != nil
	if hasRead {
		readID = p.ReadMessageID.String()
	}
	s.mu.Lock()
	defer s.unlock()
	inRoom := typing != nil || hasRead
	r := s.visibleRoomLocked(c.user, roomID)
	if inRoom && r == nil {
		return invalidParams("Unknown room %q", roomID)
	}
	// A cursor reads the room up to a message in it (§4.6): one naming a
	// message elsewhere would be relayed to the room's members as a receipt,
	// and its log_id would stand for the room's read position.
	if m := s.visibleMessageLocked(c.user, readID); hasRead && (m == nil || m.RoomID() != roomID) {
		return invalidParams("Unknown read_message_id %q in room %q", readID, roomID)
	}
	// The relays this request causes are queued before package wire sends
	// its result (§1).
	if !inRoom {
		return nil
	}
	u := c.user
	if typing != nil && admitActionLocked(u) != nil {
		// Typing beyond the user's limit is dropped: a notification has no
		// reply to carry retry_after.
		typing = nil
	}
	relay := apron.Activity{RoomID: roomID, From: u.from().User(), Typing: typing}
	if hasRead {
		id := int64(*p.ReadMessageID)
		if s.rooms.Read(r, u.id, rooms.Cursor{From: u.from(), MessageID: readID, ID: id}) {
			if u.joined[r.ID()] == nil {
				// A cursor for a room the user has not joined still syncs
				// across their own connections.
				u.send(encodeNotification(apron.Activity{RoomID: roomID, From: u.from().User(), ReadMessageID: *p.ReadMessageID}))
				if id >= r.Log().Latest() {
					// Read to the end: no earlier mention there counts.
					delete(u.Pings, r.ID())
					s.users.Touch(u.id)
				}
			}
			relay.ReadMessageID = *p.ReadMessageID
		}
	}
	read := relay.ReadMessageID != 0
	if typing != nil || read {
		s.deliverLocked(encodeNotification(relay), r)
	}
	if read {
		// Reading lowers the unread count the user's other devices show.
		s.unreadChangedLocked(u, r.ID())
	}
	return nil
}
