package server

import (
	"encoding/json/v2"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

type relayRequest struct {
	path          string
	authorization string
	payload       map[string]any
}

func TestPushWakesMentionedUsersWhoAreAway(t *testing.T) {
	received := make(chan relayRequest, 16)
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.UnmarshalRead(r.Body, &payload)
		received <- relayRequest{path: r.URL.Path, authorization: r.Header.Get("Authorization"), payload: payload}
		if r.URL.Path == "/gone" {
			w.WriteHeader(http.StatusGone)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer relay.Close()
	config := DefaultConfig()
	config.AllowInsecurePush = true
	app, httpServer := newTestServer(t, config)
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	a.result(t, "push_register", "register", map[string]any{"kind": "relay", "url": relay.URL + "/a", "token": "secret"})
	a.expectError(t, "push_register", "kind", map[string]any{"kind": "webpush", "url": relay.URL + "/a"}, codeInvalidParams)
	a.expectError(t, "push_register", "scheme", map[string]any{"kind": "relay", "url": "ftp://relay.example/a"}, codeInvalidParams)
	a.result(t, "push_unregister", "unregister", map[string]any{"url": relay.URL + "/a"})

	// Push reaches users without a connection, such as a passkey user who is
	// away, in rooms they have joined.
	app.mu.Lock()
	alice := newUserState("alice", "Alice")
	alice.passkey = &passkeyUser{user: alice}
	app.users[alice.id] = alice
	app.addMemberLocked(alice, app.rooms[defaultRoomID])
	app.pushes[relay.URL+"/alice"] = &pushRegistration{userID: "alice", kind: "relay", url: relay.URL + "/alice", token: "tok"}
	app.pushes[relay.URL+"/gone"] = &pushRegistration{userID: "alice", kind: "relay", url: relay.URL + "/gone"}
	app.mu.Unlock()
	expectMembership(t, a, "general", "alice", true)
	expectMembership(t, b, "general", "alice", true)
	pushes := func() []relayRequest {
		t.Helper()
		a.expectQuiet(t) // Requests before this one have finished waking users.
		app.push.wait()
		var requests []relayRequest
		for len(received) > 0 {
			requests = append(requests, <-received)
		}
		return requests
	}
	post := func(id string, params map[string]any) string {
		t.Helper()
		messageID, _ := save(t, a, id, params)
		b.notification(t, "message")
		return messageID
	}

	// Only body.mentions decides who is mentioned; text is never parsed.
	text := "@alice: the deploy is done"
	id := post("text", map[string]any{"body": map[string]any{"text": text, "format": "markdown"}})
	if got := pushes(); len(got) != 0 {
		t.Fatalf("text mention woke %d", len(got))
	}
	// An edit mentions the users it adds.
	post("add-mention", map[string]any{"message_id": id, "body": map[string]any{"text": text, "mentions": []any{"alice"}}})
	got := pushes()
	if len(got) != 2 {
		t.Fatalf("relay received %d requests, want one per registration", len(got))
	}
	for _, request := range got {
		want := map[string]any{"message_id": id, "room_id": "general", "from": map[string]any{"user_id": "guest_1"}, "body": map[string]any{"text": text}}
		if !reflect.DeepEqual(request.payload, want) {
			t.Fatalf("payload: %#v", request.payload)
		}
		if request.path == "/alice" && request.authorization != "Bearer tok" {
			t.Fatalf("authorization: %q", request.authorization)
		}
	}
	// A relay that answers 410 loses its registration.
	app.mu.RLock()
	_, kept := app.pushes[relay.URL+"/gone"]
	app.mu.RUnlock()
	if kept {
		t.Fatal("gone registration was kept")
	}
	post("same-mention", map[string]any{"message_id": id, "body": map[string]any{"text": "edited", "mentions": []any{"alice"}}})
	if got := pushes(); len(got) != 0 {
		t.Fatalf("an edit re-mentioned: %d", len(got))
	}

	// A connected user is woken only when every connection is away.
	b.result(t, "push_register", "b", map[string]any{"kind": "relay", "url": relay.URL + "/b"})
	mentionB := map[string]any{"body": map[string]any{"text": "@guest_2 ping", "mentions": []any{"guest_2"}}}
	post("attending", maps.Clone(mentionB))
	if got := pushes(); len(got) != 0 {
		t.Fatalf("attending user woken: %d", len(got))
	}
	b.write(t, map[string]any{"method": "activity", "params": map[string]any{"away": true}})
	b.expectQuiet(t)
	post("away", maps.Clone(mentionB))
	if got := pushes(); len(got) != 1 || got[0].path != "/b" {
		t.Fatalf("away user: %#v", got)
	}
	// Typing ends away.
	b.write(t, map[string]any{"method": "activity", "params": map[string]any{"room_id": "general", "typing": 3}})
	a.notification(t, "activity")
	b.notification(t, "activity")
	post("back", maps.Clone(mentionB))
	if got := pushes(); len(got) != 0 {
		t.Fatalf("user back from away woken: %d", len(got))
	}
	// A reply wakes the author of the message it replies to.
	own, _ := save(t, b, "own", map[string]any{"body": map[string]any{"text": "mine"}})
	a.notification(t, "message")
	b.write(t, map[string]any{"method": "activity", "params": map[string]any{"away": true}})
	b.expectQuiet(t)
	post("reply", map[string]any{"body": map[string]any{"text": "a reply"}, "reply_to": map[string]any{"message_id": own}})
	if got := pushes(); len(got) != 1 {
		t.Fatalf("reply woke %d", len(got))
	}
	// A mention wakes a user in any room they can see, joined or not; a reply
	// wakes its target's author only in a room they have joined. Mentions in
	// commands notify no one.
	ops, _ := saveRoom(t, a, "ops", map[string]any{"title": "Ops"})
	save(t, a, "elsewhere", map[string]any{"room_id": ops, "body": map[string]any{"text": "@guest_2", "mentions": []any{"guest_2"}}})
	if got := pushes(); len(got) != 1 || got[0].path != "/b" || got[0].payload["room_id"] != ops {
		t.Fatalf("mention in an unjoined room: %#v", got)
	}
	save(t, a, "reply-elsewhere", map[string]any{"room_id": ops, "body": map[string]any{"text": "a reply"}, "reply_to": map[string]any{"message_id": own}})
	if got := pushes(); len(got) != 0 {
		t.Fatalf("reply in an unjoined room woke %d", len(got))
	}
	// A mention in a private room wakes only its members (§4.3.4).
	hidden, _ := saveRoom(t, a, "hidden", map[string]any{"title": "Hidden", "private": true})
	save(t, a, "hidden-mention", map[string]any{"room_id": hidden, "body": map[string]any{"text": "@guest_2", "mentions": []any{"guest_2"}}})
	if got := pushes(); len(got) != 0 {
		t.Fatalf("mention in a private room woke a non-member: %#v", got)
	}
	before, _ := a.request(t, "command", "help", map[string]any{"body": map[string]any{"text": "/help", "mentions": []any{"guest_2"}}})
	if len(before) != 1 {
		t.Fatalf("help frames: %#v", before)
	}
	if got := pushes(); len(got) != 0 {
		t.Fatalf("command woke %d", len(got))
	}
}

func TestPushRefusesInternalAddresses(t *testing.T) {
	var hits atomic.Int32
	relay := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer relay.Close()
	deliverer := newPushDeliverer(false)
	called := false
	deliverer.deliver(pushRegistration{url: relay.URL}, []byte("{}"), func(bool) { called = true })
	deliverer.wait()
	if hits.Load() != 0 || called {
		t.Fatalf("delivered to a loopback address: hits=%d called=%v", hits.Load(), called)
	}
	app := New(DefaultConfig())
	if problem := app.checkPushURL("http://relay.example/p"); problem == "" {
		t.Fatal("http push URL accepted without AllowInsecurePush")
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
		deliverer.deliver(pushRegistration{userID: "spammer", url: slow.URL + "/slow"}, []byte("{}"), func(bool) {})
	}
	deliverer.mu.Lock()
	queued := deliverer.users["spammer"]
	deliverer.mu.Unlock()
	if queued != maxPushQueuedPerUser {
		t.Fatalf("queued deliveries for one user: %d", queued)
	}
	for i := range maxConcurrentPushPOST {
		deliverer.deliver(pushRegistration{userID: "user" + formatID(int64(i)), url: slow.URL + "/slow"}, []byte("{}"), func(bool) {})
	}
	deadline := time.Now().Add(time.Second)
	for stalled.Load() < maxPushPerHost && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := stalled.Load(); got != maxPushPerHost {
		t.Fatalf("concurrent deliveries to one host: %d", got)
	}
	// Another relay host has its own lane.
	deliverer.deliver(pushRegistration{userID: "victim", url: fast.URL + "/fast"}, []byte("{}"), func(bool) {})
	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("a stalled relay host blocked delivery to another host")
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
		"::1":               false,
	} {
		if got := publicAddress(host); got != want {
			t.Errorf("publicAddress(%s) = %v, want %v", host, got, want)
		}
	}
}
