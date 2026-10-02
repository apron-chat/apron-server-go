package server

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// status sends a status notification.
func (c *testClient) status(t *testing.T, params map[string]any) {
	t.Helper()
	c.write(t, map[string]any{"method": "status", "params": params})
}

// expectStatus reads a user notification announcing userID's status, as
// `new`, or as `you` when userID is c's own.
func expectStatus(t *testing.T, c *testClient, userID, status string) map[string]any {
	t.Helper()
	params := c.notification(t, "user")
	field := "new"
	if userID == c.userID {
		field = "you"
	}
	object, _ := params[field].(map[string]any)
	if len(params) != 1 || object["user_id"] != userID || object["status"] != status {
		t.Fatalf("user notification = %#v, want %s %s %s", params, field, userID, status)
	}
	return object
}

// listedStatus is userID's status in the users of a room_list of general.
func listedStatus(t *testing.T, c *testClient, userID string) any {
	t.Helper()
	for _, user := range listRooms(t, c, map[string]any{"room_id": "general", "members": true})["users"].([]any) {
		if user := user.(map[string]any); user["user_id"] == userID {
			return user["status"]
		}
	}
	return nil
}

// serverClient is the server's side of a test client's only connection.
func serverClient(t *testing.T, app *Server, c *testClient) *client {
	t.Helper()
	app.mu.RLock()
	defer app.mu.RUnlock()
	for connection := range app.users[c.userID].clients {
		return connection
	}
	t.Fatalf("%s has no connection", c.userID)
	return nil
}

// A user's status (§4.11) is online while a connection is attended, idle
// while an idle connection or a push registration can notify them, dnd
// when muted, and offline otherwise or when invisible. Changes go to the
// status-aware connections of those who share a room, as `new`, and to
// the user's own, as `you`; every current user object carries it.
func TestStatusDerivationAndBroadcast(t *testing.T) {
	config := DefaultConfig()
	config.WebAuthn = testWebAuthn(t)
	config.AllowInsecurePush = true
	app, httpServer := newTestServer(t, config)
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	// a reports its status, so it is told of changes; b's own changes
	// echo to b once b has sent status too.
	a.result(t, "status", "aware", map[string]any{"idle": false})
	if got := listedStatus(t, a, b.userID); got != "online" {
		t.Fatalf("listed status: %v", got)
	}
	b.status(t, map[string]any{"idle": true})
	expectStatus(t, a, b.userID, "idle")
	expectStatus(t, b, b.userID, "idle")
	if got := listedStatus(t, a, b.userID); got != "idle" {
		t.Fatalf("listed status after idle: %v", got)
	}
	b.status(t, map[string]any{"idle": false})
	expectStatus(t, a, b.userID, "online")
	expectStatus(t, b, b.userID, "online")
	// Scoped idle: false attends a room, and the connection with it.
	b.status(t, map[string]any{"idle": true})
	expectStatus(t, a, b.userID, "idle")
	expectStatus(t, b, b.userID, "idle")
	b.status(t, map[string]any{"room_id": "general", "idle": false})
	expectStatus(t, a, b.userID, "online")
	expectStatus(t, b, b.userID, "online")

	// Invisible shows offline to others, until changed; the user still
	// sees what others see in `you`.
	b.status(t, map[string]any{"invisible": true})
	expectStatus(t, a, b.userID, "offline")
	expectStatus(t, b, b.userID, "offline")
	if got := listedStatus(t, a, b.userID); got != "offline" {
		t.Fatalf("listed status while invisible: %v", got)
	}
	b.status(t, map[string]any{"room_id": "general", "invisible": false})
	b.expectQuiet(t) // A room has no invisibility.
	b.status(t, map[string]any{"invisible": false})
	expectStatus(t, a, b.userID, "online")
	expectStatus(t, b, b.userID, "online")

	// A muted user who is here is online; muted and idle is dnd. The mute
	// is echoed to the user only.
	b.status(t, map[string]any{"mute": true})
	if you := expectStatus(t, b, b.userID, "online"); you["mute"] != true {
		t.Fatalf("mute echo: %#v", you)
	}
	if kept := b.result(t, "me", "me", map[string]any{})["you"].(map[string]any); kept["mute"] != true {
		t.Fatalf("you without the mute: %#v", kept)
	}
	b.status(t, map[string]any{"idle": true})
	expectStatus(t, a, b.userID, "dnd")
	expectStatus(t, b, b.userID, "dnd")
	listed := listRooms(t, a, map[string]any{"room_id": "general", "members": true})["users"].([]any)
	for _, user := range listed {
		if _, has := user.(map[string]any)["mute"]; has {
			t.Fatalf("mute shown to others: %#v", user)
		}
	}
	b.status(t, map[string]any{"mute": 0})
	if you := expectStatus(t, b, b.userID, "idle"); you["mute"] != float64(0) {
		t.Fatalf("unmute echo: %#v", you)
	}
	expectStatus(t, a, b.userID, "idle")

	// A message from an idle connection ends its idle.
	before, _ := b.request(t, "message", "back", map[string]any{"body": map[string]any{"text": "back"}})
	if !reflect.DeepEqual(methods(before), []string{"user", "message"}) || before[0]["params"].(map[string]any)["you"].(map[string]any)["status"] != "online" {
		t.Fatalf("frames before a message from an idle connection: %#v", before)
	}
	expectStatus(t, a, b.userID, "online")
	a.notification(t, "message")

	// An account without a connection is idle while a live push
	// registration can notify it, and offline otherwise.
	owner, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	registered := registerTestPasskey(t, owner, newTestAuthenticator(t))
	ownerID := registered["you"].(map[string]any)["user_id"].(string)
	expectMembership(t, a, "general", ownerID, true)
	if joined := expectStatus(t, a, ownerID, "online"); len(joined) != 2 {
		t.Fatalf("join status: %#v", joined)
	}
	expectMembership(t, b, "general", ownerID, true)
	expectStatus(t, b, ownerID, "online")
	a.expectQuiet(t)
	owner.result(t, "push_register", "push", map[string]any{"kind": "relay", "url": "http://relay.example/p"})
	_ = owner.ws.Close(websocket.StatusNormalClosure, "bye")
	expectStatus(t, a, ownerID, "idle")
	expectStatus(t, b, ownerID, "idle")
	app.mu.Lock()
	app.removePushLocked(app.users[ownerID].pushes["http://relay.example/p"])
	app.statusChangedLocked(app.users[ownerID], nil, false)
	app.unlock()
	expectStatus(t, a, ownerID, "offline")
	expectStatus(t, b, ownerID, "offline")
	if got := listedStatus(t, a, ownerID); got != "offline" {
		t.Fatalf("listed status of an account away: %v", got)
	}
	a.expectQuiet(t)
	b.expectQuiet(t)

	// A connection that never sent status is told of no changes, though
	// current objects still carry the status.
	c := dialTestClient(t, httpServer)
	expectMembership(t, a, "general", c.userID, true)
	expectStatus(t, a, c.userID, "online")
	expectMembership(t, b, "general", c.userID, true)
	expectStatus(t, b, c.userID, "online")
	b.status(t, map[string]any{"idle": true})
	expectStatus(t, a, b.userID, "idle")
	expectStatus(t, b, b.userID, "idle")
	c.expectQuiet(t)
	if got := listedStatus(t, c, b.userID); got != "idle" {
		t.Fatalf("listed status for a client that never sent status: %v", got)
	}
}

// status is accepted before authentication: the connection's idle applies
// at once, and the user's fields once it signs in (§4.11).
func TestStatusBeforeAuth(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	a := dialTestClient(t, httpServer)
	a.result(t, "status", "aware", map[string]any{"idle": false})
	c, _ := dialRaw(t, httpServer)
	c.status(t, map[string]any{"invisible": true})
	c.status(t, map[string]any{"mute": 60})
	c.status(t, map[string]any{"room_id": "general", "mute": true})
	c.status(t, map[string]any{"room_id": "missing", "mute": true})
	c.status(t, map[string]any{"idle": true})
	// With an id, status is answered, before authentication too.
	if result := c.result(t, "status", "early", map[string]any{"idle": true}); len(result) != 0 {
		t.Fatalf("status result: %#v", result)
	}
	c.expectError(t, "status", "bad", map[string]any{"mute": -1}, codeInvalidParams)
	you := guestAuth(t, c)["you"].(map[string]any)
	if you["status"] != "offline" || you["mute"] != float64(60) {
		t.Fatalf("you after auth: %#v", you)
	}
	// Invisible, the new member's join shows nothing to others.
	expectMembership(t, a, "general", c.userID, true)
	a.expectQuiet(t)
	c.expectQuiet(t)
	general := listRooms(t, c, map[string]any{"room_id": "general"})["joined"].([]any)[0].(map[string]any)
	if general["mute"] != true {
		t.Fatalf("room mute after auth: %#v", general)
	}
	if got := listedStatus(t, a, c.userID); got != "offline" {
		t.Fatalf("status of an invisible user: %v", got)
	}
	// After auth, a room the user cannot see is invalid_params, and so is
	// a malformed field; neither changes anything.
	for i, params := range []map[string]any{
		{"room_id": "missing", "mute": true},
		{"idle": "yes"},
		{"invisible": 1},
		{"mute": false},
		{"mute": 1.5},
		{"mute": "60"},
		{"room_id": 5, "idle": true},
	} {
		c.expectError(t, "status", "bad-"+formatID(int64(i)), params, codeInvalidParams)
	}
	c.expectQuiet(t)
	a.expectQuiet(t)
}

// A user's room mute is echoed in the room records they receive (§3.4,
// §4.11), and is theirs alone.
func TestRoomMuteEcho(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	ops, _ := saveRoom(t, a, "ops", map[string]any{"title": "Ops"})
	joinRoom(t, b, ops)
	expectMembership(t, a, ops, b.userID, true)
	b.status(t, map[string]any{"room_id": ops, "mute": 120})
	if record := roomUpdated(t, b, "updated"); record["room_id"] != ops || record["mute"] != float64(120) || record["title"] != "Ops" {
		t.Fatalf("room mute echo: %#v", record)
	}
	a.expectQuiet(t)
	listed := listRooms(t, b, map[string]any{"filter": "joined"})["joined"].([]any)
	for _, entry := range listed {
		entry := entry.(map[string]any)
		if mute, has := entry["mute"]; (entry["room_id"] == ops) != has || (has && mute != float64(120)) {
			t.Fatalf("listing: %#v", entry)
		}
	}
	// An edit of the room carries the mute to b only.
	saveRoom(t, a, "rename", map[string]any{"room_id": ops, "title": "Ops!"})
	if record := roomUpdated(t, b, "updated"); record["mute"] != float64(120) {
		t.Fatalf("edit to the muting user: %#v", record)
	}
	// Joining again sends the record with the mute too.
	if before, _ := b.request(t, "room_join", "again", map[string]any{"room_id": ops}); joinedRecord(t, before[0], b.userID)["mute"] != float64(120) {
		t.Fatalf("joined record: %#v", before)
	}
	b.status(t, map[string]any{"room_id": ops, "mute": 0})
	if record := roomUpdated(t, b, "updated"); record["mute"] != float64(0) {
		t.Fatalf("room unmute echo: %#v", record)
	}
	listed = listRooms(t, b, map[string]any{"room_id": ops})["joined"].([]any)
	if _, has := listed[0].(map[string]any)["mute"]; has {
		t.Fatalf("listing after unmute: %#v", listed)
	}
	a.expectQuiet(t)
}

// A timed mute ends by itself, announced as if the user had ended it.
func TestMuteExpires(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	a.result(t, "status", "aware", map[string]any{"idle": false})
	b.status(t, map[string]any{"idle": true})
	expectStatus(t, a, b.userID, "idle")
	expectStatus(t, b, b.userID, "idle")
	b.status(t, map[string]any{"mute": 1})
	expectStatus(t, a, b.userID, "dnd")
	if you := expectStatus(t, b, b.userID, "dnd"); you["mute"] != float64(1) {
		t.Fatalf("mute echo: %#v", you)
	}
	time.Sleep(1100 * time.Millisecond)
	expectStatus(t, a, b.userID, "idle")
	if you := expectStatus(t, b, b.userID, "idle"); you["mute"] != float64(0) {
		t.Fatalf("expiry echo: %#v", you)
	}
}

// Typing goes only to connections attending the room (§4.11); read
// cursors go to every connection.
func TestTypingGoesToAttendingConnections(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 4)
	typist, elsewhere, idle, plain := clients[0], clients[1], clients[2], clients[3]
	ops, _ := saveRoom(t, typist, "ops", map[string]any{"title": "Ops"})
	elsewhere.result(t, "status", "ops", map[string]any{"room_id": ops, "idle": false})
	if before, _ := idle.request(t, "status", "idle", map[string]any{"idle": true}); len(before) != 1 || before[0]["params"].(map[string]any)["you"].(map[string]any)["status"] != "idle" {
		t.Fatalf("frames before the status result: %#v", before)
	}
	// Only connections that sent status are told.
	expectStatus(t, elsewhere, idle.userID, "idle")
	id, _ := save(t, typist, "post", map[string]any{"body": map[string]any{"text": "hi"}})
	for _, c := range []*testClient{elsewhere, idle, plain} {
		c.notification(t, "message")
	}
	typist.write(t, map[string]any{"method": "activity", "params": map[string]any{"room_id": "general", "typing": 5}})
	for _, c := range []*testClient{typist, plain} {
		if params := c.notification(t, "activity"); params["typing"] != float64(5) {
			t.Fatalf("typing: %#v", params)
		}
	}
	elsewhere.expectQuiet(t)
	idle.expectQuiet(t)
	// A read cursor in the same frame still reaches every connection,
	// without the typing.
	typist.write(t, map[string]any{"method": "activity", "params": map[string]any{"room_id": "general", "typing": 0, "read_message_id": id}})
	for _, c := range clients {
		params := c.notification(t, "activity")
		_, typing := params["typing"]
		if params["read_message_id"] != id || typing != (c == typist || c == plain) {
			t.Fatalf("activity to %s: %#v", c.userID, params)
		}
	}
	// Attending general again, the connection gets typing there.
	elsewhere.result(t, "status", "general", map[string]any{"room_id": "general", "idle": false})
	typist.write(t, map[string]any{"method": "activity", "params": map[string]any{"room_id": "general", "typing": 5}})
	for _, c := range []*testClient{typist, elsewhere, plain} {
		c.notification(t, "activity")
	}
	// Scoped idle: true leaves a room without attending another.
	elsewhere.result(t, "status", "leave-general", map[string]any{"room_id": "general", "idle": true})
	typist.write(t, map[string]any{"method": "activity", "params": map[string]any{"room_id": "general", "typing": 5}})
	for _, c := range []*testClient{typist, plain} {
		c.notification(t, "activity")
	}
	elsewhere.expectQuiet(t)
	idle.expectQuiet(t)
}

// A connection that has sent no frame for the silence that closes a
// pinging client counts as idle until its next frame (§4.11).
func TestSilentConnectionsAreIdle(t *testing.T) {
	app, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	observer, quiet := clients[0], clients[1]
	observer.result(t, "status", "aware", map[string]any{"idle": false})
	app.silenceIdle(serverClient(t, app, quiet))
	expectStatus(t, observer, quiet.userID, "idle")
	// Any frame ends the silence, such as a notification nobody answers.
	quiet.write(t, map[string]any{"method": "frobnicate"})
	expectStatus(t, observer, quiet.userID, "online")
	quiet.expectQuiet(t)

	// pingLoop measures the silence.
	config := DefaultConfig()
	config.PingInterval = 20 * time.Millisecond
	config.PingTimeout = 200 * time.Millisecond
	app, httpServer = newTestServer(t, config)
	silent := dialTestClient(t, httpServer)
	connection := serverClient(t, app, silent)
	// Reading answers the server's WebSocket pings.
	ctx := silent.ws.CloseRead(context.Background())
	deadline := time.Now().Add(2 * time.Second)
	for !connection.silent.Load() && time.Now().Before(deadline) && ctx.Err() == nil {
		time.Sleep(10 * time.Millisecond)
	}
	app.mu.RLock()
	status := app.users[silent.userID].statusAt(time.Now())
	app.mu.RUnlock()
	if !connection.silent.Load() || status != statusIdle {
		t.Fatalf("a silent connection is not idle: silent=%v status=%s", connection.silent.Load(), status)
	}
	if !reflect.DeepEqual(ctx.Err(), nil) {
		t.Fatalf("a silent connection that never pinged was closed: %v", ctx.Err())
	}
}
