package server

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/time/rate"

	"github.com/apron-chat/apron-server-go/internal/store"
)

// status sends a status request, whose result must be {}, and returns the
// notifications that precede the result on c. Its request IDs are unique
// across connections, which share a user's deduplication (§1.2).
func (c *testClient) status(t *testing.T, params map[string]any) []map[string]any {
	t.Helper()
	before, result := c.request(t, "status", statusID(), params)
	if len(result) != 0 {
		t.Fatalf("status result = %#v, want {}", result)
	}
	return before
}

// statusError sends a status request that must fail with code, and
// returns the error.
func (c *testClient) statusError(t *testing.T, params map[string]any, code int) map[string]any {
	t.Helper()
	id := statusID()
	frame := c.call(t, "status", id, params)
	failure, ok := frame["error"].(map[string]any)
	if !ok || failure["code"] != float64(code) {
		t.Fatalf("status %#v: %#v, want error %d", params, frame, code)
	}
	return failure
}

// expectEcho sends a status request from c that changes a mute, and checks
// that the change reaches c, before the result, and each of others.
func expectEcho(t *testing.T, c *testClient, params map[string]any, roomID string, mute any, others ...*testClient) {
	t.Helper()
	frames := c.status(t, params)
	if len(frames) != 1 {
		t.Fatalf("frames before the result of %#v: %#v", params, frames)
	}
	checkMute(t, frames[0], roomID, mute)
	for _, other := range others {
		expectMute(t, other, roomID, mute)
	}
}

func statusID() string {
	return fmt.Sprintf("status-%d", statusRequests.Add(1))
}

// watching has each client keep the status announcements of others, which
// read otherwise skips, from now on.
func watching(clients ...*testClient) {
	for _, c := range clients {
		c.statuses = true
	}
}

// expectStatus reads a bare announcement of userID's status, `user`
// `{new: {user_id, status}}`.
func expectStatus(t *testing.T, c *testClient, userID, status string) {
	t.Helper()
	checkStatus(t, c.read(t), userID, status)
}

func checkStatus(t *testing.T, frame map[string]any, userID, status string) {
	t.Helper()
	params := notificationParams(t, frame, "user")
	if want := map[string]any{"new": map[string]any{"user_id": userID, "status": status}}; !reflect.DeepEqual(params, want) {
		t.Fatalf("user notification = %#v, want %#v", params, want)
	}
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

// setStatus sets c's status with `me` and returns the status in `you`. Its
// request IDs are unique across connections, which share a user's
// deduplication (§1.2).
func setStatus(t *testing.T, c *testClient, status any) any {
	t.Helper()
	return c.result(t, "me", statusID(), map[string]any{"status": status})["you"].(map[string]any)["status"]
}

var statusRequests atomic.Int64

// expectProfileStatus reads the `user` `new` a profile change sends others,
// and checks the status it carries.
func expectProfileStatus(t *testing.T, c *testClient, userID, status string) {
	t.Helper()
	object, _ := notificationParams(t, c.read(t), "user")["new"].(map[string]any)
	if object["user_id"] != userID || object["status"] != status {
		t.Fatalf("profile change = %#v, want %s %q", object, userID, status)
	}
}

// Each status a user sets (§4.11), as others see it and as the user's own
// `you` shows it: online derives online, idle, or offline from the user's
// connections; "" is none; dnd is dnd while connected and offline
// otherwise; invisible is offline to others. An
// unsupported value is stored as "". Every current object carries the
// status, and a change reaches those who share a room as `user` `new`.
func TestStatusValuesAndDerivation(t *testing.T) {
	config := DefaultConfig()
	config.WebAuthn = testWebAuthn(t)
	_, httpServer := newTestServer(t, config)
	a := dialTestClient(t, httpServer)
	b, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	registered := registerTestPasskey(t, b, newTestAuthenticator(t))
	expectMembership(t, a, "general", b.userID, true)
	watching(a, b)
	b.drain(t)
	a.drain(t)
	if you := registered["you"].(map[string]any); you["status"] != "online" {
		t.Fatalf("you of a new account: %#v", you)
	}
	if got := listedStatus(t, a, b.userID); got != "online" {
		t.Fatalf("listed status: %v", got)
	}

	// online: idle once no connection is attended, online again when one
	// is, offline without connections. The user's own connections are not
	// told: their `you` shows the status they set.
	if frames := b.status(t, map[string]any{"idle": true}); len(frames) != 0 {
		t.Fatalf("frames after idle: %#v", frames)
	}
	expectStatus(t, a, b.userID, "idle")
	if got := listedStatus(t, a, b.userID); got != "idle" {
		t.Fatalf("listed status while idle: %v", got)
	}
	// An attended second connection makes the user online; idle again when
	// it goes.
	second, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	second.result(t, "auth", "resume", map[string]any{"scheme": "token", "token": registered["token"]})
	expectStatus(t, a, b.userID, "online")
	second.status(t, map[string]any{"idle": true})
	expectStatus(t, a, b.userID, "idle")
	b.status(t, map[string]any{"idle": false})
	expectStatus(t, a, b.userID, "online")
	if you := b.result(t, "me", "me", map[string]any{})["you"].(map[string]any); you["status"] != "online" {
		t.Fatalf("you while online: %#v", you)
	}
	// A scoped idle is still the connection's: room_id scopes only mute.
	b.status(t, map[string]any{"room_id": "general", "idle": true})
	expectStatus(t, a, b.userID, "idle")
	_ = second.ws.Close(websocket.StatusNormalClosure, "bye")
	_ = b.ws.Close(websocket.StatusNormalClosure, "bye")
	expectStatus(t, a, b.userID, "offline")
	if got := listedStatus(t, a, b.userID); got != "offline" {
		t.Fatalf("listed status without connections: %v", got)
	}
	a.expectQuiet(t)

	b, _ = dialOrigin(t, httpServer, testPasskeyOrigin)
	b.result(t, "auth", "resume", map[string]any{"scheme": "token", "token": registered["token"]})
	b.userID = registered["you"].(map[string]any)["user_id"].(string)
	expectStatus(t, a, b.userID, "online")
	b.drain(t)

	// dnd shows dnd, connected and attended or not.
	if got := setStatus(t, b, "dnd"); got != "dnd" {
		t.Fatalf("you after dnd: %v", got)
	}
	expectProfileStatus(t, a, b.userID, "dnd")
	b.status(t, map[string]any{"idle": true})
	b.status(t, map[string]any{"idle": false})
	if got := listedStatus(t, a, b.userID); got != "dnd" {
		t.Fatalf("listed status while dnd: %v", got)
	}
	a.expectQuiet(t)

	// invisible shows offline to others; `you` shows invisible.
	if got := setStatus(t, b, "invisible"); got != "invisible" {
		t.Fatalf("you after invisible: %v", got)
	}
	expectProfileStatus(t, a, b.userID, "offline")
	if got := listedStatus(t, a, b.userID); got != "offline" {
		t.Fatalf("listed status while invisible: %v", got)
	}

	// "" is no status.
	if got := setStatus(t, b, ""); got != "" {
		t.Fatalf("you after none: %v", got)
	}
	expectProfileStatus(t, a, b.userID, "")
	b.status(t, map[string]any{"idle": true})
	if got := listedStatus(t, a, b.userID); got != "" {
		t.Fatalf("listed status after none: %v", got)
	}
	a.expectQuiet(t)

	// A value the server does not support, derived ones included, is
	// stored as "": unchanged here, so nobody is told.
	for _, value := range []string{"away", "idle", "offline", "Online"} {
		if got := setStatus(t, b, value); got != "" {
			t.Fatalf("you after %q: %v", value, got)
		}
	}
	a.expectQuiet(t)
	b.expectQuiet(t)
	// Not a string, it is invalid.
	b.expectError(t, "me", "bad", map[string]any{"status": true}, codeInvalidParams)

	// online again derives the status from the connections, idle here.
	if got := setStatus(t, b, "online"); got != "online" {
		t.Fatalf("you after online: %v", got)
	}
	expectProfileStatus(t, a, b.userID, "idle")
	a.expectQuiet(t)
	// A status set on one connection reaches the user's others as `you`.
	other, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	other.result(t, "auth", "again", map[string]any{"scheme": "token", "token": registered["token"]})
	expectStatus(t, a, b.userID, "online")
	other.drain(t)
	setStatus(t, other, "dnd")
	if you := b.notification(t, "user")["you"].(map[string]any); you["status"] != "dnd" {
		t.Fatalf("you on another connection: %#v", you)
	}
	expectProfileStatus(t, a, b.userID, "dnd")
}

// Others see dnd only while the user has a connection, and offline
// otherwise (§4.11): the last disconnect announces offline, a reconnect dnd
// again, and listings and the snapshot after a sign-in agree.
func TestDNDOnlyWhileConnected(t *testing.T) {
	config := DefaultConfig()
	config.WebAuthn = testWebAuthn(t)
	_, httpServer := newTestServer(t, config)
	a := dialTestClient(t, httpServer)
	b, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	registered := registerTestPasskey(t, b, newTestAuthenticator(t))
	token := registered["token"]
	expectMembership(t, a, "general", b.userID, true)
	watching(a)
	b.drain(t)
	a.drain(t)
	setStatus(t, b, "dnd")
	expectProfileStatus(t, a, b.userID, "dnd")
	second, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	second.result(t, "auth", "second", map[string]any{"scheme": "token", "token": token})
	second.drain(t)
	a.expectQuiet(t)

	// Closing one of two connections changes nothing; the last shows
	// offline.
	_ = second.ws.Close(websocket.StatusNormalClosure, "bye")
	a.expectQuiet(t)
	_ = b.ws.Close(websocket.StatusNormalClosure, "bye")
	expectStatus(t, a, b.userID, "offline")
	if got := listedStatus(t, a, b.userID); got != "offline" {
		t.Fatalf("listed status while dnd without connections: %v", got)
	}
	// A sign-in elsewhere leaves the disconnected dnd user out.
	g, _ := dialRaw(t, httpServer)
	watching(g)
	guestAuth(t, g)
	frames := g.drain(t)
	if len(frames) != 1 {
		t.Fatalf("snapshot after a sign-in: %#v", frames)
	}
	checkStatus(t, frames[0], a.userID, "online")
	a.drain(t)

	// A reconnect shows dnd again; `you` still shows the status set.
	b, _ = dialOrigin(t, httpServer, testPasskeyOrigin)
	if you := b.result(t, "auth", "resume", map[string]any{"scheme": "token", "token": token})["you"].(map[string]any); you["status"] != "dnd" {
		t.Fatalf("you after a reconnect while dnd: %#v", you)
	}
	expectStatus(t, a, registered["you"].(map[string]any)["user_id"].(string), "dnd")
	if got := listedStatus(t, a, registered["you"].(map[string]any)["user_id"].(string)); got != "dnd" {
		t.Fatalf("listed status while dnd and connected: %v", got)
	}
	a.expectQuiet(t)
}

// An invisible user is offline to others in every frame that could tell:
// no announcement when they connect, go idle, or leave; none when they join
// a room; and none in the snapshot another connection gets after auth
// (§4.11). The same holds for a user who set "".
func TestInvisiblePrivacy(t *testing.T) {
	config := DefaultConfig()
	config.WebAuthn = testWebAuthn(t)
	_, httpServer := newTestServer(t, config)
	a := dialTestClient(t, httpServer)
	b, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	registered := registerTestPasskey(t, b, newTestAuthenticator(t))
	expectMembership(t, a, "general", b.userID, true)
	watching(a)
	a.drain(t)
	for _, status := range []string{"invisible", ""} {
		setStatus(t, b, status)
		b.drain(t)
		want := map[string]string{"invisible": "offline", "": ""}[status]
		expectProfileStatus(t, a, b.userID, want)
		b.status(t, map[string]any{"idle": true})
		b.status(t, map[string]any{"idle": false})
		second, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
		second.result(t, "auth", "resume", map[string]any{"scheme": "token", "token": registered["token"]})
		_ = second.ws.Close(websocket.StatusNormalClosure, "bye")
		// A newcomer's snapshot after auth leaves b out.
		c, _ := dialRaw(t, httpServer)
		watching(c)
		guestAuth(t, c)
		if frames := c.drain(t); len(frames) != 1 {
			t.Fatalf("snapshot with b %q: %#v", status, frames)
		} else {
			checkStatus(t, frames[0], a.userID, "online")
		}
		expectMembership(t, a, "general", c.userID, true)
		expectStatus(t, a, c.userID, "online")
		// Joining a room announces nothing either.
		ops, _ := saveRoom(t, c, "ops-"+status, map[string]any{"title": "Ops"})
		b.drain(t)
		joinRoom(t, b, ops)
		expectMembership(t, c, ops, b.userID, true)
		if frames := c.drain(t); len(frames) != 0 {
			t.Fatalf("frames after b %q joined: %#v", status, frames)
		}
		if got := listedStatus(t, c, b.userID); got != want {
			t.Fatalf("listed status of b %q: %v", status, got)
		}
		// A guest that goes leaves its rooms.
		_ = c.ws.Close(websocket.StatusNormalClosure, "bye")
		expectMembership(t, a, "general", c.userID, false)
		a.expectQuiet(t)
		b.drain(t)
	}
}

// A flapping user's changes reach others at once for a few, then coalesced
// to the latest at most every statusCoalesce.
func TestStatusChangesCoalesce(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	a.drain(t)
	watching(a)
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

// Each change to the user's mutes goes to all of their connections as
// `status`, the sender's included: true, the seconds left, or false when a
// mute is cleared or ends (§4.11). Others never see a mute.
func TestMuteEchoToAllConnections(t *testing.T) {
	config := DefaultConfig()
	config.WebAuthn = testWebAuthn(t)
	_, httpServer := newTestServer(t, config)
	observer := dialTestClient(t, httpServer)
	first, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	registered := registerTestPasskey(t, first, newTestAuthenticator(t))
	second, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	second.result(t, "auth", "resume", map[string]any{"scheme": "token", "token": registered["token"]})
	second.userID = first.userID
	expectMembership(t, observer, "general", first.userID, true)
	watching(observer)
	first.drain(t)
	second.drain(t)
	observer.drain(t)
	for _, step := range []struct {
		params map[string]any
		roomID string
		mute   any
	}{
		{map[string]any{"mute": 60}, "", float64(60)},
		{map[string]any{"mute": true}, "", true},
		{map[string]any{"mute": false}, "", false},
		{map[string]any{"mute": 10 * maxMuteSeconds}, "", float64(maxMuteSeconds)},
		{map[string]any{"mute": 0}, "", false},
		{map[string]any{"room_id": "general", "mute": true}, "general", true},
		{map[string]any{"room_id": "general", "mute": 0}, "general", false},
	} {
		expectEcho(t, first, step.params, step.roomID, step.mute, second)
	}
	// Invalid values, and a room the user cannot see, are invalid_params
	// and change nothing, the valid fields beside them included.
	for _, params := range []map[string]any{
		{"mute": -1}, {"mute": 1.5}, {"mute": "60"}, {"mute": nil},
		{"room_id": "missing", "mute": true}, {"room_id": 5, "mute": true},
		{"mute": true, "idle": "yes"}, {"mute": true, "room_id": nil},
	} {
		second.statusError(t, params, codeInvalidParams)
	}
	first.expectQuiet(t)
	second.expectQuiet(t)

	// Timed mutes end by themselves, sent as false to every connection.
	expectEcho(t, second, map[string]any{"mute": 1}, "", float64(1), first)
	expectEcho(t, second, map[string]any{"room_id": "general", "mute": 1}, "general", float64(1), first)
	time.Sleep(1200 * time.Millisecond)
	for _, c := range []*testClient{first, second} {
		frames := c.drain(t)
		if len(frames) != 2 {
			t.Fatalf("frames after the mutes ran out: %#v", frames)
		}
		// The two timers run in either order.
		if params, _ := frames[0]["params"].(map[string]any); params["room_id"] != nil {
			frames[0], frames[1] = frames[1], frames[0]
		}
		checkMute(t, frames[0], "", false)
		checkMute(t, frames[1], "general", false)
	}
	observer.expectQuiet(t)
	for _, user := range listRooms(t, observer, map[string]any{"room_id": "general", "members": true})["users"].([]any) {
		if _, has := user.(map[string]any)["mute"]; has {
			t.Fatalf("mute shown to others: %#v", user)
		}
	}
	for _, room := range listRooms(t, second, map[string]any{"room_id": "general"})["joined"].([]any) {
		if _, has := room.(map[string]any)["mute"]; has {
			t.Fatalf("mute in a room record: %#v", room)
		}
	}
}

// After a sign-in, a connection is sent, after the auth result, one
// `status` for each of its user's mutes in effect, then the status others
// see of each user who shares a room with it, other than offline and ""
// (§4.11). A `status` before sign-in is denied like any request, and one
// without an id is ignored, so neither changes anything: the connection
// starts attended.
func TestAfterAuthMutesAndStatuses(t *testing.T) {
	config := DefaultConfig()
	config.WebAuthn = testWebAuthn(t)
	app, httpServer := newTestServer(t, config)
	clients := dialGroup(t, httpServer, 3)
	attended, idle, dnd := clients[0], clients[1], clients[2]
	idle.status(t, map[string]any{"idle": true})
	setStatus(t, dnd, "dnd")
	invisible := dialTestClient(t, httpServer)
	setStatus(t, invisible, "invisible")
	for _, c := range clients {
		c.drain(t)
	}
	// An account without connections, which shows offline, and one with
	// dnd, which without them shows offline too: both are left out.
	addAccount(t, app, "away", clients...)
	addAccount(t, app, "busy", clients...)
	app.mu.Lock()
	app.users["busy"].chosen = statusDND
	app.unlock()

	owner, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	registered := registerTestPasskey(t, owner, newTestAuthenticator(t))
	owner.drain(t)
	for _, c := range clients {
		c.drain(t)
	}
	ops, _ := saveRoom(t, owner, "ops", map[string]any{"title": "Ops"})
	owner.status(t, map[string]any{"mute": true})
	owner.status(t, map[string]any{"room_id": ops, "mute": true})
	owner.status(t, map[string]any{"room_id": "general", "mute": 60})
	owner.status(t, map[string]any{"room_id": "general", "mute": false})
	owner.drain(t)

	// A connection that sends status before it signs in: as a request it
	// is denied, and without an id ignored. Nothing changes, and the
	// owner's connection is told nothing.
	c, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	watching(c)
	c.write(t, map[string]any{"method": "status", "params": map[string]any{"mute": false}})
	c.write(t, map[string]any{"method": "status", "params": map[string]any{"idle": true}})
	c.statusError(t, map[string]any{"mute": false}, codeDenied)
	c.statusError(t, map[string]any{"idle": true}, codeDenied)
	c.statusError(t, map[string]any{"room_id": "missing", "mute": true}, codeDenied)
	before, _ := c.request(t, "auth", "resume", map[string]any{"scheme": "token", "token": registered["token"]})
	if len(before) != 0 {
		t.Fatalf("frames before the auth result: %#v", before)
	}
	owner.expectQuiet(t)
	frames := c.drain(t)
	if len(frames) != 5 {
		t.Fatalf("frames after auth: %#v", frames)
	}
	checkMute(t, frames[0], "", true)
	checkMute(t, frames[1], ops, true)
	statuses := map[string]string{}
	for _, frame := range frames[2:] {
		object := notificationParams(t, frame, "user")["new"].(map[string]any)
		statuses[object["user_id"].(string)] = object["status"].(string)
	}
	if want := map[string]string{attended.userID: "online", idle.userID: "idle", dnd.userID: "dnd"}; !reflect.DeepEqual(statuses, want) {
		t.Fatalf("statuses after auth: %#v, want %#v", statuses, want)
	}
	// The idle sent before sign-in did not apply: the new connection
	// starts attended, so the owner stays online when the first one goes,
	// and is idle only once the new one says so.
	attended.drain(t)
	watching(attended)
	_ = owner.ws.Close(websocket.StatusNormalClosure, "bye")
	attended.expectQuiet(t)
	app.mu.RLock()
	status := app.users[owner.userID].shownStatus()
	app.mu.RUnlock()
	if status != statusOnline {
		t.Fatalf("owner status: %s", status)
	}
	c.status(t, map[string]any{"idle": true})
	expectStatus(t, attended, owner.userID, "idle")
	c.expectQuiet(t)

	// A guest gets the snapshot too, after the auth result.
	g, _ := dialRaw(t, httpServer)
	watching(g)
	guestAuth(t, g)
	if frames := g.drain(t); len(frames) != 4 {
		t.Fatalf("guest snapshot: %#v", frames)
	}
}

// The status a user sets and their mutes survive a restart (§4.11): the
// auth result's `you` carries the status, and the mutes follow it.
func TestStatusSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	open := func() store.Store {
		s, err := store.OpenSQLite(filepath.Join(dir, "aprond.db"))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	_, httpServer, stop := startWithStore(t, open(), filepath.Join(dir, "uploads"))
	owner, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	registered := registerTestPasskey(t, owner, newTestAuthenticator(t))
	owner.drain(t)
	setStatus(t, owner, "dnd")
	owner.status(t, map[string]any{"mute": true})
	owner.status(t, map[string]any{"room_id": "general", "mute": 3600})
	stop()

	_, httpServer, _ = startWithStore(t, open(), filepath.Join(dir, "uploads"))
	c, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	you := c.result(t, "auth", "resume", map[string]any{"scheme": "token", "token": registered["token"]})["you"].(map[string]any)
	if you["status"] != "dnd" {
		t.Fatalf("you after restart: %#v", you)
	}
	frames := c.drain(t)
	if len(frames) != 2 {
		t.Fatalf("frames after auth: %#v", frames)
	}
	checkMute(t, frames[0], "", true)
	if params := notificationParams(t, frames[1], "status"); params["room_id"] != "general" || params["mute"].(float64) < 3590 || params["mute"].(float64) > 3600 {
		t.Fatalf("room mute after restart: %#v", params)
	}
}

// Only a sign-in is sent the mutes and statuses after its auth result
// (§4.11): an auth that adds a passkey or an address to a signed-in
// connection is sent nothing after it, while a sign-in to the same account
// elsewhere is.
func TestAdditionsAreNotSignIns(t *testing.T) {
	_, mailbox, httpServer := emailTestServer(t, func(config *Config) {
		config.WebAuthn = testWebAuthn(t)
	})
	other := dialTestClient(t, httpServer)
	c, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	guestAuth(t, c)
	expectMembership(t, other, "general", c.userID, true)
	watching(c)
	c.drain(t)
	frames := c.status(t, map[string]any{"mute": true})
	if len(frames) != 1 {
		t.Fatalf("mute echo: %#v", frames)
	}
	checkMute(t, frames[0], "", true)

	// A passkey registration adds the passkey: its result, and nothing
	// after it.
	authenticator := newTestAuthenticator(t)
	options := passkeyResult(t, passkeyCall(t, c, "register-begin", "register", "begin", nil))
	registered := passkeyResult(t, passkeyCall(t, c, "register-finish", "register", "finish", map[string]any{"credential": authenticator.registration(t, options, testPasskeyOrigin)}))
	if registered["you"].(map[string]any)["user_id"] != c.userID || registered["token"] == nil {
		t.Fatalf("passkey registration: %#v", registered)
	}
	if frames := c.drain(t); len(frames) != 0 {
		t.Fatalf("frames after a passkey registration: %#v", frames)
	}

	// Nor is an address added to the account.
	propose(t, c, "added@example.com")
	message := mailbox.receive(t)
	if !message.Add {
		t.Fatalf("addition email: %#v", message)
	}
	if _, result := approve(t, c, message.Code, nil); len(result) != 0 {
		t.Fatalf("approving an addition: %#v", result)
	}
	if frames := c.drain(t); len(frames) != 0 {
		t.Fatalf("frames after an address addition: %#v", frames)
	}

	// A sign-in to the account on another connection is sent the mute and
	// the statuses after its result.
	signedIn, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	watching(signedIn)
	before, _ := signedIn.request(t, "auth", "resume", map[string]any{"scheme": "token", "token": registered["token"]})
	if len(before) != 0 {
		t.Fatalf("frames before the sign-in result: %#v", before)
	}
	frames = signedIn.drain(t)
	if len(frames) != 2 {
		t.Fatalf("frames after a sign-in: %#v", frames)
	}
	checkMute(t, frames[0], "", true)
	checkStatus(t, frames[1], other.userID, "online")
}

// room_id scopes only mute (§4.11): idle is the sending connection's with
// any room_id, one that names a room the user cannot see included, and the
// unscoped mute is untouched by a scoped one. A mute of a room the user
// cannot see is invalid_params, and then the idle beside it does not apply
// either.
func TestRoomIDScopesOnlyMute(t *testing.T) {
	app, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	watching(a)
	a.drain(t)

	// A mute of a room the user cannot see changes nothing.
	b.statusError(t, map[string]any{"room_id": "missing", "idle": true, "mute": true}, codeInvalidParams)
	a.expectQuiet(t)
	// Without a mute, room_id is not looked at: idle applies.
	if frames := b.status(t, map[string]any{"room_id": "missing", "idle": true}); len(frames) != 0 {
		t.Fatalf("frames after an idle with a missing room: %#v", frames)
	}
	expectStatus(t, a, b.userID, "idle")
	// A visible room: idle applies to the connection, the mute to the room.
	frames := b.status(t, map[string]any{"room_id": "general", "idle": false, "mute": true})
	if len(frames) != 1 {
		t.Fatalf("frames after a mute of general: %#v", frames)
	}
	checkMute(t, frames[0], "general", true)
	expectStatus(t, a, b.userID, "online")
	a.expectQuiet(t)
	app.mu.RLock()
	defer app.mu.RUnlock()
	u := app.users[b.userID]
	if now := time.Now(); u.mute.active(now) || !u.roomMuted(app.rooms["general"], now) {
		t.Fatalf("mutes: unscoped %#v, rooms %#v", u.mute, u.roomMutes)
	}
}

// A client's `status` is a request (§4.11): answered {} once applied,
// denied before sign-in, and with a bad id or params answered like any
// request. A `status` without an id is a notification with no meaning,
// ignored as one with an unknown method is (§1), before sign-in and after,
// whatever its params.
func TestStatusIsARequest(t *testing.T) {
	app, httpServer := newTestServer(t, DefaultConfig())
	a := dialTestClient(t, httpServer)
	watching(a)
	a.drain(t)
	b, _ := dialRaw(t, httpServer)
	raw := func(frame string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := b.ws.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
			t.Fatal(err)
		}
	}
	notifications := []string{
		`{"method":"status","params":{"idle":true}}`,
		`{"method":"status","params":{"mute":true}}`,
		`{"method":"status","params":{"mute":-1}}`,
		`{"method":"status","params":[]}`,
		`{"method":"status","params":null}`,
		`{"method":"status"}`,
	}
	for _, frame := range notifications {
		raw(frame)
	}
	b.expectQuiet(t)
	b.statusError(t, map[string]any{"idle": true}, codeDenied)
	b.statusError(t, map[string]any{"mute": true}, codeDenied)

	guestAuth(t, b)
	expectMembership(t, a, "general", b.userID, true)
	expectStatus(t, a, b.userID, "online")
	b.drain(t)
	a.expectQuiet(t)
	for _, frame := range notifications {
		raw(frame)
	}
	b.expectQuiet(t)
	a.expectQuiet(t)
	app.mu.RLock()
	u := app.users[b.userID]
	if now := time.Now(); u.mute.active(now) || u.shownStatus() != statusOnline {
		app.mu.RUnlock()
		t.Fatalf("after status notifications: mute %#v, status %s", u.mute, u.shownStatus())
	}
	app.mu.RUnlock()

	// As a request it applies, and answers {}.
	if frames := b.status(t, map[string]any{"idle": true}); len(frames) != 0 {
		t.Fatalf("frames before the idle result: %#v", frames)
	}
	expectStatus(t, a, b.userID, "idle")
	// A notification does not end it.
	raw(`{"method":"status","params":{"idle":false}}`)
	b.expectQuiet(t)
	a.expectQuiet(t)
	if got := listedStatus(t, a, b.userID); got != "idle" {
		t.Fatalf("listed status after an idle notification: %v", got)
	}
	// A bad id or params are answered like those of any request.
	raw(`{"method":"status","id":5,"params":{"idle":false}}`)
	if reply := b.read(t); reply["id"] != nil || reply["error"].(map[string]any)["code"] != float64(codeInvalidRequest) {
		t.Fatalf("bad id on status: %#v", reply)
	}
	raw(`{"method":"status","id":"bad-params","params":[]}`)
	if reply := b.read(t); reply["id"] != "bad-params" || reply["error"].(map[string]any)["code"] != float64(codeInvalidParams) {
		t.Fatalf("bad params on status: %#v", reply)
	}
	b.statusError(t, map[string]any{"idle": "no"}, codeInvalidParams)
	a.expectQuiet(t)
	b.status(t, map[string]any{"idle": false})
	expectStatus(t, a, b.userID, "online")
}

// The server limits each user's `status` requests (§4.11): beyond the
// limit a request is retry_after, with the seconds to wait, and changes
// nothing, neither idle nor mute.
func TestStatusRateLimit(t *testing.T) {
	app, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	watching(a)
	a.drain(t)

	// The default: a burst of statusRequestBurst, refilled one a second, so
	// a quick run of more is refused.
	for i := range statusRequestBurst + 2 {
		b.write(t, map[string]any{"method": "status", "id": statusID(), "params": map[string]any{"room_id": fmt.Sprint(i)}})
	}
	limited := 0
	for range statusRequestBurst + 2 {
		frame := b.read(t)
		if failure, ok := frame["error"].(map[string]any); ok {
			if failure["code"] != float64(codeRetryAfter) {
				t.Fatalf("status beyond the burst: %#v", frame)
			}
			limited++
		}
	}
	if limited == 0 {
		t.Fatalf("no status of %d was limited", statusRequestBurst+2)
	}

	// A limit of two, refilled hourly.
	app.mu.Lock()
	app.users[b.userID].statusRequests = rate.NewLimiter(rate.Every(time.Hour), 2)
	app.unlock()
	b.status(t, map[string]any{"idle": true})
	expectStatus(t, a, b.userID, "idle")
	expectEcho(t, b, map[string]any{"mute": true}, "", true)
	for _, params := range []map[string]any{
		{"idle": false}, {"mute": false}, {"room_id": "general", "mute": true},
	} {
		failure := b.statusError(t, params, codeRetryAfter)
		if wait, _ := failure["data"].(map[string]any)["retry_after"].(float64); wait < 1 {
			t.Fatalf("retry_after of %#v: %#v", params, failure)
		}
	}
	b.expectQuiet(t)
	a.expectQuiet(t)
	app.mu.RLock()
	defer app.mu.RUnlock()
	u := app.users[b.userID]
	if now := time.Now(); !u.mute.forever || u.roomMuted(app.rooms["general"], now) || u.shownStatus() != statusIdle {
		t.Fatalf("after limited requests: mute %#v, room mutes %#v, status %s", u.mute, u.roomMutes, u.shownStatus())
	}
}

// A repeat auth as the user the connection is signed in as is no sign-in
// (§4.11): a guest auth on a signed-in connection, and a token or passkey
// sign-in as the same account, are answered with their result and nothing
// after it, mutes and statuses alike. Another connection signing in as the
// account is sent both.
func TestRepeatAuthIsNoSignIn(t *testing.T) {
	_, httpServer := passkeyTestServer(t)
	other := dialTestClient(t, httpServer)

	g := dialTestClient(t, httpServer)
	watching(g)
	g.drain(t)
	expectEcho(t, g, map[string]any{"mute": true}, "", true)
	if before, result := g.request(t, "auth", "again", map[string]any{"scheme": "guest"}); len(before) != 0 || result["you"].(map[string]any)["user_id"] != g.userID {
		t.Fatalf("guest auth again: %#v then %#v", before, result)
	}
	g.expectQuiet(t)

	owner, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	authenticator := newTestAuthenticator(t)
	registered := registerTestPasskey(t, owner, authenticator)
	watching(owner)
	owner.drain(t)
	expectEcho(t, owner, map[string]any{"room_id": "general", "mute": true}, "general", true)
	before, result := owner.request(t, "auth", "resume-again", map[string]any{"scheme": "token", "token": registered["token"]})
	if len(before) != 0 || result["you"].(map[string]any)["user_id"] != owner.userID {
		t.Fatalf("token auth as the same user: %#v then %#v", before, result)
	}
	owner.expectQuiet(t)
	options := passkeyResult(t, passkeyCall(t, owner, "login-begin", "login", "begin", nil))
	loggedIn := passkeyResult(t, passkeyCall(t, owner, "login-finish", "login", "finish", map[string]any{"credential": authenticator.assertion(t, options, testPasskeyOrigin, "localhost", 0x05)}))
	if loggedIn["you"].(map[string]any)["user_id"] != owner.userID {
		t.Fatalf("passkey login as the same user: %#v", loggedIn)
	}
	owner.expectQuiet(t)

	// A sign-in as the account elsewhere is sent the mute and the statuses.
	signedIn, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	watching(signedIn)
	signedIn.result(t, "auth", "resume", map[string]any{"scheme": "token", "token": loggedIn["token"]})
	frames := signedIn.drain(t)
	if len(frames) != 3 {
		t.Fatalf("frames after a sign-in: %#v", frames)
	}
	checkMute(t, frames[0], "general", true)
	statuses := map[string]string{}
	for _, frame := range frames[1:] {
		object := notificationParams(t, frame, "user")["new"].(map[string]any)
		statuses[object["user_id"].(string)] = object["status"].(string)
	}
	if want := map[string]string{other.userID: "online", g.userID: "online"}; !reflect.DeepEqual(statuses, want) {
		t.Fatalf("statuses after a sign-in: %#v, want %#v", statuses, want)
	}
}

// Every current user object in room_list and room_update carries the status
// others see (§4.11), offline and "" included: a room's members and users
// in a room_list with members, and in a room_update joined.
func TestListingsCarryStatus(t *testing.T) {
	app, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 3)
	online, none, invisible := clients[0], clients[1], clients[2]
	setStatus(t, none, "")
	setStatus(t, invisible, "invisible")
	for _, c := range clients {
		c.drain(t)
	}
	addAccount(t, app, "away", clients...)
	want := map[string]any{online.userID: "online", none.userID: "", invisible.userID: "offline", "away": "offline"}
	check := func(what string, members, users []any) {
		t.Helper()
		for _, list := range [][]any{members, users} {
			got := map[string]any{}
			for _, object := range list {
				object := object.(map[string]any)
				status, has := object["status"]
				if !has {
					t.Fatalf("%s: %#v without status", what, object)
				}
				got[object["user_id"].(string)] = status
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s statuses = %#v, want %#v", what, got, want)
			}
		}
	}
	listed := listRooms(t, online, map[string]any{"filter": "joined", "members": true})
	check("room_list", listed["joined"].([]any)[0].(map[string]any)["members"].([]any), listed["users"].([]any))

	room, _ := saveRoom(t, none, "room", map[string]any{"title": "Room"})
	joinRoom(t, invisible, room)
	app.mu.Lock()
	app.addMemberLocked(app.users["away"], app.rooms[room])
	app.unlock()
	online.drain(t)
	before, _ := online.request(t, "room_join", "join", map[string]any{"room_id": room})
	update := notificationParams(t, before[0], "room_update")
	check("room_update joined", update["joined"].([]any)[0].(map[string]any)["members"].([]any), update["users"].([]any))
}
