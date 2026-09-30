package server

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestShutdownClosesConnections(t *testing.T) {
	app, httpServer := newTestServer(t, DefaultConfig())
	closed := dialTestClient(t, httpServer)
	active := dialTestClient(t, httpServer)
	_ = closed.ws.Close(websocket.StatusNormalClosure, "finished")
	// The closed guest's leave is logged in general.
	expectMembership(t, active, "general", closed.userID, false)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown with accepted connections: %v", err)
	}
	// The connection is told to retry later (an error without id), then
	// closed.
	failure := active.read(t)
	if failure["id"] != nil || failure["error"].(map[string]any)["code"] != float64(codeRetryAfter) {
		t.Fatalf("shutdown frame = %#v", failure)
	}
	if _, _, err := active.ws.Read(ctx); err == nil {
		t.Fatal("active connection remained open after shutdown")
	}
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("repeated shutdown: %v", err)
	}
	_, response, err := websocket.Dial(ctx, "ws"+httpServer.URL[len("http"):]+"/ws", nil)
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("connection after shutdown: response=%v, error=%v", response, err)
	}
}

func TestServerFrame(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	_, frame := dialRaw(t, httpServer)
	params := frame["params"].(map[string]any)
	if params["protocol"] != float64(7) || params["ping"] != float64(30) {
		t.Fatalf("protocol and ping: %#v", params)
	}
	if !reflect.DeepEqual(params["caps"], []any{"history", "edit", "rooms", "reactions", "activity", "embed:upload", "embed:stream", "command"}) {
		t.Fatalf("caps: %#v", params["caps"])
	}
	if !reflect.DeepEqual(params["auth"], []any{"guest"}) {
		t.Fatalf("auth: %#v", params["auth"])
	}
	if !reflect.DeepEqual(params["push"], map[string]any{"relay": map[string]any{}}) {
		t.Fatalf("push: %#v", params["push"])
	}
	limits := params["ext"].(map[string]any)["apron-go"].(map[string]any)
	if limits["max_upload_bytes"] != float64(defaultMaxUploadBytes) || limits["stream_keep_bytes"] != float64(defaultStreamKeepBytes) {
		t.Fatalf("ext limits: %#v", limits)
	}

	config := DefaultConfig()
	config.DisablePush = true
	_, quiet := newTestServer(t, config)
	c, frame := dialRaw(t, quiet)
	if _, has := frame["params"].(map[string]any)["push"]; has {
		t.Fatal("push advertised while disabled")
	}
	guestAuth(t, c)
	c.expectError(t, "push_register", "p", map[string]any{"kind": "relay", "url": "https://relay.example/p"}, codeUnsupported)
}

func TestGuestAuth(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	c, _ := dialRaw(t, httpServer)
	c.expectError(t, "message", "early", map[string]any{"room_id": "general", "body": map[string]any{}}, codeDenied)
	c.expectError(t, "auth", "bad-scheme", map[string]any{"scheme": "password"}, codeUnsupported)
	// The new guest's join to general is a logged membership, delivered to
	// its connection before the auth result (§1).
	before, result := c.request(t, "auth", "auth", map[string]any{"scheme": "guest", "name": "Ada"})
	you := result["you"].(map[string]any)
	if you["user_id"] != "guest_1" || you["name"] != "Ada" || len(before) != 1 {
		t.Fatalf("guest identity %#v after %#v", you, before)
	}
	membership := notificationParams(t, before[0], "membership")
	wantMembership := map[string]any{
		"log_id": membership["log_id"], "room_id": "general",
		"members": []any{map[string]any{"user": map[string]any{"user_id": "guest_1", "name": "Ada"}, "joined": true}},
	}
	if !reflect.DeepEqual(membership, wantMembership) {
		t.Fatalf("membership = %#v, want %#v", membership, wantMembership)
	}
	// No room_update follows auth; the client lists the rooms it has joined.
	c.expectQuiet(t)
	listed := listRooms(t, c, map[string]any{"filter": "joined", "members": true})
	if _, has := listed["not_joined"]; has || len(listed["joined"].([]any)) != 1 {
		t.Fatalf("joined rooms: %#v", listed)
	}
	general := listed["joined"].([]any)[0].(map[string]any)
	logID := general["log_id"]
	parseID(t, logID)
	want := map[string]any{
		"room_id": "general", "title": "General", "log_id": logID, "latest_log_id": membership["log_id"], "history_log_id": logID,
		"members": []any{map[string]any{"user_id": "guest_1"}},
	}
	if !reflect.DeepEqual(general, want) || !reflect.DeepEqual(listed["users"], []any{you}) {
		t.Fatalf("general = %#v with users %#v, want %#v", general, listed["users"], want)
	}
	c.expectQuiet(t)
}

func TestAuthHonorsRequestedUserIDs(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	auth := func(id string, params map[string]any) string {
		t.Helper()
		c, _ := dialRaw(t, httpServer)
		params["scheme"] = "guest"
		_, result := c.request(t, "auth", id, params)
		return result["you"].(map[string]any)["user_id"].(string)
	}
	// An honored request takes no guest number.
	if got := auth("ada", map[string]any{"user_id": "ada", "name": "Ada"}); got != "ada" {
		t.Fatalf("requested user_id: %q", got)
	}
	// A used ID, in any case, a system identity, a room, IDs outside the
	// mentionable set, and anything in the counter's guest_ namespace are not
	// honored; each such auth takes the next guest number.
	next := 1
	for i, requested := range []string{"ada", "ADA", "@server", "~server", "~alice", "general", "General", "1724803200042", "bad id", "trailing.", "", "guest_1", "guest_99", "GUEST_98", "Guest_7", "guest_05", "guest_abc", "guest_"} {
		want := fmt.Sprintf("guest_%d", next)
		if got := auth(fmt.Sprint("r", i), map[string]any{"user_id": requested}); got != want {
			t.Fatalf("requested %q was assigned %q, want %q", requested, got, want)
		}
		next++
	}
	// Refused guest_<n> requests claimed nothing: the counter reaches those
	// numbers in sequence.
	for i := next; i <= 99; i++ {
		if got, want := auth(fmt.Sprint("n", i), map[string]any{}), fmt.Sprintf("guest_%d", i); got != want {
			t.Fatalf("guest %d was assigned %q", i, got)
		}
	}
	// Prefixes that merely resemble the namespace are ordinary requests.
	for _, requested := range []string{"guest", "guest-1", "guests_1"} {
		if got := auth(requested, map[string]any{"user_id": requested}); got != requested {
			t.Fatalf("requested %q was assigned %q", requested, got)
		}
	}
	if got := auth("last", map[string]any{}); got != "guest_100" {
		t.Fatalf("counter after honored requests: %q", got)
	}
	c, _ := dialRaw(t, httpServer)
	c.expectError(t, "auth", "bad", map[string]any{"scheme": "guest", "user_id": 5}, codeInvalidParams)
}

func TestGuestNumbersAreSequentialAndNeverReused(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 3)
	// Retire guest_1: its last connection closes, and guest_2 sees the leave.
	if err := clients[0].ws.Close(websocket.StatusNormalClosure, "bye"); err != nil {
		t.Fatalf("close: %v", err)
	}
	for _, observer := range clients[1:] {
		expectMembership(t, observer, "general", "guest_1", false)
	}
	// Neither a new guest nor a request for the retired ID gets guest_1.
	d := dialTestClient(t, httpServer)
	if d.userID != "guest_4" {
		t.Fatalf("guest after a retirement was assigned %q", d.userID)
	}
	e, _ := dialRaw(t, httpServer)
	_, result := e.request(t, "auth", "e", map[string]any{"scheme": "guest", "user_id": "guest_1"})
	if got := result["you"].(map[string]any)["user_id"]; got != "guest_5" {
		t.Fatalf("request for a retired guest ID was assigned %q", got)
	}
}

func TestLivenessPing(t *testing.T) {
	config := DefaultConfig()
	config.PingInterval = 20 * time.Millisecond
	config.PingTimeout = 200 * time.Millisecond
	_, httpServer := newTestServer(t, config)
	c, frame := dialRaw(t, httpServer)
	if frame["params"].(map[string]any)["ping"] != float64(1) {
		t.Fatalf("ping interval rounds up to one second: %#v", frame)
	}
	// Pings are answered before authentication too, exact bytes or not.
	ping := func(raw string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := c.ws.Write(ctx, websocket.MessageText, []byte(raw)); err != nil {
			t.Fatal(err)
		}
		if pong := c.read(t); !reflect.DeepEqual(pong, map[string]any{"method": "pong"}) {
			t.Fatalf("%s answered with %#v", raw, pong)
		}
	}
	ping(`{"method":"ping"}`)
	ping(`{ "method": "ping", "params": {} }`)
	c.request(t, "auth", "auth", map[string]any{"scheme": "guest"})
	ping(`{"method":"ping"}`)

	// A connection that pinged and then fell silent is closed.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for {
		if _, _, err := c.ws.Read(ctx); err != nil {
			if ctx.Err() != nil {
				t.Fatal("a silent connection stayed open")
			}
			break
		}
	}
	// One that never pinged stays open while it answers WebSocket pings.
	quiet := dialTestClient(t, httpServer)
	for deadline := time.Now().Add(500 * time.Millisecond); time.Now().Before(deadline); {
		quiet.expectQuiet(t)
	}
}

// auth is a barrier (§3.2): requests pipelined behind it run after it, as
// the authenticated user, and are denied when it fails.
func TestAuthIsABarrier(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	c, _ := dialRaw(t, httpServer)
	c.write(t, map[string]any{"method": "auth", "id": "auth", "params": map[string]any{"scheme": "guest", "name": "Ada"}})
	c.write(t, map[string]any{"method": "room_list", "id": "list", "params": map[string]any{"filter": "joined", "members": true}})
	c.write(t, map[string]any{"method": "history", "id": "history", "params": map[string]any{}})
	c.write(t, map[string]any{"method": "message", "id": "post", "params": map[string]any{"body": map[string]any{"text": "hi"}}})
	frames := []map[string]any{}
	for len(frames) < 6 {
		frames = append(frames, c.read(t))
	}
	if got := methods(frames); !reflect.DeepEqual(got, []string{"membership", "reply", "reply", "reply", "message", "reply"}) {
		t.Fatalf("pipelined frames: %v", got)
	}
	membership := notificationParams(t, frames[0], "membership")
	for i, id := range []string{"auth", "list", "history"} {
		if frames[i+1]["id"] != id || frames[i+1]["result"] == nil {
			t.Fatalf("reply %d = %#v, want a result for %s", i, frames[i+1], id)
		}
	}
	listed := frames[2]["result"].(map[string]any)
	if got := roomIDs(t, listed["joined"]); !reflect.DeepEqual(got, []string{"general"}) || !reflect.DeepEqual(memberIDs(listed["joined"].([]any)[0].(map[string]any)), []string{"guest_1"}) {
		t.Fatalf("room_list behind auth: %#v", listed)
	}
	page := frames[3]["result"].(map[string]any)
	if got := logIDs(t, page, "membership"); !reflect.DeepEqual(got, []string{membership["log_id"].(string)}) || page["latest_log_id"] != membership["log_id"] {
		t.Fatalf("history behind auth: %#v", page)
	}
	if frames[5]["id"] != "post" || frames[4]["params"].(map[string]any)["message_id"] != frames[5]["result"].(map[string]any)["message_id"] {
		t.Fatalf("message behind auth: %#v", frames[4:])
	}

	// Requests behind a failed auth are denied; notifications are ignored.
	for i, auth := range []map[string]any{{"scheme": "password"}, {"scheme": "guest", "user_id": 5}} {
		c, _ := dialRaw(t, httpServer)
		c.write(t, map[string]any{"method": "auth", "id": "auth", "params": auth})
		c.write(t, map[string]any{"method": "activity", "params": map[string]any{"room_id": "general", "typing": 3}})
		c.write(t, map[string]any{"method": "room_list", "id": "list", "params": map[string]any{"filter": "joined"}})
		c.write(t, map[string]any{"method": "history", "id": "history", "params": map[string]any{}})
		failed := c.read(t)
		if failed["id"] != "auth" || failed["error"] == nil {
			t.Fatalf("auth %d: %#v", i, failed)
		}
		for _, id := range []string{"list", "history"} {
			if denied := c.read(t); denied["id"] != id || denied["error"].(map[string]any)["code"] != float64(codeDenied) {
				t.Fatalf("auth %d: %s behind a failed auth: %#v", i, id, denied)
			}
		}
	}
}

func TestUnimplementedAndUnknownMethodsAreUnsupported(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	c := dialTestClient(t, httpServer)
	for _, method := range []string{"frobnicate", "room_teleport"} {
		c.expectError(t, method, method, map[string]any{"room_id": "general"}, codeUnsupported)
	}
	c.expectQuiet(t)
}

func TestMessageSnapshotsReplaceEditableState(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	owner := dialTestClient(t, httpServer)
	observer := dialTestClient(t, httpServer)
	expectMembership(t, owner, "general", observer.userID, true)
	owner.result(t, "me", "name", map[string]any{"name": "Alice"})
	if renamed := observer.notification(t, "user"); !reflect.DeepEqual(renamed, map[string]any{"new": map[string]any{"user_id": "guest_1", "name": "Alice"}}) {
		t.Fatalf("rename notification: %#v", renamed)
	}
	ext := map[string]any{"irc": map[string]any{"nick": "ada_"}}
	id, creation := save(t, owner, "create", map[string]any{
		"from":    map[string]any{"user_id": "forged"},
		"log_id":  "123",
		"body":    map[string]any{"text": "hello", "format": "plain", "embeds": []any{map[string]any{"kind": "file"}}},
		"ext":     ext,
		"custom":  true,
		"deleted": false,
	})
	if first := observer.notification(t, "message"); !reflect.DeepEqual(first, creation) {
		t.Fatalf("observer snapshot %#v differs from %#v", first, creation)
	}
	wantKeys := []string{"body", "ext", "from", "log_id", "message_id", "room_id"}
	if creation["log_id"] != id || !reflect.DeepEqual(slices.Sorted(maps.Keys(creation)), wantKeys) {
		t.Fatalf("creation snapshot: %#v", creation)
	}
	from := creation["from"].(map[string]any)
	if from["user_id"] != "guest_1" || from["name"] != "Alice" || !reflect.DeepEqual(creation["ext"], ext) {
		t.Fatalf("creation fields: %#v", creation)
	}

	owner.result(t, "me", "rename", map[string]any{"name": "Later"})
	observer.notification(t, "user")
	stable, edit := save(t, owner, "edit", map[string]any{"message_id": id, "body": map[string]any{"text": "edited"}, "from": nil})
	observer.notification(t, "message")
	if stable != id || parseID(t, edit["log_id"]) <= parseID(t, id) {
		t.Fatalf("edit snapshot: %#v", edit)
	}
	if _, kept := edit["ext"]; kept || len(edit["body"].(map[string]any)) != 1 || edit["from"].(map[string]any)["name"] != "Alice" {
		t.Fatalf("replacement merged editable fields or changed author: %#v", edit)
	}

	_, deleted := save(t, owner, "delete", map[string]any{"message_id": id, "deleted": true, "body": "discard even invalid body"})
	observer.notification(t, "message")
	if _, exists := deleted["body"]; exists || deleted["deleted"] != true {
		t.Fatalf("tombstone: %#v", deleted)
	}

	page := historyPage(t, owner, "general", map[string]any{"after": "0"})
	entries := records(t, page, "messages")
	if len(entries) != 3 {
		t.Fatalf("history: %#v", page)
	}
	// Deletion redacts earlier snapshots into tombstones at their log_ids.
	redacted := func(snapshot map[string]any) map[string]any {
		value := maps.Clone(snapshot)
		delete(value, "body")
		value["deleted"] = true
		return value
	}
	for i, expected := range []map[string]any{redacted(creation), redacted(edit), deleted} {
		if !reflect.DeepEqual(entries[i], any(expected)) {
			t.Fatalf("history entry %d = %#v, want %#v", i, entries[i], expected)
		}
	}
	observer.expectError(t, "message", "forged", map[string]any{"room_id": "general", "message_id": id, "body": map[string]any{"text": "forged"}}, codeDenied)
}

func TestLogIDsFormOneSequenceAcrossRoomsAndKinds(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	c := dialTestClient(t, httpServer)
	last := parseID(t, listRooms(t, c, map[string]any{"room_id": "general"})["joined"].([]any)[0].(map[string]any)["log_id"])
	check := func(label string, value any) {
		t.Helper()
		id := parseID(t, value)
		if id <= last {
			t.Fatalf("%s log_id %d does not follow %d", label, id, last)
		}
		last = id
	}
	id, message := save(t, c, "m1", map[string]any{"body": map[string]any{"text": "a"}})
	check("message", message["log_id"])
	room, record := saveRoom(t, c, "r1", map[string]any{"title": "Ops"})
	check("room", record["log_id"])
	if room != record["log_id"] {
		t.Fatalf("room_id %q should be its creation log_id %q", room, record["log_id"])
	}
	check("reactions", react(t, c, "x1", id, "👍")["log_id"])
	_, other := save(t, c, "m2", map[string]any{"room_id": room, "body": map[string]any{"text": "b"}})
	check("message in other room", other["log_id"])
}

func TestRepliesMayCrossRooms(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	c := dialTestClient(t, httpServer)
	body := map[string]any{"text": "hello"}
	root, _ := save(t, c, "root", map[string]any{"body": body})
	ops, _ := saveRoom(t, c, "ops", map[string]any{"title": "Ops"})

	reply, created := save(t, c, "reply", map[string]any{"room_id": ops, "body": body, "reply_to": map[string]any{"message_id": root}})
	if !reflect.DeepEqual(created["reply_to"], map[string]any{"message_id": root}) {
		t.Fatalf("cross-room reply_to: %#v", created)
	}
	// A client may pass back a snapshot; only its message_id is used.
	_, echoed := save(t, c, "reply-snapshot", map[string]any{"body": body, "reply_to": map[string]any{"message_id": reply, "room_id": ops, "body": body}})
	if !reflect.DeepEqual(echoed["reply_to"], map[string]any{"message_id": reply}) {
		t.Fatalf("reply_to not bare: %#v", echoed)
	}

	before := historyPage(t, c, "general", map[string]any{})
	for i, params := range []map[string]any{
		{"reply_to": nil}, {"reply_to": 123}, {"reply_to": root}, {"reply_to": map[string]any{}},
		{"reply_to": map[string]any{"message_id": ""}}, {"reply_to": map[string]any{"message_id": "0"}},
		{"reply_to": map[string]any{"message_id": "bad"}}, {"reply_to": map[string]any{"message_id": "999"}},
		{"message_id": root, "reply_to": map[string]any{"message_id": root}},
		{"reply_to": map[string]any{"message_id": root}, "room_id": "missing-room"},
	} {
		if _, ok := params["room_id"]; !ok {
			params["room_id"] = "general"
		}
		params["body"] = body
		c.expectError(t, "message", fmt.Sprint("bad-reply-", i), params, codeInvalidParams)
	}
	after := historyPage(t, c, "general", map[string]any{})
	if before["last_log_id"] != after["last_log_id"] {
		t.Fatal("rejected reply changed history")
	}

	// Replies to tombstones remain valid, and omitting reply_to on a save removes it.
	_, _ = save(t, c, "delete-root", map[string]any{"message_id": root, "deleted": true})
	_, _ = save(t, c, "reply-to-tombstone", map[string]any{"body": body, "reply_to": map[string]any{"message_id": root}})
	_, removed := save(t, c, "remove-reply", map[string]any{"message_id": reply, "room_id": ops, "body": body})
	if _, present := removed["reply_to"]; present {
		t.Fatal("omitted reply_to was retained")
	}
}

func TestRoomSetCreatesAndEditsRoomsAndThreads(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	c, observer := clients[0], clients[1]
	description := "Deploy status\n*second* line"

	// A new thread joins its creator, logging the creator's membership; the
	// parent's other members learn of it as updated, without joining. A
	// thread without a title gets one from its description.
	thread, record := saveRoom(t, c, "thread", map[string]any{"parent_room_id": "general", "description": description})
	if observed := roomUpdated(t, observer, "updated"); !reflect.DeepEqual(observed, record) {
		t.Fatalf("observer room %#v differs from %#v", observed, record)
	}
	want := map[string]any{
		"room_id": thread, "log_id": thread, "parent_room_id": "general", "title": "Deploy status",
		"description": description, "latest_log_id": record["latest_log_id"], "history_log_id": thread,
	}
	if !reflect.DeepEqual(record, want) {
		t.Fatalf("thread record = %#v, want %#v", record, want)
	}
	// The observer has not joined the thread, so no room_update for its nested
	// thread reaches them.
	_, untitled := saveRoom(t, c, "untitled", map[string]any{"parent_room_id": thread})
	if untitled["title"] != "Thread" || untitled["parent_room_id"] != thread {
		t.Fatalf("nested untitled thread: %#v", untitled)
	}
	observer.expectQuiet(t)

	// Edits replace every client field except parent_room_id, and reach the
	// room's members and, for a thread, the parent's.
	ext := map[string]any{"app": map[string]any{"pinned": true}}
	sameID, updated := saveRoom(t, c, "update", map[string]any{"room_id": thread, "parent_room_id": "ignored", "title": "Renamed", "ext": ext})
	if observed := roomUpdated(t, observer, "updated"); !reflect.DeepEqual(observed, updated) {
		t.Fatalf("observer update %#v differs from %#v", observed, updated)
	}
	if sameID != thread || updated["parent_room_id"] != "general" || updated["title"] != "Renamed" || !reflect.DeepEqual(updated["ext"], ext) {
		t.Fatalf("thread update: %#v", updated)
	}
	if _, kept := updated["description"]; kept || updated["history_log_id"] != thread || updated["latest_log_id"] != updated["log_id"] {
		t.Fatalf("thread update fields: %#v", updated)
	}
	// A new top-level room joins only its creator.
	top, topRecord := saveRoom(t, observer, "top", map[string]any{"title": "Ops"})
	c.expectQuiet(t)
	if _, isThread := topRecord["parent_room_id"]; isThread || topRecord["title"] != "Ops" {
		t.Fatalf("top-level room: %#v", topRecord)
	}
	_, cleared := saveRoom(t, observer, "clear", map[string]any{"room_id": top})
	if _, present := cleared["title"]; present {
		t.Fatalf("omitted title was retained on a top-level room: %#v", cleared)
	}
	// An editor who has not joined the room still receives the update.
	_, renamed := saveRoom(t, c, "rename-top", map[string]any{"room_id": top, "title": "Ops 2"})
	if observed := roomUpdated(t, observer, "updated"); !reflect.DeepEqual(observed, renamed) {
		t.Fatalf("member's update %#v differs from %#v", observed, renamed)
	}

	// Room records and memberships are part of the room's own log.
	page := historyPage(t, c, thread, map[string]any{})
	if got := logIDs(t, page, "rooms"); !reflect.DeepEqual(got, []string{thread, updated["log_id"].(string)}) {
		t.Fatalf("thread room records: %#v", page)
	}
	if got := logIDs(t, page, "membership"); !reflect.DeepEqual(got, []string{record["latest_log_id"].(string)}) {
		t.Fatalf("thread memberships: %#v", page)
	}
	if _, has := page["messages"]; has || page["history_log_id"] != thread || page["latest_log_id"] != updated["log_id"] {
		t.Fatalf("thread history: %#v", page)
	}
	general := historyPage(t, c, "general", map[string]any{})
	if len(records(t, general, "rooms")) != 1 || len(records(t, general, "membership")) != 2 {
		t.Fatalf("thread records leaked into the parent log: %#v", general)
	}

	for i, params := range []map[string]any{
		{"room_id": "missing", "title": "x"},
		{"room_id": nil},
		{"parent_room_id": "missing"},
		{"parent_room_id": ""},
		{"parent_room_id": 12},
		{"title": 12},
		{"title": nil},
		{"description": 12},
		{"description": nil},
		{"description": strings.Repeat("x", maxDescriptionBytes+1)},
		{"ext": []any{}},
		{"ext": nil},
	} {
		c.expectError(t, "room_set", fmt.Sprint("bad-room-", i), params, codeInvalidParams)
	}
	c.expectQuiet(t)
	observer.expectQuiet(t)

	// A description is set with room_set like any client field, such as by a
	// bot keeping a thread's summary current, and listed rooms carry it.
	summary := "Root cause: **expired cert**."
	_, described := saveRoom(t, c, "describe", map[string]any{"room_id": thread, "title": "Renamed", "description": summary})
	if observed := roomUpdated(t, observer, "updated"); !reflect.DeepEqual(observed, described) || described["description"] != summary {
		t.Fatalf("description update %#v, observed %#v", described, observed)
	}
	listed := listRooms(t, observer, map[string]any{"room_id": thread})
	if _, has := listed["joined"]; !has || len(listed["joined"].([]any)) != 0 {
		t.Fatalf("observer has not joined the thread: %#v", listed)
	}
	if entry := listed["not_joined"].([]any)[0].(map[string]any); entry["description"] != summary || entry["title"] != "Renamed" {
		t.Fatalf("listed room: %#v", entry)
	}
}

// A thread's description is independent of any message: deleting the
// message a thread was started from changes none of its room records.
func TestDeletingAThreadsFirstMessageKeepsItsRecord(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	id, _ := save(t, a, "root", map[string]any{"body": map[string]any{"text": "Secret line\nmore"}})
	b.notification(t, "message")
	thread, record := saveRoom(t, a, "thread", map[string]any{"parent_room_id": "general", "description": "Why the deploy failed"})
	roomUpdated(t, b, "updated")
	if record["title"] != "Why the deploy failed" {
		t.Fatalf("derived title: %#v", record)
	}
	before, _ := a.request(t, "message", "delete", map[string]any{"room_id": "general", "message_id": id, "deleted": true})
	if got := methods(before); !reflect.DeepEqual(got, []string{"message"}) {
		t.Fatalf("frames before the delete result: %v", got)
	}
	b.notification(t, "message")
	rooms := records(t, historyPage(t, b, thread, map[string]any{}), "rooms")
	if len(rooms) != 1 || rooms[0].(map[string]any)["description"] != "Why the deploy failed" {
		t.Fatalf("thread room records: %#v", rooms)
	}
}

func TestMoveAppearsInBothRoomsAndCarriesReactions(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	author, reactor := clients[0], clients[1]
	body := map[string]any{"text": "move me"}
	id, created := save(t, author, "create", map[string]any{"body": body})
	reactor.notification(t, "message")
	thread, _ := saveRoom(t, author, "thread", map[string]any{"parent_room_id": "general", "description": "move me"})
	roomUpdated(t, reactor, "updated")
	joinRoom(t, reactor, thread)
	expectMembership(t, author, thread, reactor.userID, true)
	first := react(t, author, "react-a", id, "👍")
	reactor.notification(t, "reactions")
	react(t, reactor, "react-b", id, "🎉", "👍")
	author.notification(t, "reactions")
	react(t, reactor, "react-b-clear", id)
	author.notification(t, "reactions")
	react(t, reactor, "react-b-again", id, "🚀")
	author.notification(t, "reactions")

	reactor.expectError(t, "message", "not-owner", map[string]any{"message_id": id, "room_id": thread, "body": body}, codeDenied)
	author.expectError(t, "message", "missing-room", map[string]any{"message_id": id, "room_id": "missing", "body": body}, codeInvalidParams)

	// The move snapshot and the reactions record it carries both precede the
	// result (§1).
	before, result := author.request(t, "message", "move", map[string]any{"message_id": id, "room_id": thread, "body": body})
	if !reflect.DeepEqual(methods(before), []string{"message", "reactions"}) || result["message_id"] != id {
		t.Fatalf("move frames: %#v then %#v", before, result)
	}
	moved := notificationParams(t, before[0], "message")
	carried := notificationParams(t, before[1], "reactions")
	// The move snapshot names the room holding its previous snapshot.
	if moved["room_id"] != thread || moved["prev_log_id"] != id || moved["prev_room_id"] != "general" {
		t.Fatalf("move snapshot: %#v", moved)
	}
	if observed := reactor.notification(t, "message"); !reflect.DeepEqual(observed, moved) {
		t.Fatalf("move broadcast differs: %#v", observed)
	}
	if observed := reactor.notification(t, "reactions"); !reflect.DeepEqual(observed, carried) {
		t.Fatalf("carried reactions broadcast differs: %#v", observed)
	}
	if carried["room_id"] != thread || carried["message_id"] != id || parseID(t, carried["log_id"]) <= parseID(t, moved["log_id"]) {
		t.Fatalf("carried reactions record: %#v", carried)
	}
	wantSets := []any{
		map[string]any{"from": map[string]any{"user_id": "guest_1"}, "emojis": []any{"👍"}},
		map[string]any{"from": map[string]any{"user_id": "guest_2"}, "emojis": []any{"🚀"}},
	}
	if !reflect.DeepEqual(carried["reactions"], wantSets) {
		t.Fatalf("carried sets = %#v, want %#v", carried["reactions"], wantSets)
	}

	general := historyPage(t, author, "general", map[string]any{})
	if got := logIDs(t, general, "messages"); !reflect.DeepEqual(got, []string{id, moved["log_id"].(string)}) {
		t.Fatalf("source history entries: %#v", got)
	}
	if got := logIDs(t, general, "reactions"); len(got) != 4 || got[0] != first["log_id"] {
		t.Fatalf("source history keeps earlier reaction records: %#v", got)
	}
	if general["latest_log_id"] != moved["log_id"] {
		t.Fatalf("source latest_log_id: %#v", general)
	}
	threadPage := historyPage(t, author, thread, map[string]any{})
	if got := logIDs(t, threadPage, "messages"); !reflect.DeepEqual(got, []string{moved["log_id"].(string)}) {
		t.Fatalf("destination history entries: %#v", got)
	}
	if got := logIDs(t, threadPage, "reactions"); !reflect.DeepEqual(got, []string{carried["log_id"].(string)}) {
		t.Fatalf("destination history reactions: %#v", got)
	}
	if threadPage["latest_log_id"] != carried["log_id"] || threadPage["history_log_id"] != thread {
		t.Fatalf("destination bounds: %#v", threadPage)
	}
	if historical := general["messages"].([]any)[0]; !reflect.DeepEqual(historical, any(created)) {
		t.Fatalf("earlier snapshot changed: %#v", historical)
	}

	// Later reactions are logged in the message's current room; edits in place
	// stay in one room.
	later := react(t, author, "react-later", id, "✅")
	if later["room_id"] != thread {
		t.Fatalf("reaction after move: %#v", later)
	}
	_, edited := save(t, author, "edit-in-thread", map[string]any{"message_id": id, "room_id": thread, "body": map[string]any{"text": "edited"}})
	if _, has := edited["prev_room_id"]; has || edited["prev_log_id"] != moved["log_id"] {
		t.Fatalf("edit after a move: %#v", edited)
	}
	if after := historyPage(t, author, "general", map[string]any{}); after["latest_log_id"] != moved["log_id"] {
		t.Fatalf("in-place edit leaked into source room: %#v (edit %v)", after, edited["log_id"])
	}

	// A move without reactions logs no reactions record.
	plain, _ := save(t, author, "plain", map[string]any{"body": body})
	_, _ = save(t, author, "plain-move", map[string]any{"message_id": plain, "room_id": thread, "body": body})
	author.expectQuiet(t)
}

func TestReactions(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	c := dialTestClient(t, httpServer)
	id, _ := save(t, c, "create", map[string]any{"body": map[string]any{"text": "react to me"}})

	set := react(t, c, "set", id, "👍", "🎉", "👍")
	want := map[string]any{
		"log_id": set["log_id"], "message_id": id, "room_id": "general",
		"reactions": []any{map[string]any{"from": map[string]any{"user_id": "guest_1"}, "emojis": []any{"👍", "🎉"}}},
	}
	if !reflect.DeepEqual(set, want) {
		t.Fatalf("reactions broadcast = %#v, want %#v", set, want)
	}
	// An unchanged set, in any order, logs nothing.
	if result := c.result(t, "reactions", "same", map[string]any{"message_id": id, "emojis": []any{"🎉", "👍"}}); len(result) != 0 {
		t.Fatalf("unchanged result: %#v", result)
	}
	c.expectQuiet(t)
	// Request deduplication returns the original result without rebroadcasting.
	c.result(t, "reactions", "set", map[string]any{"message_id": id, "emojis": []any{"👍", "🎉", "👍"}})
	c.expectQuiet(t)
	c.expectError(t, "reactions", "set", map[string]any{"message_id": id, "emojis": []any{"👎"}}, codeInvalidParams)

	cleared := react(t, c, "clear", id)
	if sets := cleared["reactions"].([]any); len(sets) != 1 || len(sets[0].(map[string]any)["emojis"].([]any)) != 0 {
		t.Fatalf("clear broadcast: %#v", cleared)
	}
	c.result(t, "reactions", "clear-again", map[string]any{"message_id": id, "emojis": []any{}})
	c.expectQuiet(t)

	tooMany := make([]any, maxDistinctEmoji+1)
	for i := range tooMany {
		tooMany[i] = strconv.Itoa(i)
	}
	for i, params := range []map[string]any{
		{"message_id": "999", "emojis": []any{"👍"}},
		{"message_id": id},
		{"message_id": id, "emojis": nil},
		{"message_id": id, "emojis": "👍"},
		{"message_id": id, "emojis": []any{12}},
		{"message_id": id, "emojis": []any{""}},
		{"message_id": id, "emojis": tooMany},
		{"emojis": []any{"👍"}},
	} {
		c.expectError(t, "reactions", fmt.Sprint("bad-", i), params, codeInvalidParams)
	}
	c.expectQuiet(t)

	page := historyPage(t, c, "general", map[string]any{})
	if got := logIDs(t, page, "reactions"); !reflect.DeepEqual(got, []string{set["log_id"].(string), cleared["log_id"].(string)}) {
		t.Fatalf("history reactions: %#v", page)
	}
}

func TestHistoryPaginatesAcrossRecordKinds(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	c := dialTestClient(t, httpServer)
	listed := listRooms(t, c, map[string]any{"filter": "joined"})["joined"].([]any)[0].(map[string]any)
	generalID, joinID := listed["log_id"].(string), listed["latest_log_id"].(string)
	id, message := save(t, c, "m1", map[string]any{"body": map[string]any{"text": "one"}})
	reaction := react(t, c, "x1", id, "👍")
	_, updated := saveRoom(t, c, "r1", map[string]any{"room_id": "general", "title": "Lobby"})
	_, second := save(t, c, "m2", map[string]any{"body": map[string]any{"text": "two"}})
	all := []string{generalID, joinID, message["log_id"].(string), reaction["log_id"].(string), updated["log_id"].(string), second["log_id"].(string)}

	full := historyPage(t, c, "general", map[string]any{})
	if full["first_log_id"] != all[0] || full["last_log_id"] != all[5] || full["more"] != false {
		t.Fatalf("full page: %#v", full)
	}
	if full["history_log_id"] != generalID || full["latest_log_id"] != all[5] {
		t.Fatalf("full page bounds: %#v", full)
	}
	if !reflect.DeepEqual(logIDs(t, full, "rooms"), []string{all[0], all[4]}) ||
		!reflect.DeepEqual(logIDs(t, full, "membership"), []string{all[1]}) ||
		!reflect.DeepEqual(logIDs(t, full, "messages"), []string{all[2], all[5]}) ||
		!reflect.DeepEqual(logIDs(t, full, "reactions"), []string{all[3]}) {
		t.Fatalf("partitioned page: %#v", full)
	}
	if lobby := full["rooms"].([]any)[1].(map[string]any); lobby["title"] != "Lobby" || lobby["latest_log_id"] != nil {
		t.Fatalf("logged room record carries delivery fields: %#v", lobby)
	}
	// Records keep the user objects they were logged with; without room_id,
	// history pages the default room.
	c.result(t, "me", "rename", map[string]any{"name": "Ada"})
	defaulted := c.result(t, "history", "default", map[string]any{})
	if defaulted["last_log_id"] != all[5] {
		t.Fatalf("default room page: %#v", defaulted)
	}
	if from := defaulted["messages"].([]any)[0].(map[string]any)["from"]; !reflect.DeepEqual(from, map[string]any{"user_id": "guest_1"}) {
		t.Fatalf("logged from changed: %#v", from)
	}

	// Forward paging over every record kind, continuing from last_log_id + 1.
	collected := make([]string, 0)
	after := "0"
	for pages := 0; ; pages++ {
		page := historyPage(t, c, "general", map[string]any{"after": after, "limit": 2})
		var ids []string
		for _, key := range []string{"rooms", "membership", "messages", "reactions"} {
			ids = append(ids, logIDs(t, page, key)...)
		}
		if len(ids) == 0 || len(ids) > 2 || page["latest_log_id"] != all[5] {
			t.Fatalf("forward page: %#v", page)
		}
		collected = append(collected, ids...)
		if page["more"] == false {
			if pages != 2 || page["last_log_id"] != all[5] {
				t.Fatalf("final forward page: %#v", page)
			}
			break
		}
		after = strconv.FormatInt(parseID(t, page["last_log_id"])+1, 10)
	}
	if slices.Sort(collected); !reflect.DeepEqual(collected, all) {
		t.Fatalf("forward paging collected %v, want %v", collected, all)
	}

	// Backward paging selects the newest matches first.
	page := historyPage(t, c, "general", map[string]any{"limit": 2})
	if page["first_log_id"] != all[4] || page["last_log_id"] != all[5] || page["more"] != true {
		t.Fatalf("newest page: %#v", page)
	}
	page = historyPage(t, c, "general", map[string]any{"before": strconv.FormatInt(parseID(t, page["first_log_id"])-1, 10), "limit": 2})
	if page["first_log_id"] != all[2] || page["last_log_id"] != all[3] || page["more"] != true {
		t.Fatalf("backward continuation: %#v", page)
	}
	// Inclusive bounds on both sides.
	page = historyPage(t, c, "general", map[string]any{"after": all[3], "before": all[4]})
	if page["first_log_id"] != all[3] || page["last_log_id"] != all[4] || page["more"] != false {
		t.Fatalf("inclusive window: %#v", page)
	}
	// Empty arrays are omitted.
	page = historyPage(t, c, "general", map[string]any{"after": all[2], "before": all[2]})
	if !reflect.DeepEqual(slices.Sorted(maps.Keys(page)), []string{"first_log_id", "history_log_id", "last_log_id", "latest_log_id", "messages", "more"}) {
		t.Fatalf("one-message page: %#v", page)
	}
	// An empty window has no arrays and neither bound, but keeps the room's
	// latest_log_id and history_log_id.
	empty := historyPage(t, c, "general", map[string]any{"after": strconv.FormatInt(parseID(t, all[5])+1, 10)})
	want := map[string]any{"more": false, "latest_log_id": all[5], "history_log_id": generalID}
	if !reflect.DeepEqual(empty, want) {
		t.Fatalf("empty page = %#v, want %#v", empty, want)
	}
	// Invalid parameters are rejected.
	for i, params := range []map[string]any{
		{"room_id": "missing"}, {"room_id": "general", "limit": 0}, {"room_id": "general", "after": 5},
		{"room_id": "general", "before": "-1"},
	} {
		c.expectError(t, "history", fmt.Sprint("bad-", i), params, codeInvalidParams)
	}
}

// A window bounded to one log_id returns exactly that record, so a client
// walks prev_log_id back through a message's edits. A move snapshot names
// the source room in prev_room_id, whose log holds the earlier snapshots.
func TestHistorySingleRecordWalksPrevLogID(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	c := dialTestClient(t, httpServer)
	id, created := save(t, c, "create", map[string]any{"body": map[string]any{"text": "one"}})
	_, edited := save(t, c, "edit", map[string]any{"message_id": id, "body": map[string]any{"text": "two"}})
	save(t, c, "other", map[string]any{"body": map[string]any{"text": "unrelated"}})
	thread, _ := saveRoom(t, c, "thread", map[string]any{"parent_room_id": "general", "title": "Moved"})
	_, moved := save(t, c, "move", map[string]any{"message_id": id, "room_id": thread, "body": map[string]any{"text": "three"}})
	_, latest := save(t, c, "edit-again", map[string]any{"message_id": id, "room_id": thread, "body": map[string]any{"text": "four"}})

	var walked []any
	var rooms []string
	for record := latest; ; {
		walked = append(walked, record)
		prev, ok := record["prev_log_id"].(string)
		if !ok {
			break
		}
		room := record["room_id"].(string)
		if previous, ok := record["prev_room_id"].(string); ok {
			room = previous
		}
		rooms = append(rooms, room)
		page := historyPage(t, c, room, map[string]any{"after": prev, "before": prev})
		entries := records(t, page, "messages")
		if len(entries) != 1 || page["first_log_id"] != prev || page["last_log_id"] != prev || page["more"] != false {
			t.Fatalf("single-record page for %s in %s: %#v", prev, room, page)
		}
		record = entries[0].(map[string]any)
	}
	if !reflect.DeepEqual(walked, []any{latest, moved, edited, created}) || !reflect.DeepEqual(rooms, []string{thread, "general", "general"}) {
		t.Fatalf("walked %#v through %v", walked, rooms)
	}
	// The destination's log does not hold the earlier snapshots.
	if page := historyPage(t, c, thread, map[string]any{"after": edited["log_id"], "before": edited["log_id"]}); page["messages"] != nil || page["first_log_id"] != nil {
		t.Fatalf("earlier snapshot in the destination: %#v", page)
	}
}

func TestInvalidSavesLeaveStateUnchanged(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	c := dialTestClient(t, httpServer)
	id, _ := save(t, c, "create", map[string]any{"body": map[string]any{"text": "hello"}})
	for i, params := range []map[string]any{
		{"body": map[string]any{}, "deleted": true},
		{"message_id": "999", "body": map[string]any{}},
		{"message_id": nil, "body": map[string]any{}},
		{"message_id": id},
		{"message_id": id, "body": nil},
		{"message_id": id, "body": map[string]any{"text": nil}},
		{"message_id": id, "body": map[string]any{"format": "html"}},
		{"message_id": id, "body": map[string]any{"embeds": nil}},
		{"message_id": id, "body": map[string]any{}, "ext": nil},
		{"message_id": id, "body": map[string]any{}, "ext": "text"},
		{"body": map[string]any{}, "deleted": nil},
		{"body": map[string]any{}, "room_id": nil},
		{"body": map[string]any{}, "room_id": "missing"},
		{"body": map[string]any{"text": "x", "mentions": "guest_1"}},
		{"body": map[string]any{"text": "x", "mentions": []any{7}}},
		{"body": map[string]any{"text": "x", "mentions": []any{""}}},
	} {
		if _, ok := params["room_id"]; !ok {
			params["room_id"] = "general"
		}
		c.expectError(t, "message", fmt.Sprint("bad-", i), params, codeInvalidParams)
	}
	c.expectQuiet(t)
	page := historyPage(t, c, "general", map[string]any{})
	if len(records(t, page, "messages")) != 1 || page["latest_log_id"] != id {
		t.Fatalf("invalid operations appended history: %#v", page)
	}
}

// A message without room_id goes to the default room; a new message with no
// text and no embeds is neither logged nor broadcast.
func TestDefaultRoomAndEmptyMessages(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	c := dialTestClient(t, httpServer)
	before, result := c.request(t, "message", "default", map[string]any{"body": map[string]any{"text": "hi"}})
	if snapshot := notificationParams(t, before[0], "message"); len(before) != 1 || snapshot["room_id"] != "general" || snapshot["message_id"] != result["message_id"] {
		t.Fatalf("default room snapshot: %#v", snapshot)
	}
	for i, body := range []map[string]any{{}, {"text": ""}, {"text": "", "embeds": []any{}, "mentions": []any{"guest_1"}}} {
		if result := c.result(t, "message", fmt.Sprint("empty-", i), map[string]any{"body": body}); len(result) != 0 {
			t.Fatalf("empty message result: %#v", result)
		}
	}
	c.expectQuiet(t)
	if page := historyPage(t, c, "general", map[string]any{}); len(records(t, page, "messages")) != 1 {
		t.Fatalf("empty messages were logged: %#v", page)
	}
	// An empty edit is an ordinary save.
	_, edited := save(t, c, "edit", map[string]any{"message_id": result["message_id"], "body": map[string]any{}})
	if !reflect.DeepEqual(edited["body"], map[string]any{}) {
		t.Fatalf("empty edit: %#v", edited)
	}
}

func TestRequestDeduplication(t *testing.T) {
	app, httpServer := newTestServer(t, DefaultConfig())
	c := dialTestClient(t, httpServer)
	thread, _ := saveRoom(t, c, "thread", map[string]any{"parent_room_id": "general", "title": "Deploy"})
	c.write(t, map[string]any{"method": "room_set", "id": "thread", "params": map[string]any{"title": "Deploy", "parent_room_id": "general"}})
	if c.read(t)["result"].(map[string]any)["room_id"] != thread {
		t.Fatal("room retry minted another ID")
	}
	c.expectQuiet(t)

	id, _ := save(t, c, "create", map[string]any{"body": map[string]any{"text": "initial"}})
	params := map[string]any{"room_id": thread, "message_id": id, "body": map[string]any{"text": "edited"}}
	_, move := save(t, c, "edit", params)
	if resultID := c.result(t, "message", "edit", params)["message_id"]; resultID != id {
		t.Fatal("replacement retry changed message ID")
	}
	c.expectQuiet(t)
	params["body"] = map[string]any{"text": "conflicting retry"}
	c.expectError(t, "message", "edit", params, codeInvalidParams)
	c.expectError(t, "history", "edit", map[string]any{"room_id": thread}, codeInvalidParams)
	page := historyPage(t, c, thread, map[string]any{})
	if len(records(t, page, "messages")) != 1 || page["last_log_id"] != move["log_id"] {
		t.Fatalf("retry appended log entries: %#v", page)
	}

	// Reads are not kept: a finished history runs again under the same ID.
	first := c.result(t, "history", "read", map[string]any{"room_id": thread})
	save(t, c, "post", map[string]any{"room_id": thread, "body": map[string]any{"text": "new"}})
	second := c.result(t, "history", "read", map[string]any{"room_id": thread})
	if len(logIDs(t, second, "messages")) != len(logIDs(t, first, "messages"))+1 {
		t.Fatalf("a finished read was answered from the cache: %#v", second)
	}
	app.mu.RLock()
	defer app.mu.RUnlock()
	if entry := app.users[c.userID].dedup.get("read"); entry != nil {
		t.Fatal("history result kept for deduplication")
	}
	if entry := app.users[c.userID].dedup.get("post"); entry == nil {
		t.Fatal("message result not kept for deduplication")
	}
}

func TestNotificationsGetNoReplies(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	c := dialTestClient(t, httpServer)
	c.write(t, map[string]any{"method": "unknown_notification"})
	c.write(t, map[string]any{"method": "message", "params": []any{}})
	c.write(t, map[string]any{"method": "me", "params": map[string]any{"name": nil}})
	c.write(t, map[string]any{"method": "reactions", "params": map[string]any{"message_id": "999", "emojis": []any{}}})
	c.write(t, map[string]any{"method": "auth"})
	// Errors not tied to a request omit id (PROTOCOL.md §1.1).
	for _, raw := range []string{`{`, `{"method":"me","id":1}`} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if err := c.ws.Write(ctx, websocket.MessageText, []byte(raw)); err != nil {
			t.Fatal(err)
		}
		cancel()
		failure := c.read(t)
		if _, hasID := failure["id"]; hasID || failure["error"] == nil {
			t.Fatalf("%s: error frame = %#v, want an error without id", raw, failure)
		}
	}
	c.write(t, map[string]any{"method": "me", "id": "n1", "params": map[string]any{"name": "A"}})
	response := c.read(t)
	if response["id"] != "n1" {
		t.Fatalf("notification produced a response before me: %#v", response)
	}
}

func TestHTTPEndpoints(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{"index.html": "app", ".env": "SECRET=1", "assets/app.js": "js"} {
		path := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	config := DefaultConfig()
	config.StaticDir = dir
	_, httpServer := newTestServer(t, config)
	// The static directory serves files, but not dotfiles or listings.
	for path, want := range map[string]int{"/healthz": 200, "/": 200, "/assets/app.js": 200, "/.env": 404, "/assets/": 404} {
		if status, _, _ := httpDo(t, http.MethodGet, httpServer.URL+path, nil, ""); status != want {
			t.Errorf("GET %s: %d, want %d", path, status, want)
		}
	}

	// The HTTP request host is authorized by coder/websocket; a configured origin
	// outside the allowlist is rejected during the WebSocket handshake.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, err := websocket.Dial(ctx, wsURL(httpServer), &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://untrusted.example"}}}); err == nil {
		t.Fatal("untrusted origin unexpectedly connected")
	}
}

func TestActivityRelaysTypingWithInlineIdentity(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	c := dialTestClient(t, httpServer)
	thread, _ := saveRoom(t, c, "thread", map[string]any{"parent_room_id": "general"})
	c.write(t, map[string]any{"method": "activity", "params": map[string]any{"room_id": thread, "typing": 8, "from": map[string]any{"user_id": "forged"}}})
	params := c.notification(t, "activity")
	want := map[string]any{"room_id": thread, "from": map[string]any{"user_id": "guest_1"}, "typing": float64(8)}
	if !reflect.DeepEqual(params, want) {
		t.Fatalf("activity = %#v, want %#v", params, want)
	}
	c.write(t, map[string]any{"method": "activity", "params": map[string]any{"room_id": thread, "typing": 0}})
	if stop := c.notification(t, "activity"); stop["typing"] != float64(0) {
		t.Fatalf("stop: %#v", stop)
	}
	c.expectError(t, "activity", "missing", map[string]any{"room_id": "missing", "typing": 8}, codeInvalidParams)
	c.expectError(t, "activity", "negative", map[string]any{"room_id": thread, "typing": -1}, codeInvalidParams)

	// away applies to the connection and is never delivered.
	c.write(t, map[string]any{"method": "activity", "params": map[string]any{"away": true}})
	c.write(t, map[string]any{"method": "activity", "params": map[string]any{"room_id": thread, "away": false}})
	c.expectQuiet(t)
	c.expectError(t, "activity", "bad-away", map[string]any{"away": "yes"}, codeInvalidParams)
}
