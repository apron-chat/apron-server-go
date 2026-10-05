package server

import (
	"maps"
	"slices"
	"time"
)

// Unread counts (§4.7): one count per user, the same in every push to any of
// their registrations. A message counts toward a user's unread when it
// arrived in a room after the user's read position there, is by someone
// else, is not deleted, and
//
//   - mentions the user, or
//   - is in a joined room the user has not muted (their own mute of it or of
//     a room it is a thread of), or
//   - in a room they have not joined, replies to them, unmuted likewise.
//
// The read position in a joined room is the latest of the user's read
// cursor (§4.4), where the message it names arrived in the room, their
// join, and their latest message there. A room the
// user has not joined counts only once a message there mentioned or replied
// to them (userState.pings), from that message. The unscoped mute silences
// pushes but not the count.
//
// Counts are kept per user and room for users with push registrations
// (userState.unread): a new message adds to the rooms whose count is
// known, and anything else that may change one, such as reading, posting,
// joining, leaving, a deletion, a move, or a room mute, forgets the counts
// it touches, which are counted again from the log when next needed. A
// recount looks at most at a room's newest maxUnreadScan records, so it
// stays cheap for a user who never reads a busy room, and the total stops
// at maxUnread.
const (
	maxUnread     = 999
	maxUnreadScan = 5000
	// badgeDelay is how long a user's badge pushes wait, so that one push
	// carries the count after a burst of changes.
	badgeDelay = 2 * time.Second
)

// readPositionLocked is the log_id after which messages in r are unread for
// u, and false when r does not count for u.
func (s *Server) readPositionLocked(u *userState, r *roomState) (int64, bool) {
	cursor := r.reads[u.id]
	since := cursor.id
	// A cursor names a message by its message_id; one moved into r arrived
	// later, at its move.
	if m := s.messages[cursor.messageID]; m != nil && m.roomID == r.id {
		since = max(since, arrival(m, r))
	}
	if u.joined[r.id] != nil {
		return max(since, r.active[u.id]), true
	}
	if ping, ok := u.pings[r.id]; ok {
		return max(since, ping-1), true
	}
	return 0, false
}

// arrival is the log_id at which m arrived in room r: its first snapshot in
// r's log, a move snapshot for a message moved there.
func arrival(m *messageState, r *roomState) int64 {
	for _, record := range m.records {
		if slices.Contains(record.rooms, r.id) {
			return record.id
		}
	}
	return 0
}

// countsLocked reports whether m, in room r, counts toward u's unread once
// it is past the read position.
func (s *Server) countsLocked(u *userState, r *roomState, m *messageState, now time.Time) bool {
	if m.owner == u.id {
		return false
	}
	info := m.info()
	switch {
	case info.deleted:
		return false
	case slices.Contains(info.mentions, u.id):
		return true
	case u.roomMuted(r, now):
		return false
	case u.joined[r.id] != nil:
		return true
	}
	target := s.messages[info.replyTo]
	return target != nil && target.owner == u.id
}

// recountLocked counts u's unread messages in r from the log.
func (s *Server) recountLocked(u *userState, r *roomState, now time.Time) int {
	since, counted := s.readPositionLocked(u, r)
	if !counted || !r.visibleTo(u) {
		return 0
	}
	start, _ := slices.BinarySearchFunc(r.log, since+1, compareLogID)
	start = max(start, len(r.log)-maxUnreadScan)
	count := 0
	seen := make(map[*messageState]bool)
	for _, record := range r.log[start:] {
		if record.kind != kindMessage {
			continue
		}
		m := s.messages[record.message]
		if m == nil || m.roomID != r.id || seen[m] {
			continue
		}
		seen[m] = true
		if arrival(m, r) > since && s.countsLocked(u, r, m, now) {
			count++
		}
	}
	return count
}

// unreadLocked returns u's unread count, counting again the rooms whose
// count is not known.
func (s *Server) unreadLocked(u *userState, now time.Time) int {
	// A room mute that ran out changes what counts in the room and its
	// threads: it is forgotten, and so are the counts.
	for id, mute := range u.roomMutes {
		if !mute.active(now) {
			delete(u.roomMutes, id)
			s.touchUser(u.id)
			u.unread = nil
		}
	}
	if u.unread == nil {
		u.unread = make(map[string]int)
	}
	rooms := maps.Clone(u.joined)
	for id := range u.pings {
		if r := s.rooms[id]; r != nil {
			rooms[id] = r
		}
	}
	total := 0
	for _, id := range slices.Sorted(maps.Keys(rooms)) {
		count, known := u.unread[id]
		if !known {
			count = s.recountLocked(u, rooms[id], now)
			u.unread[id] = count
		}
		if total += count; total >= maxUnread {
			return maxUnread
		}
	}
	return total
}

// unreadChangedLocked forgets u's count for a room, or every room when
// roomID is empty, after something that may change it, and schedules a
// badge push.
func (s *Server) unreadChangedLocked(u *userState, roomID string) {
	if roomID == "" {
		u.unread = nil
	} else {
		delete(u.unread, roomID)
	}
	s.scheduleBadgeLocked(u)
}

// messageUnreadLocked keeps the counts of the users a new or saved message
// concerns. previous is the snapshot the save replaced, nil for a new
// message, and from the room it was in before, for a move.
func (s *Server) messageUnreadLocked(m *messageState, previous map[string]any, from *roomState) {
	if len(s.pushes) == 0 {
		return
	}
	r := s.rooms[m.roomID]
	concerned := make(map[string]*userState)
	add := func(u *userState) {
		if u != nil && len(u.pushes) > 0 && u.id != m.owner {
			concerned[u.id] = u
		}
	}
	for _, room := range []*roomState{r, from} {
		if room == nil {
			continue
		}
		for _, u := range room.members {
			add(u)
		}
	}
	snapshots := []map[string]any{m.snapshot()}
	if previous != nil {
		snapshots = append(snapshots, previous)
	}
	for _, snapshot := range snapshots {
		body, _ := snapshot["body"].(map[string]any)
		for _, id := range mentions(body) {
			add(s.users[id])
		}
		if ref, ok := snapshot["reply_to"].(map[string]any); ok {
			if target := s.messages[ref["message_id"].(string)]; target != nil {
				add(s.users[target.owner])
			}
		}
	}
	now := time.Now()
	messageID := m.records[0].id
	for _, id := range slices.Sorted(maps.Keys(concerned)) {
		u := concerned[id]
		if !r.visibleTo(u) {
			continue
		}
		info := m.info()
		if u.joined[r.id] == nil && (slices.Contains(info.mentions, u.id) || s.messages[info.replyTo] != nil && s.messages[info.replyTo].owner == u.id) {
			// A room the user has not joined counts from the first message
			// there that mentioned or replied to them.
			if since, ok := u.pings[r.id]; !ok || since > messageID {
				u.pings[r.id] = messageID
				s.touchUser(u.id)
				s.unreadChangedLocked(u, r.id)
				continue
			}
		}
		count, known := u.unread[r.id]
		switch {
		case previous != nil || from != nil:
			// An edit, deletion, or move counted again when it touches a
			// message past the read position.
			since, counted := s.readPositionLocked(u, r)
			if from != nil {
				s.unreadChangedLocked(u, from.id)
				s.unreadChangedLocked(u, r.id)
			} else if counted && arrival(m, r) > since {
				s.unreadChangedLocked(u, r.id)
			}
		case !known:
			if _, counted := s.readPositionLocked(u, r); counted {
				s.scheduleBadgeLocked(u)
			}
		case s.countsLocked(u, r, m, now):
			u.unread[r.id] = count + 1
			s.scheduleBadgeLocked(u)
		}
	}
}

// scheduleBadgeLocked sends u's badge pushes after badgeDelay, when u has a
// registration with scope badge (§4.7). Later changes within the delay ride
// along.
func (s *Server) scheduleBadgeLocked(u *userState) {
	if u.badgeTimer != nil || !slices.ContainsFunc(slices.Collect(maps.Values(u.pushes)), (*pushRegistration).takesBadges) {
		return
	}
	s.badges.Add(1)
	u.badgeTimer = time.AfterFunc(s.badgeDelay, func() {
		defer s.badges.Done()
		s.mu.Lock()
		defer s.unlock()
		u.badgeTimer = nil
		if !s.closed && s.users[u.id] == u {
			s.badgeLocked(u, time.Now())
		}
	})
}

// stopBadgeLocked cancels u's pending badge pushes.
func (s *Server) stopBadgeLocked(u *userState) {
	if u.badgeTimer != nil && u.badgeTimer.Stop() {
		s.badges.Done()
	}
	u.badgeTimer = nil
}

// badgeLocked sends a badge push (§4.7), {push_id, unread} without a
// message, to each of the user's registrations that takes them and last
// accepted another count. It goes to attended and muted users too: what a
// mute silences still changes the count.
func (s *Server) badgeLocked(u *userState, now time.Time) {
	unread := -1
	for _, p := range sortedPushes(u) {
		if !p.takesBadges() || !p.live(now) {
			continue
		}
		if unread < 0 {
			unread = s.unreadLocked(u, now)
		}
		if unread == p.lastUnread || p.inFlight && unread == p.pendingUnread {
			continue
		}
		payload := map[string]any{"unread": unread}
		if p.pushID != "" {
			payload["push_id"] = p.pushID
		}
		s.deliverPushLocked(p, unread, encodeJSON(payload), "low", true)
	}
}
