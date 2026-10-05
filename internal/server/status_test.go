package server

import (
	"fmt"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/apron-chat/apron-server-go/internal/store"
)

// status sends a status notification and returns the frames it caused on
// c, after the server has processed it.
func (c *testClient) status(t *testing.T, params map[string]any) []map[string]any {
	t.Helper()
	c.write(t, map[string]any{"method": "status", "params": params})
	return c.drain(t)
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
	id := fmt.Sprintf("status-%d", statusRequests.Add(1))
	return c.result(t, "me", id, map[string]any{"status": status})["you"].(map[string]any)["status"]
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
	both := func(roomID string, mute any) {
		t.Helper()
		for _, c := range []*testClient{first, second} {
			expectMute(t, c, roomID, mute)
		}
	}
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
		first.write(t, map[string]any{"method": "status", "params": step.params})
		both(step.roomID, step.mute)
	}
	// Invalid values, and a room the user cannot see, change nothing and
	// are not answered.
	for _, params := range []map[string]any{
		{"mute": -1}, {"mute": 1.5}, {"mute": "60"}, {"mute": nil},
		{"room_id": "missing", "mute": true}, {"room_id": 5, "mute": true},
	} {
		if frames := second.status(t, params); len(frames) != 0 {
			t.Fatalf("frames after %#v: %#v", params, frames)
		}
	}
	first.expectQuiet(t)

	// Timed mutes end by themselves, sent as false to every connection.
	second.write(t, map[string]any{"method": "status", "params": map[string]any{"mute": 1}})
	second.write(t, map[string]any{"method": "status", "params": map[string]any{"room_id": "general", "mute": 1}})
	both("", float64(1))
	both("general", float64(1))
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
// (§4.11). Mutes sent before auth apply once it signs in, and
// reach the user's other connections.
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
	owner.write(t, map[string]any{"method": "status", "params": map[string]any{"mute": 600}})
	owner.write(t, map[string]any{"method": "status", "params": map[string]any{"room_id": ops, "mute": true}})
	owner.write(t, map[string]any{"method": "status", "params": map[string]any{"room_id": "general", "mute": 60}})
	owner.drain(t)

	// A connection that sends mutes before it signs in: the first replaces
	// the unscoped mute, and the room's mute ends; its other connections
	// are told, it is not, but gets the snapshot.
	c, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	watching(c)
	c.write(t, map[string]any{"method": "status", "params": map[string]any{"mute": true}})
	c.write(t, map[string]any{"method": "status", "params": map[string]any{"room_id": "general", "mute": false}})
	c.write(t, map[string]any{"method": "status", "params": map[string]any{"idle": true}})
	c.write(t, map[string]any{"method": "status", "id": "early", "params": map[string]any{"room_id": "missing", "mute": true}})
	before, _ := c.request(t, "auth", "resume", map[string]any{"scheme": "token", "token": registered["token"]})
	if len(before) != 0 {
		t.Fatalf("frames before the auth result: %#v", before)
	}
	expectMute(t, owner, "", true)
	expectMute(t, owner, "general", false)
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
	// The connection's idle, sent before auth, applies: the owner, attended
	// on its first connection, stays online.
	app.mu.RLock()
	status := app.users[owner.userID].statusAt(time.Now())
	app.mu.RUnlock()
	if status != statusOnline {
		t.Fatalf("owner status: %s", status)
	}
	attended.drain(t)
	watching(attended)
	_ = owner.ws.Close(websocket.StatusNormalClosure, "bye")
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
// unscoped mute is untouched by a scoped one.
func TestRoomIDScopesOnlyMute(t *testing.T) {
	app, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	watching(a)
	a.drain(t)

	// A room the user cannot see: idle applies, the mute is ignored.
	if frames := b.status(t, map[string]any{"room_id": "missing", "idle": true, "mute": true}); len(frames) != 0 {
		t.Fatalf("frames after a mute of a missing room: %#v", frames)
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
