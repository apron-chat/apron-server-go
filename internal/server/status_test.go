package server

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
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
// `{new: {user_id, status}}`, with seen when status is offline, and returns
// the seen.
func expectStatus(t *testing.T, c *testClient, userID, status string) string {
	t.Helper()
	return checkStatus(t, c.read(t), userID, status)
}

func checkStatus(t *testing.T, frame map[string]any, userID, status string) string {
	t.Helper()
	params := notificationParams(t, frame, "user")
	want := map[string]any{"new": map[string]any{"user_id": userID, "status": status}}
	seen, _ := params["new"].(map[string]any)["seen"].(string)
	if status == statusOffline {
		checkSeen(t, seen)
		want["new"].(map[string]any)["seen"] = seen
	}
	if !reflect.DeepEqual(params, want) {
		t.Fatalf("user notification = %#v, want %#v", params, want)
	}
	return seen
}

// checkSeen checks that seen is in the format of a log_id, and recent
// (§4.5.2).
func checkSeen(t *testing.T, seen string) {
	t.Helper()
	ms, err := strconv.ParseInt(seen, 10, 64)
	if err != nil || strconv.FormatInt(ms, 10) != seen || ms <= 0 || ms >= 1<<53 {
		t.Fatalf("seen = %q, want a log_id", seen)
	}
	if at := time.UnixMilli(ms); time.Since(at) > time.Minute || time.Until(at) > time.Second {
		t.Fatalf("seen = %v, want about now", at)
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

// Each status a user sets (§4.5), as others see it and as the user's own
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
	if frames := b.status(t, map[string]any{"idle": 300}); len(frames) != 0 {
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
	// Zero seconds since attention stopped is idle too (§4.5.3).
	second.status(t, map[string]any{"idle": 0})
	expectStatus(t, a, b.userID, "idle")
	b.status(t, map[string]any{"idle": false})
	expectStatus(t, a, b.userID, "online")
	if you := b.result(t, "me", "me", map[string]any{})["you"].(map[string]any); you["status"] != "online" {
		t.Fatalf("you while online: %#v", you)
	}
	// room_id scopes only mute, so room_id without mute is invalid_params
	// and changes nothing (§4.5).
	b.statusError(t, map[string]any{"room_id": "general", "idle": 300}, codeInvalidParams)
	a.expectQuiet(t)
	b.status(t, map[string]any{"idle": 300})
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
	b.status(t, map[string]any{"idle": 300})
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
	b.status(t, map[string]any{"idle": 300})
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
// otherwise (§4.5): the last disconnect announces offline, a reconnect dnd
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
// (§4.5). The same holds for a user who set "".
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
		b.status(t, map[string]any{"idle": 300})
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
		b.status(t, map[string]any{"idle": idleParam(idle)})
		expectStatus(t, a, b.userID, map[bool]string{true: "idle", false: "online"}[idle])
	}
	// Past the burst, changes wait, and only the latest is sent.
	for _, idle := range []bool{false, true, false} {
		b.status(t, map[string]any{"idle": idleParam(idle)})
	}
	a.expectQuiet(t)
	time.Sleep(statusCoalesce + 200*time.Millisecond)
	expectStatus(t, a, b.userID, "online")
	a.expectQuiet(t)
}

// Each change to the user's mutes goes to all of their connections as
// `status`, the sender's included: true, the seconds left, or false when a
// mute is cleared or ends (§4.5). Others never see a mute.
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
		{map[string]any{"room_id": "general", "mute": true}, "general", true},
		{map[string]any{"room_id": "general", "mute": false}, "general", false},
	} {
		expectEcho(t, first, step.params, step.roomID, step.mute, second)
	}
	// Invalid values, and a room the user cannot see, are invalid_params
	// and change nothing, the valid fields beside them included.
	for _, params := range []map[string]any{
		// mute seconds are a positive integer: 0 is not false (§4.5).
		{"mute": -1}, {"mute": 0}, {"room_id": "general", "mute": 0}, {"mute": 1.5}, {"mute": "60"}, {"mute": nil},
		{"room_id": "general"}, {"room_id": "general", "idle": false},
		{"room_id": "missing", "mute": true}, {"room_id": 5, "mute": true},
		{"mute": true, "idle": "yes"}, {"mute": true, "room_id": nil},
		// idle is false or seconds since attention stopped (§4.5.3).
		{"mute": true, "idle": true}, {"mute": true, "idle": -1}, {"mute": true, "idle": 1.5}, {"mute": true, "idle": nil},
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
// (§4.5). A `status` before sign-in is denied like any request, and one
// without an id is ignored, so neither changes anything: the connection
// starts attended.
func TestAfterAuthMutesAndStatuses(t *testing.T) {
	config := DefaultConfig()
	config.WebAuthn = testWebAuthn(t)
	app, httpServer := newTestServer(t, config)
	clients := dialGroup(t, httpServer, 3)
	attended, idle, dnd := clients[0], clients[1], clients[2]
	idle.status(t, map[string]any{"idle": 300})
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
	c.write(t, map[string]any{"method": "status", "params": map[string]any{"idle": 300}})
	c.statusError(t, map[string]any{"mute": false}, codeDenied)
	c.statusError(t, map[string]any{"idle": 300}, codeDenied)
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
	c.status(t, map[string]any{"idle": 300})
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

// The status a user sets and their mutes survive a restart (§4.5): the
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
// (§4.5): an auth that adds a passkey or an address to a signed-in
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

// room_id scopes only mute (§4.5): idle is the sending connection's with
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
	b.statusError(t, map[string]any{"room_id": "missing", "idle": 300, "mute": true}, codeInvalidParams)
	a.expectQuiet(t)
	// Without a mute, room_id is invalid_params (§4.5), and the idle beside
	// it does not apply either.
	b.statusError(t, map[string]any{"room_id": "missing", "idle": 300}, codeInvalidParams)
	a.expectQuiet(t)
	if frames := b.status(t, map[string]any{"idle": 300}); len(frames) != 0 {
		t.Fatalf("frames after an idle: %#v", frames)
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

// A client's `status` is a request (§4.5): answered {} once applied,
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
	b.statusError(t, map[string]any{"idle": 300}, codeDenied)
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
	if frames := b.status(t, map[string]any{"idle": 300}); len(frames) != 0 {
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

// The server limits each user's `status` requests (§4.5): beyond the
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
		b.write(t, map[string]any{"method": "status", "id": statusID(), "params": map[string]any{"idle": false, "seq": i}})
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
	b.status(t, map[string]any{"idle": 300})
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
// (§4.5): a guest auth on a signed-in connection, and a token or passkey
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
// others see (§4.5), offline and "" included: a room's members and users
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

// The statuses sent after a sign-in are only of the users that the user's
// rooms list in members (§4.5): past MaxListedMembers, a room lists its most
// recently active members (§4.2.1), the user who just joined included.
func TestAfterAuthStatusesAreBounded(t *testing.T) {
	config := DefaultConfig()
	config.MaxListedMembers = 2
	_, httpServer := newTestServer(t, config)
	clients := dialGroup(t, httpServer, 4)
	save(t, clients[0], "post", map[string]any{"body": map[string]any{"text": "hi"}})
	for _, c := range clients {
		c.drain(t)
	}
	g, _ := dialRaw(t, httpServer)
	watching(g)
	guestAuth(t, g)
	var got []string
	for _, frame := range g.drain(t) {
		object := notificationParams(t, frame, "user")["new"].(map[string]any)
		got = append(got, object["user_id"].(string))
	}
	// general lists g, who joined last, and clients[0], who posted.
	want := []string{clients[0].userID}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("statuses after sign-in for %v, want the listed members %v", got, want)
	}
}

// A `me` that changes the status counts against the user's limit on status
// changes, as a status request does (§4.5): beyond it the `me` is
// retry_after and changes nothing, while one that leaves the status as it
// is passes.
func TestMeStatusChangesAreLimited(t *testing.T) {
	app, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	watching(a)
	a.drain(t)
	app.mu.Lock()
	app.users[b.userID].statusRequests = rate.NewLimiter(rate.Every(time.Hour), 1)
	app.unlock()
	if got := setStatus(t, b, "dnd"); got != "dnd" {
		t.Fatalf("status = %v", got)
	}
	expectProfileStatus(t, a, b.userID, "dnd")
	frame := b.call(t, "me", statusID(), map[string]any{"status": "online", "name": "Bea"})
	if failure, ok := frame["error"].(map[string]any); !ok || failure["code"] != float64(codeRetryAfter) {
		t.Fatalf("me beyond the limit: %#v", frame)
	}
	if got := setStatus(t, b, "dnd"); got != "dnd" {
		t.Fatalf("unchanged status = %v", got)
	}
	b.expectQuiet(t)
	a.expectQuiet(t)
	app.mu.RLock()
	defer app.mu.RUnlock()
	if u := app.users[b.userID]; u.chosen != statusDND || u.name == "Bea" {
		t.Fatalf("after a limited me: status %q name %q", u.chosen, u.name)
	}
}

// A user mutes at most maxRoomMutes rooms at once: muting another is
// denied, while changing or removing a room's mute is not.
func TestRoomMutesAreBounded(t *testing.T) {
	app, httpServer := newTestServer(t, DefaultConfig())
	c := dialTestClient(t, httpServer)
	room, _ := saveRoom(t, c, "room", map[string]any{"title": "Room"})
	expectEcho(t, c, map[string]any{"room_id": room, "mute": true}, room, true)
	app.mu.Lock()
	u := app.users[c.userID]
	for i := range maxRoomMutes - 1 {
		u.roomMutes[fmt.Sprintf("gone-%d", i)] = muteState{forever: true}
	}
	app.unlock()
	c.statusError(t, map[string]any{"room_id": "general", "mute": 60}, codeDenied)
	expectEcho(t, c, map[string]any{"room_id": room, "mute": 60}, room, float64(60))
	expectEcho(t, c, map[string]any{"room_id": "general", "mute": false}, "general", false)
	expectEcho(t, c, map[string]any{"room_id": room, "mute": false}, room, false)
	expectEcho(t, c, map[string]any{"room_id": "general", "mute": true}, "general", true)
}

// A guest connection that signs in to an existing account is announced as
// the guest's departure, never as a `user` change with `old` (§3.3): its
// leaves reach the room's members as when it disconnects, and the account
// then shows through its own status, so an invisible account, or one
// without a status, shows nothing new, and others never learn it
// connected. A guest that becomes a new account keeps its user_id, so
// others are told of no change at all.
func TestSignInToExistingAccountIsADeparture(t *testing.T) {
	app, httpServer := passkeyTestServer(t)
	observer := dialTestClient(t, httpServer)
	watching(observer)
	observer.drain(t)

	// A guest that registers a passkey becomes an account in place.
	newcomer, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	registerTestPasskey(t, newcomer, newTestAuthenticator(t))
	// The guest's join, with its status, and then nothing.
	expectMembership(t, observer, "general", newcomer.userID, true)
	expectStatus(t, observer, newcomer.userID, "online")
	observer.expectQuiet(t)

	for _, status := range []string{"online", "invisible", ""} {
		owner, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
		registered := registerTestPasskey(t, owner, newTestAuthenticator(t))
		setStatus(t, owner, status)
		_ = owner.ws.Close(websocket.StatusNormalClosure, "away")
		// Wait for the detach, so the account's going offline is drained
		// here.
		waitConnections(t, app, owner.userID, 0)
		observer.drain(t)

		guest, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
		guestAuth(t, guest)
		expectMembership(t, observer, "general", guest.userID, true)
		observer.drain(t)
		guest.request(t, "auth", "resume", map[string]any{"scheme": "token", "token": registered["token"]})
		expectMembership(t, observer, "general", guest.userID, false)
		frames := observer.drain(t)
		switch status {
		case "online":
			if len(frames) != 1 {
				t.Fatalf("after a sign-in to an online account: %#v", frames)
			}
			checkStatus(t, frames[0], owner.userID, "online")
		default:
			if len(frames) != 0 {
				t.Fatalf("after a sign-in to an account with status %q: %#v", status, frames)
			}
		}
		_ = guest.ws.Close(websocket.StatusNormalClosure, "done")
		observer.drain(t)
	}
}

// waitConnections waits until userID has n connections: the server
// detaches a closed connection on its own time, so a test that closes one
// waits for it before it checks what the close did, or did not, send.
func waitConnections(t *testing.T, app *Server, userID string, n int) {
	t.Helper()
	for deadline := time.Now().Add(time.Second); ; time.Sleep(time.Millisecond) {
		app.mu.RLock()
		open := len(app.users[userID].clients)
		app.mu.RUnlock()
		if open == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s has %d connections, want %d", userID, open, n)
		}
	}
}

// idleParam is the `idle` of a status request: false, or seconds since
// attention stopped (§4.5.3).
func idleParam(idle bool) any {
	if idle {
		return 300
	}
	return false
}

// listedObjects returns userID's bare member object in general and its
// complete object in users, from a room_list of general with members.
func listedObjects(t *testing.T, c *testClient, userID string) (member, user map[string]any) {
	t.Helper()
	listed := listRooms(t, c, map[string]any{"room_id": "general", "members": true})
	for _, object := range listed["joined"].([]any)[0].(map[string]any)["members"].([]any) {
		if object := object.(map[string]any); object["user_id"] == userID {
			member = object
		}
	}
	for _, object := range listed["users"].([]any) {
		if object := object.(map[string]any); object["user_id"] == userID {
			user = object
		}
	}
	if member == nil || user == nil {
		t.Fatalf("%s is not listed: %#v", userID, listed)
	}
	return member, user
}

// expectListedSeen checks that userID is listed with status, and with seen
// in both its member object and its complete object when seen is not ""
// (§4.5.2), and without it otherwise.
func expectListedSeen(t *testing.T, c *testClient, userID, status, seen string) {
	t.Helper()
	member, user := listedObjects(t, c, userID)
	for _, object := range []map[string]any{member, user} {
		got, has := object["seen"]
		if object["status"] != status || has != (seen != "") || has && got != seen {
			t.Fatalf("listed %#v, want status %q and seen %q", object, status, seen)
		}
	}
}

// expectNoSeen checks that a user's own object carries no seen (§4.5.2).
func expectNoSeen(t *testing.T, what string, you any) {
	t.Helper()
	if _, has := you.(map[string]any)["seen"]; has {
		t.Fatalf("%s carries seen: %#v", what, you)
	}
}

// seenAfter requires seen to be later than before, both in the format of
// a log_id.
func seenAfter(t *testing.T, seen, before string) {
	t.Helper()
	checkSeen(t, seen)
	later, _ := strconv.ParseInt(seen, 10, 64)
	earlier, _ := strconv.ParseInt(before, 10, 64)
	if later <= earlier {
		t.Fatalf("seen %s is not after %s", seen, before)
	}
}

// When a user's last connection closes, others are sent the shown offline
// with seen, the time it became offline, and listings carry the same seen
// in the member and the complete object (§4.5.2). A shown status other than
// offline carries no seen, though one is kept, and the user's own `you`
// never does. The next change to offline brings a later seen.
func TestSeenOnDisconnect(t *testing.T) {
	config := DefaultConfig()
	config.WebAuthn = testWebAuthn(t)
	_, httpServer := newTestServer(t, config)
	a := dialTestClient(t, httpServer)
	b, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	registered := registerTestPasskey(t, b, newTestAuthenticator(t))
	token, userID := registered["token"], b.userID
	expectMembership(t, a, "general", userID, true)
	watching(a)
	a.drain(t)
	b.drain(t)
	// Never offline since it signed up, so no seen yet.
	expectListedSeen(t, a, userID, "online", "")

	_ = b.ws.Close(websocket.StatusNormalClosure, "bye")
	seen := expectStatus(t, a, userID, "offline")
	expectListedSeen(t, a, userID, "offline", seen)
	// A newcomer learns seen from a listing; its snapshot after auth leaves
	// offline users out.
	g, _ := dialRaw(t, httpServer)
	guestAuth(t, g)
	g.drain(t)
	expectListedSeen(t, g, userID, "offline", seen)
	a.drain(t)

	// Back online: no seen, in the announcement or in listings, and none in
	// the user's own you.
	time.Sleep(2 * time.Millisecond)
	b, _ = dialOrigin(t, httpServer, testPasskeyOrigin)
	expectNoSeen(t, "you after a reconnect", b.result(t, "auth", "resume", map[string]any{"scheme": "token", "token": token})["you"])
	b.userID = userID
	expectStatus(t, a, userID, "online")
	expectListedSeen(t, a, userID, "online", "")
	_, own := listedObjects(t, b, userID)
	if _, has := own["seen"]; has || own["status"] != "online" {
		t.Fatalf("own listed object: %#v", own)
	}
	b.status(t, map[string]any{"idle": 300})
	expectStatus(t, a, userID, "idle")
	expectListedSeen(t, a, userID, "idle", "")
	b.status(t, map[string]any{"idle": false})
	expectStatus(t, a, userID, "online")
	// In order, so each is a change and the last leaves "" for the dnd
	// below to change.
	for _, status := range []string{"dnd", ""} {
		setStatus(t, b, status)
		expectProfileStatus(t, a, userID, status)
		expectListedSeen(t, a, userID, status, "")
	}
	// dnd shows offline without a connection, with a new seen.
	setStatus(t, b, "dnd")
	expectProfileStatus(t, a, userID, "dnd")
	_ = b.ws.Close(websocket.StatusNormalClosure, "bye")
	later := expectStatus(t, a, userID, "offline")
	seenAfter(t, later, seen)
	expectListedSeen(t, a, userID, "offline", later)
	a.expectQuiet(t)
}

// A connected user who chooses invisible is shown offline with seen, the
// same as a disconnect, in the profile change others are sent, but not in
// their own you (§4.5.2). While the chosen status stays invisible, their
// connections coming and going send nothing and leave seen as it is, in
// listings too, so seen tells nothing of them (§5.3). Choosing online again
// shows them without seen, and a later disconnect brings a new one.
func TestSeenWithInvisible(t *testing.T) {
	app, httpServer := passkeyTestServer(t)
	a := dialTestClient(t, httpServer)
	b, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	registered := registerTestPasskey(t, b, newTestAuthenticator(t))
	token, userID := registered["token"], b.userID
	expectMembership(t, a, "general", userID, true)
	watching(a)
	a.drain(t)
	b.drain(t)
	other, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	other.result(t, "auth", "other", map[string]any{"scheme": "token", "token": token})
	other.drain(t)
	a.expectQuiet(t)

	you := b.result(t, "me", statusID(), map[string]any{"status": "invisible"})["you"]
	expectNoSeen(t, "me result", you)
	expectNoSeen(t, "you on another connection", other.notification(t, "user")["you"])
	profile, _ := notificationParams(t, a.read(t), "user")["new"].(map[string]any)
	seen, _ := profile["seen"].(string)
	if profile["user_id"] != userID || profile["status"] != "offline" {
		t.Fatalf("profile change to invisible = %#v", profile)
	}
	checkSeen(t, seen)
	expectListedSeen(t, a, userID, "offline", seen)

	// Connections come and go: nothing is sent, and seen stays.
	time.Sleep(2 * time.Millisecond)
	third, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	expectNoSeen(t, "you of an invisible user", third.result(t, "auth", "third", map[string]any{"scheme": "token", "token": token})["you"])
	b.status(t, map[string]any{"idle": 300})
	_ = third.ws.Close(websocket.StatusNormalClosure, "bye")
	_ = other.ws.Close(websocket.StatusNormalClosure, "bye")
	_ = b.ws.Close(websocket.StatusNormalClosure, "bye")
	waitConnections(t, app, userID, 0)
	a.expectQuiet(t)
	expectListedSeen(t, a, userID, "offline", seen)
	b, _ = dialOrigin(t, httpServer, testPasskeyOrigin)
	b.result(t, "auth", "back", map[string]any{"scheme": "token", "token": token})
	b.userID = userID
	b.drain(t)
	a.expectQuiet(t)
	expectListedSeen(t, a, userID, "offline", seen)
	// The user's own listed object is the shown one, seen included; the
	// chosen status is in you.
	if _, own := listedObjects(t, b, userID); own["status"] != "offline" || own["seen"] != seen {
		t.Fatalf("own listed object while invisible: %#v", own)
	}

	setStatus(t, b, "online")
	if profile := notificationParams(t, a.read(t), "user")["new"].(map[string]any); profile["status"] != "online" || profile["seen"] != nil {
		t.Fatalf("profile change to online = %#v", profile)
	}
	expectListedSeen(t, a, userID, "online", "")
	_ = b.ws.Close(websocket.StatusNormalClosure, "bye")
	seenAfter(t, expectStatus(t, a, userID, "offline"), seen)
}

// seen is kept in the store with the user, so after a restart listings
// carry the one from before it, until the shown status next becomes
// offline (§4.5.2). A user without one, such as one stored before seen
// was kept, is listed without it.
func TestSeenSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	open := func() store.Store {
		s, err := store.OpenSQLite(filepath.Join(dir, "aprond.db"))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	app, httpServer, stop := startWithStore(t, open(), filepath.Join(dir, "uploads"))
	a, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	registerTestPasskey(t, a, newTestAuthenticator(t))
	b, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	registered := registerTestPasskey(t, b, newTestAuthenticator(t))
	userID := b.userID
	expectMembership(t, a, "general", userID, true)
	addAccount(t, app, "never", a, b)
	watching(a)
	a.drain(t)
	setStatus(t, b, "invisible")
	expectProfileStatus(t, a, userID, "offline")
	_, user := listedObjects(t, a, userID)
	seen, _ := user["seen"].(string)
	checkSeen(t, seen)
	expectListedSeen(t, a, "never", "offline", "")
	stop()

	_, httpServer, _ = startWithStore(t, open(), filepath.Join(dir, "uploads"))
	c, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	you := c.result(t, "auth", "resume", map[string]any{"scheme": "token", "token": registered["token"]})["you"]
	if you.(map[string]any)["status"] != "invisible" {
		t.Fatalf("you after restart: %#v", you)
	}
	expectNoSeen(t, "you after restart", you)
	c.userID = userID
	c.drain(t)
	expectListedSeen(t, c, userID, "offline", seen)
	expectListedSeen(t, c, "never", "offline", "")
}

// resume signs a new connection in to userID's account with its session
// token, and drains what follows the result.
func resume(t *testing.T, httpServer *httptest.Server, token any, userID string) *testClient {
	t.Helper()
	c, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	c.result(t, "auth", c.nextID("resume"), map[string]any{"scheme": "token", "token": token})
	c.userID = userID
	c.drain(t)
	return c
}

// seenMillis is seen, in the format of a log_id, as a number.
func seenMillis(t *testing.T, seen string) int64 {
	t.Helper()
	checkSeen(t, seen)
	ms, _ := strconv.ParseInt(seen, 10, 64)
	return ms
}

// expectSeenBetween requires seen to be no earlier than from and no later
// than to, in Unix milliseconds.
func expectSeenBetween(t *testing.T, what, seen string, from, to int64) {
	t.Helper()
	if ms := seenMillis(t, seen); ms < from || ms > to {
		t.Fatalf("%s: seen %d is not within [%d, %d]", what, ms, from, to)
	}
}

// A shown offline that waits past the user's burst of status changes, to
// be sent statusCoalesce later, carries seen too: when the shown status
// became offline, not when it was sent (§4.5.2).
func TestCoalescedOfflineCarriesSeen(t *testing.T) {
	app, httpServer := passkeyTestServer(t)
	a := dialTestClient(t, httpServer)
	b, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	registerTestPasskey(t, b, newTestAuthenticator(t))
	userID := b.userID
	expectMembership(t, a, "general", userID, true)
	watching(a)
	a.drain(t)
	// b's first status, online when it signed in, took one of the burst.
	for i := range statusBurst - 1 {
		idle := i%2 == 0
		b.status(t, map[string]any{"idle": idleParam(idle)})
		expectStatus(t, a, userID, map[bool]string{true: "idle", false: "online"}[idle])
	}
	closing := time.Now().UnixMilli()
	_ = b.ws.Close(websocket.StatusNormalClosure, "bye")
	waitConnections(t, app, userID, 0)
	closed := time.Now().UnixMilli()
	a.expectQuiet(t)
	time.Sleep(statusCoalesce + 200*time.Millisecond)
	seen := expectStatus(t, a, userID, "offline")
	expectSeenBetween(t, "coalesced offline", seen, closing, closed)
	expectListedSeen(t, a, userID, "offline", seen)
	a.expectQuiet(t)
}

// A flap that coalescing hides from others, online and back to offline
// within statusCoalesce once the user's burst is spent, is hidden
// everywhere: no listing shows it, and seen stays the one announced, so
// nobody sees seen change while the shown status stays offline (§4.5.2).
func TestHiddenFlapKeepsSeen(t *testing.T) {
	app, httpServer := passkeyTestServer(t)
	a := dialTestClient(t, httpServer)
	b, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	token := registerTestPasskey(t, b, newTestAuthenticator(t))["token"]
	userID := b.userID
	expectMembership(t, a, "general", userID, true)
	watching(a)
	a.drain(t)
	// b has one change of its burst left, which refills no more.
	app.mu.Lock()
	app.users[userID].statusLimit = rate.NewLimiter(rate.Every(time.Hour), 1)
	app.unlock()
	_ = b.ws.Close(websocket.StatusNormalClosure, "bye")
	seen := expectStatus(t, a, userID, "offline")
	expectListedSeen(t, a, userID, "offline", seen)

	time.Sleep(2 * time.Millisecond)
	flap := resume(t, httpServer, token, userID)
	a.expectQuiet(t)
	// Until the change is sent, a listing shows what was last announced.
	expectListedSeen(t, a, userID, "offline", seen)
	time.Sleep(2 * time.Millisecond)
	_ = flap.ws.Close(websocket.StatusNormalClosure, "bye")
	waitConnections(t, app, userID, 0)
	a.expectQuiet(t)
	expectListedSeen(t, a, userID, "offline", seen)
	time.Sleep(statusCoalesce + 200*time.Millisecond)
	a.expectQuiet(t)
	expectListedSeen(t, a, userID, "offline", seen)
	app.mu.RLock()
	kept := app.users[userID].seen
	app.mu.RUnlock()
	if kept != seenMillis(t, seen) {
		t.Fatalf("seen after a hidden flap = %d, want %s", kept, seen)
	}
}

// Closing one of a user's connections while another stays open changes
// nothing others see: nothing is sent, and seen stays as it was (§4.5.2).
// The last one's close sets a later seen.
func TestSeenOnlyWithTheLastConnection(t *testing.T) {
	app, httpServer := passkeyTestServer(t)
	a := dialTestClient(t, httpServer)
	b, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	token := registerTestPasskey(t, b, newTestAuthenticator(t))["token"]
	userID := b.userID
	expectMembership(t, a, "general", userID, true)
	watching(a)
	a.drain(t)
	_ = b.ws.Close(websocket.StatusNormalClosure, "bye")
	seen := expectStatus(t, a, userID, "offline")

	time.Sleep(2 * time.Millisecond)
	first := resume(t, httpServer, token, userID)
	expectStatus(t, a, userID, "online")
	second := resume(t, httpServer, token, userID)
	a.expectQuiet(t)
	_ = first.ws.Close(websocket.StatusNormalClosure, "bye")
	waitConnections(t, app, userID, 1)
	a.expectQuiet(t)
	expectListedSeen(t, a, userID, "online", "")
	app.mu.RLock()
	kept := app.users[userID].seen
	app.mu.RUnlock()
	if kept != seenMillis(t, seen) {
		t.Fatalf("seen after closing one of two connections = %d, want %s", kept, seen)
	}

	_ = second.ws.Close(websocket.StatusNormalClosure, "bye")
	later := expectStatus(t, a, userID, "offline")
	seenAfter(t, later, seen)
	expectListedSeen(t, a, userID, "offline", later)
}

// When an account's last connection signs in as another account, the
// first is left without connections: others are told it is offline, with
// seen, before the status of the second (§4.5.2).
func TestSeenWhenTheLastConnectionSignsInElsewhere(t *testing.T) {
	_, httpServer := passkeyTestServer(t)
	a := dialTestClient(t, httpServer)
	b, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	registerTestPasskey(t, b, newTestAuthenticator(t))
	expectMembership(t, a, "general", b.userID, true)
	c, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	other := registerTestPasskey(t, c, newTestAuthenticator(t))
	expectMembership(t, a, "general", c.userID, true)
	watching(a)
	a.drain(t)
	_ = c.ws.Close(websocket.StatusNormalClosure, "bye")
	expectStatus(t, a, c.userID, "offline")

	b.drain(t)
	switching := time.Now().UnixMilli()
	b.result(t, "auth", "elsewhere", map[string]any{"scheme": "token", "token": other["token"]})
	switched := time.Now().UnixMilli()
	seen := expectStatus(t, a, b.userID, "offline")
	expectSeenBetween(t, "the account left", seen, switching, switched)
	expectStatus(t, a, c.userID, "online")
	a.expectQuiet(t)
	expectListedSeen(t, a, b.userID, "offline", seen)
}

// seen goes with a shown offline in every listing (§4.5.2): the members and
// users of a room_update joined, and of a room_list filtered to one
// user_id.
func TestListingsCarrySeen(t *testing.T) {
	_, httpServer := passkeyTestServer(t)
	a := dialTestClient(t, httpServer)
	b, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	registerTestPasskey(t, b, newTestAuthenticator(t))
	userID := b.userID
	expectMembership(t, a, "general", userID, true)
	room, _ := saveRoom(t, b, "room", map[string]any{"title": "Room"})
	watching(a)
	a.drain(t)
	_ = b.ws.Close(websocket.StatusNormalClosure, "bye")
	seen := expectStatus(t, a, userID, "offline")
	check := func(what string, lists ...[]any) {
		t.Helper()
		for _, list := range lists {
			var found map[string]any
			for _, object := range list {
				if object := object.(map[string]any); object["user_id"] == userID {
					found = object
				}
			}
			if found == nil || found["status"] != "offline" || found["seen"] != seen {
				t.Fatalf("%s: %#v, want %s offline with seen %s", what, list, userID, seen)
			}
		}
	}

	before, _ := a.request(t, "room_join", "join", map[string]any{"room_id": room})
	update := notificationParams(t, before[0], "room_update")
	check("room_update joined", update["joined"].([]any)[0].(map[string]any)["members"].([]any), update["users"].([]any))
	listed := listRooms(t, a, map[string]any{"room_id": room, "members": true, "user_id": userID})
	members := listed["joined"].([]any)[0].(map[string]any)["members"].([]any)
	if len(members) != 1 {
		t.Fatalf("room_list members for %s: %#v", userID, members)
	}
	check("room_list by user_id", members, listed["users"].([]any))
}

// waitStored waits until the SQLite store at path, which a running server
// writes in the background, holds a user entry for userID that want
// accepts, and returns it.
func waitStored(t *testing.T, path, userID string, want func(storedUser) bool) storedUser {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		s, err := store.OpenSQLite(path)
		if err != nil {
			t.Fatal(err)
		}
		var stored storedUser
		found := false
		err = s.Load(func(e store.Entry) error {
			if e.Kind != entryUser || e.ID != userID {
				return nil
			}
			found = true
			return json.Unmarshal(e.Value, &stored)
		})
		_ = s.Close()
		if err != nil {
			t.Fatal(err)
		}
		if found && want(stored) {
			return stored
		}
		if time.Now().After(deadline) {
			t.Fatalf("stored %s = %#v (found %v)", userID, stored, found)
		}
	}
}

// A crash closes no connection, so nothing sets the seen of the accounts
// then connected. A server started on the store afterwards gives each one
// stored as shown other than offline a seen at its start, which is no
// earlier than when others last saw it (§4.5.2): one that had come back
// since its last seen, and one that had never been offline. Its seen is
// stored again. An invisible account, always shown offline, keeps its seen,
// so a crash tells nothing of its connections; one shown "" stays so, and
// one that never connected stays without seen.
func TestSeenAfterCrash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aprond.db")
	open := func() store.Store {
		s, err := store.OpenSQLite(path)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	app, httpServer, _ := startWithStore(t, open(), filepath.Join(dir, "uploads"))
	a := dialTestClient(t, httpServer)
	signUp := func() (*testClient, any) {
		c, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
		token := registerTestPasskey(t, c, newTestAuthenticator(t))["token"]
		expectMembership(t, a, "general", c.userID, true)
		return c, token
	}
	back, token := signUp()
	fresh, _ := signUp()
	hidden, _ := signUp()
	none, _ := signUp()
	addAccount(t, app, "never", a)
	watching(a)
	for _, c := range []*testClient{a, back, fresh, hidden, none} {
		c.drain(t)
	}

	_ = back.ws.Close(websocket.StatusNormalClosure, "bye")
	left := seenMillis(t, expectStatus(t, a, back.userID, "offline"))
	time.Sleep(2 * time.Millisecond)
	back = resume(t, httpServer, token, back.userID)
	expectStatus(t, a, back.userID, "online")
	returned := time.Now().UnixMilli()
	setStatus(t, hidden, "invisible")
	expectProfileStatus(t, a, hidden.userID, "offline")
	_, user := listedObjects(t, a, hidden.userID)
	hiddenSeen, _ := user["seen"].(string)
	none.drain(t)
	setStatus(t, none, "")
	expectProfileStatus(t, a, none.userID, "")

	// The store has all of it, and the server crashes: it goes on running,
	// unseen, while another starts on the same file.
	waitStored(t, path, back.userID, func(u storedUser) bool { return u.Shown && u.Seen == left })
	waitStored(t, path, fresh.userID, func(u storedUser) bool { return u.Shown && u.Seen == 0 })
	waitStored(t, path, hidden.userID, func(u storedUser) bool { return !u.Shown && u.Seen == seenMillis(t, hiddenSeen) })
	waitStored(t, path, none.userID, func(u storedUser) bool { return u.Shown && u.Seen == 0 })
	crashed := time.Now().UnixMilli()
	_, httpServer, _ = startWithStore(t, open(), filepath.Join(dir, "uploads"))
	restored := time.Now().UnixMilli()
	g := dialTestClient(t, httpServer)
	g.drain(t)
	for _, id := range []string{back.userID, fresh.userID} {
		_, user := listedObjects(t, g, id)
		seen, _ := user["seen"].(string)
		expectSeenBetween(t, id+" after a crash", seen, max(crashed, returned), restored)
		expectListedSeen(t, g, id, "offline", seen)
		waitStored(t, path, id, func(u storedUser) bool { return !u.Shown && u.Seen == seenMillis(t, seen) })
	}
	expectListedSeen(t, g, hidden.userID, "offline", hiddenSeen)
	expectListedSeen(t, g, none.userID, "", "")
	expectListedSeen(t, g, "never", "offline", "")
}
