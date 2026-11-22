package apron

import (
	"fmt"
	"slices"
	"sort"
	"time"

	"golang.org/x/time/rate"

	uts51 "github.com/apron-chat/apron-server-go/apron/internal/emoji"
	"github.com/apron-chat/apron-server-go/apron/internal/roomlog"
	"github.com/apron-chat/apron-server-go/wire"
)

type reactionSet struct {
	from   roomlog.Author
	emojis []string
}

// messageState is the current state of one message. Only non-empty reaction
// sets are kept; clearing removes the user's entry.
type messageState struct {
	id string
	// from is the author identity of every snapshot.
	from      roomlog.Author
	logID     int64
	owner     string
	roomID    string
	reactions map[string]reactionSet
	// records are every logged snapshot, for redaction; the last is current.
	records []*roomlog.Record
	// cached is what push counting reads of the current snapshot, decoded
	// once; nil until read, and after a new snapshot.
	cached *messageInfo
	// pushHeld is set while the push of a new message waits for its live
	// append text to end (§4.8.5).
	pushHeld bool
}

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
		snapshot := m.snapshot()
		body, _ := snapshot["body"].(map[string]any)
		info := &messageInfo{deleted: snapshot["deleted"] == true, mentions: mentions(body)}
		if ref, ok := snapshot["reply_to"].(map[string]any); ok {
			info.replyTo, _ = ref["message_id"].(string)
		}
		m.cached = info
	}
	return m.cached
}

// snapshot decodes the message's current snapshot, a copy the caller owns.
func (m *messageState) snapshot() map[string]any {
	return decodeObject(m.records[len(m.records)-1].Raw)
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
func (s *Server) saveMessage(c *client, p wire.EditParams, reply wire.Reply[wire.MessageResult]) *Error {
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
			return wire.Missing("body")
		}
		body = bodyObject(p.Body)
		if err := validateBody(body); err != nil {
			return err
		}
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
		if current.owner != u.id {
			if deleted || !sharesText(current.snapshot(), u.id) {
				return &Error{Code: CodeDenied, Message: "Only the author may edit, move, or delete this message"}
			}
			// Live text is still writing into body.text (§4.8.5).
			if hasLiveAppend(current.snapshot()) {
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
			destination = s.rooms[current.roomID]
			roomID = destination.id
			hasReply = false
		}
		// A move snapshot is logged in both rooms and names both, so a
		// message may not move to a room that some who see its room cannot
		// see, such as a private room: the move would show them its room_id
		// and the message.
		if source := s.rooms[current.roomID]; source != destination && source.revealsTo(destination) {
			return &Error{Code: CodeDenied, Message: "A message cannot move to a room that fewer people can see"}
		}
		from = current.from
	}
	if hasReply {
		target := s.visibleMessageLocked(u, replyID)
		if target == nil || (replacing && replyID == messageID) {
			return invalidParams("reply_to must name another existing message")
		}
		// A reply names its target's message_id to everyone who sees the
		// reply, so it may not name a message some of them cannot see.
		if destination.revealsTo(s.rooms[target.roomID]) {
			return &Error{Code: CodeDenied, Message: "A reply cannot quote a message that fewer people can see"}
		}
	}
	var previous map[string]any
	if current != nil {
		previous = current.snapshot()
	}
	if editor {
		body = sharedTextBody(previous, body)
		// The rest of the body is the sender's: the whole stays a body that
		// its sender can send back in one frame (ReadLimit).
		if encodedSize(body) > int(s.config.ReadLimit) {
			return &Error{Code: CodeTooLarge, Message: "The message would be too large to send back"}
		}
		if ref, ok := previous["reply_to"].(map[string]any); ok {
			replyID, hasReply = ref["message_id"].(string)
		}
		extWrite = nil
	}
	// ext merges into the current snapshot's, and the limit applies to the
	// result: one over it is too_large and changes nothing (§4.12). A
	// creation, and a save onto a tombstone, merges into an empty ext, and a
	// save with deleted: true drops it (§4.12).
	var ext extObject
	if !deleted {
		var kept extObject
		if previous["deleted"] != true {
			kept = extOf(previous["ext"])
		}
		ext = mergeExt(kept, extWrite)
		// ext:editors: edited_by names who saved the snapshot, when not
		// the sender, and requests do not set it.
		if editor {
			ext = withEditedBy(ext, u.id)
		} else {
			ext = withEditedBy(ext, "")
		}
		if encodedSize(ext) > maxMessageExtBytes {
			return &Error{Code: CodeTooLarge, Message: fmt.Sprintf("The message's ext would be over %d bytes", maxMessageExtBytes)}
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
	var written []any
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

	snapshot := map[string]any{
		"message_id": messageID,
		"log_id":     formatID(logID),
		"room_id":    roomID,
		"from":       from,
	}
	if deleted {
		snapshot["deleted"] = true
	} else {
		snapshot["body"] = body
	}
	if hasReply {
		snapshot["reply_to"] = map[string]any{"message_id": replyID}
	}
	if len(ext) > 0 {
		snapshot["ext"] = ext
	}

	if current == nil {
		current = &messageState{id: messageID, from: from, owner: u.id, reactions: make(map[string]reactionSet)}
		s.messages[messageID] = current
	}
	source := current.roomID
	moved := s.commitSnapshotLocked(current, snapshot, logID)
	if deleted {
		s.redactLocked(current)
	}
	if moved && len(current.reactions) > 0 {
		s.commitReactionsLocked(current, current.reactionElements())
	}
	var movedFrom *roomState
	if moved {
		movedFrom = s.rooms[source]
	}
	s.messageUnreadLocked(current, previous, movedFrom)
	if !replacing {
		// The author's own message moves their read position (§4.9).
		s.unreadChangedLocked(u, destination.id)
	}
	switch {
	case !replacing:
		s.wakeNewLocked(current, snapshot)
	case current.pushHeld:
		// A save that ends the live append text sends the held push.
		if deleted || !hasLiveAppend(snapshot) {
			s.releasePushLocked(current)
		}
	default:
		s.wakeLocked(current, snapshot, previous)
	}
	result := wire.MessageResult{MessageID: wire.LogID(logID)}
	if replacing {
		result.MessageID = p.MessageID
	}
	for _, write := range written {
		result.Embeds = append(result.Embeds, encodeJSON(write))
	}
	_ = reply.Send(result)
	return nil
}

// commitSnapshotLocked logs and delivers a message snapshot, adding
// prev_log_id, in the message's room and, for a move, the previous one too;
// a move snapshot names the previous room in prev_room_id, since the
// previous snapshot is in that room's log (§4.3.1). It reports whether the
// snapshot moved the message.
func (s *Server) commitSnapshotLocked(m *messageState, snapshot map[string]any, logID int64) bool {
	delete(snapshot, "prev_room_id")
	if m.logID != 0 {
		snapshot["prev_log_id"] = formatID(m.logID)
	}
	destination := s.rooms[snapshot["room_id"].(string)]
	rooms := []*roomState{destination}
	moved := m.roomID != "" && m.roomID != destination.id
	if moved {
		source := s.rooms[m.roomID]
		rooms = []*roomState{source, destination}
		// A move out of a room that some who see the destination cannot see,
		// such as a private one, does not name it to them.
		if !destination.revealsTo(source) {
			snapshot["prev_room_id"] = m.roomID
		}
	}
	m.logID = logID
	m.roomID = destination.id
	m.cached = nil
	if !moved && len(m.records) == 0 && destination.members[m.owner] != nil {
		destination.active[m.owner] = logID
	}
	record := roomlog.New(logID, roomlog.KindMessage, snapshot)
	record.Message = m.id
	m.records = append(m.records, record)
	s.touchMessage(m)
	s.appendLocked(record, rooms...)
	s.deliverLocked(rawNotification("message", record.Raw), rooms...)
	return moved
}

// republishLocked publishes a server-made snapshot of a message, such as a
// finished upload or live text (§4.8.3): edit rewrites a copy of the
// current body, and nothing is published when it reports no change.
func (s *Server) republishLocked(m *messageState, edit func(body map[string]any) bool) {
	snapshot := m.snapshot()
	body, _ := snapshot["body"].(map[string]any)
	if body == nil || snapshot["deleted"] == true || !edit(body) {
		return
	}
	logID := s.log.NextID()
	snapshot["log_id"] = formatID(logID)
	s.commitSnapshotLocked(m, snapshot, logID)
}

// redactLocked rewrites a deleted message's earlier snapshots into
// tombstones at their original log_ids (§4.4). At an equal log_id a
// tombstone wins (§2.3), so a client that kept one of them as it was
// replaces it with the redacted one it reads later.
func (s *Server) redactLocked(m *messageState) {
	for _, record := range m.records {
		s.log.Redact(record)
	}
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
		wait := int64((delay + time.Second - 1) / time.Second)
		return &Error{Code: CodeRetryAfter, Message: "Too many messages; slow down", Data: map[string]any{"retry_after": wait}}
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
		return retryAfter("Too many changes; slow down", delay)
	}
	return nil
}

// reactionElements returns every non-empty reaction set, ordered by user ID.
func (m *messageState) reactionElements() []roomlog.Reaction {
	users := make([]string, 0, len(m.reactions))
	for userID := range m.reactions {
		users = append(users, userID)
	}
	sort.Strings(users)
	elements := make([]roomlog.Reaction, 0, len(users))
	for _, userID := range users {
		set := m.reactions[userID]
		elements = append(elements, roomlog.Reaction{From: set.from, Emojis: slices.Clone(set.emojis)})
	}
	return elements
}

func validMessageID(id string) bool {
	if len(id) == 0 || id[0] < '1' || id[0] > '9' {
		return false
	}
	for _, ch := range id {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

const (
	// maxMentions bounds body.mentions.
	maxMentions = 256
	// maxMessageExtBytes bounds a message's ext, merged (§4.12), which
	// every snapshot of the message carries.
	maxMessageExtBytes = 64 << 10
)

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

func validateBody(body map[string]any) *Error {
	if raw, ok := body["text"]; ok {
		if _, ok := raw.(string); !ok {
			return invalidParams("body.text must be a string")
		}
	}
	if raw, ok := body["format"]; ok {
		format, ok := raw.(string)
		if !ok || (format != "plain" && format != "markdown") {
			return invalidParams("body.format must be plain or markdown")
		}
	}
	if raw, ok := body["mentions"]; ok {
		mentions, ok := raw.([]any)
		if !ok {
			return invalidParams("body.mentions must be an array of user_id strings")
		}
		if len(mentions) > maxMentions {
			// A count limit the server sets is denied (§1.1).
			return &Error{Code: CodeDenied, Message: fmt.Sprintf("A message mentions at most %d users", maxMentions)}
		}
		for _, value := range mentions {
			if id, ok := value.(string); !ok || !validID(id) {
				return invalidParams("body.mentions must be an array of user_id strings")
			}
		}
	}
	if raw, ok := body["embeds"]; ok {
		embeds, ok := raw.([]any)
		if !ok {
			return invalidParams("body.embeds must be an array")
		}
		if len(embeds) > maxEmbedsPerMessage {
			return &Error{Code: CodeDenied, Message: fmt.Sprintf("A message holds at most %d embeds", maxEmbedsPerMessage)}
		}
		for i, value := range embeds {
			embed, ok := value.(map[string]any)
			if !ok {
				return invalidParams("body.embeds[%d] must be an object", i)
			}
			if kind, ok := embed["kind"].(string); !ok || kind == "" {
				return invalidParams("body.embeds[%d].kind must be a non-empty string", i)
			}
		}
	}
	return nil
}

const (
	maxEmojiBytes    = 64
	maxDistinctEmoji = 20
)

// react replaces the caller's complete reaction set on one message
// (§4.7). Entries are kept fully qualified, and duplicates collapse; an
// unchanged set logs nothing. The broadcast precedes the result (§1).
func (s *Server) react(c *client, p wire.ReactParams, reply wire.Reply[wire.Empty]) *Error {
	messageID := p.MessageID.String()
	emojis, err := normalizeEmojis(p.Emojis)
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
		if emojiKeyPattern.MatchString(emoji) && s.emoji[emoji] == nil && !slices.Contains(m.reactions[u.id].emojis, emoji) {
			return invalidParams("Unknown custom emoji %s", emoji)
		}
	}
	if !sameEmojiSet(m.reactions[u.id].emojis, emojis) {
		if err := admitActionLocked(u); err != nil {
			return err
		}
		s.touchMessage(m)
		from := u.from()
		if len(emojis) == 0 {
			delete(m.reactions, u.id)
		} else {
			m.reactions[u.id] = reactionSet{from: from, emojis: emojis}
		}
		s.commitReactionsLocked(m, []roomlog.Reaction{{From: from, Emojis: slices.Clone(emojis)}})
	}
	_ = reply.Send(wire.Empty{})
	return nil
}

// commitReactionsLocked logs and delivers one reactions record in the
// message's current room. Reaction sets carry no prev_log_id (§4.3.1).
func (s *Server) commitReactionsLocked(m *messageState, elements []roomlog.Reaction) {
	logID := s.log.NextID()
	r := s.rooms[m.roomID]
	record := roomlog.NewReactions(logID, m.id, m.roomID, elements)
	s.appendLocked(record, r)
	s.deliverLocked(rawNotification("reactions", record.Raw), r)
}

func normalizeEmojis(values []string) ([]string, *Error) {
	emojis := make([]string, 0, len(values))
	for _, emoji := range values {
		// Entries that differ only in U+FE0F are the same entry, kept fully
		// qualified (§4.7).
		if emoji = uts51.Qualify(emoji); emoji == "" {
			return nil, invalidParams("emojis must be non-empty strings")
		}
		if len(emoji) > maxEmojiBytes {
			return nil, &Error{Code: CodeTooLarge, Message: fmt.Sprintf("An emoji is at most %d bytes", maxEmojiBytes)}
		}
		if !slices.Contains(emojis, emoji) {
			emojis = append(emojis, emoji)
		}
	}
	if len(emojis) > maxDistinctEmoji {
		// A count limit the server sets is denied (§1.1).
		return nil, &Error{Code: CodeDenied, Message: fmt.Sprintf("At most %d distinct emoji per message", maxDistinctEmoji)}
	}
	return emojis, nil
}

func sameEmojiSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, emoji := range a {
		if !slices.Contains(b, emoji) {
			return false
		}
	}
	return true
}
