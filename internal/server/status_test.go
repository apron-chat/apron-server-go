package server

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// status sends a status notification and returns the frames it caused on
// c, after the server has processed it.
func (c *testClient) status(t *testing.T, params map[string]any) []map[string]any {
	t.Helper()
	c.write(t, map[string]any{"method": "status", "params": params})
	return c.drain(t)
}

// expectStatus reads a user notification announcing userID's status, as
// `new`, or as `you` when userID is c's own, and returns the object.
func expectStatus(t *testing.T, c *testClient, userID, status string) map[string]any {
	t.Helper()
	return checkStatus(t, c.read(t), c, userID, status)
}

func checkStatus(t *testing.T, frame map[string]any, c *testClient, userID, status string) map[string]any {
	t.Helper()
	params := notificationParams(t, frame, "user")
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

// echoed checks that a status update caused exactly one `you` echo on its
// connection, and returns it.
func echoed(t *testing.T, c *testClient, frames []map[string]any, status string) map[string]any {
	t.Helper()
	if len(frames) != 1 {
		t.Fatalf("frames after status: %#v", frames)
	}
	return checkStatus(t, frames[0], c, c.userID, status)
}

// snapshot splits the frames after a connection's first idle into the
// statuses of the connected users it shares a room with, as `new`, by
// user_id, which the server sends it then (§4.11), and the rest.
func snapshot(t *testing.T, c *testClient, frames []map[string]any) (map[string]string, []map[string]any) {
	t.Helper()
	statuses := map[string]string{}
	var rest []map[string]any
	for _, frame := range frames {
		params, _ := frame["params"].(map[string]any)
		if object, ok := params["new"].(map[string]any); ok && frame["method"] == "user" && len(params) == 1 && len(object) == 2 && object["user_id"] != c.userID {
			if _, dup := statuses[object["user_id"].(string)]; dup {
				t.Fatalf("status of %v sent twice: %#v", object["user_id"], frames)
			}
			statuses[object["user_id"].(string)] = object["status"].(string)
			continue
		}
		rest = append(rest, frame)
	}
	return statuses, rest
}

// firstIdle sends a connection's first idle and returns the statuses it is
// sent of others and the other frames.
func firstIdle(t *testing.T, c *testClient, idle bool) (map[string]string, []map[string]any) {
	t.Helper()
	return snapshot(t, c, c.status(t, map[string]any{"idle": idle}))
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

// A user's status (§4.11) is, in order: offline when invisible, dnd while
// the unscoped mute is set and the user is connected, online while a
// connection is attended, idle while an idle connection or, unmuted, a push
// registration can notify them, and offline otherwise. Changes go to the status-aware connections of those
// who share a room, as `new`, and to the user's own, as `you`; every
// current user object carries it.
func TestStatusDerivationAndBroadcast(t *testing.T) {
	config := DefaultConfig()
	config.WebAuthn = testWebAuthn(t)
	config.AllowInsecurePush = true
	app, httpServer := newTestServer(t, config)
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	// a reports idle, so it is told of changes, and is sent b's status,
	// but its own did not change.
	if statuses, frames := firstIdle(t, a, false); len(frames) != 0 || !reflect.DeepEqual(statuses, map[string]string{b.userID: "online"}) {
		t.Fatalf("frames after a first, unchanged idle: %#v %#v", statuses, frames)
	}
	// Only the first idle sends the statuses of others.
	if frames := a.status(t, map[string]any{"idle": false}); len(frames) != 0 {
		t.Fatalf("frames after an unchanged idle: %#v", frames)
	}
	if got := listedStatus(t, a, b.userID); got != "online" {
		t.Fatalf("listed status: %v", got)
	}
	statuses, frames := firstIdle(t, b, true)
	echoed(t, b, frames, "idle")
	if !reflect.DeepEqual(statuses, map[string]string{a.userID: "online"}) {
		t.Fatalf("statuses after b's first idle: %#v", statuses)
	}
	expectStatus(t, a, b.userID, "idle")
	if got := listedStatus(t, a, b.userID); got != "idle" {
		t.Fatalf("listed status after idle: %v", got)
	}
	echoed(t, b, b.status(t, map[string]any{"idle": false}), "online")
	expectStatus(t, a, b.userID, "online")
	// idle is unscoped: a scoped idle, or invisible, is ignored.
	if frames := b.status(t, map[string]any{"room_id": "general", "idle": true, "invisible": true}); len(frames) != 0 {
		t.Fatalf("frames after a scoped idle: %#v", frames)
	}
	a.expectQuiet(t)

	// Invisible shows offline to others; the user's own status ignores it,
	// and `you` echoes invisible.
	you := echoed(t, b, b.status(t, map[string]any{"invisible": true}), "online")
	if you["invisible"] != true || you["mute"] != float64(0) {
		t.Fatalf("invisible echo: %#v", you)
	}
	expectStatus(t, a, b.userID, "offline")
	if got := listedStatus(t, a, b.userID); got != "offline" {
		t.Fatalf("listed status while invisible: %v", got)
	}
	if got := b.result(t, "me", "me-invisible", map[string]any{})["you"].(map[string]any); got["status"] != "online" || got["invisible"] != true {
		t.Fatalf("you while invisible: %#v", got)
	}
	if you := echoed(t, b, b.status(t, map[string]any{"invisible": false}), "online"); you["invisible"] != false {
		t.Fatalf("visible echo: %#v", you)
	}
	expectStatus(t, a, b.userID, "online")

	// The unscoped mute is dnd while connected, attended or not, and is
	// echoed to the user only.
	if you := echoed(t, b, b.status(t, map[string]any{"mute": true}), "dnd"); you["mute"] != true {
		t.Fatalf("mute echo: %#v", you)
	}
	expectStatus(t, a, b.userID, "dnd")
	if kept := b.result(t, "me", "me", map[string]any{})["you"].(map[string]any); kept["mute"] != true {
		t.Fatalf("you without the mute: %#v", kept)
	}
	for _, user := range listRooms(t, a, map[string]any{"room_id": "general", "members": true})["users"].([]any) {
		if _, has := user.(map[string]any)["mute"]; has {
			t.Fatalf("mute shown to others: %#v", user)
		}
	}
	// A room's mute is not dnd.
	frames = b.status(t, map[string]any{"mute": 0})
	if len(frames) != 1 || checkStatus(t, frames[0], b, b.userID, "online")["mute"] != float64(0) {
		t.Fatalf("unmute echo: %#v", frames)
	}
	expectStatus(t, a, b.userID, "online")
	if frames := b.status(t, map[string]any{"room_id": "general", "mute": true}); len(frames) != 1 || frames[0]["method"] != "room_update" {
		t.Fatalf("frames after a room mute: %#v", frames)
	}
	a.expectQuiet(t)

	// Only idle: false ends idle; a message from the connection does not.
	echoed(t, b, b.status(t, map[string]any{"idle": true}), "idle")
	expectStatus(t, a, b.userID, "idle")
	save(t, b, "still-idle", map[string]any{"body": map[string]any{"text": "still idle"}})
	a.notification(t, "message")
	a.expectQuiet(t)
	echoed(t, b, b.status(t, map[string]any{"idle": false}), "online")
	expectStatus(t, a, b.userID, "online")

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
	// A registration that wakes only for badge cannot notify; one that
	// wakes for messages can.
	owner.result(t, "push_register", "badge", map[string]any{"kind": "relay", "url": "http://relay.example/badge", "wake": []any{"badge"}})
	app.mu.RLock()
	badgeOnly := app.users[ownerID].notifiable(time.Now())
	app.mu.RUnlock()
	if badgeOnly {
		t.Fatal("a badge-only registration makes the user notifiable")
	}
	owner.result(t, "push_unregister", "unregister-badge", map[string]any{"url": "http://relay.example/badge"})
	owner.result(t, "push_register", "push", map[string]any{"kind": "relay", "url": "http://relay.example/p"})
	_ = owner.ws.Close(websocket.StatusNormalClosure, "bye")
	expectStatus(t, a, ownerID, "idle")
	expectStatus(t, b, ownerID, "idle")
	// An expired registration no longer counts, and the sweep says so.
	app.mu.Lock()
	app.users[ownerID].pushes["http://relay.example/p"].renewed = time.Now().Add(-pushExpiry)
	app.expirePushesLocked(app.users[ownerID], time.Now())
	app.unlock()
	expectStatus(t, a, ownerID, "offline")
	expectStatus(t, b, ownerID, "offline")
	if got := listedStatus(t, a, ownerID); got != "offline" {
		t.Fatalf("listed status of an account away: %v", got)
	}

	// A connection that never sent status is told of no changes, though
	// current objects still carry the status.
	c := dialTestClient(t, httpServer)
	expectMembership(t, a, "general", c.userID, true)
	expectStatus(t, a, c.userID, "online")
	expectMembership(t, b, "general", c.userID, true)
	expectStatus(t, b, c.userID, "online")
	echoed(t, b, b.status(t, map[string]any{"idle": true}), "idle")
	expectStatus(t, a, b.userID, "idle")
	c.expectQuiet(t)
	if got := listedStatus(t, c, b.userID); got != "idle" {
		t.Fatalf("listed status for a client that never sent status: %v", got)
	}
}

// A flapping user's changes reach others at once for a few, then coalesced
// to the latest at most every statusCoalesce.
func TestStatusChangesCoalesce(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	a.status(t, map[string]any{"idle": false})
	// b's first status, online when it signed in, took one of the burst.
	for i := range statusBurst - 1 {
		idle := i%2 == 0
		b.status(t, map[string]any{"idle": idle})
		expectStatus(t, a, b.userID, map[bool]string{true: "idle", false: "online"}[idle])
	}
	// Past the burst, changes wait, and only the latest is sent.
	for _, idle := range []bool{false, true, false} {
		b.status(t, map[string]any{"idle": idle})
	}
	a.expectQuiet(t)
	time.Sleep(statusCoalesce + 200*time.Millisecond)
	expectStatus(t, a, b.userID, "online")
	a.expectQuiet(t)
}

// status is accepted before authentication: the connection's idle applies
// at once, and the user's fields once it signs in (§4.11). It is never
// answered, even with an id (§1).
func TestStatusBeforeAuth(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	a := dialTestClient(t, httpServer)
	a.status(t, map[string]any{"idle": false})
	c, _ := dialRaw(t, httpServer)
	c.write(t, map[string]any{"method": "status", "params": map[string]any{"invisible": true}})
	c.write(t, map[string]any{"method": "status", "params": map[string]any{"mute": 60}})
	c.write(t, map[string]any{"method": "status", "params": map[string]any{"room_id": "general", "mute": true}})
	c.write(t, map[string]any{"method": "status", "params": map[string]any{"room_id": "missing", "mute": true}})
	c.write(t, map[string]any{"method": "status", "id": "early", "params": map[string]any{"idle": true}})
	c.write(t, map[string]any{"method": "status", "id": "bad", "params": map[string]any{"mute": -1}})
	// Having sent idle, it is sent the status of the connected users of the
	// room it joins, after the membership (§4.11).
	before, result := c.request(t, "auth", "auth", map[string]any{"scheme": "guest"})
	c.userID = result["you"].(map[string]any)["user_id"].(string)
	statuses, before := snapshot(t, c, before)
	if len(before) != 1 || !reflect.DeepEqual(statuses, map[string]string{a.userID: "online"}) {
		t.Fatalf("frames before the guest auth result: %#v %#v", statuses, before)
	}
	checkMembership(t, membershipOnly(t, before[0]), "general", c.userID, true)
	you := result["you"].(map[string]any)
	if you["status"] != "dnd" || you["mute"] != float64(60) || you["invisible"] != true {
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
	// After auth, a room the user cannot see, or a room_id that is not a
	// string, ignores the update; a malformed field is ignored on its own.
	for _, params := range []map[string]any{
		{"room_id": "missing", "mute": true},
		{"room_id": 5, "mute": 5},
		{"idle": "yes"},
	} {
		if frames := c.status(t, params); len(frames) != 0 {
			t.Fatalf("frames after %#v: %#v", params, frames)
		}
	}
	// An unscoped mute or invisible, even malformed, echoes the values that
	// result to the sender alone (§4.11).
	for _, params := range []map[string]any{
		{"mute": false},
		{"mute": 1.5},
		{"mute": "60", "idle": "yes", "invisible": 1},
	} {
		if you := echoed(t, c, c.status(t, params), "dnd"); you["mute"] != float64(60) || you["invisible"] != true {
			t.Fatalf("echo after %#v: %#v", params, you)
		}
	}
	a.expectQuiet(t)
	if you := echoed(t, c, c.status(t, map[string]any{"idle": "yes", "invisible": false, "mute": 1.5}), "dnd"); you["invisible"] != false {
		t.Fatalf("echo of the valid field: %#v", you)
	}
	expectStatus(t, a, c.userID, "dnd")
	a.expectQuiet(t)

	// Unset, invisible and mute are absent from the auth result's you.
	d, _ := dialRaw(t, httpServer)
	you = guestAuth(t, d)["you"].(map[string]any)
	if _, has := you["mute"]; has || you["invisible"] != nil || you["status"] != "online" {
		t.Fatalf("you of a user who set nothing: %#v", you)
	}
}

// A user's room mute is a delivery field of the room records they receive
// (§3.4, §4.11), and is theirs alone.
func TestRoomMuteEcho(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	ops, _ := saveRoom(t, a, "ops", map[string]any{"title": "Ops"})
	joinRoom(t, b, ops)
	expectMembership(t, a, ops, b.userID, true)
	frames := b.status(t, map[string]any{"room_id": ops, "mute": 120})
	if len(frames) != 1 {
		t.Fatalf("frames after a room mute: %#v", frames)
	}
	if record := updateRecord(t, frames[0], "updated"); record["room_id"] != ops || record["mute"] != float64(120) || record["title"] != "Ops" {
		t.Fatalf("room mute echo: %#v", record)
	}
	a.expectQuiet(t)
	for _, entry := range listRooms(t, b, map[string]any{"filter": "joined"})["joined"].([]any) {
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
	// Scoped mute 0 removes the room's own mute, echoed as 0.
	frames = b.status(t, map[string]any{"room_id": ops, "mute": 0})
	if len(frames) != 1 || updateRecord(t, frames[0], "updated")["mute"] != float64(0) {
		t.Fatalf("room unmute echo: %#v", frames)
	}
	if listed := listRooms(t, b, map[string]any{"room_id": ops})["joined"].([]any); listed[0].(map[string]any)["mute"] != nil {
		t.Fatalf("listing after unmute: %#v", listed)
	}
	a.expectQuiet(t)

	// A visible room the user has not joined may be muted: nothing is
	// echoed until they join it, and its records carry the mute after.
	news, _ := saveRoom(t, a, "news", map[string]any{"title": "News"})
	if frames := b.status(t, map[string]any{"room_id": news, "mute": true}); len(frames) != 0 {
		t.Fatalf("frames after muting an unjoined room: %#v", frames)
	}
	if listed := listRooms(t, b, map[string]any{"room_id": news})["not_joined"].([]any); listed[0].(map[string]any)["mute"] != nil {
		t.Fatalf("unjoined listing: %#v", listed)
	}
	if record := joinRoom(t, b, news); record["mute"] != true {
		t.Fatalf("joined record of a muted room: %#v", record)
	}
	expectMembership(t, a, news, b.userID, true)
	// Others never see it: a's records of the room do not carry it.
	saveRoom(t, a, "rename-news", map[string]any{"room_id": news, "title": "News!"})
	if record := roomUpdated(t, b, "updated"); record["mute"] != true {
		t.Fatalf("edit to the muting user: %#v", record)
	}
	if listed := listRooms(t, a, map[string]any{"room_id": news})["joined"].([]any); listed[0].(map[string]any)["mute"] != nil {
		t.Fatalf("another member's listing: %#v", listed)
	}
	page := historyPage(t, b, news, map[string]any{})
	for _, record := range records(t, page, "rooms") {
		if _, has := record.(map[string]any)["mute"]; has {
			t.Fatalf("history room record with a mute: %#v", record)
		}
	}
}

// A change to mute or invisible is echoed to every connection of the user,
// status-aware or not (§4.11).
func TestMuteEchoReachesEveryConnection(t *testing.T) {
	config := DefaultConfig()
	config.WebAuthn = testWebAuthn(t)
	_, httpServer := newTestServer(t, config)
	first, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	registered := registerTestPasskey(t, first, newTestAuthenticator(t))
	second, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	second.result(t, "auth", "resume", map[string]any{"scheme": "token", "token": registered["token"]})
	second.userID = first.userID
	first.drain(t)
	first.status(t, map[string]any{"mute": 60})
	if you := expectStatus(t, second, second.userID, "dnd"); you["mute"] != float64(60) || you["invisible"] != false {
		t.Fatalf("echo to a connection that never sent status: %#v", you)
	}
	// A status that changes nothing still echoes the values to its sender,
	// and to no other connection (§4.11).
	if you := echoed(t, second, second.status(t, map[string]any{"invisible": false}), "dnd"); you["mute"] != float64(60) || you["invisible"] != false {
		t.Fatalf("echo of an unchanged mute: %#v", you)
	}
	first.expectQuiet(t)
	// A mute past a year is shortened to one, and the echo says so.
	first.write(t, map[string]any{"method": "status", "params": map[string]any{"mute": 10 * maxMuteSeconds}})
	for _, c := range []*testClient{first, second} {
		if you := expectStatus(t, c, c.userID, "dnd"); you["mute"] != float64(maxMuteSeconds) {
			t.Fatalf("echo of a mute past a year: %#v", you)
		}
	}
}

// dnd needs a connection (§4.11): a muted user is offline once their last
// connection closes, push registration or not, and dnd again when they
// reconnect. Unmuted, a push registration makes them idle instead.
func TestMutedUserWithoutConnectionIsOffline(t *testing.T) {
	config := DefaultConfig()
	config.WebAuthn = testWebAuthn(t)
	config.AllowInsecurePush = true
	app, httpServer := newTestServer(t, config)
	a := dialTestClient(t, httpServer)
	a.status(t, map[string]any{"idle": false})
	owner, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	registered := registerTestPasskey(t, owner, newTestAuthenticator(t))
	ownerID := registered["you"].(map[string]any)["user_id"].(string)
	expectMembership(t, a, "general", ownerID, true)
	expectStatus(t, a, ownerID, "online")
	resume := func(want string) *testClient {
		t.Helper()
		c, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
		you := c.result(t, "auth", "resume", map[string]any{"scheme": "token", "token": registered["token"]})["you"].(map[string]any)
		if you["status"] != want {
			t.Fatalf("you on resume: %#v, want %s", you, want)
		}
		c.userID = ownerID
		c.drain(t)
		expectStatus(t, a, ownerID, want)
		return c
	}
	closeLast := func(c *testClient, want string) {
		t.Helper()
		_ = c.ws.Close(websocket.StatusNormalClosure, "bye")
		expectStatus(t, a, ownerID, want)
		if got := listedStatus(t, a, ownerID); got != want {
			t.Fatalf("listed status after the last connection closed: %v, want %s", got, want)
		}
		app.mu.RLock()
		own := app.users[ownerID].ownStatus
		app.mu.RUnlock()
		if own != want {
			t.Fatalf("own status after the last connection closed: %s, want %s", own, want)
		}
	}

	// Muted and connected is dnd; muted and disconnected is offline.
	echoed(t, owner, owner.status(t, map[string]any{"mute": true}), "dnd")
	expectStatus(t, a, ownerID, "dnd")
	closeLast(owner, "offline")
	owner = resume("dnd")

	// A push registration does not make a muted user idle.
	owner.result(t, "push_register", "push", map[string]any{"kind": "relay", "url": "http://relay.example/p"})
	closeLast(owner, "offline")
	owner = resume("dnd")

	// Unmuted, the registration makes a disconnected user idle.
	echoed(t, owner, owner.status(t, map[string]any{"mute": 0}), "online")
	expectStatus(t, a, ownerID, "online")
	closeLast(owner, "idle")
	a.expectQuiet(t)
}

// Invisible shows offline to others even while muted and connected; the
// user's own status ignores it (§4.11).
func TestInvisibleOutranksDND(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	a.status(t, map[string]any{"idle": false})
	echoed(t, b, b.status(t, map[string]any{"mute": true, "invisible": true}), "dnd")
	expectStatus(t, a, b.userID, "offline")
	if got := listedStatus(t, a, b.userID); got != "offline" {
		t.Fatalf("listed status while invisible and muted: %v", got)
	}
}

// A timed mute ends by itself. Clients count it down, so the mute is not
// echoed, but the status that changes with it is announced.
func TestMuteExpires(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	a.status(t, map[string]any{"idle": false})
	b.status(t, map[string]any{"idle": false})
	if you := echoed(t, b, b.status(t, map[string]any{"mute": 1}), "dnd"); you["mute"] != float64(1) {
		t.Fatalf("mute echo: %#v", you)
	}
	expectStatus(t, a, b.userID, "dnd")
	time.Sleep(1100 * time.Millisecond)
	expectStatus(t, a, b.userID, "online")
	if you := expectStatus(t, b, b.userID, "online"); len(you) != 2 {
		t.Fatalf("expiry echo: %#v", you)
	}
}

// Typing is held back from idle connections (§4.11); read cursors go to
// every connection.
func TestTypingSkipsIdleConnections(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 3)
	typist, idle, plain := clients[0], clients[1], clients[2]
	_, frames := firstIdle(t, idle, true)
	echoed(t, idle, frames, "idle")
	id, _ := save(t, typist, "post", map[string]any{"body": map[string]any{"text": "hi"}})
	for _, c := range []*testClient{idle, plain} {
		c.notification(t, "message")
	}
	typist.write(t, map[string]any{"method": "activity", "params": map[string]any{"room_id": "general", "typing": 5}})
	for _, c := range []*testClient{typist, plain} {
		if params := c.notification(t, "activity"); params["typing"] != float64(5) {
			t.Fatalf("typing: %#v", params)
		}
	}
	idle.expectQuiet(t)
	// A read cursor in the same frame still reaches every connection,
	// without the typing.
	typist.write(t, map[string]any{"method": "activity", "params": map[string]any{"room_id": "general", "typing": 0, "read_message_id": id}})
	for _, c := range clients {
		params := c.notification(t, "activity")
		_, typing := params["typing"]
		if params["read_message_id"] != id || typing != (c != idle) {
			t.Fatalf("activity to %s: %#v", c.userID, params)
		}
	}
}

// A connection that never sent status counts as idle after sending nothing
// but liveness pings for Config.SilentIdleAfter, until its next other frame
// (§4.11).
func TestSilentConnectionsAreIdle(t *testing.T) {
	app, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	observer, quiet := clients[0], clients[1]
	observer.status(t, map[string]any{"idle": false})
	app.silenceIdle(serverClient(t, app, quiet))
	expectStatus(t, observer, quiet.userID, "idle")
	// A liveness ping does not end it; any other frame does, such as a
	// notification nobody answers.
	quiet.write(t, map[string]any{"method": "ping"})
	if pong := quiet.read(t); pong["method"] != "pong" {
		t.Fatalf("ping answered with %#v", pong)
	}
	observer.expectQuiet(t)
	quiet.write(t, map[string]any{"method": "frobnicate"})
	expectStatus(t, observer, quiet.userID, "online")
	quiet.expectQuiet(t)
	// A connection that sent idle is never idle by silence; one that sent
	// status without idle still is.
	app.silenceIdle(serverClient(t, app, observer))
	if serverClient(t, app, observer).silent.Load() {
		t.Fatal("a connection that sent idle went idle by silence")
	}
	quiet.status(t, map[string]any{"invisible": false})
	app.silenceIdle(serverClient(t, app, quiet))
	expectStatus(t, observer, quiet.userID, "idle")

	// pingLoop measures the silence.
	config := DefaultConfig()
	config.PingInterval = 20 * time.Millisecond
	config.PingTimeout = 200 * time.Millisecond
	config.SilentIdleAfter = 100 * time.Millisecond
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
	if !connection.silent.Load() || status != statusIdle || ctx.Err() != nil {
		t.Fatalf("a silent connection: silent=%v status=%s closed=%v", connection.silent.Load(), status, ctx.Err())
	}
}

// A user changes mute or invisible userStatusBurst times at once; further
// changes are dropped until the budget refills.
func TestMuteChangesAreLimited(t *testing.T) {
	app, httpServer := newTestServer(t, DefaultConfig())
	c := dialTestClient(t, httpServer)
	for i := range userStatusBurst {
		frames := c.status(t, map[string]any{"mute": i + 1})
		if len(frames) != 1 {
			t.Fatalf("change %d: %#v", i, frames)
		}
	}
	// A change past the limit is dropped, and the sender told the values
	// that stand (§4.11).
	if you := echoed(t, c, c.status(t, map[string]any{"mute": 0, "invisible": true}), "dnd"); you["mute"] != float64(userStatusBurst) || you["invisible"] != false {
		t.Fatalf("echo of a change past the limit: %#v", you)
	}
	if you := c.result(t, "me", "me", map[string]any{})["you"].(map[string]any); you["mute"] != float64(userStatusBurst) || you["invisible"] != nil {
		t.Fatalf("you after the limit: %#v", you)
	}
	// idle is not limited (the muted user stays dnd, so nothing is echoed).
	c.status(t, map[string]any{"idle": true})
	app.mu.RLock()
	idle := serverClientLocked(app, c).idle
	app.mu.RUnlock()
	if !idle {
		t.Fatal("idle past the limit was dropped")
	}
}

// serverClientLocked is the server's side of c's only connection; the
// caller holds app.mu.
func serverClientLocked(app *Server, c *testClient) *client {
	for connection := range app.users[c.userID].clients {
		return connection
	}
	return nil
}

// Only connections that have sent idle are told of status changes, and a
// connection's first idle sends it the status of each user with a
// connection who shares a room with it, once (§4.11).
func TestStatusSnapshotOnFirstIdle(t *testing.T) {
	app, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 3)
	a, b, c := clients[0], clients[1], clients[2]
	addAccount(t, app, "alice", a, b, c)
	echoed(t, c, c.status(t, map[string]any{"invisible": true}), "online")
	// A status without a valid idle does not make a connection
	// status-aware.
	if frames := a.status(t, map[string]any{"idle": "no", "room_id": "general", "mute": 0}); len(frames) != 1 || frames[0]["method"] != "room_update" {
		t.Fatalf("frames after a status without idle: %#v", frames)
	}
	statuses, frames := firstIdle(t, b, true)
	echoed(t, b, frames, "idle")
	if !reflect.DeepEqual(statuses, map[string]string{a.userID: "online", c.userID: "offline"}) {
		t.Fatalf("statuses after b's first idle: %#v", statuses)
	}
	a.expectQuiet(t)
	// a's first idle: alice has no connection, so no status to send.
	statuses, frames = firstIdle(t, a, false)
	if len(frames) != 0 || !reflect.DeepEqual(statuses, map[string]string{b.userID: "idle", c.userID: "offline"}) {
		t.Fatalf("statuses after a's first idle: %#v %#v", statuses, frames)
	}
	echoed(t, b, b.status(t, map[string]any{"idle": false}), "online")
	expectStatus(t, a, b.userID, "online")
	a.expectQuiet(t)
}
