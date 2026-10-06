package server

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"sort"
	"time"

	"golang.org/x/time/rate"
)

type reactionSet struct {
	from   map[string]any
	emojis []string
}

// messageState is the current state of one message. Only non-empty reaction
// sets are kept; clearing removes the user's entry.
type messageState struct {
	id string
	// from is the author identity of every snapshot.
	from      map[string]any
	logID     int64
	owner     string
	roomID    string
	reactions map[string]reactionSet
	// records are every logged snapshot, for redaction; the last is current.
	records []*logRecord
	// cached is what push counting reads of the current snapshot, decoded
	// once; nil until read, and after a new snapshot.
	cached *messageInfo
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

// currentRaw is the JSON of the message's current snapshot.
func (m *messageState) currentRaw() jsontext.Value {
	return m.records[len(m.records)-1].raw
}

// snapshot decodes the message's current snapshot, a copy the caller owns.
func (m *messageState) snapshot() map[string]any {
	return m.records[len(m.records)-1].value()
}

// saveMessage creates a message (no message_id) or saves an existing one
// (§4.4): every client field is replaced by the submitted state. Without
// room_id the message goes to the default room (§3.5). A save naming a
// different room_id moves the message; the snapshot is logged in and
// broadcast to both rooms, followed by a reactions record in the destination
// when the message has reactions. The broadcasts precede the result (§1),
// so a sender who has joined receives a new upload's pending snapshot before
// the result carrying its write URL. Posting does not join the room
// (§4.3.2): a poster who has not joined gets only the result. A new message
// with no text and no embeds is neither logged nor broadcast, and its
// result is {}.
func (s *Server) saveMessage(c *client, req request) (any, bool, *rpcError) {
	roomID, err := parseString(req.params, "room_id", false)
	if err != nil {
		return nil, false, err
	}
	if _, has := req.params["room_id"]; !has {
		roomID = defaultRoomID
	}
	messageID, err := parseString(req.params, "message_id", false)
	if err != nil {
		return nil, false, err
	}
	_, replacing := req.params["message_id"]
	if replacing && !validMessageID(messageID) {
		return nil, false, invalidParams("message_id must be a positive decimal string")
	}
	replyID, hasReply, err := parseMessageRef(req.params, "reply_to")
	if err != nil {
		return nil, false, err
	}
	deleted, err := parseBool(req.params, "deleted", false)
	if err != nil {
		return nil, false, err
	}
	if deleted && !replacing {
		return nil, false, invalidParams("Cannot create a deleted message")
	}
	var body map[string]any
	if !deleted {
		body, err = parseObject(req.params, "body", true)
		if err != nil {
			return nil, false, err
		}
		if err := validateBody(body); err != nil {
			return nil, false, err
		}
	}
	extWrite, _, err := parseExt(req.params, "ext")
	if err != nil {
		return nil, false, err
	}

	s.mu.Lock()
	defer s.unlock()
	u := c.user
	destination := s.visibleRoomLocked(u, roomID)
	if destination == nil {
		return nil, false, invalidParams("Unknown room %q", roomID)
	}
	from := u.from()
	var current *messageState
	if replacing {
		current = s.visibleMessageLocked(u, messageID)
		if current == nil {
			return nil, false, invalidParams("Unknown message %q", messageID)
		}
		if current.owner != u.id {
			return nil, false, &rpcError{Code: codeDenied, Message: "Only the author may edit, move, or delete this message"}
		}
		// A move snapshot is logged in both rooms and names both, so a
		// message may not move to a room that some who see its room cannot
		// see, such as a private room: the move would show them its room_id
		// and the message.
		if source := s.rooms[current.roomID]; source != destination && source.revealsTo(destination) {
			return nil, false, &rpcError{Code: codeDenied, Message: "A message cannot move to a room that fewer people can see"}
		}
		from = current.from
	}
	if hasReply {
		target := s.visibleMessageLocked(u, replyID)
		if target == nil || (replacing && replyID == messageID) {
			return nil, false, invalidParams("reply_to must name another existing message")
		}
		// A reply names its target's message_id to everyone who sees the
		// reply, so it may not name a message some of them cannot see.
		if destination.revealsTo(s.rooms[target.roomID]) {
			return nil, false, &rpcError{Code: codeDenied, Message: "A reply cannot quote a message that fewer people can see"}
		}
	}
	var previous map[string]any
	if current != nil {
		previous = current.snapshot()
	}
	// ext merges into the current snapshot's (§3.5), and the limit applies
	// to the result. A creation, and a save of a tombstone, merges into an
	// empty ext (§3.5), and a tombstone carries none (§4.4).
	var ext extObject
	if !deleted {
		ext = mergeExt(extOf(previous["ext"]), extWrite)
		if encodedSize(ext) > maxMessageExtBytes {
			return nil, false, &rpcError{Code: codeTooLarge, Message: fmt.Sprintf("The message's ext would be over %d bytes", maxMessageExtBytes)}
		}
	}
	if !replacing {
		if text, _ := body["text"].(string); text == "" && len(asList(body["embeds"])) == 0 {
			result := map[string]any{}
			if req.hasID {
				c.sendResult(req, result)
			}
			return result, true, nil
		}
		if err := s.admitPostLocked(u); err != nil {
			return nil, false, err
		}
	}

	logID := s.nextIDLocked()
	if !replacing {
		messageID = formatID(logID)
	}
	// Embeds are resolved last: a new upload or stream embed reserves a write.
	var written []any
	if body != nil {
		var embeds []any
		embeds, written, err = s.resolveEmbedsLocked(c, messageID, previous, body)
		if err != nil {
			return nil, false, err
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
	s.wakeLocked(current, snapshot, previous)
	result := map[string]any{"message_id": messageID}
	if len(written) > 0 {
		result["embeds"] = written
	}
	if req.hasID {
		c.sendResult(req, result)
	}
	return result, true, nil
}

// commitSnapshotLocked logs and delivers a message snapshot, adding
// prev_log_id, in the message's room and, for a move, the previous one too;
// a move snapshot names the previous room in prev_room_id, since the
// previous snapshot is in that room's log (§2). It reports whether the
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
	record := newLogRecord(logID, kindMessage, snapshot)
	record.message = m.id
	m.records = append(m.records, record)
	s.touchMessage(m)
	s.appendLocked(record, rooms...)
	s.deliverLocked(rawNotification("message", record.raw), rooms...)
	return moved
}

// republishLocked publishes a server-made snapshot of a message, such as a
// finished upload or stream (§4.8.3): edit rewrites a copy of the
// current body, and nothing is published when it reports no change.
func (s *Server) republishLocked(m *messageState, edit func(body map[string]any) bool) {
	snapshot := m.snapshot()
	body, _ := snapshot["body"].(map[string]any)
	if body == nil || snapshot["deleted"] == true || !edit(body) {
		return
	}
	logID := s.nextIDLocked()
	snapshot["log_id"] = formatID(logID)
	s.commitSnapshotLocked(m, snapshot, logID)
}

// redactLocked rewrites a deleted message's earlier snapshots into
// tombstones at their original log_ids (§4.4).
func (s *Server) redactLocked(m *messageState) {
	for _, record := range m.records {
		record.rewrite(tombstone)
		s.touchRecord(record)
	}
}

func tombstone(snapshot map[string]any) {
	if _, embedded := snapshot["log_id"]; !embedded {
		return
	}
	delete(snapshot, "body")
	delete(snapshot, "ext")
	snapshot["deleted"] = true
}

// admitPostLocked applies MessagesPerMinute to new messages, room_set, and
// /avatar: each user may send a burst of MessagesPerMinute, refilled evenly
// over a minute.
func (s *Server) admitPostLocked(u *userState) *rpcError {
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
		return &rpcError{Code: codeRetryAfter, Message: "Too many messages; slow down", Data: map[string]any{"retry_after": wait}}
	}
	return nil
}

// reactionElements returns every non-empty reaction set, ordered by user ID.
func (m *messageState) reactionElements() []any {
	users := make([]string, 0, len(m.reactions))
	for userID := range m.reactions {
		users = append(users, userID)
	}
	sort.Strings(users)
	elements := make([]any, 0, len(users))
	for _, userID := range users {
		set := m.reactions[userID]
		elements = append(elements, map[string]any{"from": cloneObject(set.from), "emojis": slices.Clone(set.emojis)})
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

// parseMessageRef reads a message reference (reply_to). Clients
// send bare references; any other keys, such as an echoed snapshot, are ignored.
func parseMessageRef(params map[string]jsontext.Value, name string) (string, bool, *rpcError) {
	raw, present := params[name]
	if !present {
		return "", false, nil
	}
	var ref map[string]jsontext.Value
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &ref) != nil || ref == nil {
		return "", false, invalidParams("%s must be a message object", name)
	}
	id, err := parseString(ref, "message_id", true)
	if err != nil || !validMessageID(id) {
		return "", false, invalidParams("%s.message_id must be a positive decimal string", name)
	}
	return id, true, nil
}

const (
	// maxMentions bounds body.mentions.
	maxMentions = 256
	// maxMessageExtBytes bounds a message's ext, merged (§3.5), which
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

func validateBody(body map[string]any) *rpcError {
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
			return &rpcError{Code: codeDenied, Message: fmt.Sprintf("A message mentions at most %d users", maxMentions)}
		}
		for _, value := range mentions {
			if id, ok := value.(string); !ok || id == "" {
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
			return &rpcError{Code: codeDenied, Message: fmt.Sprintf("A message holds at most %d embeds", maxEmbedsPerMessage)}
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
// (§4.7). Duplicates collapse; an unchanged set logs nothing. The broadcast
// precedes the result (§1).
func (s *Server) react(c *client, req request) (any, bool, *rpcError) {
	messageID, err := parseString(req.params, "message_id", true)
	if err != nil {
		return nil, false, err
	}
	emojis, err := parseEmojis(req.params)
	if err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.unlock()
	u := c.user
	m := s.visibleMessageLocked(u, messageID)
	if m == nil {
		return nil, false, invalidParams("Unknown message %q", messageID)
	}
	if !sameEmojiSet(m.reactions[u.id].emojis, emojis) {
		s.touchMessage(m)
		from := u.from()
		if len(emojis) == 0 {
			delete(m.reactions, u.id)
		} else {
			m.reactions[u.id] = reactionSet{from: from, emojis: emojis}
		}
		s.commitReactionsLocked(m, []any{map[string]any{"from": cloneObject(from), "emojis": slices.Clone(emojis)}})
	}
	result := map[string]any{}
	if req.hasID {
		c.sendResult(req, result)
	}
	return result, true, nil
}

// commitReactionsLocked logs and delivers one reactions record in the
// message's current room. Reaction sets carry no prev_log_id (§2).
func (s *Server) commitReactionsLocked(m *messageState, elements []any) {
	logID := s.nextIDLocked()
	r := s.rooms[m.roomID]
	record := newLogRecord(logID, kindReactions, map[string]any{
		"log_id":     formatID(logID),
		"message_id": m.id,
		"room_id":    m.roomID,
		"reactions":  elements,
	})
	s.appendLocked(record, r)
	s.deliverLocked(rawNotification("reactions", record.raw), r)
}

func parseEmojis(params map[string]jsontext.Value) ([]string, *rpcError) {
	raw, ok := params["emojis"]
	if !ok {
		return nil, invalidParams("Missing emojis")
	}
	var values []jsontext.Value
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &values) != nil {
		return nil, invalidParams("emojis must be an array of strings")
	}
	emojis := make([]string, 0, len(values))
	for _, value := range values {
		var emoji string
		if json.Unmarshal(value, &emoji) != nil || emoji == "" {
			return nil, invalidParams("emojis must be non-empty strings")
		}
		if len(emoji) > maxEmojiBytes {
			return nil, &rpcError{Code: codeTooLarge, Message: fmt.Sprintf("An emoji is at most %d bytes", maxEmojiBytes)}
		}
		if !slices.Contains(emojis, emoji) {
			emojis = append(emojis, emoji)
		}
	}
	if len(emojis) > maxDistinctEmoji {
		// A count limit the server sets is denied (§1.1).
		return nil, &rpcError{Code: codeDenied, Message: fmt.Sprintf("At most %d distinct emoji per message", maxDistinctEmoji)}
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
