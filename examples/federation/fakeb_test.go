package main

import (
	"context"
	"encoding/json/v2"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeB is just enough of a remote Apron server with ext:remote links
// (the federation contract's wire) to test the bridge against: one private
// room, token sign-in for one link account bound to the bridge's server,
// history, deliveries, and message, reactions, and me requests that act as
// shadow users when params.ext.remote.user_id is set. Its own users act
// through its methods, not over the wire.
type fakeB struct {
	t      *testing.T
	server *httptest.Server
	// self is the bridge's server's address, which the link is bound to.
	self, selfAuthority string
	room, link          string

	mu       sync.Mutex
	tokens   map[string]bool
	rotateTo string
	auths    int
	last     int64
	users    map[string]*fakeUser
	members  map[string]bool
	messages map[string]*fakeMessage
	records  []fakeRecord
	conns    map[*fakeConn]bool
	// requests are the link's requests, other than auth and history.
	requests []map[string]any
}

type fakeUser struct {
	id, name string
	remote   map[string]any
}

type fakeMessage struct {
	owner     string
	snapshot  map[string]any
	reactions map[string][]string
	records   []int
}

type fakeRecord struct {
	logID int64
	kind  string
	value map[string]any
}

type fakeConn struct {
	ws   *websocket.Conn
	user string
	out  chan []byte
}

func newFakeB(t *testing.T, self, token string) *fakeB {
	authority := strings.TrimSuffix(strings.TrimPrefix(self, "ws://"), "/ws")
	b := &fakeB{
		t: t, self: self, selfAuthority: authority, room: "r1", link: "link_a",
		tokens: map[string]bool{token: true}, users: make(map[string]*fakeUser), members: make(map[string]bool),
		messages: make(map[string]*fakeMessage), conns: make(map[*fakeConn]bool),
	}
	b.users[b.link] = &fakeUser{id: b.link, name: "Link to A"}
	b.members[b.link] = true
	b.server = httptest.NewServer(http.HandlerFunc(b.serve))
	t.Cleanup(func() {
		b.drop()
		b.server.Close()
	})
	return b
}

func (b *fakeB) address() string { return "ws" + strings.TrimPrefix(b.server.URL, "http") + "/ws" }

func (b *fakeB) authority() string { return strings.TrimPrefix(b.server.URL, "http://") }

// addUser adds a user of B's own to the room.
func (b *fakeB) addUser(id, name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.users[id] = &fakeUser{id: id, name: name}
	b.members[id] = true
}

func (b *fakeB) nextLocked() int64 {
	b.last = max(time.Now().UnixMilli(), b.last+1)
	return b.last
}

func (u *fakeUser) from() map[string]any {
	from := map[string]any{"user_id": u.id}
	if u.name != "" {
		from["name"] = u.name
	}
	if u.remote != nil {
		from["ext"] = map[string]any{"remote": maps.Clone(u.remote)}
	}
	return from
}

func (u *fakeUser) profile() map[string]any { return u.from() }

// encode marshals a value the way a server sends it.
func encode(value any) []byte {
	data, _ := json.Marshal(value, json.Deterministic(true))
	return data
}

// clone deep-copies a JSON object.
func clone(value map[string]any) map[string]any {
	var copied map[string]any
	_ = json.Unmarshal(encode(value), &copied)
	return copied
}

// deliverLocked sends a notification to the connections of the room's
// members.
func (b *fakeB) deliverLocked(method string, params map[string]any) {
	frame := encode(map[string]any{"method": method, "params": params})
	for c := range b.conns {
		if b.members[c.user] {
			select {
			case c.out <- frame:
			default:
			}
		}
	}
}

func (b *fakeB) appendLocked(kind string, value map[string]any) int {
	b.records = append(b.records, fakeRecord{logID: b.last, kind: kind, value: value})
	b.deliverLocked(kind, clone(value))
	return len(b.records) - 1
}

func (b *fakeB) joinLocked(u *fakeUser) {
	if b.members[u.id] {
		return
	}
	b.members[u.id] = true
	logID := b.nextLocked()
	b.records = append(b.records, fakeRecord{logID: logID, kind: "membership", value: map[string]any{
		"log_id": strconv.FormatInt(logID, 10), "room_id": b.room, "members": []any{map[string]any{"user": u.from(), "joined": true}},
	}})
}

// saveLocked creates or saves a message as u, as §3.5 and §4.4 do.
func (b *fakeB) saveLocked(u *fakeUser, params map[string]any, remote map[string]any) (string, *rpcError) {
	if room, _ := params["room_id"].(string); room != b.room {
		return "", &rpcError{Code: codeInvalidParams, Message: "Unknown room " + room}
	}
	messageID, saving := params["message_id"].(string)
	deleted := params["deleted"] == true
	var m *fakeMessage
	if saving {
		if m = b.messages[messageID]; m == nil {
			return "", &rpcError{Code: codeInvalidParams, Message: "Unknown message"}
		}
		if m.owner != u.id {
			return "", &rpcError{Code: codeDenied, Message: "Only the author may edit"}
		}
	} else if deleted {
		return "", &rpcError{Code: codeInvalidParams, Message: "Cannot create a deleted message"}
	}
	var replyTo map[string]any
	if ref, ok := params["reply_to"].(map[string]any); ok {
		if target, _ := ref["message_id"].(string); b.messages[target] == nil || target == messageID {
			return "", &rpcError{Code: codeInvalidParams, Message: "reply_to must name another existing message"}
		}
		replyTo = map[string]any{"message_id": ref["message_id"]}
	}
	body, _ := params["body"].(map[string]any)
	if !deleted && body == nil {
		return "", &rpcError{Code: codeInvalidParams, Message: "Missing body"}
	}
	b.joinLocked(u)
	logID := b.nextLocked()
	if !saving {
		messageID = strconv.FormatInt(logID, 10)
		m = &fakeMessage{owner: u.id, reactions: make(map[string][]string)}
		b.messages[messageID] = m
	}
	snapshot := map[string]any{"message_id": messageID, "log_id": strconv.FormatInt(logID, 10), "room_id": b.room, "from": u.from()}
	if saving {
		snapshot["from"] = m.snapshot["from"]
	}
	if deleted {
		snapshot["deleted"] = true
	} else {
		snapshot["body"] = body
		if replyTo != nil {
			snapshot["reply_to"] = replyTo
		}
		ext := map[string]any{}
		if m.snapshot != nil && m.snapshot["deleted"] != true {
			if kept, ok := m.snapshot["ext"].(map[string]any); ok {
				ext = maps.Clone(kept)
			}
		}
		if written, ok := params["ext"].(map[string]any); ok {
			for key, value := range written {
				if key != "remote" {
					ext[key] = value
				}
			}
		}
		if remote != nil {
			ext["remote"] = remote
		}
		if len(ext) > 0 {
			snapshot["ext"] = ext
		}
	}
	m.snapshot = snapshot
	if deleted {
		// Every delete redacts (§4.4).
		for _, i := range m.records {
			value := b.records[i].value
			delete(value, "body")
			delete(value, "ext")
			value["deleted"] = true
		}
	}
	m.records = append(m.records, b.appendLocked("message", clone(snapshot)))
	return messageID, nil
}

func (b *fakeB) reactLocked(u *fakeUser, messageID string, emojis []string) *rpcError {
	m := b.messages[messageID]
	if m == nil {
		return &rpcError{Code: codeInvalidParams, Message: "Unknown message"}
	}
	m.reactions[u.id] = emojis
	logID := b.nextLocked()
	b.appendLocked("reactions", map[string]any{
		"log_id": strconv.FormatInt(logID, 10), "message_id": messageID, "room_id": b.room,
		"reactions": []any{map[string]any{"from": u.from(), "emojis": emojis}},
	})
	return nil
}

func (b *fakeB) renameLocked(u *fakeUser, name string) {
	u.name = name
	frame := encode(map[string]any{"method": "user", "params": map[string]any{"new": u.profile()}})
	for c := range b.conns {
		select {
		case c.out <- frame:
		default:
		}
	}
}

// Users of B act through these.

func (b *fakeB) post(userID, text string, mentions []string, replyTo string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	params := map[string]any{"room_id": b.room, "body": map[string]any{"text": text}}
	if len(mentions) > 0 {
		params["body"].(map[string]any)["mentions"] = toAny(mentions)
	}
	if replyTo != "" {
		params["reply_to"] = map[string]any{"message_id": replyTo}
	}
	id, err := b.saveLocked(b.users[userID], params, nil)
	if err != nil {
		b.t.Fatalf("B post: %v", err)
	}
	return id
}

func (b *fakeB) edit(userID, messageID, text string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, err := b.saveLocked(b.users[userID], map[string]any{"room_id": b.room, "message_id": messageID, "body": map[string]any{"text": text}}, nil); err != nil {
		b.t.Fatalf("B edit: %v", err)
	}
}

func (b *fakeB) remove(userID, messageID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, err := b.saveLocked(b.users[userID], map[string]any{"room_id": b.room, "message_id": messageID, "deleted": true}, nil); err != nil {
		b.t.Fatalf("B delete: %v", err)
	}
}

func (b *fakeB) react(userID, messageID string, emojis ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.reactLocked(b.users[userID], messageID, emojis); err != nil {
		b.t.Fatalf("B react: %v", err)
	}
}

func (b *fakeB) rename(userID, name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.renameLocked(b.users[userID], name)
}

// drop closes every connection, as a restart of B would.
func (b *fakeB) drop() {
	b.mu.Lock()
	conns := slices.Collect(maps.Keys(b.conns))
	b.mu.Unlock()
	for _, c := range conns {
		_ = c.ws.CloseNow()
	}
}

func toAny(values []string) []any {
	list := make([]any, len(values))
	for i, v := range values {
		list[i] = v
	}
	return list
}

// snapshot is a message's current snapshot.
func (b *fakeB) snapshot(messageID string) map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	if m := b.messages[messageID]; m != nil {
		return clone(m.snapshot)
	}
	return nil
}

// find returns the current snapshots that match.
func (b *fakeB) find(match func(m map[string]any) bool) []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	var found []map[string]any
	for _, id := range slices.Sorted(maps.Keys(b.messages)) {
		if snapshot := clone(b.messages[id].snapshot); match(snapshot) {
			found = append(found, snapshot)
		}
	}
	return found
}

func (b *fakeB) user(id string) (fakeUser, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	u := b.users[id]
	if u == nil {
		return fakeUser{}, false
	}
	return *u, true
}

func (b *fakeB) reactionsOf(messageID, userID string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if m := b.messages[messageID]; m != nil {
		return slices.Clone(m.reactions[userID])
	}
	return nil
}

func (b *fakeB) linkRequests() []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.requests)
}

// The wire.

func (b *fakeB) serve(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(1 << 24)
	c := &fakeConn{ws: ws, out: make(chan []byte, 1024)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			select {
			case frame := <-c.out:
				if ws.Write(ctx, websocket.MessageText, frame) != nil {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	c.out <- encode(map[string]any{"method": "server", "params": map[string]any{
		"apron": 10, "auth": []string{"token"}, "ping": 1,
		"capabilities": []string{"history", "edit", "rooms", "reactions", "ext", "ext:remote"},
	}})
	b.mu.Lock()
	b.conns[c] = true
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.conns, c)
		b.mu.Unlock()
		_ = ws.CloseNow()
	}()
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		var f struct {
			Method string         `json:"method"`
			ID     string         `json:"id"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(data, &f) != nil {
			continue
		}
		if f.Method == "ping" {
			c.out <- []byte(`{"method":"pong"}`)
			continue
		}
		result, failure := b.handle(c, f.Method, f.Params, data)
		reply := map[string]any{"id": f.ID}
		if failure != nil {
			reply["error"] = failure
		} else {
			reply["result"] = result
		}
		c.out <- encode(reply)
	}
}

func (b *fakeB) handle(c *fakeConn, method string, params map[string]any, raw []byte) (map[string]any, *rpcError) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if method == "auth" {
		b.auths++
		token, _ := params["token"].(string)
		if params["scheme"] != "token" || !b.tokens[token] {
			return nil, &rpcError{Code: codeDenied, Message: "Unknown token"}
		}
		c.user = b.link
		result := map[string]any{"you": b.users[b.link].profile()}
		if b.rotateTo != "" && token != b.rotateTo {
			b.tokens[b.rotateTo] = true
			result["token"] = b.rotateTo
		}
		return result, nil
	}
	if c.user == "" {
		return nil, &rpcError{Code: codeDenied, Message: "Sign in first"}
	}
	if method == "history" {
		return b.historyLocked(params), nil
	}
	var request map[string]any
	_ = json.Unmarshal(raw, &request)
	b.requests = append(b.requests, request)

	// The link acts as the shadow of the user that ext.remote names.
	actor := b.users[c.user]
	var remote map[string]any
	if ext, ok := params["ext"].(map[string]any); ok {
		if r, ok := ext["remote"].(map[string]any); ok {
			if id, _ := r["user_id"].(string); id != "" {
				shadowID := id + "@" + b.selfAuthority
				if b.users[shadowID] == nil {
					b.users[shadowID] = &fakeUser{id: shadowID, remote: map[string]any{"server": b.self, "user_id": id}}
				}
				actor = b.users[shadowID]
				remote = map[string]any{"server": b.self, "user_id": id}
				for _, key := range []string{"room_id", "message_id"} {
					if value, ok := r[key].(string); ok {
						remote[key] = value
					}
				}
			}
		}
	}
	switch method {
	case "message":
		if remote != nil && params["message_id"] != nil {
			// A save keeps the stored ext.remote unless it writes one.
			if _, hasMessage := remote["message_id"]; !hasMessage {
				remote = nil
			}
		}
		id, err := b.saveLocked(actor, params, remote)
		if err != nil {
			return nil, err
		}
		return map[string]any{"message_id": id}, nil
	case "reactions":
		messageID, _ := params["message_id"].(string)
		var emojis []string
		for _, e := range asList(params["emojis"]) {
			if s, ok := e.(string); ok {
				emojis = append(emojis, s)
			}
		}
		if err := b.reactLocked(actor, messageID, emojis); err != nil {
			return nil, err
		}
		return map[string]any{}, nil
	case "me":
		if name, ok := params["name"].(string); ok {
			b.renameLocked(actor, name)
		}
		return map[string]any{"you": actor.profile()}, nil
	}
	return nil, &rpcError{Code: -32601, Message: "Unsupported method"}
}

func asList(value any) []any {
	list, _ := value.([]any)
	return list
}

func (b *fakeB) historyLocked(params map[string]any) map[string]any {
	after, _ := strconv.ParseInt(stringOf(params["after"]), 10, 64)
	limit := 200
	if n, ok := params["limit"].(float64); ok && n > 0 {
		limit = int(n)
	}
	result := map[string]any{"more": false}
	var messages, reactions, memberships []any
	var first, last int64
	count := 0
	for _, r := range b.records {
		if r.logID < after {
			continue
		}
		if count == limit {
			result["more"] = true
			break
		}
		count++
		if first == 0 {
			first = r.logID
		}
		last = r.logID
		switch r.kind {
		case "message":
			messages = append(messages, clone(r.value))
		case "reactions":
			reactions = append(reactions, clone(r.value))
		case "membership":
			memberships = append(memberships, clone(r.value))
		}
	}
	result["messages"], result["reactions"], result["memberships"] = messages, reactions, memberships
	if count > 0 {
		result["first_log_id"], result["last_log_id"] = strconv.FormatInt(first, 10), strconv.FormatInt(last, 10)
	}
	result["latest_log_id"] = strconv.FormatInt(b.last, 10)
	return result
}

func stringOf(value any) string {
	s, _ := value.(string)
	return s
}
