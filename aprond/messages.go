package aprond

import (
	"encoding/json/v2"
	"slices"
	"time"

	"golang.org/x/time/rate"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/aprond/internal/messages"
	"github.com/apron-chat/apron-server-go/aprond/internal/roomlog"
	"github.com/apron-chat/apron-server-go/aprond/internal/rooms"
)

// messageState is a message as the rooms module keeps it (rooms.Message):
// its room, sender, snapshots, and reaction sets; with what the server
// keeps of it beyond them. It is the rooms.MessageHolder of the server's
// rooms.Table, which keeps it by message_id.
type messageState struct {
	*rooms.Message
	// cached is what push counting reads of the current snapshot, decoded
	// once; nil until read, and after a new snapshot.
	cached *messageInfo
	// pushHeld is set while the push of a new message waits for its live
	// append text to end (§4.8.5).
	pushHeld bool
}

// holdMessage is the state the server keeps of a new or restored message.
func holdMessage(m *rooms.Message) *messageState { return &messageState{Message: m} }

// messageInfo is what unread counts (§4.9) read of a message's current
// snapshot.
type messageInfo struct {
	deleted  bool
	mentions []string
	replyTo  string
}

// info returns what unread counts read of the current snapshot.
func (m *messageState) info() *messageInfo {
	if m.cached == nil {
		snapshot := m.Current()
		m.cached = &messageInfo{deleted: snapshot.Deleted, mentions: snapshot.Mentions(), replyTo: snapshot.ReplyID()}
	}
	return m.cached
}

// storedBody is a body the server edited as a JSON object (bodyObject), as
// a snapshot holds it.
func storedBody(body map[string]any) *roomlog.Body {
	stored := new(roomlog.Body)
	_ = json.Unmarshal(encodeJSON(body), stored)
	return stored
}

// saveMessage creates a message (no message_id) or saves an existing one
// (§4.4): every client field is replaced by the submitted state. Without
// room_id the message goes to the default room (§3.5). A save naming a
// different room_id moves the message; the snapshot is logged in and
// broadcast to both rooms, followed by a reactions record in the destination
// when the message has reactions. The broadcasts precede the result (§1),
// so a sender who has joined receives a new upload's pending snapshot before
// the result carrying its write URL. Posting does not join the room
// (§4.2.2): a poster who has not joined gets only the result. A new message
// with no text and no embeds is invalid_params (§3.5.1). A new message is a
// save with no message_id, as backend.Message makes it.
func (s *Server) saveMessage(c *client, p apron.EditParams, reply apron.Reply[apron.MessageResult]) *Error {
	roomID := p.RoomID
	if roomID == "" {
		roomID = defaultRoomID
	}
	messageID, replacing := "", p.MessageID != 0
	if replacing {
		messageID = p.MessageID.String()
	}
	replyID, hasReply := "", p.ReplyTo != nil
	if hasReply {
		replyID = p.ReplyTo.MessageID.String()
	}
	// package wire refuses a deletion without message_id.
	deleted := p.Deleted
	var body map[string]any
	if !deleted {
		if p.Body == nil {
			return apron.Missing("body")
		}
		if err := messages.CheckBody(p.Body); err != nil {
			return err
		}
		body = bodyObject(p.Body)
	}
	extWrite := p.Ext
	// The extensions' filters see what is saved, outside s.mu, and may
	// change it or refuse it (Registrar.FilterMessage).
	sharedFiltered := false
	if !deleted {
		draft := &Draft{RoomID: roomID, Body: body, ext: extWrite}
		if replacing {
			draft.MessageID = messageID
		}
		if hasReply {
			draft.ReplyTo = replyID
		}
		if err := s.filterMessage(c, draft); err != nil {
			return err
		}
		body, extWrite = draft.Body, draft.ext
		sharedFiltered = draft.shared
	}

	s.mu.Lock()
	defer s.unlock()
	if c.conn.Closing() {
		return connectionClosing()
	}
	u := c.user
	destination := s.visibleRoomLocked(u, roomID)
	if destination == nil {
		return invalidParams("Unknown room %q", roomID)
	}
	from := u.from()
	var current *messageState
	// editor is set for a save by a mentioned user of the text that the
	// sender shares (ext:editors), which changes only body.text.
	editor := false
	if replacing {
		current = s.visibleMessageLocked(u, messageID)
		if current == nil {
			return invalidParams("Unknown message %q", messageID)
		}
		if current.Owner() != u.id {
			if deleted || !sharesText(current.Current(), u.id) {
				return &Error{Code: CodeDenied, Message: "Only the author may edit, move, or delete this message"}
			}
			// Live text is still writing into body.text (§4.8.5).
			if hasLiveAppend(current.Current()) {
				return &Error{Code: CodeDenied, Message: "The message's text is still being written"}
			}
			// The filters ran before the grant or the mention reached this
			// user, so they skipped the save: it goes only filtered.
			if len(s.filters) > 0 && !sharedFiltered {
				return &Error{Code: CodeDenied, Message: "The message changed while it was saved; try again"}
			}
			// The save keeps every other field, so it neither moves the
			// message nor changes what it replies to.
			editor = true
			destination = s.rooms.Get(current.RoomID())
			roomID = destination.ID()
			hasReply = false
		}
		// A move snapshot is logged in both rooms and names both, so a
		// message may not move to a room that some who see its room cannot
		// see, such as a private room: the move would show them its room_id
		// and the message.
		if source := s.rooms.Get(current.RoomID()); source != destination && source.RevealsTo(destination) {
			return &Error{Code: CodeDenied, Message: "A message cannot move to a room that fewer people can see"}
		}
		from = current.From()
	}
	if hasReply {
		target := s.visibleMessageLocked(u, replyID)
		if target == nil || (replacing && replyID == messageID) {
			return invalidParams("reply_to must name another existing message")
		}
		// A reply names its target's message_id to everyone who sees the
		// reply, so it may not name a message some of them cannot see.
		if destination.RevealsTo(s.rooms.Get(target.RoomID())) {
			return &Error{Code: CodeDenied, Message: "A reply cannot quote a message that fewer people can see"}
		}
	}
	// previous is the snapshot the save replaces.
	var previous *roomlog.Message
	if current != nil {
		previous = current.Current()
	}
	if editor {
		body = sharedTextBody(previous, body)
		// The rest of the body is the sender's: the whole stays a body that
		// its sender can send back in one frame (ReadLimit).
		if encodedSize(body) > int(s.config.ReadLimit) {
			return &Error{Code: CodeTooLarge, Message: "The message would be too large to send back"}
		}
		replyID = previous.ReplyID()
		hasReply = replyID != ""
		extWrite = nil
	}
	// ext merges into the current snapshot's, and the limit applies to the
	// result: one over it is too_large and changes nothing (§4.12). A
	// creation, and a save onto a tombstone, merges into an empty ext, and a
	// save with deleted: true drops it (§4.12).
	var ext extObject
	if !deleted {
		var kept extObject
		if previous != nil && !previous.Deleted {
			kept = previous.Ext
		}
		ext = mergeExt(kept, extWrite)
		// ext:editors: edited_by names who saved the snapshot, when not
		// the sender, and requests do not set it.
		if editor {
			ext = withEditedBy(ext, u.id)
		} else {
			ext = withEditedBy(ext, "")
		}
		if err := messages.CheckExt(ext); err != nil {
			return err
		}
	}
	if !replacing {
		if text, _ := body["text"].(string); text == "" && len(asList(body["embeds"])) == 0 {
			return invalidParams("A new message needs text or embeds")
		}
		if err := s.admitPostLocked(u); err != nil {
			return err
		}
	}

	logID := s.log.NextID()
	if !replacing {
		messageID = formatID(logID)
	}
	// Embeds are resolved last: a new upload, terminal, or append embed
	// reserves a write.
	var written []apron.EmbedWrite
	// An editor's save keeps the embeds as they are stored: it starts no
	// upload or live text, and releases none (ext:editors).
	if body != nil && !editor {
		var embeds []any
		var err *Error
		embeds, written, err = s.resolveEmbedsLocked(c, messageID, previous, body)
		if err != nil {
			return err
		}
		// body was decoded for this request, so it is the snapshot's own.
		if len(embeds) > 0 {
			body["embeds"] = embeds
		} else {
			delete(body, "embeds")
		}
	}
	s.releaseEmbedsLocked(previous, body)

	snapshot := &roomlog.Message{MessageID: messageID, LogID: formatID(logID), RoomID: roomID, From: from, Deleted: deleted, Ext: ext}
	if !deleted {
		snapshot.Body = storedBody(body)
	}
	if hasReply {
		snapshot.ReplyTo = &roomlog.Ref{MessageID: replyID}
	}

	if current == nil {
		current = s.rooms.NewMessage(messageID, from, u.id)
	}
	source := current.RoomID()
	moved := s.commitSnapshotLocked(current, snapshot, logID)
	if deleted {
		s.rooms.Redact(current.Message)
	}
	if reactions := current.Reactions(); moved && len(reactions) > 0 {
		// The reactions follow the message into its room.
		record, r := s.rooms.LogReactions(current.Message, reactions)
		s.deliverLocked(rawNotification("reactions", record.Raw), r)
	}
	var movedFrom *roomState
	if moved {
		movedFrom = s.rooms.Get(source)
	}
	s.messageUnreadLocked(current, previous, movedFrom)
	if !replacing {
		// The author's own message moves their read position (§4.9).
		s.unreadChangedLocked(u, destination.ID())
	}
	saved := current.Current()
	switch {
	case !replacing:
		s.wakeNewLocked(current, saved)
	case current.pushHeld:
		// A save that ends the live append text sends the held push.
		if deleted || !hasLiveAppend(saved) {
			s.releasePushLocked(current)
		}
	default:
		s.wakeLocked(current, saved, previous)
	}
	result := apron.MessageResult{MessageID: apron.LogID(logID)}
	if replacing {
		result.MessageID = p.MessageID
	}
	result.Embeds = written
	_ = reply.Send(result)
	return nil
}

// commitSnapshotLocked logs a message snapshot (rooms.Table.Snapshot) and
// delivers it to the rooms whose logs hold it: the message's, and for a
// move the previous one too. It reports whether the snapshot moved the
// message.
func (s *Server) commitSnapshotLocked(m *messageState, snapshot *roomlog.Message, logID int64) bool {
	record, rooms, moved := s.rooms.Snapshot(m.Message, snapshot, logID)
	m.cached = nil
	s.deliverLocked(rawNotification("message", record.Raw), rooms...)
	return moved
}

// republishLocked publishes a server-made snapshot of a message, such as a
// finished upload or live text (§4.8.3): edit rewrites a copy of the
// current body, and nothing is published when it reports no change.
func (s *Server) republishLocked(m *messageState, edit func(body *roomlog.Body) bool) {
	snapshot := m.Current()
	if snapshot.Body == nil || snapshot.Deleted {
		return
	}
	if !edit(snapshot.Body) {
		return
	}
	logID := s.log.NextID()
	snapshot.LogID = formatID(logID)
	s.commitSnapshotLocked(m, snapshot, logID)
}

// admitPostLocked applies MessagesPerMinute to new messages, room_set,
// /avatar, and /emoji add: each user may send a burst of
// MessagesPerMinute, refilled evenly over a minute.
func (s *Server) admitPostLocked(u *userState) *Error {
	limit := s.config.MessagesPerMinute
	if limit <= 0 {
		return nil
	}
	if u.posts == nil {
		u.posts = rate.NewLimiter(rate.Every(time.Minute/time.Duration(limit)), limit)
	}
	now := time.Now()
	reservation := u.posts.ReserveN(now, 1)
	if delay := reservation.DelayFrom(now); delay > 0 {
		reservation.CancelAt(now)
		return RetryAfter("Too many messages; slow down", delay)
	}
	return nil
}

const (
	// Each user may make actionBurst changes at once that are logged or
	// sent to others but are not posts: reaction sets, joins and leaves,
	// profile edits, and typing. They refill one every actionRefill.
	actionBurst  = 120
	actionRefill = 500 * time.Millisecond
)

// admitActionLocked applies the user's limit on such changes, across all of
// their connections, so that toggling a reaction or a membership in a loop
// cannot grow every room's log and broadcast to its members without end.
// Beyond it the request is retry_after and changes nothing.
func admitActionLocked(u *userState) *Error {
	if u.actions == nil {
		u.actions = rate.NewLimiter(rate.Every(actionRefill), actionBurst)
	}
	now := time.Now()
	reservation := u.actions.ReserveN(now, 1)
	if delay := reservation.DelayFrom(now); delay > 0 {
		reservation.CancelAt(now)
		return RetryAfter("Too many changes; slow down", delay)
	}
	return nil
}

// mentions returns a message body's body.mentions (§3.5).
func mentions(body map[string]any) []string {
	var ids []string
	for _, value := range asList(body["mentions"]) {
		if id, ok := value.(string); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// react replaces the caller's complete reaction set on one message
// (§4.7). Entries are kept fully qualified, and duplicates collapse; an
// unchanged set logs nothing. The broadcast precedes the result (§1).
func (s *Server) react(c *client, p apron.ReactParams, reply apron.Reply[apron.OK]) *Error {
	messageID := p.MessageID.String()
	emojis, err := messages.Emojis(p.Emojis)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.unlock()
	u := c.user
	m := s.visibleMessageLocked(u, messageID)
	if m == nil {
		return invalidParams("Unknown message %q", messageID)
	}
	// A key must be a custom emoji (ext:emoji), unless the set has it
	// already, so that a removed one does not keep the user from changing
	// the rest of their set.
	for _, emoji := range emojis {
		if emojiKeyPattern.MatchString(emoji) && s.emoji.Get(emoji) == nil && !slices.Contains(m.Emojis(u.id), emoji) {
			return invalidParams("Unknown custom emoji %s", emoji)
		}
	}
	if !m.SameEmojis(u.id, emojis) {
		if err := admitActionLocked(u); err != nil {
			return err
		}
		record, r := s.rooms.React(m.Message, u.from(), emojis)
		s.deliverLocked(rawNotification("reactions", record.Raw), r)
	}
	_ = reply.Send(apron.OK{})
	return nil
}
