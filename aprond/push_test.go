package aprond

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/time/rate"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/aprond/internal/push"
	"github.com/apron-chat/apron-server-go/aprond/internal/roomlog"
	"github.com/apron-chat/apron-server-go/aprond/internal/users"
	"github.com/apron-chat/apron-server-go/aprond/internal/webpush"
	"github.com/apron-chat/apron-server-go/store"
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
	// failing makes paths ending in /flaky answer 500 while set.
	failing atomic.Bool
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
		case strings.HasSuffix(r.URL.Path, "/flaky") && relay.failing.Load():
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusCreated)
		}
	}))
	t.Cleanup(relay.Close)
	return relay
}

// pushes returns the deliveries made so far, sorted by path, after the
// requests c sent before, and any request whose notification the test has
// read, have finished waking users and their badge pushes have gone out.
func (relay *testRelay) pushes(t *testing.T, app *Server, c *testClient) []relayRequest {
	t.Helper()
	// A handler sends its notifications holding app.mu, and goes on before
	// it unlocks, as activity relays a read to the room and schedules its
	// badge after it sends the reader theirs: a notification the test has
	// read can come before the others, and before the push it goes with is
	// scheduled. Taking app.mu waits for such a handler to finish, and the
	// drain, which a Server answers after c's requests, takes what it sent
	// c.
	app.mu.Lock()
	app.mu.Unlock()
	c.drain(t)
	app.pushes.Wait()
	app.push.Wait()
	// A later Add to either WaitGroup must happen after these Waits, and a
	// server timer, such as a mute running out, can add one at any time. The
	// server adds only holding app.mu, so taking it here orders those Adds
	// after the Waits.
	app.mu.Lock()
	app.mu.Unlock()
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
	app.pushes.SetBadgeDelay(0)
	return app, httpServer
}

// addAccount adds an account that has joined general and has no connection,
// like a passkey user who is away. others are told of the join.
func addAccount(t *testing.T, app *Server, id string, others ...*testClient) *userState {
	t.Helper()
	app.mu.Lock()
	u := newUserState(id, strings.ToUpper(id[:1])+id[1:])
	u.Identities = map[string]string{"webauthn": id}
	app.users.Add(u)
	app.addMemberLocked(u, app.rooms.Get(defaultRoomID))
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
	p.User = u.id
	if p.Kind == "" {
		p.Kind = "relay"
	}
	if p.Renewed.IsZero() {
		p.Renewed = time.Now()
	}
	registration := push.New(p)
	app.pushes.Add(registration)
	return registration
}

// pushEntries are the store entries of registrations.
func pushEntries(registrations ...*pushRegistration) []store.Entry {
	table := push.NewTable(nil)
	for _, p := range registrations {
		table.Add(p)
	}
	return table.Entries()
}

// goIdle tells the server nobody attends c, which tells c nothing.
func goIdle(t *testing.T, c *testClient) {
	t.Helper()
	if frames := c.status(t, map[string]any{"idle": 300}); len(frames) != 0 {
		t.Fatalf("frames after idle: %#v", frames)
	}
}

func TestPushWakesMentionsAndRepliesOfIdleUsers(t *testing.T) {
	relay := newTestRelay(t)
	app, httpServer := pushTestServer(t)
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	alice := addAccount(t, app, "alice", a, b)
	addPush(app, alice, pushRegistration{URL: relay.URL + "/alice", Token: "tok", PushID: "alice-phone", Wake: defaultWake})
	addPush(app, alice, pushRegistration{URL: relay.URL + "/alice/gone", Wake: defaultWake})
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
	// The payload is the envelope (§4.9): push_id, unread, and the message
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
	kept := app.pushes.Get(alice.id, relay.URL+"/alice/gone") != nil
	app.mu.RUnlock()
	if kept {
		t.Fatal("gone registration was kept")
	}
	post("same-mention", map[string]any{"message_id": id, "body": map[string]any{"text": "edited", "mentions": []any{"alice"}}})
	if got := relay.pushes(t, app, a); len(got) != 0 {
		t.Fatalf("an edit re-mentioned: %d", len(got))
	}

	// A connected user is woken only when no connection of theirs is
	// attended (§4.5).
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
	// idle: false does (§4.5).
	b.write(t, map[string]any{"method": "activity", "params": map[string]any{"room_id": "general", "typing": 3}})
	a.notification(t, "activity")
	b.notification(t, "activity")
	own, _ := save(t, b, "own", map[string]any{"body": map[string]any{"text": "mine"}})
	a.notification(t, "message")
	post("still-idle", maps.Clone(mentionB))
	if got := relay.pushes(t, app, a); len(got) != 1 {
		t.Fatalf("typing or a message ended idle: %d", len(got))
	}
	if frames := b.status(t, map[string]any{"idle": false}); len(frames) != 0 {
		t.Fatalf("frames after idle: false: %#v", frames)
	}
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
	// A mention in a private room wakes only its members (§4.2.4).
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
	} {
		a.expectError(t, "push_register", "bad-"+formatID(int64(i)), params, CodeInvalidParams)
	}
	// A url rejected for its size is too_large (§1.1).
	a.expectError(t, "push_register", "long-url", map[string]any{"kind": "relay", "url": "https://relay.example/" + strings.Repeat("a", maxPushURLBytes)}, CodeTooLarge)
	registration := func(u string) *pushRegistration {
		app.mu.RLock()
		defer app.mu.RUnlock()
		return app.pushes.Get(u, endpoint)
	}
	// A registration belongs to its user and url: another user's of the
	// same url is their own, and re-registering replaces only the caller's.
	// Scopes the server does not implement are ignored.
	a.result(t, "push_register", "webpush", map[string]any{"kind": "webpush", "url": endpoint, "keys": keys, "push_id": "t65S5XBst9bSDpjJ", "wake": []any{"mentions", "private", "ext:later", "unknown"}, "token": "ignored"})
	b.result(t, "push_register", "relay", map[string]any{"kind": "relay", "url": endpoint, "token": "tok"})
	first := registration(a.userID)
	if first.Kind != "webpush" || first.Token != "" || first.PushID != "t65S5XBst9bSDpjJ" || first.Wake != wakeMentions|wakePrivate || first.Keys == nil {
		t.Fatalf("webpush registration: %#v", first)
	}
	if other := registration(b.userID); other.Kind != "relay" || other.Token != "tok" || other.Wake != defaultWake || other.Keys != nil {
		t.Fatalf("relay registration: %#v", other)
	}
	a.result(t, "push_register", "again", map[string]any{"kind": "relay", "url": endpoint, "keys": keys, "wake": []any{}})
	if again := registration(a.userID); again == first || again.Kind != "relay" || again.Keys == nil || again.PushID != "" || again.Wake != 0 || again.Renewed.Before(first.Renewed) {
		t.Fatalf("re-registration: %#v", again)
	}
	// Another spelling of the same endpoint is the same registration.
	a.result(t, "push_register", "spelling", map[string]any{"kind": "relay", "url": "HTTPS://Push.Example.NET:443/s/abc", "keys": keys, "wake": []any{}})
	app.mu.RLock()
	spellings := app.pushes.Count(a.userID)
	app.mu.RUnlock()
	if spellings != 1 {
		t.Fatalf("registrations of one endpoint under two spellings: %d", spellings)
	}
	a.expectError(t, "push_register", "long", map[string]any{"kind": "relay", "url": "https://push.example.net/" + strings.Repeat("a", maxPushURLBytes-len("https://push.example.net/")+1)}, CodeTooLarge)
	// Unregistering removes only the caller's; an unknown url succeeds.
	a.result(t, "push_unregister", "unregister", map[string]any{"url": "https://PUSH.example.net/s/abc"})
	a.result(t, "push_unregister", "unknown", map[string]any{"url": "https://push.example.net/never"})
	if registration(a.userID) != nil || registration(b.userID) == nil {
		t.Fatal("unregister removed the wrong registration")
	}
	a.expectError(t, "push_unregister", "no-url", map[string]any{}, CodeInvalidParams)

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
	for _, p := range app.pushes.Of(a.userID) {
		urls = append(urls, strings.TrimPrefix(p.URL, endpoint))
	}
	app.mu.RUnlock()
	if len(urls) != maxPushesPerUser || slices.Contains(urls, "1") || !slices.Contains(urls, "0") || !slices.Contains(urls, "new") {
		t.Fatalf("registrations after the eleventh: %v", urls)
	}
	a.expectQuiet(t)
	b.expectQuiet(t)
}

// Wake scopes (§4.9): mentions, replies, private, joined; [] wakes for
// nothing, and without wake the default is mentions and replies.
func TestPushWakeScopes(t *testing.T) {
	relay := newTestRelay(t)
	app, httpServer := pushTestServer(t)
	a := dialTestClient(t, httpServer)
	carol := addAccount(t, app, "carol", a)
	for name, wake := range map[string]wakeScope{
		"default": defaultWake, "joined": wakeJoined, "private": wakePrivate, "replies": wakeReplies, "mentions": wakeMentions, "nothing": 0,
	} {
		addPush(app, carol, pushRegistration{URL: relay.URL + "/" + name, Wake: wake})
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
	logID := app.log.NextID()
	own := formatID(logID)
	m := app.rooms.NewMessage(own, carol.from(), carol.id)
	app.commitSnapshotLocked(m, &roomlog.Message{MessageID: own, LogID: own, RoomID: "general", From: carol.from(), Body: &roomlog.Body{Body: apron.Body{Text: "carol's"}}}, logID)
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

// expectMute reads the `status` notification that tells c of a mute
// (§4.5): everywhere, or of roomID.
func expectMute(t *testing.T, c *testClient, roomID string, mute any) {
	t.Helper()
	checkMute(t, c.read(t), roomID, mute)
}

func checkMute(t *testing.T, frame map[string]any, roomID string, mute any) {
	t.Helper()
	want := map[string]any{"mute": mute}
	if roomID != "" {
		want["room_id"] = roomID
	}
	if params := notificationParams(t, frame, "status"); !reflect.DeepEqual(params, want) {
		t.Fatalf("status = %#v, want %#v", params, want)
	}
}

// Muted users, and those whose status is dnd, get no pushes; a muted room
// wakes only for mentions, as in other chat apps (§4.9, §4.5).
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
	expectEcho(t, b, map[string]any{"room_id": "general", "mute": true}, "general", true)
	post(maps.Clone(chatter))
	if got := relay.pushes(t, app, a); len(got) != 0 {
		t.Fatalf("muted room woke %d for chatter", len(got))
	}
	post(maps.Clone(mention))
	if got := relay.pushes(t, app, a); len(got) != 1 {
		t.Fatalf("muted room woke %d for a mention", len(got))
	}
	expectEcho(t, b, map[string]any{"room_id": "general", "mute": false}, "general", false)
	post(maps.Clone(chatter))
	if got := relay.pushes(t, app, a); len(got) != 1 {
		t.Fatalf("unmuted room woke %d", len(got))
	}
	// A muted user gets no pushes at all, mentions included.
	expectEcho(t, b, map[string]any{"mute": 3600}, "", float64(3600))
	post(maps.Clone(mention))
	if got := relay.pushes(t, app, a); len(got) != 0 {
		t.Fatalf("muted user woken %d", len(got))
	}
	expectEcho(t, b, map[string]any{"mute": false}, "", false)
	post(maps.Clone(mention))
	if got := relay.pushes(t, app, a); len(got) != 1 {
		t.Fatalf("unmuted user woken %d", len(got))
	}
	// A dnd status silences like the mute.
	if you := b.result(t, "me", "dnd", map[string]any{"status": "dnd"})["you"].(map[string]any); you["status"] != "dnd" {
		t.Fatalf("you after dnd: %#v", you)
	}
	post(maps.Clone(mention))
	if got := relay.pushes(t, app, a); len(got) != 0 {
		t.Fatalf("dnd user woken %d", len(got))
	}
	b.result(t, "me", "online", map[string]any{"status": "online"})
	post(maps.Clone(mention))
	if got := relay.pushes(t, app, a); len(got) != 1 {
		t.Fatalf("online user woken %d", len(got))
	}

	// A room's mute covers its threads, and applies to a room the user has
	// not joined: replies are silenced there, mentions still wake.
	thread, _ := saveRoom(t, a, "thread", map[string]any{"parent_room_id": "general", "title": "Thread"})
	b.notification(t, "room_update")
	joinRoom(t, b, thread)
	expectMembership(t, a, thread, b.userID, true)
	b.status(t, map[string]any{"room_id": "general", "mute": true})
	save(t, a, "in-thread-chatter", map[string]any{"room_id": thread, "body": map[string]any{"text": "chatter"}})
	b.notification(t, "message")
	if got := relay.pushes(t, app, a); len(got) != 0 {
		t.Fatalf("thread of a muted room woke %d for chatter", len(got))
	}
	save(t, a, "in-thread", map[string]any{"room_id": thread, "body": map[string]any{"text": "@guest_2", "mentions": []any{"guest_2"}}})
	b.notification(t, "message")
	if got := relay.pushes(t, app, a); len(got) != 1 {
		t.Fatalf("thread of a muted room woke %d for a mention", len(got))
	}
	b.status(t, map[string]any{"room_id": "general", "mute": false})
	ops, _ := saveRoom(t, a, "ops", map[string]any{"title": "Ops"})
	frames := b.status(t, map[string]any{"room_id": ops, "mute": true})
	if len(frames) != 1 {
		t.Fatalf("frames after muting an unjoined room: %#v", frames)
	}
	checkMute(t, frames[0], ops, true)
	theirs := formatID(serverMessage(t, app, b.userID))
	a.notification(t, "message")
	b.notification(t, "message")
	save(t, a, "ops-reply", map[string]any{"room_id": ops, "body": map[string]any{"text": "re"}, "reply_to": map[string]any{"message_id": theirs}})
	if got := relay.pushes(t, app, a); len(got) != 0 {
		t.Fatalf("muted unjoined room woke %d for a reply", len(got))
	}
	save(t, a, "ops-mention", map[string]any{"room_id": ops, "body": map[string]any{"text": "@guest_2", "mentions": []any{"guest_2"}}})
	if got := relay.pushes(t, app, a); len(got) != 1 {
		t.Fatalf("muted unjoined room woke %d for a mention", len(got))
	}
	b.status(t, map[string]any{"room_id": ops, "mute": false})
	save(t, a, "ops-mention-again", map[string]any{"room_id": ops, "body": map[string]any{"text": "@guest_2", "mentions": []any{"guest_2"}}})
	if got := relay.pushes(t, app, a); len(got) != 1 || got[0].header.Get("Urgency") != "high" {
		t.Fatalf("mention in an unmuted unjoined room: %#v", got)
	}
}

// serverMessage is the message_id of a message by userID, made in general.
func serverMessage(t *testing.T, app *Server, userID string) int64 {
	t.Helper()
	app.mu.Lock()
	defer app.unlock()
	u := app.users.Get(userID)
	logID := app.log.NextID()
	id := formatID(logID)
	m := app.rooms.NewMessage(id, u.from(), u.id)
	app.commitSnapshotLocked(m, &roomlog.Message{MessageID: id, LogID: id, RoomID: "general", From: u.from(), Body: &roomlog.Body{Body: apron.Body{Text: "theirs"}}}, logID)
	return logID
}

// Badge pushes wait push.BadgeDelay, so one push carries the count after a burst
// of changes; a guest's retirement sends none.
func TestBadgePushesCoalesce(t *testing.T) {
	relay := newTestRelay(t)
	app, httpServer := pushTestServer(t)
	app.pushes.SetBadgeDelay(300 * time.Millisecond)
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

// Shutdown cancels pending badge pushes rather than waiting out
// push.BadgeDelay.
func TestShutdownCancelsPendingBadges(t *testing.T) {
	relay := newTestRelay(t)
	app, httpServer := pushTestServer(t)
	app.pushes.SetBadgeDelay(time.Hour)
	a := dialTestClient(t, httpServer)
	dora := addAccount(t, app, "dora", a)
	addPush(app, dora, pushRegistration{URL: relay.URL + "/dora", Wake: wakeBadge})
	save(t, a, "post", map[string]any{"body": map[string]any{"text": "hi"}})
	app.mu.Lock()
	pending := app.pushes.BadgePending(dora.id)
	app.mu.Unlock()
	if !pending {
		t.Fatal("no badge push pending")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown with a pending badge: %v", err)
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
	// Relays get TTL and Urgency too (§4.9).
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
	kept := app.pushes.Get(c.userID, relay.URL+"/web/missing") != nil
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
	typed := func(snapshot map[string]any) *roomlog.Message { return roomlog.DecodeMessage(encodeJSON(snapshot)) }
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
	got := decode(pushPayload("p", 3, typed(snapshot)))
	want := map[string]any{"push_id": "p", "unread": float64(3), "message": map[string]any{
		"message_id": "1724803200042", "room_id": "general", "from": map[string]any{"user_id": "alice", "name": "Alice"},
		"reply_to": map[string]any{"message_id": "1724803200001"}, "body": map[string]any{"text": "short"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("payload: %#v", got)
	}
	// Text is truncated to maxPushTextRunes.
	snapshot["body"] = map[string]any{"text": strings.Repeat("e", maxPushTextRunes+500)}
	text := decode(pushPayload("", 0, typed(snapshot)))["message"].(map[string]any)["body"].(map[string]any)["text"].(string)
	if runes := []rune(text); len(runes) != maxPushTextRunes || runes[len(runes)-1] != '…' {
		t.Fatalf("truncated text: %d runes", len(runes))
	}
	// Mentions go first.
	many := make([]any, 200)
	for i := range many {
		many[i] = "a_rather_long_user_id_" + formatID(int64(i))
	}
	snapshot["body"] = map[string]any{"text": "hi", "mentions": many}
	if body := decode(pushPayload("", 0, typed(snapshot)))["message"].(map[string]any)["body"]; !reflect.DeepEqual(body, map[string]any{"text": "hi"}) {
		t.Fatalf("body without mentions: %#v", body)
	}
	// Then text is shortened to fit.
	snapshot["body"] = map[string]any{"text": strings.Repeat(" ", maxPushTextRunes)}
	payload := pushPayload("", 0, typed(snapshot))
	text = decode(payload)["message"].(map[string]any)["body"].(map[string]any)["text"].(string)
	if !strings.HasSuffix(text, "…") || len(payload) < maxPushPayloadBytes-16 {
		t.Fatalf("shortened text: %d bytes of payload", len(payload))
	}
	// Then body goes, from keeps only user_id, and reply_to goes.
	snapshot["body"] = map[string]any{"text": "hi"}
	snapshot["from"] = map[string]any{"user_id": "alice", "name": strings.Repeat("\\", 3000)}
	message := decode(pushPayload("", 0, typed(snapshot)))["message"].(map[string]any)
	if _, has := message["body"]; has || !reflect.DeepEqual(message["from"], map[string]any{"user_id": "alice"}) || message["reply_to"] == nil {
		t.Fatalf("message without body and name: %#v", message)
	}
}

func TestPushRegistrationsExpire(t *testing.T) {
	relay := newTestRelay(t)
	app, httpServer := pushTestServer(t)
	a := dialTestClient(t, httpServer)
	dora := addAccount(t, app, "dora", a)
	stale := addPush(app, dora, pushRegistration{URL: relay.URL + "/stale", Wake: defaultWake, Renewed: time.Now().Add(-pushExpiry - time.Minute)})
	addPush(app, dora, pushRegistration{URL: relay.URL + "/fresh", Wake: defaultWake, Renewed: time.Now().Add(-pushExpiry + time.Hour)})
	save(t, a, "mention", map[string]any{"body": map[string]any{"text": "@dora", "mentions": []any{"dora"}}})
	if got := paths(relay.pushes(t, app, a)); !reflect.DeepEqual(got, []string{"/fresh"}) {
		t.Fatalf("deliveries: %v", got)
	}
	app.mu.RLock()
	kept := app.pushes.Get("dora", stale.URL) != nil
	app.mu.RUnlock()
	if kept {
		t.Fatal("an expired registration was kept")
	}
}

// unread is the user's one count of messages after their read positions,
// the same in every registration's pushes, and badge pushes, without a
// message and with Urgency low, send each change of it to the relay
// registrations with scope badge (§4.9).
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
	// So does what a dnd status silences.
	b.status(t, map[string]any{"mute": false})
	b.result(t, "me", "dnd", map[string]any{"status": "dnd"})
	post(map[string]any{"body": map[string]any{"text": "@guest_2", "mentions": []any{"guest_2"}}})
	expect(delivery{"/mentions", float64(2), "low", false}, delivery{"/phone", float64(2), "low", false})
	// A room's mute leaves what it silences out of unread, but not mentions,
	// which still wake.
	b.result(t, "me", "online", map[string]any{"status": "online"})
	b.status(t, map[string]any{"room_id": "general", "mute": true})
	expect()
	post(map[string]any{"body": map[string]any{"text": "chatter"}})
	expect()
	post(map[string]any{"body": map[string]any{"text": "@guest_2", "mentions": []any{"guest_2"}}})
	expect(delivery{"/mentions", float64(3), "high", true}, delivery{"/phone", float64(3), "low", false})
}

// Registrations, with their keys, push_id, and scopes, survive a restart,
// and so do the VAPID key and the status and mutes the user set (§4.5). A
// stored status the server does not support loads as "".
func TestPushAndStatusSurviveRestart(t *testing.T) {
	memory := store.NewMemory()
	_, keys := testSubscription(t)
	if err := memory.Apply([]store.Entry{
		{Kind: entryUser, ID: "erin", Value: encodeJSON(users.Stored{Name: "Erin", Passkey: jsontext.Value(`{"handle":"aA=="}`)})},
		{Kind: entryUser, ID: "finn", Value: []byte(`{"name":"Finn","passkey":{"handle":"aA=="},"status":"invisible"}`)},
		{Kind: entryUser, ID: "gail", Value: []byte(`{"name":"Gail","passkey":{"handle":"aQ=="},"status":"away"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := memory.Apply(pushEntries(&pushRegistration{User: "erin", Kind: "relay", URL: "https://relay.example/erin", Token: "tok", Wake: defaultWake, Renewed: time.Now()})); err != nil {
		t.Fatal(err)
	}
	app, httpServer, stop := startWithStore(t, memory, t.TempDir())
	_, frame := dialRaw(t, httpServer)
	key := frame["params"].(map[string]any)["push"].(map[string]any)["webpush"].(map[string]any)["key"]
	app.mu.Lock()
	erin := app.users.Get("erin")
	relay := app.pushes.Get("erin", "https://relay.example/erin")
	if relay == nil || relay.Token != "tok" || relay.Wake != defaultWake || !relay.Live(time.Now()) {
		app.mu.Unlock()
		t.Fatalf("relay registration: %#v", relay)
	}
	subscription, _ := webpush.ParseKeys(keys["p256dh"].(string), keys["auth"].(string))
	app.pushes.Add(push.New(pushRegistration{
		User: "erin", Kind: "webpush", URL: "https://push.example/s", PushID: "p1", Keys: &subscription,
		Wake: wakeJoined | wakeBadge, Renewed: time.Now(),
	}))
	if finn, gail := app.users.Get("finn"), app.users.Get("gail"); finn.Presence.Chosen != statusInvisible || gail.Presence.Chosen != statusNone || erin.Presence.Chosen != statusOnline {
		app.mu.Unlock()
		t.Fatalf("loaded statuses: erin %q finn %q gail %q", erin.Presence.Chosen, finn.Presence.Chosen, gail.Presence.Chosen)
	}
	erin.Presence.Chosen = statusDND
	app.users.Get("finn").Presence.Chosen = statusNone
	erin.Mute = muteState{Until: time.Now().Add(time.Hour)}
	erin.RoomMutes["general"] = muteState{Forever: true}
	app.users.Touch("erin")
	app.users.Touch("finn")
	app.unlock()
	stop()

	app, httpServer, _ = startWithStore(t, memory, t.TempDir())
	_, frame = dialRaw(t, httpServer)
	if again := frame["params"].(map[string]any)["push"].(map[string]any)["webpush"].(map[string]any)["key"]; again != key {
		t.Fatalf("VAPID key changed across a restart: %v then %v", key, again)
	}
	app.mu.RLock()
	erin = app.users.Get("erin")
	web := app.pushes.Get("erin", "https://push.example/s")
	if web == nil || web.Kind != "webpush" || web.PushID != "p1" || web.Wake != wakeJoined|wakeBadge || web.Keys == nil || webpush.EncodeBase64URL(web.Keys.Auth) != keys["auth"] {
		t.Fatalf("webpush registration after restart: %#v", web)
	}
	if app.pushes.Get("erin", "https://relay.example/erin") == nil || app.pushes.Len() != 2 {
		t.Fatalf("registrations after restart: %v", app.pushes)
	}
	if erin.Presence.Chosen != statusDND || app.users.Get("finn").Presence.Chosen != statusNone || !erin.Mute.Active(time.Now()) || !erin.RoomMutes["general"].Forever {
		t.Fatalf("status after restart: %q %q %v %v", erin.Presence.Chosen, app.users.Get("finn").Presence.Chosen, erin.Mute, erin.RoomMutes)
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
	if !reflect.DeepEqual(stored, []string{"erin https://push.example/s", "erin https://relay.example/erin"}) {
		t.Fatalf("stored registrations: %v", stored)
	}
	// A configured VAPID key replaces the stored one, and a malformed one
	// stops the server from starting.
	configured, _ := webpush.NewVAPIDKey()
	config := DefaultConfig()
	config.VAPIDPrivateKey = configured.Encoded()
	if app, _ := newTestServer(t, config); app.push.VAPID.Public != configured.Public {
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
	addPush(app, dora, pushRegistration{URL: relay.URL + "/broken/missing", Wake: defaultWake})
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
	count := dora.pushDay.Count(time.Now())
	app.unlock()
	if count != 0 {
		t.Fatalf("a refused push counted: %d", count)
	}
	addPush(app, dora, pushRegistration{URL: relay.URL + "/dora", Wake: defaultWake})
	if got := mention(); got != 1 {
		t.Fatalf("deliveries: %d", got)
	}
	app.mu.Lock()
	count = dora.pushDay.Count(time.Now())
	for range maxPushesPerUserDay - count {
		dora.pushDay.Accepted(time.Now())
	}
	app.unlock()
	if count != 1 {
		t.Fatalf("an accepted push counted %d", count)
	}
	if got := mention(); got != 0 {
		t.Fatalf("deliveries past the daily cap: %d", got)
	}
	// A new day starts over.
	app.mu.Lock()
	dora.pushDay = push.Day{}
	app.unlock()
	if got := mention(); got != 1 {
		t.Fatalf("deliveries on a new day: %d", got)
	}
}

// With push disabled, stored registrations are kept but not loaded;
// enabled again, they apply.
func TestDisabledPushIgnoresStoredRegistrations(t *testing.T) {
	memory := store.NewMemory()
	url := "https://relay.example/erin"
	if err := memory.Apply([]store.Entry{
		{Kind: entryUser, ID: "erin", Value: encodeJSON(users.Stored{Name: "Erin", Passkey: jsontext.Value(`{"handle":"aA=="}`)})},
	}); err != nil {
		t.Fatal(err)
	}
	if err := memory.Apply(pushEntries(&pushRegistration{User: "erin", Kind: "relay", URL: url, Renewed: time.Now()})); err != nil {
		t.Fatal(err)
	}
	pushesOf := func(app *Server) int {
		app.mu.RLock()
		defer app.mu.RUnlock()
		return app.pushes.Len()
	}
	app, _, stop := startWith(t, memory, t.TempDir(), func(c *Config) { c.DisablePush = true })
	if pushes := pushesOf(app); pushes != 0 {
		t.Fatalf("with push disabled: %d registrations", pushes)
	}
	stop()
	app, _, _ = startWithStore(t, memory, t.TempDir())
	if pushes := pushesOf(app); pushes != 1 {
		t.Fatalf("with push enabled again: %d registrations", pushes)
	}
}

// A count is recorded as an endpoint's only once it accepts the push with a
// 2xx: a badge push that failed is sent again at the next change, even one
// that leaves the count where it was (§4.9).
func TestBadgeCountRecordedOnlyWhenAccepted(t *testing.T) {
	relay := newTestRelay(t)
	app, httpServer := pushTestServer(t)
	a := dialTestClient(t, httpServer)
	alice := addAccount(t, app, "alice", a)
	addPush(app, alice, pushRegistration{URL: relay.URL + "/flaky", Wake: wakeBadge})
	relay.failing.Store(true)
	save(t, a, "post", map[string]any{"body": map[string]any{"text": "hi"}})
	if got := relay.pushes(t, app, a); len(got) != 1 || got[0].payload["unread"] != float64(1) {
		t.Fatalf("first badge: %#v", got)
	}
	relay.failing.Store(false)
	recount := func() {
		app.mu.Lock()
		app.unreadChangedLocked(alice, "")
		app.unlock()
	}
	recount()
	if got := relay.pushes(t, app, a); len(got) != 1 || got[0].payload["unread"] != float64(1) {
		t.Fatalf("badge after a failed one: %#v", got)
	}
	// Accepted, the same count is not sent again.
	recount()
	if got := relay.pushes(t, app, a); len(got) != 0 {
		t.Fatalf("badge of an accepted count: %#v", got)
	}
}

// A timed room mute that runs out changes what counts, so the count is
// taken again and a badge push sent (§4.9, §4.5).
func TestRoomMuteExpiryUpdatesBadge(t *testing.T) {
	relay := newTestRelay(t)
	app, httpServer := pushTestServer(t)
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	b.result(t, "push_register", "badge", map[string]any{"kind": "relay", "url": relay.URL + "/b", "wake": []any{"badge"}})
	if frames := b.status(t, map[string]any{"room_id": "general", "mute": 1}); len(frames) != 1 {
		t.Fatalf("room mute echo: %#v", frames)
	}
	save(t, a, "post", map[string]any{"body": map[string]any{"text": "hi"}})
	b.notification(t, "message")
	for _, got := range relay.pushes(t, app, a) {
		if got.payload["unread"] != float64(0) {
			t.Fatalf("badge while the room is muted: %#v", got)
		}
	}
	time.Sleep(1200 * time.Millisecond)
	if got := relay.pushes(t, app, a); len(got) != 1 || got[0].payload["unread"] != float64(1) {
		t.Fatalf("badge after the room mute ran out: %#v", got)
	}
	// Its end is sent to the user's connections (§4.5).
	expectMute(t, b, "general", false)
	b.expectQuiet(t)
}

// A wake list's names past the first maxWakeScopes, and names longer than
// maxWakeScopeBytes, are ignored rather than refused (§4.9).
func TestWakeNamesPastTheBoundsAreIgnored(t *testing.T) {
	app, httpServer := pushTestServer(t)
	a := dialTestClient(t, httpServer)
	endpoint := "http://127.0.0.1:1/p"
	wakeOf := func(id string, wake []any) wakeScope {
		t.Helper()
		a.result(t, "push_register", id, map[string]any{"kind": "relay", "url": endpoint, "wake": wake})
		app.mu.RLock()
		defer app.mu.RUnlock()
		return app.pushes.Get(a.userID, endpoint).Wake
	}
	many := make([]any, maxWakeScopes+4)
	for i := range many {
		many[i] = "ext:filler"
	}
	many[0], many[maxWakeScopes] = "replies", "mentions"
	if got := wakeOf("many", many); got != wakeReplies {
		t.Fatalf("wake of %d names: %v", len(many), got)
	}
	if got := wakeOf("long", []any{"mentions" + strings.Repeat(" ", maxWakeScopeBytes), "private"}); got != wakePrivate {
		t.Fatalf("wake with a long name: %v", got)
	}
}

// private wakes the members of a private room for messages in its threads,
// joined or not (§4.9).
func TestPrivateWakesForThreadsOfJoinedRooms(t *testing.T) {
	relay := newTestRelay(t)
	app, httpServer := pushTestServer(t)
	a := dialTestClient(t, httpServer)
	carol := addAccount(t, app, "carol", a)
	addPush(app, carol, pushRegistration{URL: relay.URL + "/private", Wake: wakePrivate})
	addAccount(t, app, "dave", a)
	addPush(app, app.users.Get("dave"), pushRegistration{URL: relay.URL + "/dave", Wake: wakePrivate | wakeJoined})
	secret, _ := saveRoom(t, a, "secret", map[string]any{"title": "Secret", "private": true})
	a.request(t, "room_join", "add-carol", map[string]any{"room_id": secret, "user_id": "carol"})
	thread, _ := saveRoom(t, a, "thread", map[string]any{"parent_room_id": secret, "private": false})
	nested, _ := saveRoom(t, a, "nested", map[string]any{"parent_room_id": thread})
	relay.pushes(t, app, a)
	for i, room := range []string{thread, nested} {
		save(t, a, fmt.Sprint("post-", i), map[string]any{"room_id": room, "body": map[string]any{"text": "in a thread"}})
		got := relay.pushes(t, app, a)
		if !reflect.DeepEqual(paths(got), []string{"/private"}) || got[0].header.Get("Urgency") != "high" {
			t.Fatalf("a message in a thread of a private room carol joined woke %v", paths(got))
		}
	}
	// A public room's thread wakes no one for private.
	public, _ := saveRoom(t, a, "public-thread", map[string]any{"parent_room_id": "general"})
	save(t, a, "post-public", map[string]any{"room_id": public, "body": map[string]any{"text": "public"}})
	if got := relay.pushes(t, app, a); len(got) != 0 {
		t.Fatalf("a public thread woke %v", paths(got))
	}
}

// A mention in a private room wakes a member who wakes for mentions alone,
// and never someone outside it, mentioned or not (§4.9, §4.2.4).
func TestMentionsWakeInPrivateRooms(t *testing.T) {
	relay := newTestRelay(t)
	app, httpServer := pushTestServer(t)
	a := dialTestClient(t, httpServer)
	carol := addAccount(t, app, "carol", a)
	addPush(app, carol, pushRegistration{URL: relay.URL + "/carol", Wake: wakeMentions})
	dave := addAccount(t, app, "dave", a)
	addPush(app, dave, pushRegistration{URL: relay.URL + "/dave", Wake: wakeMentions})
	dm, _ := saveRoom(t, a, "dm", map[string]any{"private": true})
	a.request(t, "room_join", "add-carol", map[string]any{"room_id": dm, "user_id": "carol"})
	relay.pushes(t, app, a)
	save(t, a, "plain", map[string]any{"room_id": dm, "body": map[string]any{"text": "no mention"}})
	if got := relay.pushes(t, app, a); len(got) != 0 {
		t.Fatalf("a message without a mention woke %v", paths(got))
	}
	save(t, a, "mention", map[string]any{"room_id": dm, "body": map[string]any{"text": "@carol @dave look", "mentions": []any{"carol", "dave"}}})
	got := relay.pushes(t, app, a)
	if !reflect.DeepEqual(paths(got), []string{"/carol"}) || got[0].header.Get("Urgency") != "high" {
		t.Fatalf("a mention in a private room woke %v", paths(got))
	}
	if message, _ := got[0].payload["message"].(map[string]any); message["room_id"] != dm {
		t.Fatalf("pushed %#v", got[0].payload)
	}
}

// unreadState is u's unread count as kept, and as counted again from the
// log.
func unreadState(app *Server, u *userState) (kept, fresh int) {
	app.mu.Lock()
	defer app.unlock()
	now := time.Now()
	kept = app.unreadLocked(u, now)
	u.unread = nil
	return kept, app.unreadLocked(u, now)
}

// Muting or unmuting a room takes the unread counts of its threads again,
// which the mute silences too (§4.9, §4.5).
func TestRoomMuteRecountsThreads(t *testing.T) {
	relay := newTestRelay(t)
	app, httpServer := pushTestServer(t)
	a := dialTestClient(t, httpServer)
	carol := addAccount(t, app, "carol", a)
	addPush(app, carol, pushRegistration{URL: relay.URL + "/badge", Wake: wakeBadge})
	thread, _ := saveRoom(t, a, "thread", map[string]any{"parent_room_id": "general", "title": "T"})
	a.request(t, "room_join", "add-carol", map[string]any{"room_id": thread, "user_id": "carol"})
	save(t, a, "p1", map[string]any{"room_id": thread, "body": map[string]any{"text": "one"}})
	save(t, a, "p2", map[string]any{"room_id": thread, "body": map[string]any{"text": "two"}})
	relay.pushes(t, app, a)
	if kept, fresh := unreadState(app, carol); kept != 2 || fresh != 2 {
		t.Fatalf("before the mute: kept %d, counted %d", kept, fresh)
	}
	for _, mute := range []bool{true, false} {
		app.mu.Lock()
		app.applyStatusLocked(nil, carol, statusUpdate{roomID: "general", scoped: true, hasMute: true, muteForever: mute})
		app.unlock()
		want := 2
		if mute {
			want = 0
		}
		if got := relay.pushes(t, app, a); len(got) != 1 || got[0].payload["unread"] != float64(want) {
			t.Fatalf("badge after mute %v: %#v", mute, got)
		}
		if kept, fresh := unreadState(app, carol); kept != want || fresh != want {
			t.Fatalf("after mute %v: kept %d, counted %d, want %d", mute, kept, fresh, want)
		}
	}
}

// Leaving a private room takes the unread counts of its threads again: a
// thread the user could see there, and was mentioned in without joining,
// no longer counts (§4.9).
func TestLeavingPrivateRoomRecountsThreads(t *testing.T) {
	relay := newTestRelay(t)
	app, httpServer := pushTestServer(t)
	a := dialTestClient(t, httpServer)
	carol := addAccount(t, app, "carol", a)
	addPush(app, carol, pushRegistration{URL: relay.URL + "/badge", Wake: wakeBadge})
	secret, _ := saveRoom(t, a, "secret", map[string]any{"title": "Secret", "private": true})
	a.request(t, "room_join", "add-carol", map[string]any{"room_id": secret, "user_id": "carol"})
	thread, _ := saveRoom(t, a, "thread", map[string]any{"parent_room_id": secret, "private": false})
	save(t, a, "ping", map[string]any{"room_id": thread, "body": map[string]any{"text": "@carol", "mentions": []any{"carol"}}})
	relay.pushes(t, app, a)
	if kept, fresh := unreadState(app, carol); kept != 1 || fresh != 1 {
		t.Fatalf("before leaving: kept %d, counted %d", kept, fresh)
	}
	a.request(t, "room_leave", "remove-carol", map[string]any{"room_id": secret, "user_id": "carol"})
	if got := relay.pushes(t, app, a); len(got) != 1 || got[0].payload["unread"] != float64(0) {
		t.Fatalf("badge after leaving: %#v", got)
	}
	if kept, fresh := unreadState(app, carol); kept != 0 || fresh != 0 {
		t.Fatalf("after leaving: kept %d, counted %d", kept, fresh)
	}
}

// Shutdown stops the timers that end timed mutes and send coalesced
// statuses, and no badge push is scheduled after it.
func TestShutdownStopsTimers(t *testing.T) {
	relay := newTestRelay(t)
	app, httpServer := pushTestServer(t)
	app.pushes.SetBadgeDelay(time.Hour)
	a := dialTestClient(t, httpServer)
	dora := addAccount(t, app, "dora", a)
	addPush(app, dora, pushRegistration{URL: relay.URL + "/dora", Wake: wakeBadge})
	app.mu.Lock()
	now := time.Now()
	app.users.SetMute(dora, "", muteState{Until: now.Add(time.Hour)})
	app.users.SetMute(dora, "general", muteState{Until: now.Add(time.Hour)})
	dora.Presence.SetPublishLimit(rate.NewLimiter(rate.Every(time.Hour), 0))
	dora.Presence.Chosen = statusNone
	app.announceStatusLocked(dora)
	pending := app.users.Pending(dora.id) == 3
	app.unlock()
	if !pending {
		t.Fatal("timers not pending")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown with pending timers: %v", err)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	app.scheduleBadgeLocked(dora)
	app.users.ScheduleMutes(dora)
	if pending, badge := app.users.Pending(dora.id), app.pushes.BadgePending(dora.id); pending != 0 || badge {
		t.Fatalf("timers after shutdown: %d, badge %v", pending, badge)
	}
}
