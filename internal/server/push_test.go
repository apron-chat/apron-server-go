package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/json/v2"
	"io"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/apron-chat/apron-server-go/internal/store"
)

type relayRequest struct {
	path    string
	header  http.Header
	body    []byte
	payload map[string]any
}

// testRelay records the push deliveries it receives. A path ending in /gone
// answers 410 and one ending in /missing 404.
type testRelay struct {
	*httptest.Server
	received chan relayRequest
}

func newTestRelay(t *testing.T) *testRelay {
	t.Helper()
	relay := &testRelay{received: make(chan relayRequest, 64)}
	relay.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		request := relayRequest{path: r.URL.Path, header: r.Header.Clone(), body: body}
		if r.Header.Get("Content-Encoding") == "" {
			_ = json.Unmarshal(body, &request.payload)
		}
		relay.received <- request
		switch {
		case strings.HasSuffix(r.URL.Path, "/gone"):
			w.WriteHeader(http.StatusGone)
		case strings.HasSuffix(r.URL.Path, "/missing"):
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusCreated)
		}
	}))
	t.Cleanup(relay.Close)
	return relay
}

// pushes returns the deliveries made so far, sorted by path, after the
// requests c sent before have finished waking users and their badge
// pushes have gone out.
func (relay *testRelay) pushes(t *testing.T, app *Server, c *testClient) []relayRequest {
	t.Helper()
	c.drain(t)
	app.badges.Wait()
	app.push.wait()
	var requests []relayRequest
	for len(relay.received) > 0 {
		requests = append(requests, <-relay.received)
	}
	slices.SortStableFunc(requests, func(a, b relayRequest) int { return strings.Compare(a.path, b.path) })
	return requests
}

// paths lists the paths of deliveries.
func paths(requests []relayRequest) []string {
	list := make([]string, len(requests))
	for i, request := range requests {
		list[i] = request.path
	}
	return list
}

// pushTestServer accepts internal push endpoints and sends badge pushes
// without waiting to coalesce them.
func pushTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	config := DefaultConfig()
	config.AllowInsecurePush = true
	app, httpServer := newTestServer(t, config)
	app.badgeDelay = 0
	return app, httpServer
}

// addAccount adds an account that has joined general and has no connection,
// like a passkey user who is away. others are told of the join.
func addAccount(t *testing.T, app *Server, id string, others ...*testClient) *userState {
	t.Helper()
	app.mu.Lock()
	u := newUserState(id, strings.ToUpper(id[:1])+id[1:])
	u.passkey = &passkeyUser{user: u}
	app.users[u.id] = u
	app.addMemberLocked(u, app.rooms[defaultRoomID])
	app.unlock()
	for _, other := range others {
		expectMembership(t, other, "general", id, true)
	}
	return u
}

// addPush registers an endpoint for a user without a connection.
func addPush(app *Server, u *userState, p pushRegistration) *pushRegistration {
	app.mu.Lock()
	defer app.unlock()
	p.userID = u.id
	if p.kind == "" {
		p.kind = "relay"
	}
	if p.renewed.IsZero() {
		p.renewed = time.Now()
	}
	p.lastUnread = -1
	registration := &p
	app.addPushLocked(u, registration)
	return registration
}

// goIdle tells the server nobody attends c, and checks the status echo.
func goIdle(t *testing.T, c *testClient) {
	t.Helper()
	echoed(t, c, c.status(t, map[string]any{"idle": true}), "idle")
}

func TestPushWakesMentionsAndRepliesOfIdleUsers(t *testing.T) {
	relay := newTestRelay(t)
	app, httpServer := pushTestServer(t)
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	alice := addAccount(t, app, "alice", a, b)
	addPush(app, alice, pushRegistration{url: relay.URL + "/alice", token: "tok", pushID: "alice-phone", wake: defaultWake})
	addPush(app, alice, pushRegistration{url: relay.URL + "/alice/gone", wake: defaultWake})
	post := func(id string, params map[string]any) string {
		t.Helper()
		messageID, _ := save(t, a, id, params)
		b.notification(t, "message")
		return messageID
	}

	// Only body.mentions decides who is mentioned; text is never parsed.
	text := "@alice: the deploy is done"
	id := post("text", map[string]any{"body": map[string]any{"text": text, "format": "markdown"}})
	if got := relay.pushes(t, app, a); len(got) != 0 {
		t.Fatalf("text mention woke %d", len(got))
	}
	// An edit wakes the users it adds to mentions, on every registration.
	post("add-mention", map[string]any{"message_id": id, "body": map[string]any{"text": text, "format": "markdown", "mentions": []any{"alice"}}})
	got := relay.pushes(t, app, a)
	if !reflect.DeepEqual(paths(got), []string{"/alice", "/alice/gone"}) {
		t.Fatalf("deliveries: %v", paths(got))
	}
	// The payload is the envelope (§4.7): push_id, unread, and the message
	// without log_id, format, or embeds; the token is the bearer.
	message := map[string]any{"message_id": id, "room_id": "general", "from": map[string]any{"user_id": "guest_1"}, "body": map[string]any{"text": text, "mentions": []any{"alice"}}}
	if want := map[string]any{"push_id": "alice-phone", "unread": float64(1), "message": message}; !reflect.DeepEqual(got[0].payload, want) {
		t.Fatalf("payload: %#v", got[0].payload)
	}
	if got[0].header.Get("Authorization") != "Bearer tok" || got[0].header.Get("Content-Type") != "application/json" || got[0].header.Get("Content-Encoding") != "" {
		t.Fatalf("relay headers: %v", got[0].header)
	}
	if want := map[string]any{"unread": float64(1), "message": message}; !reflect.DeepEqual(got[1].payload, want) || got[1].header.Get("Authorization") != "" {
		t.Fatalf("payload without push_id or token: %#v %v", got[1].payload, got[1].header)
	}
	// An endpoint that answers 410 loses its registration.
	app.mu.RLock()
	_, kept := alice.pushes[relay.URL+"/alice/gone"]
	app.mu.RUnlock()
	if kept {
		t.Fatal("gone registration was kept")
	}
	post("same-mention", map[string]any{"message_id": id, "body": map[string]any{"text": "edited", "mentions": []any{"alice"}}})
	if got := relay.pushes(t, app, a); len(got) != 0 {
		t.Fatalf("an edit re-mentioned: %d", len(got))
	}

	// A connected user is woken only when no connection of theirs is
	// attended (§4.11).
	b.result(t, "push_register", "b", map[string]any{"kind": "relay", "url": relay.URL + "/b"})
	mentionB := map[string]any{"body": map[string]any{"text": "@guest_2 ping", "mentions": []any{"guest_2"}}}
	post("attended", maps.Clone(mentionB))
	if got := relay.pushes(t, app, a); len(got) != 0 {
		t.Fatalf("attended user woken: %d", len(got))
	}
	goIdle(t, b)
	post("idle", maps.Clone(mentionB))
	if got := relay.pushes(t, app, a); !reflect.DeepEqual(paths(got), []string{"/b"}) || got[0].header.Get("Authorization") != "" {
		t.Fatalf("idle user: %#v", got)
	}
	// Neither typing nor a message from the connection ends idle; only
	// idle: false does (§4.11).
	b.write(t, map[string]any{"method": "activity", "params": map[string]any{"room_id": "general", "typing": 3}})
	a.notification(t, "activity")
	own, _ := save(t, b, "own", map[string]any{"body": map[string]any{"text": "mine"}})
	a.notification(t, "message")
	post("still-idle", maps.Clone(mentionB))
	if got := relay.pushes(t, app, a); len(got) != 1 {
		t.Fatalf("typing or a message ended idle: %d", len(got))
	}
	echoed(t, b, b.status(t, map[string]any{"idle": false}), "online")
	post("back", maps.Clone(mentionB))
	if got := relay.pushes(t, app, a); len(got) != 0 {
		t.Fatalf("user back from idle woken: %d", len(got))
	}
	// A reply wakes the author of the message it replies to.
	goIdle(t, b)
	post("reply", map[string]any{"body": map[string]any{"text": "a reply"}, "reply_to": map[string]any{"message_id": own}})
	if got := relay.pushes(t, app, a); len(got) != 1 || got[0].payload["message"].(map[string]any)["reply_to"] == nil {
		t.Fatalf("reply: %#v", got)
	}
	// Mentions and replies wake a user in any room they can see, joined or
	// not. Mentions in commands notify no one.
	ops, _ := saveRoom(t, a, "ops", map[string]any{"title": "Ops"})
	save(t, a, "elsewhere", map[string]any{"room_id": ops, "body": map[string]any{"text": "@guest_2", "mentions": []any{"guest_2"}}})
	if got := relay.pushes(t, app, a); len(got) != 1 || got[0].payload["message"].(map[string]any)["room_id"] != ops {
		t.Fatalf("mention in an unjoined room: %#v", got)
	}
	save(t, a, "reply-elsewhere", map[string]any{"room_id": ops, "body": map[string]any{"text": "a reply"}, "reply_to": map[string]any{"message_id": own}})
	if got := relay.pushes(t, app, a); len(got) != 1 {
		t.Fatalf("reply in an unjoined room woke %d", len(got))
	}
	// A mention in a private room wakes only its members (§4.3.4).
	hidden, _ := saveRoom(t, a, "hidden", map[string]any{"title": "Hidden", "private": true})
	save(t, a, "hidden-mention", map[string]any{"room_id": hidden, "body": map[string]any{"text": "@guest_2", "mentions": []any{"guest_2"}}})
	if got := relay.pushes(t, app, a); len(got) != 0 {
		t.Fatalf("mention in a private room woke a non-member: %#v", got)
	}
	before, _ := a.request(t, "command", "help", map[string]any{"body": map[string]any{"text": "/help", "mentions": []any{"guest_2"}}})
	if len(before) != 1 {
		t.Fatalf("help frames: %#v", before)
	}
	if got := relay.pushes(t, app, a); len(got) != 0 {
		t.Fatalf("command woke %d", len(got))
	}
}

func TestPushRegistration(t *testing.T) {
	app, httpServer := pushTestServer(t)
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	_, keys := testSubscription(t)
	endpoint := "https://push.example.net/s/abc"
	for i, params := range []map[string]any{
		{"kind": "ext:other", "url": endpoint},
		{"kind": "relay"},
		{"kind": "relay", "url": "ftp://relay.example/a"},
		{"kind": "relay", "url": "/relative"},
		{"kind": "relay", "url": "https://user:pass@relay.example/a"},
		{"kind": "relay", "url": "https://relay.example/a b"},
		{"kind": "relay", "url": "https://relay.example/" + strings.Repeat("a", maxPushURLBytes)},
		{"kind": "relay", "url": endpoint, "token": 5},
		{"kind": "relay", "url": endpoint, "push_id": ""},
		{"kind": "relay", "url": endpoint, "push_id": "has space"},
		{"kind": "relay", "url": endpoint, "push_id": "dot.ted"},
		{"kind": "relay", "url": endpoint, "push_id": strings.Repeat("a", 65)},
		{"kind": "relay", "url": endpoint, "push_id": 7},
		{"kind": "relay", "url": endpoint, "keys": "abc"},
		{"kind": "relay", "url": endpoint, "keys": map[string]any{"p256dh": keys["p256dh"]}},
		{"kind": "relay", "url": endpoint, "keys": map[string]any{"p256dh": "AAAA", "auth": keys["auth"]}},
		{"kind": "webpush", "url": endpoint},
		{"kind": "webpush", "url": endpoint, "keys": map[string]any{"p256dh": keys["p256dh"], "auth": "AAAA"}},
		{"kind": "relay", "url": endpoint, "wake": "mentions"},
		{"kind": "relay", "url": endpoint, "wake": []any{"mentions", 5}},
		{"kind": "relay", "url": endpoint, "wake": nil},
		{"kind": "relay", "url": endpoint, "wake": []any{strings.Repeat("x", 65)}},
		{"kind": "relay", "url": endpoint, "wake": make([]any, maxWakeScopes+1)},
	} {
		a.expectError(t, "push_register", "bad-"+formatID(int64(i)), params, codeInvalidParams)
	}
	registration := func(u string) *pushRegistration {
		app.mu.RLock()
		defer app.mu.RUnlock()
		return app.users[u].pushes[endpoint]
	}
	// A registration belongs to its user and url: another user's of the
	// same url is their own, and re-registering replaces only the caller's.
	// Scopes the server does not implement are ignored.
	a.result(t, "push_register", "webpush", map[string]any{"kind": "webpush", "url": endpoint, "keys": keys, "push_id": "t65S5XBst9bSDpjJ", "wake": []any{"mentions", "private", "ext:later", "unknown"}, "token": "ignored"})
	b.result(t, "push_register", "relay", map[string]any{"kind": "relay", "url": endpoint, "token": "tok"})
	first := registration(a.userID)
	if first.kind != "webpush" || first.token != "" || first.pushID != "t65S5XBst9bSDpjJ" || first.wake != wakeMentions|wakePrivate || first.keys == nil {
		t.Fatalf("webpush registration: %#v", first)
	}
	if other := registration(b.userID); other.kind != "relay" || other.token != "tok" || other.wake != defaultWake || other.keys != nil {
		t.Fatalf("relay registration: %#v", other)
	}
	a.result(t, "push_register", "again", map[string]any{"kind": "relay", "url": endpoint, "keys": keys, "wake": []any{}})
	if again := registration(a.userID); again == first || again.kind != "relay" || again.keys == nil || again.pushID != "" || again.wake != 0 || again.renewed.Before(first.renewed) {
		t.Fatalf("re-registration: %#v", again)
	}
	// Another spelling of the same endpoint is the same registration.
	a.result(t, "push_register", "spelling", map[string]any{"kind": "relay", "url": "HTTPS://Push.Example.NET:443/s/abc", "keys": keys, "wake": []any{}})
	app.mu.RLock()
	spellings := len(app.users[a.userID].pushes)
	app.mu.RUnlock()
	if spellings != 1 {
		t.Fatalf("registrations of one endpoint under two spellings: %d", spellings)
	}
	a.expectError(t, "push_register", "long", map[string]any{"kind": "relay", "url": "https://push.example.net/" + strings.Repeat("a", maxPushURLBytes-len("https://push.example.net/")+1)}, codeInvalidParams)
	// Unregistering removes only the caller's; an unknown url succeeds.
	a.result(t, "push_unregister", "unregister", map[string]any{"url": "https://PUSH.example.net/s/abc"})
	a.result(t, "push_unregister", "unknown", map[string]any{"url": "https://push.example.net/never"})
	if registration(a.userID) != nil || registration(b.userID) == nil {
		t.Fatal("unregister removed the wrong registration")
	}
	a.expectError(t, "push_unregister", "no-url", map[string]any{}, codeInvalidParams)

	// A user holds at most ten; another replaces the least recently
	// registered, and registering a url again renews it.
	for i := range maxPushesPerUser {
		a.result(t, "push_register", "many-"+formatID(int64(i)), map[string]any{"kind": "relay", "url": endpoint + formatID(int64(i))})
		time.Sleep(time.Millisecond)
	}
	a.result(t, "push_register", "renew", map[string]any{"kind": "relay", "url": endpoint + "0"})
	a.result(t, "push_register", "eleventh", map[string]any{"kind": "relay", "url": endpoint + "new"})
	app.mu.RLock()
	var urls []string
	for url := range app.users[a.userID].pushes {
		urls = append(urls, strings.TrimPrefix(url, endpoint))
	}
	app.mu.RUnlock()
	if len(urls) != maxPushesPerUser || slices.Contains(urls, "1") || !slices.Contains(urls, "0") || !slices.Contains(urls, "new") {
		t.Fatalf("registrations after the eleventh: %v", urls)
	}
	a.expectQuiet(t)
	b.expectQuiet(t)
}

// Wake scopes (§4.7): mentions, replies, private, joined; [] wakes for
// nothing, and without wake the default is mentions and replies.
func TestPushWakeScopes(t *testing.T) {
	relay := newTestRelay(t)
	app, httpServer := pushTestServer(t)
	a := dialTestClient(t, httpServer)
	carol := addAccount(t, app, "carol", a)
	for name, wake := range map[string]wakeScope{
		"default": defaultWake, "joined": wakeJoined, "private": wakePrivate, "replies": wakeReplies, "mentions": wakeMentions, "nothing": 0,
	} {
		addPush(app, carol, pushRegistration{url: relay.URL + "/" + name, wake: wake})
	}
	post := func(params map[string]any) string {
		t.Helper()
		id, _ := save(t, a, a.nextID("post"), params)
		return id
	}

	// A message in a joined room wakes only joined.
	post(map[string]any{"body": map[string]any{"text": "chatter"}})
	if got := paths(relay.pushes(t, app, a)); !reflect.DeepEqual(got, []string{"/joined"}) {
		t.Fatalf("joined room message woke %v", got)
	}
	// A mention wakes mentions and the default, and joined in a joined room.
	post(map[string]any{"body": map[string]any{"text": "@carol", "mentions": []any{"carol"}}})
	if got := paths(relay.pushes(t, app, a)); !reflect.DeepEqual(got, []string{"/default", "/joined", "/mentions"}) {
		t.Fatalf("mention woke %v", got)
	}
	// A reply to carol's message wakes replies and the default.
	app.mu.Lock()
	logID := app.nextIDLocked()
	own := formatID(logID)
	m := &messageState{id: own, from: carol.from(), owner: carol.id, reactions: make(map[string]reactionSet)}
	app.messages[own] = m
	app.commitSnapshotLocked(m, map[string]any{"message_id": own, "log_id": own, "room_id": "general", "from": carol.from(), "body": map[string]any{"text": "carol's"}}, logID)
	app.unlock()
	a.notification(t, "message")
	post(map[string]any{"body": map[string]any{"text": "re"}, "reply_to": map[string]any{"message_id": own}})
	if got := paths(relay.pushes(t, app, a)); !reflect.DeepEqual(got, []string{"/default", "/joined", "/replies"}) {
		t.Fatalf("reply woke %v", got)
	}
	// A message in a private room carol joined wakes private and joined,
	// and so does one in a thread of it.
	secret, _ := saveRoom(t, a, "secret", map[string]any{"title": "Secret", "private": true})
	a.request(t, "room_join", "add-carol", map[string]any{"room_id": secret, "user_id": "carol"})
	post(map[string]any{"room_id": secret, "body": map[string]any{"text": "psst"}})
	if got := paths(relay.pushes(t, app, a)); !reflect.DeepEqual(got, []string{"/joined", "/private"}) {
		t.Fatalf("private room message woke %v", got)
	}
	thread, _ := saveRoom(t, a, "thread", map[string]any{"parent_room_id": secret, "private": false})
	a.request(t, "room_join", "add-carol-thread", map[string]any{"room_id": thread, "user_id": "carol"})
	post(map[string]any{"room_id": thread, "body": map[string]any{"text": "in the thread"}})
	if got := paths(relay.pushes(t, app, a)); !reflect.DeepEqual(got, []string{"/joined", "/private"}) {
		t.Fatalf("private thread message woke %v", got)
	}
	// A room carol has not joined wakes neither joined nor private.
	open, _ := saveRoom(t, a, "open", map[string]any{"title": "Open"})
	post(map[string]any{"room_id": open, "body": map[string]any{"text": "elsewhere"}})
	if got := relay.pushes(t, app, a); len(got) != 0 {
		t.Fatalf("unjoined room message woke %v", paths(got))
	}
}

// Muted users get no pushes; a muted room wakes only for mentions (§4.7,
// §4.11).
func TestPushRespectsMutes(t *testing.T) {
	relay := newTestRelay(t)
	app, httpServer := pushTestServer(t)
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	b.result(t, "push_register", "all", map[string]any{"kind": "relay", "url": relay.URL + "/b", "wake": []any{"joined", "mentions"}})
	goIdle(t, b)
	post := func(params map[string]any) {
		t.Helper()
		save(t, a, a.nextID("post"), params)
		b.notification(t, "message")
	}
	mention := map[string]any{"body": map[string]any{"text": "@guest_2", "mentions": []any{"guest_2"}}}
	chatter := map[string]any{"body": map[string]any{"text": "chatter"}}

	// A muted room wakes only for mentions.
	b.write(t, map[string]any{"method": "status", "params": map[string]any{"room_id": "general", "mute": true}})
	if record := roomUpdated(t, b, "updated"); record["room_id"] != "general" || record["mute"] != true {
		t.Fatalf("room mute echo: %#v", record)
	}
	post(maps.Clone(chatter))
	if got := relay.pushes(t, app, a); len(got) != 0 {
		t.Fatalf("muted room woke %d", len(got))
	}
	post(maps.Clone(mention))
	if got := relay.pushes(t, app, a); len(got) != 1 {
		t.Fatalf("mention in a muted room woke %d", len(got))
	}
	b.write(t, map[string]any{"method": "status", "params": map[string]any{"room_id": "general", "mute": 0}})
	if record := roomUpdated(t, b, "updated"); record["mute"] != float64(0) {
		t.Fatalf("room unmute echo: %#v", record)
	}
	post(maps.Clone(chatter))
	if got := relay.pushes(t, app, a); len(got) != 1 {
		t.Fatalf("unmuted room woke %d", len(got))
	}
	// A muted user gets no pushes at all, mentions included.
	b.write(t, map[string]any{"method": "status", "params": map[string]any{"mute": 3600}})
	if you := b.notification(t, "user")["you"].(map[string]any); you["mute"] != float64(3600) || you["status"] != "dnd" {
		t.Fatalf("mute echo: %#v", you)
	}
	post(maps.Clone(mention))
	if got := relay.pushes(t, app, a); len(got) != 0 {
		t.Fatalf("muted user woken %d", len(got))
	}
	b.write(t, map[string]any{"method": "status", "params": map[string]any{"mute": 0}})
	if you := b.notification(t, "user")["you"].(map[string]any); you["mute"] != float64(0) || you["status"] != "idle" {
		t.Fatalf("unmute echo: %#v", you)
	}
	post(maps.Clone(mention))
	if got := relay.pushes(t, app, a); len(got) != 1 {
		t.Fatalf("unmuted user woken %d", len(got))
	}

	// A room's mute covers its threads, and applies to a room the user has
	// not joined, whose mentions still wake. (The user has used up the
	// changes to mute allowed at once: start again.)
	app.mu.Lock()
	app.users[b.userID].statusChanges = nil
	app.unlock()
	thread, _ := saveRoom(t, a, "thread", map[string]any{"parent_room_id": "general", "title": "Thread"})
	b.notification(t, "room_update")
	joinRoom(t, b, thread)
	expectMembership(t, a, thread, b.userID, true)
	b.status(t, map[string]any{"room_id": "general", "mute": true})
	save(t, a, "in-thread", map[string]any{"room_id": thread, "body": map[string]any{"text": "chatter"}})
	b.notification(t, "message")
	if got := relay.pushes(t, app, a); len(got) != 0 {
		t.Fatalf("thread of a muted room woke %d", len(got))
	}
	b.status(t, map[string]any{"room_id": "general", "mute": 0})
	ops, _ := saveRoom(t, a, "ops", map[string]any{"title": "Ops"})
	if frames := b.status(t, map[string]any{"room_id": ops, "mute": true}); len(frames) != 0 {
		t.Fatalf("echo of an unjoined room's mute: %#v", frames)
	}
	theirs := formatID(serverMessage(t, app, b.userID))
	a.notification(t, "message")
	b.notification(t, "message")
	save(t, a, "ops-reply", map[string]any{"room_id": ops, "body": map[string]any{"text": "re"}, "reply_to": map[string]any{"message_id": theirs}})
	if got := relay.pushes(t, app, a); len(got) != 0 {
		t.Fatalf("reply in a muted unjoined room woke %d", len(got))
	}
	save(t, a, "ops-mention", map[string]any{"room_id": ops, "body": map[string]any{"text": "@guest_2", "mentions": []any{"guest_2"}}})
	if got := relay.pushes(t, app, a); len(got) != 1 || got[0].header.Get("Urgency") != "high" {
		t.Fatalf("mention in a muted unjoined room: %#v", got)
	}
}

// serverMessage is the message_id of a message by userID, made in general.
func serverMessage(t *testing.T, app *Server, userID string) int64 {
	t.Helper()
	app.mu.Lock()
	defer app.unlock()
	u := app.users[userID]
	logID := app.nextIDLocked()
	id := formatID(logID)
	m := &messageState{id: id, from: u.from(), owner: u.id, reactions: make(map[string]reactionSet)}
	app.messages[id] = m
	app.commitSnapshotLocked(m, map[string]any{"message_id": id, "log_id": id, "room_id": "general", "from": u.from(), "body": map[string]any{"text": "theirs"}}, logID)
	return logID
}

// Badge pushes wait badgeDelay, so one push carries the count after a burst
// of changes; a guest's retirement sends none.
func TestBadgePushesCoalesce(t *testing.T) {
	relay := newTestRelay(t)
	app, httpServer := pushTestServer(t)
	app.badgeDelay = 300 * time.Millisecond
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	b.result(t, "push_register", "phone", map[string]any{"kind": "relay", "url": relay.URL + "/phone", "wake": []any{"badge"}})
	var ids []string
	for i := range 3 {
		id, _ := save(t, a, a.nextID("post"), map[string]any{"body": map[string]any{"text": formatID(int64(i))}})
		b.notification(t, "message")
		ids = append(ids, id)
	}
	for _, id := range ids[:2] {
		b.write(t, map[string]any{"method": "activity", "params": map[string]any{"room_id": "general", "read_message_id": id}})
		a.notification(t, "activity")
		b.notification(t, "activity")
	}
	got := relay.pushes(t, app, a)
	if len(got) != 1 || got[0].payload["unread"] != float64(1) {
		t.Fatalf("coalesced badge pushes: %#v", got)
	}
	// A guest that leaves takes its registrations with it: its leaves send
	// nothing.
	save(t, a, "more", map[string]any{"body": map[string]any{"text": "more"}})
	b.notification(t, "message")
	_ = b.ws.Close(websocket.StatusNormalClosure, "bye")
	expectMembership(t, a, "general", b.userID, false)
	if got := relay.pushes(t, app, a); len(got) != 0 {
		t.Fatalf("pushes after a guest left: %#v", got)
	}
}

// webpush deliveries are RFC 8291 ciphertext signed with VAPID (RFC 8292),
// and so are relay deliveries with keys, with the relay's token.
func TestWebPushDelivery(t *testing.T) {
	relay := newTestRelay(t)
	config := DefaultConfig()
	config.AllowInsecurePush = true
	config.PublicURL = "https://chat.example"
	app, httpServer := newTestServer(t, config)
	c, frame := dialRaw(t, httpServer)
	guestAuth(t, c)
	key := frame["params"].(map[string]any)["push"].(map[string]any)["webpush"].(map[string]any)["key"].(string)
	browser, keys := testSubscription(t)
	auth := mustDecode(t, keys["auth"].(string))
	c.result(t, "push_register", "web", map[string]any{"kind": "webpush", "url": relay.URL + "/web", "keys": keys, "push_id": "web-1"})
	c.result(t, "push_register", "relay", map[string]any{"kind": "relay", "url": relay.URL + "/relay", "keys": keys, "token": "tok", "push_id": "relay-1"})
	goIdle(t, c)
	poster := dialTestClient(t, httpServer)
	expectMembership(t, c, "general", poster.userID, true)
	// c sent status, so it learns that the new member is online (§4.11).
	if joined := c.notification(t, "user"); !reflect.DeepEqual(joined, map[string]any{"new": map[string]any{"user_id": poster.userID, "status": "online"}}) {
		t.Fatalf("status of a new member: %#v", joined)
	}
	save(t, poster, "mention", map[string]any{"body": map[string]any{"text": "hi @guest_1", "format": "markdown", "mentions": []any{"guest_1"}}})
	c.notification(t, "message")
	got := relay.pushes(t, app, poster)
	if !reflect.DeepEqual(paths(got), []string{"/relay", "/web"}) {
		t.Fatalf("deliveries: %v", paths(got))
	}
	for _, request := range got {
		header := request.header
		if header.Get("Content-Encoding") != "aes128gcm" || header.Get("Content-Type") != "application/octet-stream" {
			t.Fatalf("%s headers: %v", request.path, header)
		}
		var payload map[string]any
		if err := json.Unmarshal(decryptPush(t, request.body, browser, auth), &payload); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"push_id": strings.TrimPrefix(request.path, "/") + "-1", "unread": float64(1), "message": map[string]any{
			"message_id": payload["message"].(map[string]any)["message_id"], "room_id": "general", "from": map[string]any{"user_id": "guest_2"},
			"body": map[string]any{"text": "hi @guest_1", "mentions": []any{"guest_1"}},
		}}
		if !reflect.DeepEqual(payload, want) {
			t.Fatalf("%s payload: %#v", request.path, payload)
		}
	}
	// Relays get TTL and Urgency too (§4.7).
	if relayHeader := got[0].header; relayHeader.Get("Authorization") != "Bearer tok" || relayHeader.Get("TTL") != "86400" || relayHeader.Get("Urgency") != "high" {
		t.Fatalf("relay headers: %v", relayHeader)
	}
	header := got[1].header
	if header.Get("TTL") != "86400" || header.Get("Urgency") != "high" {
		t.Fatalf("webpush headers: %v", header)
	}
	// The VAPID token is for the endpoint's origin, signed with the key the
	// server frame advertises, with the public URL as its contact.
	token, k, ok := strings.Cut(strings.TrimPrefix(header.Get("Authorization"), "vapid t="), ", k=")
	if !ok || k != key {
		t.Fatalf("Authorization: %q", header.Get("Authorization"))
	}
	parts := strings.Split(token, ".")
	var claims map[string]any
	if err := json.Unmarshal(mustDecode(t, parts[1]), &claims); err != nil {
		t.Fatal(err)
	}
	if claims["aud"] != relay.URL || claims["sub"] != "https://chat.example" {
		t.Fatalf("claims: %#v", claims)
	}
	public, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), mustDecode(t, key))
	if err != nil {
		t.Fatal(err)
	}
	signature := mustDecode(t, parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(public, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
		t.Fatal("VAPID signature does not verify with server.push.webpush.key")
	}
	// A subscription that answers 404 is removed, for any kind.
	c.result(t, "push_register", "missing", map[string]any{"kind": "webpush", "url": relay.URL + "/web/missing", "keys": keys})
	save(t, poster, "again", map[string]any{"body": map[string]any{"text": "again", "mentions": []any{"guest_1"}}})
	c.notification(t, "message")
	if got := paths(relay.pushes(t, app, poster)); !reflect.DeepEqual(got, []string{"/relay", "/web", "/web/missing"}) {
		t.Fatalf("deliveries: %v", got)
	}
	app.mu.RLock()
	_, kept := app.users[c.userID].pushes[relay.URL+"/web/missing"]
	app.mu.RUnlock()
	if kept {
		t.Fatal("a webpush subscription answering 404 was kept")
	}
}

func TestPushPayloadFitsInLimit(t *testing.T) {
	snapshot := map[string]any{
		"message_id": "1724803200042", "log_id": "1724803200043", "room_id": "general",
		"from":     map[string]any{"user_id": "alice", "name": "Alice"},
		"reply_to": map[string]any{"message_id": "1724803200001"},
		"body":     map[string]any{"text": "short", "format": "markdown", "embeds": []any{map[string]any{"kind": "upload"}}},
		"ext":      map[string]any{"x": 1},
	}
	decode := func(payload []byte) map[string]any {
		t.Helper()
		if len(payload) > maxPushPayloadBytes {
			t.Fatalf("payload of %d bytes", len(payload))
		}
		var value map[string]any
		if err := json.Unmarshal(payload, &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	got := decode(pushPayload("p", 3, snapshot))
	want := map[string]any{"push_id": "p", "unread": float64(3), "message": map[string]any{
		"message_id": "1724803200042", "room_id": "general", "from": map[string]any{"user_id": "alice", "name": "Alice"},
		"reply_to": map[string]any{"message_id": "1724803200001"}, "body": map[string]any{"text": "short"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("payload: %#v", got)
	}
	// Text is truncated to maxPushTextRunes.
	snapshot["body"] = map[string]any{"text": strings.Repeat("e", maxPushTextRunes+500)}
	text := decode(pushPayload("", 0, snapshot))["message"].(map[string]any)["body"].(map[string]any)["text"].(string)
	if runes := []rune(text); len(runes) != maxPushTextRunes || runes[len(runes)-1] != '…' {
		t.Fatalf("truncated text: %d runes", len(runes))
	}
	// Mentions go first.
	many := make([]any, 200)
	for i := range many {
		many[i] = "a_rather_long_user_id_" + formatID(int64(i))
	}
	snapshot["body"] = map[string]any{"text": "hi", "mentions": many}
	if body := decode(pushPayload("", 0, snapshot))["message"].(map[string]any)["body"]; !reflect.DeepEqual(body, map[string]any{"text": "hi"}) {
		t.Fatalf("body without mentions: %#v", body)
	}
	// Then text is shortened to fit.
	snapshot["body"] = map[string]any{"text": strings.Repeat(" ", maxPushTextRunes)}
	payload := pushPayload("", 0, snapshot)
	text = decode(payload)["message"].(map[string]any)["body"].(map[string]any)["text"].(string)
	if !strings.HasSuffix(text, "…") || len(payload) < maxPushPayloadBytes-16 {
		t.Fatalf("shortened text: %d bytes of payload", len(payload))
	}
	// Then body goes, from keeps only user_id, and reply_to goes.
	snapshot["body"] = map[string]any{"text": "hi"}
	snapshot["from"] = map[string]any{"user_id": "alice", "name": strings.Repeat("\\", 3000)}
	message := decode(pushPayload("", 0, snapshot))["message"].(map[string]any)
	if _, has := message["body"]; has || !reflect.DeepEqual(message["from"], map[string]any{"user_id": "alice"}) || message["reply_to"] == nil {
		t.Fatalf("message without body and name: %#v", message)
	}
}

func TestPushRegistrationsExpire(t *testing.T) {
	relay := newTestRelay(t)
	app, httpServer := pushTestServer(t)
	a := dialTestClient(t, httpServer)
	dora := addAccount(t, app, "dora", a)
	stale := addPush(app, dora, pushRegistration{url: relay.URL + "/stale", wake: defaultWake, renewed: time.Now().Add(-pushExpiry - time.Minute)})
	addPush(app, dora, pushRegistration{url: relay.URL + "/fresh", wake: defaultWake, renewed: time.Now().Add(-pushExpiry + time.Hour)})
	save(t, a, "mention", map[string]any{"body": map[string]any{"text": "@dora", "mentions": []any{"dora"}}})
	if got := paths(relay.pushes(t, app, a)); !reflect.DeepEqual(got, []string{"/fresh"}) {
		t.Fatalf("deliveries: %v", got)
	}
	app.mu.RLock()
	_, kept := dora.pushes[stale.url]
	_, indexed := app.pushes[pushKey("dora", stale.url)]
	app.mu.RUnlock()
	if kept || indexed {
		t.Fatal("an expired registration was kept")
	}
	// A user whose only registration expired is offline, not idle.
	app.mu.Lock()
	idle := dora.statusAt(time.Now())
	for _, p := range dora.pushes {
		p.renewed = time.Now().Add(-pushExpiry)
	}
	expired := dora.statusAt(time.Now())
	app.mu.Unlock()
	if idle != statusIdle || expired != statusOffline {
		t.Fatalf("status with a live registration %s, with expired ones %s", idle, expired)
	}
}

// unread is the user's one count of messages after their read positions,
// the same in every registration's pushes, and badge pushes, without a
// message and with Urgency low, send each change of it to the relay
// registrations with scope badge (§4.7).
func TestPushUnreadAndBadge(t *testing.T) {
	relay := newTestRelay(t)
	app, httpServer := pushTestServer(t)
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	b.result(t, "push_register", "phone", map[string]any{"kind": "relay", "url": relay.URL + "/phone", "push_id": "phone", "wake": []any{"joined", "badge"}})
	b.result(t, "push_register", "web", map[string]any{"kind": "relay", "url": relay.URL + "/mentions", "wake": []any{"mentions", "badge"}})
	_, keys := testSubscription(t)
	b.result(t, "push_register", "browser", map[string]any{"kind": "webpush", "url": relay.URL + "/browser", "keys": keys, "wake": []any{"joined", "badge"}})
	goIdle(t, b)
	// expect checks each delivery: its path, unread, Urgency, and whether
	// it carries a message.
	type delivery struct {
		path    string
		unread  any
		urgency string
		message bool
	}
	expect := func(want ...delivery) {
		t.Helper()
		got := relay.pushes(t, app, a)
		var deliveries []delivery
		for _, request := range got {
			d := delivery{path: request.path, urgency: request.header.Get("Urgency")}
			if request.payload != nil {
				d.unread = request.payload["unread"]
				_, d.message = request.payload["message"]
			} else {
				d.unread, d.message = "encrypted", true
			}
			deliveries = append(deliveries, d)
		}
		if !reflect.DeepEqual(deliveries, want) {
			t.Fatalf("deliveries %+v, want %+v", deliveries, want)
		}
	}
	browser := delivery{"/browser", "encrypted", "normal", true}
	post := func(params map[string]any) string {
		t.Helper()
		id, _ := save(t, a, a.nextID("post"), params)
		b.notification(t, "message")
		return id
	}
	read := func(roomID, messageID string, others ...*testClient) {
		t.Helper()
		b.write(t, map[string]any{"method": "activity", "params": map[string]any{"room_id": roomID, "read_message_id": messageID}})
		for _, c := range append(others, b) {
			c.notification(t, "activity")
		}
	}
	// A message no scope of /mentions selects still changes the count it
	// shows: it gets a badge push.
	first := post(map[string]any{"body": map[string]any{"text": "one"}})
	expect(browser, delivery{"/mentions", float64(1), "low", false}, delivery{"/phone", float64(1), "normal", true})
	second := post(map[string]any{"body": map[string]any{"text": "@guest_2", "mentions": []any{"guest_2"}}})
	expect(delivery{"/browser", "encrypted", "normal", true}, delivery{"/mentions", float64(2), "high", true}, delivery{"/phone", float64(2), "normal", true})
	// Reading on another connection sends badge pushes to relay
	// registrations whose count changed; webpush ignores badge.
	read("general", first, a)
	expect(delivery{"/mentions", float64(1), "low", false}, delivery{"/phone", float64(1), "low", false})
	read("general", second, a)
	expect(delivery{"/mentions", float64(0), "low", false}, delivery{"/phone", float64(0), "low", false})
	// Deleting a counted message lowers the count.
	third := post(map[string]any{"body": map[string]any{"text": "three"}})
	expect(browser, delivery{"/mentions", float64(1), "low", false}, delivery{"/phone", float64(1), "normal", true})
	a.request(t, "message", "delete", map[string]any{"room_id": "general", "message_id": third, "deleted": true})
	b.notification(t, "message")
	expect(delivery{"/mentions", float64(0), "low", false}, delivery{"/phone", float64(0), "low", false})
	// The user's own message moves their read position.
	post(map[string]any{"body": map[string]any{"text": "four"}})
	expect(browser, delivery{"/mentions", float64(1), "low", false}, delivery{"/phone", float64(1), "normal", true})
	save(t, b, "own", map[string]any{"body": map[string]any{"text": "mine"}})
	a.notification(t, "message")
	expect(delivery{"/mentions", float64(0), "low", false}, delivery{"/phone", float64(0), "low", false})
	// A mention in a room the user has not joined counts until read there.
	ops, _ := saveRoom(t, a, "ops", map[string]any{"title": "Ops"})
	elsewhere, _ := save(t, a, "elsewhere", map[string]any{"room_id": ops, "body": map[string]any{"text": "@guest_2", "mentions": []any{"guest_2"}}})
	expect(delivery{"/mentions", float64(1), "high", true}, delivery{"/phone", float64(1), "low", false})
	read(ops, elsewhere)
	expect(delivery{"/mentions", float64(0), "low", false}, delivery{"/phone", float64(0), "low", false})
	// A message moved into a joined room counts there from its move.
	moved, _ := save(t, a, "to-move", map[string]any{"room_id": ops, "body": map[string]any{"text": "moving"}})
	expect()
	before, _ := a.request(t, "message", "move", map[string]any{"message_id": moved, "room_id": "general", "body": map[string]any{"text": "moving"}})
	if len(before) != 1 {
		t.Fatalf("frames before the move result: %#v", before)
	}
	b.notification(t, "message")
	expect(delivery{"/mentions", float64(1), "low", false}, delivery{"/phone", float64(1), "low", false})
	read("general", moved, a)
	expect(delivery{"/mentions", float64(0), "low", false}, delivery{"/phone", float64(0), "low", false})
	// What a mute silences reaches only registrations with scope badge,
	// without a message.
	b.status(t, map[string]any{"mute": true})
	post(map[string]any{"body": map[string]any{"text": "@guest_2", "mentions": []any{"guest_2"}}})
	expect(delivery{"/mentions", float64(1), "low", false}, delivery{"/phone", float64(1), "low", false})
}

// Registrations, with their keys, push_id, and scopes, survive a restart,
// and so do the VAPID key and the user's status. A registration stored by
// url alone, before registrations belonged to their user, is kept.
func TestPushAndStatusSurviveRestart(t *testing.T) {
	memory := store.NewMemory()
	_, keys := testSubscription(t)
	if err := memory.Apply([]store.Entry{
		{Kind: entryUser, ID: "erin", Value: encodeJSON(storedUser{Name: "Erin", Passkey: &storedPasskey{Handle: []byte("h")}})},
		{Kind: entryPush, ID: "https://relay.example/legacy", Value: []byte(`{"user":"erin","kind":"relay","token":"tok"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	app, httpServer, stop := startWithStore(t, memory, t.TempDir())
	_, frame := dialRaw(t, httpServer)
	key := frame["params"].(map[string]any)["push"].(map[string]any)["webpush"].(map[string]any)["key"]
	app.mu.Lock()
	erin := app.users["erin"]
	migrated := erin.pushes["https://relay.example/legacy"]
	if migrated == nil || migrated.token != "tok" || migrated.wake != defaultWake || !migrated.live(time.Now()) {
		app.mu.Unlock()
		t.Fatalf("legacy registration: %#v", migrated)
	}
	subscription, _ := parsePushKeys(keys["p256dh"].(string), keys["auth"].(string))
	app.addPushLocked(erin, &pushRegistration{
		userID: "erin", kind: "webpush", url: "https://push.example/s", pushID: "p1", keys: &subscription,
		wake: wakeJoined | wakeBadge, renewed: time.Now(), lastUnread: -1,
	})
	erin.invisible = true
	erin.mute = muteState{until: time.Now().Add(time.Hour)}
	erin.roomMutes["general"] = muteState{forever: true}
	app.touchUser("erin")
	app.unlock()
	stop()

	app, httpServer, _ = startWithStore(t, memory, t.TempDir())
	_, frame = dialRaw(t, httpServer)
	if again := frame["params"].(map[string]any)["push"].(map[string]any)["webpush"].(map[string]any)["key"]; again != key {
		t.Fatalf("VAPID key changed across a restart: %v then %v", key, again)
	}
	app.mu.RLock()
	erin = app.users["erin"]
	web := erin.pushes["https://push.example/s"]
	if web == nil || web.kind != "webpush" || web.pushID != "p1" || web.wake != wakeJoined|wakeBadge || web.keys == nil || encodeBase64URL(web.keys.auth) != keys["auth"] {
		t.Fatalf("webpush registration after restart: %#v", web)
	}
	if erin.pushes["https://relay.example/legacy"] == nil || len(app.pushes) != 2 {
		t.Fatalf("registrations after restart: %v", app.pushes)
	}
	if !erin.invisible || !erin.mute.active(time.Now()) || !erin.roomMutes["general"].forever {
		t.Fatalf("status after restart: %v %v %v", erin.invisible, erin.mute, erin.roomMutes)
	}
	app.mu.RUnlock()
	var stored []string
	_ = memory.Load(func(e store.Entry) error {
		if e.Kind == entryPush {
			stored = append(stored, e.ID)
		}
		return nil
	})
	slices.Sort(stored)
	if !reflect.DeepEqual(stored, []string{"erin https://push.example/s", "erin https://relay.example/legacy"}) {
		t.Fatalf("stored registrations: %v", stored)
	}
	// A configured VAPID key replaces the stored one, and a malformed one
	// stops the server from starting.
	configured, _ := newVAPIDKey()
	config := DefaultConfig()
	config.VAPIDPrivateKey = configured.encoded()
	if app, _ := newTestServer(t, config); app.vapid.public != configured.public {
		t.Fatal("the configured VAPID key is not used")
	}
	config.VAPIDPrivateKey = "nope"
	if _, err := Open(config); err == nil {
		t.Fatal("a malformed VAPID key was accepted")
	}
}

// A user gets at most maxPushesPerUserDay pushes a day, counting only
// those their endpoints accepted.
func TestPushesPerUserDay(t *testing.T) {
	relay := newTestRelay(t)
	app, httpServer := pushTestServer(t)
	a := dialTestClient(t, httpServer)
	dora := addAccount(t, app, "dora", a)
	addPush(app, dora, pushRegistration{url: relay.URL + "/broken/missing", wake: defaultWake})
	mention := func() int {
		t.Helper()
		save(t, a, a.nextID("mention"), map[string]any{"body": map[string]any{"text": "@dora", "mentions": []any{"dora"}}})
		return len(relay.pushes(t, app, a))
	}
	// Refused deliveries count nothing (and 404 forgets the registration).
	if got := mention(); got != 1 {
		t.Fatalf("deliveries: %d", got)
	}
	app.mu.Lock()
	count := dora.pushesTodayLocked(time.Now())
	app.unlock()
	if count != 0 {
		t.Fatalf("a refused push counted: %d", count)
	}
	addPush(app, dora, pushRegistration{url: relay.URL + "/dora", wake: defaultWake})
	if got := mention(); got != 1 {
		t.Fatalf("deliveries: %d", got)
	}
	app.mu.Lock()
	count = dora.pushesTodayLocked(time.Now())
	dora.pushesToday = maxPushesPerUserDay
	app.unlock()
	if count != 1 {
		t.Fatalf("an accepted push counted %d", count)
	}
	if got := mention(); got != 0 {
		t.Fatalf("deliveries past the daily cap: %d", got)
	}
	// A new day starts over.
	app.mu.Lock()
	dora.pushDay = "2000-01-01"
	app.unlock()
	if got := mention(); got != 1 {
		t.Fatalf("deliveries on a new day: %d", got)
	}
}

func TestPushRefusesInternalAddresses(t *testing.T) {
	var hits atomic.Int32
	relay := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer relay.Close()
	deliverer := newPushDeliverer(false)
	called := false
	deliverer.deliver(pushRegistration{url: relay.URL}, []byte("{}"), "normal", "", func(int) { called = true })
	deliverer.wait()
	if hits.Load() != 0 || called {
		t.Fatalf("delivered to a loopback address: hits=%d called=%v", hits.Load(), called)
	}
	app, _ := newTestServer(t, DefaultConfig())
	if _, problem := app.checkPushURL("http://relay.example/p"); problem == "" {
		t.Fatal("http push URL accepted without AllowInsecurePush")
	}
	// Internal address literals, localhost, and hosts ending in a dot are
	// refused at registration.
	for _, endpoint := range []string{"https://10.0.0.1/p", "https://[::1]/p", "https://[fd00::1]:8443/p", "https://169.254.169.254/p", "https://localhost/p", "https://push.localhost/p", "https://192.0.2.1/p", "https://push.example.net./p", "mailto:a@b.example"} {
		if _, problem := app.checkPushURL(endpoint); problem == "" {
			t.Errorf("%s accepted", endpoint)
		}
	}
	// Endpoints are kept in one form: scheme and host lowercased, no
	// default port.
	for endpoint, want := range map[string]string{
		"https://93.184.215.14/p":                   "https://93.184.215.14/p",
		"https://fcm.googleapis.com/fcm/send/x":     "https://fcm.googleapis.com/fcm/send/x",
		"HTTPS://FCM.GoogleAPIs.com:443/fcm/send/x": "https://fcm.googleapis.com/fcm/send/x",
		"https://push.example.net:8443/A?b=C":       "https://push.example.net:8443/A?b=C",
		"https://[2606:4700::1111]:443/p":           "https://[2606:4700::1111]/p",
	} {
		if got, problem := app.checkPushURL(endpoint); got != want {
			t.Errorf("%s normalized to %q (%s), want %q", endpoint, got, problem, want)
		}
	}
}

// Badge deliveries for one registration that have not started give way to
// the latest; lanes are per host, ignoring case.
func TestPushDeliveriesCoalesceBadges(t *testing.T) {
	release := make(chan struct{})
	var bodies []string
	var mu sync.Mutex
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		<-release
	}))
	defer relay.Close()
	deliverer := newPushDeliverer(true)
	// Fill the host's lane so later deliveries wait.
	local := strings.Replace(relay.URL, "127.0.0.1", "localhost", 1)
	for range maxPushPerHost {
		deliverer.deliver(pushRegistration{userID: "filler", url: local + "/busy"}, []byte("{}"), "normal", "", func(int) {})
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		mu.Lock()
		busy := len(bodies) == maxPushPerHost
		mu.Unlock()
		if busy {
			break
		}
	}
	upper := strings.Replace(relay.URL, "127.0.0.1", "LocalHost", 1)
	for i := range 3 {
		deliverer.deliver(pushRegistration{userID: "u", url: upper + "/badge"}, []byte(formatID(int64(i))), "low", "u "+upper+"/badge", func(int) {})
	}
	deliverer.mu.Lock()
	lanes, queued := len(deliverer.hosts), deliverer.users["u"]
	deliverer.mu.Unlock()
	close(release)
	deliverer.wait()
	if lanes != 1 || queued != 1 {
		t.Fatalf("lanes %d, queued badge deliveries %d", lanes, queued)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(bodies, "2") || slices.Contains(bodies, "0") || slices.Contains(bodies, "1") {
		t.Fatalf("delivered bodies: %v", bodies)
	}
}

func TestPushLanesIsolateStalledRelays(t *testing.T) {
	release := make(chan struct{})
	var stalled atomic.Int32
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stalled.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	delivered := make(chan struct{}, 1)
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delivered <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer fast.Close()
	deliverer := newPushDeliverer(true)
	defer func() {
		close(release)
		deliverer.wait()
	}()

	// One user queues at most maxPushQueuedPerUser deliveries.
	for range maxPushQueuedPerUser + 5 {
		deliverer.deliver(pushRegistration{userID: "spammer", url: slow.URL + "/slow"}, []byte("{}"), "normal", "", func(int) {})
	}
	deliverer.mu.Lock()
	queued := deliverer.users["spammer"]
	deliverer.mu.Unlock()
	if queued != maxPushQueuedPerUser {
		t.Fatalf("queued deliveries for one user: %d", queued)
	}
	for i := range maxConcurrentPushPOST {
		deliverer.deliver(pushRegistration{userID: "user" + formatID(int64(i)), url: slow.URL + "/slow"}, []byte("{}"), "normal", "", func(int) {})
	}
	deadline := time.Now().Add(time.Second)
	for stalled.Load() < maxPushPerHost && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := stalled.Load(); got != maxPushPerHost {
		t.Fatalf("concurrent deliveries to one host: %d", got)
	}
	// Another push host has its own lane.
	deliverer.deliver(pushRegistration{userID: "victim", url: fast.URL + "/fast"}, []byte("{}"), "normal", "", func(int) {})
	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("a stalled push host blocked delivery to another host")
	}
}

func TestPushRefusesNonPublicRanges(t *testing.T) {
	for host, want := range map[string]bool{
		"93.184.215.14":     true,
		"2606:4700::1111":   true,
		"127.0.0.1":         false,
		"10.1.2.3":          false,
		"169.254.169.254":   false,
		"100.64.0.1":        false,
		"100.100.100.200":   false,
		"198.18.0.1":        false,
		"192.0.0.1":         false,
		"240.0.0.1":         false,
		"::ffff:100.64.0.1": false,
		"64:ff9b::a00:1":    false,
		"2002:a00:1::1":     false,
		"fd00::1":           false,
		"192.0.2.1":         false,
		"198.51.100.1":      false,
		"203.0.113.1":       false,
		"::a00:1":           false,
		"100::1":            false,
		"2001:db8::1":       false,
		"fec0::1":           false,
		"3fff::1":           false,
		"::1":               false,
	} {
		if got := publicAddress(host); got != want {
			t.Errorf("publicAddress(%s) = %v, want %v", host, got, want)
		}
	}
}
