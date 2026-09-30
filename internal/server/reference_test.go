package server

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestRoomListFiltersAndOrder(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	ops, _ := saveRoom(t, a, "ops", map[string]any{"title": "Ops"})
	deploy, _ := saveRoom(t, a, "deploy", map[string]any{"parent_room_id": ops, "title": "Deploy"})
	random, _ := saveRoom(t, b, "random", map[string]any{"title": "Random"})
	idle, _ := saveRoom(t, b, "idle", map[string]any{"parent_room_id": "general", "title": "Idle"})
	roomUpdated(t, a, "updated")
	save(t, a, "post", map[string]any{"room_id": ops, "body": map[string]any{"text": "most recent"}})

	// joined holds joined rooms at every depth, most recently active first;
	// not_joined holds visible unjoined top-level rooms. Without members,
	// neither members nor users is sent.
	listed := listRooms(t, a, map[string]any{})
	if got := roomIDs(t, listed["joined"]); !reflect.DeepEqual(got, []string{ops, deploy, "general"}) {
		t.Fatalf("joined: %v", got)
	}
	if got := roomIDs(t, listed["not_joined"]); !reflect.DeepEqual(got, []string{random}) {
		t.Fatalf("not_joined: %v", got)
	}
	if !reflect.DeepEqual(slices.Sorted(maps.Keys(listed)), []string{"joined", "not_joined"}) {
		t.Fatalf("listing without members: %#v", listed)
	}
	for _, entry := range slices.Concat(listed["joined"].([]any), listed["not_joined"].([]any)) {
		if _, has := entry.(map[string]any)["members"]; has {
			t.Fatalf("members without members: true: %#v", entry)
		}
	}
	if opsEntry := listed["joined"].([]any)[0].(map[string]any); opsEntry["title"] != "Ops" || opsEntry["latest_log_id"] == nil {
		t.Fatalf("ops entry: %#v", opsEntry)
	}

	// members: true lists every member of every room, joined or not, with
	// each user's current object once in users.
	a.result(t, "me", "name", map[string]any{"name": "Ada"})
	b.notification(t, "user")
	withMembers := listRooms(t, a, map[string]any{"members": true})
	general := withMembers["joined"].([]any)[2].(map[string]any)
	if !reflect.DeepEqual(memberIDs(general), []string{"guest_1", "guest_2"}) || !reflect.DeepEqual(general["members"].([]any)[0], map[string]any{"user_id": "guest_1"}) {
		t.Fatalf("general members: %#v", general)
	}
	if randomEntry := withMembers["not_joined"].([]any)[0].(map[string]any); !reflect.DeepEqual(memberIDs(randomEntry), []string{"guest_2"}) {
		t.Fatalf("unjoined room members: %#v", randomEntry)
	}
	wantUsers := []any{map[string]any{"user_id": "guest_1", "name": "Ada"}, map[string]any{"user_id": "guest_2"}}
	if !reflect.DeepEqual(withMembers["users"], wantUsers) {
		t.Fatalf("users: %#v", withMembers["users"])
	}

	// filter leaves out the other array; an array it asks for is present
	// even when empty.
	if only := listRooms(t, a, map[string]any{"filter": "joined"}); only["not_joined"] != nil || len(only["joined"].([]any)) != 3 {
		t.Fatalf("filter joined: %#v", only)
	}
	if not := listRooms(t, a, map[string]any{"filter": "not_joined"}); not["joined"] != nil || !reflect.DeepEqual(roomIDs(t, not["not_joined"]), []string{random}) {
		t.Fatalf("filter not_joined: %#v", not)
	}
	if all := listRooms(t, a, map[string]any{"filter": "all"}); len(all["joined"].([]any)) != 3 || len(all["not_joined"].([]any)) != 1 {
		t.Fatalf("filter all: %#v", all)
	}
	if empty := listRooms(t, a, map[string]any{"parent_room_id": "general", "filter": "joined"}); !reflect.DeepEqual(empty, map[string]any{"joined": []any{}}) {
		t.Fatalf("empty joined threads: %#v", empty)
	}
	// parent_room_id lists that room's threads, joined or not.
	threads := listRooms(t, a, map[string]any{"parent_room_id": "general"})
	if len(threads["joined"].([]any)) != 0 || !reflect.DeepEqual(roomIDs(t, threads["not_joined"]), []string{idle}) {
		t.Fatalf("general's threads: %#v", threads)
	}
	if threads := listRooms(t, b, map[string]any{"parent_room_id": ops}); !reflect.DeepEqual(roomIDs(t, threads["not_joined"]), []string{deploy}) {
		t.Fatalf("ops threads for a non-member: %#v", threads)
	}
	// room_id lists one room and overrides parent_room_id.
	one := listRooms(t, b, map[string]any{"room_id": ops, "parent_room_id": "general", "members": true})
	if !reflect.DeepEqual(roomIDs(t, one["not_joined"]), []string{ops}) || len(one["joined"].([]any)) != 0 || !reflect.DeepEqual(one["users"], wantUsers[:1]) {
		t.Fatalf("one room: %#v", one)
	}
	for i, params := range []map[string]any{
		{"parent_room_id": "missing"}, {"room_id": "missing"}, {"filter": "mine"}, {"filter": 5},
		{"members": "yes"}, {"latest_log_id": 5}, {"parent_room_id": 5},
	} {
		a.expectError(t, "room_list", fmt.Sprint("bad-", i), params, codeInvalidParams)
	}
	a.expectQuiet(t)
	b.expectQuiet(t)
}

// With latest_log_id, room_list lists only rooms changed since, and `left`
// the rooms the user left since (§4.3.1).
func TestRoomListLatestLogIDDelta(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	ops, _ := saveRoom(t, a, "ops", map[string]any{"title": "Ops"})
	joinRoom(t, b, ops)
	since := expectMembership(t, a, ops, b.userID, true)["log_id"].(string)

	leaveRoom(t, b, ops)
	expectMembership(t, a, ops, b.userID, false)
	news, _ := saveRoom(t, a, "news", map[string]any{"title": "News"})
	mine, _ := saveRoom(t, b, "mine", map[string]any{"title": "Mine"})

	// Joined since then in joined, left since then in left; general has not
	// changed since, so it is not listed.
	delta := listRooms(t, b, map[string]any{"filter": "joined", "latest_log_id": since})
	want := map[string]any{"joined": delta["joined"], "left": []any{map[string]any{"room_id": ops}}}
	if !reflect.DeepEqual(delta, want) || !reflect.DeepEqual(roomIDs(t, delta["joined"]), []string{mine}) {
		t.Fatalf("joined delta: %#v", delta)
	}
	// With not_joined too, a room left since then is also a visible unjoined
	// room that changed.
	delta = listRooms(t, b, map[string]any{"latest_log_id": since, "members": true})
	if !reflect.DeepEqual(roomIDs(t, delta["not_joined"]), []string{news, ops}) || !reflect.DeepEqual(delta["left"], []any{map[string]any{"room_id": ops}}) {
		t.Fatalf("full delta: %#v", delta)
	}
	if !reflect.DeepEqual(memberIDs(delta["not_joined"].([]any)[1].(map[string]any)), []string{"guest_1"}) {
		t.Fatalf("members in a delta: %#v", delta)
	}
	// left accompanies joined only.
	if delta := listRooms(t, b, map[string]any{"filter": "not_joined", "latest_log_id": since}); delta["left"] != nil {
		t.Fatalf("not_joined delta: %#v", delta)
	}
	// left is present even when empty.
	latest := listRooms(t, b, map[string]any{"filter": "joined"})["joined"].([]any)[0].(map[string]any)["latest_log_id"]
	if nothing := listRooms(t, b, map[string]any{"filter": "joined", "latest_log_id": latest}); !reflect.DeepEqual(nothing, map[string]any{"joined": []any{}, "left": []any{}}) {
		t.Fatalf("empty delta: %#v", nothing)
	}
	// A room left before the position is not in left, even when it changed.
	save(t, a, "later", map[string]any{"room_id": ops, "body": map[string]any{"text": "later"}})
	if delta := listRooms(t, b, map[string]any{"latest_log_id": latest}); !reflect.DeepEqual(delta["left"], []any{}) || !reflect.DeepEqual(roomIDs(t, delta["not_joined"]), []string{ops}) {
		t.Fatalf("delta after an old leave: %#v", delta)
	}
	// A room rejoined since the position is in joined, not left.
	joinRoom(t, b, ops)
	expectMembership(t, a, ops, b.userID, true)
	delta = listRooms(t, b, map[string]any{"filter": "joined", "latest_log_id": since})
	if !reflect.DeepEqual(roomIDs(t, delta["joined"]), []string{ops, mine}) || !reflect.DeepEqual(delta["left"], []any{}) {
		t.Fatalf("delta after rejoining: %#v", delta)
	}
	// A room the user was removed from is left too.
	kick := map[string]any{"room_id": ops, "body": map[string]any{"text": "/kick @guest_2", "mentions": []any{"guest_2"}}}
	a.request(t, "command", "kick", kick)
	b.drain(t)
	delta = listRooms(t, b, map[string]any{"filter": "joined", "latest_log_id": since})
	if !reflect.DeepEqual(roomIDs(t, delta["joined"]), []string{mine}) || !reflect.DeepEqual(delta["left"], []any{map[string]any{"room_id": ops}}) {
		t.Fatalf("delta after removal: %#v", delta)
	}
}

func TestRoomListTruncatesOnlyUnjoinedRooms(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	owner, other := clients[0], clients[1]
	var threads []string
	for i := range maxListedRooms + 1 {
		thread, _ := saveRoom(t, owner, fmt.Sprint("thread-", i), map[string]any{"parent_room_id": "general"})
		roomUpdated(t, other, "updated")
		threads = append(threads, thread)
	}
	if joined := listRooms(t, owner, map[string]any{"filter": "joined"})["joined"].([]any); len(joined) != maxListedRooms+2 {
		t.Fatalf("owner joined %d rooms", len(joined))
	}
	// The unjoined listing keeps the most recently active.
	rooms := roomIDs(t, listRooms(t, other, map[string]any{"parent_room_id": "general"})["not_joined"])
	if len(rooms) != maxListedRooms || rooms[0] != threads[len(threads)-1] || slices.Contains(rooms, threads[0]) {
		t.Fatalf("unjoined threads: %d, first %v", len(rooms), rooms[0])
	}
}

// Every join and leave is a logged membership, delivered to the room's
// members before and after the change and ordered before the room_update
// and the result on the requesting connection (§4.3.2).
func TestJoinLeaveAndDeliveries(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 3)
	a, b, c := clients[0], clients[1], clients[2]
	ops, opsRecord := saveRoom(t, a, "ops", map[string]any{"title": "Ops"})

	// A member receives the room's records; a poster who has not joined gets
	// only the result.
	b.write(t, map[string]any{"method": "message", "id": "outside", "params": map[string]any{"room_id": ops, "body": map[string]any{"text": "from outside"}}})
	if frames := b.drain(t); !reflect.DeepEqual(methods(frames), []string{"reply"}) {
		t.Fatalf("non-member post: %#v", frames)
	}
	outside := a.notification(t, "message")
	if outside["from"].(map[string]any)["user_id"] != "guest_2" {
		t.Fatalf("non-member post broadcast: %#v", outside)
	}
	// Reactions and typing in a room not joined reach only its members.
	react(t, a, "react", outside["message_id"].(string), "👍")
	b.write(t, map[string]any{"method": "activity", "params": map[string]any{"room_id": ops, "typing": 5}})
	a.notification(t, "activity")
	b.expectQuiet(t)

	// Joining: the membership goes to the members and the joiner, then the
	// joiner gets room_update joined with the members, then the result.
	record := joinRoom(t, b, ops)
	if record["title"] != "Ops" || record["log_id"] != opsRecord["log_id"] || !reflect.DeepEqual(memberIDs(record), []string{"guest_1", "guest_2"}) {
		t.Fatalf("joined record: %#v", record)
	}
	joined := expectMembership(t, a, ops, "guest_2", true)
	if joined["log_id"] != record["latest_log_id"] {
		t.Fatalf("member's copy %#v disagrees with %#v", joined, record)
	}
	c.expectQuiet(t)
	save(t, a, "inside", map[string]any{"room_id": ops, "body": map[string]any{"text": "welcome"}})
	b.notification(t, "message")
	// Joining again logs nothing and re-sends the room to the calling
	// connection only.
	before, _ := b.request(t, "room_join", "again", map[string]any{"room_id": ops})
	if len(before) != 1 || !reflect.DeepEqual(memberIDs(joinedRecord(t, before[0], "guest_2")), []string{"guest_1", "guest_2"}) {
		t.Fatalf("second join: %#v", before)
	}
	a.expectQuiet(t)
	b.expectError(t, "room_join", "missing", map[string]any{"room_id": "missing"}, codeInvalidParams)

	// Leaving: the membership goes to the members and the leaver, then the
	// leaver gets room_update left, then the result; a result sent after the
	// leave reflects it.
	b.write(t, map[string]any{"method": "room_leave", "id": "leave", "params": map[string]any{"room_id": ops}})
	b.write(t, map[string]any{"method": "room_list", "id": "after-leave", "params": map[string]any{"filter": "joined"}})
	frames := b.drain(t)
	if !reflect.DeepEqual(methods(frames), []string{"membership", "room_update", "reply", "reply"}) ||
		!reflect.DeepEqual(frames[1]["params"], map[string]any{"left": []any{map[string]any{"room_id": ops}}}) {
		t.Fatalf("leave frames: %#v", frames)
	}
	checkMembership(t, frames[0]["params"].(map[string]any), ops, "guest_2", false)
	if got := roomIDs(t, frames[3]["result"].(map[string]any)["joined"]); !reflect.DeepEqual(got, []string{"general"}) {
		t.Fatalf("joined after leave: %v", got)
	}
	if left := expectMembership(t, a, ops, "guest_2", false); !reflect.DeepEqual(left, frames[0]["params"]) {
		t.Fatalf("member's copy of the leave: %#v", left)
	}
	save(t, a, "after", map[string]any{"room_id": ops, "body": map[string]any{"text": "gone"}})
	b.expectQuiet(t)
	// Leaving a room not joined changes nothing.
	b.result(t, "room_leave", "leave-again", map[string]any{"room_id": ops})
	b.expectError(t, "room_leave", "missing", map[string]any{"room_id": "missing"}, codeInvalidParams)
	a.expectQuiet(t)

	// Leaves are memberships only, never user notifications (§3.3), even from
	// the last shared room.
	leaveRoom(t, b, "general")
	expectMembership(t, a, "general", "guest_2", false)
	expectMembership(t, c, "general", "guest_2", false)
	a.expectQuiet(t)
	joinRoom(t, b, ops)
	expectMembership(t, a, ops, "guest_2", true)

	// A guest whose last connection closes is retired: it leaves every room
	// it joined, each a logged membership for the remaining members.
	_ = b.ws.Close(websocket.StatusNormalClosure, "done")
	expectMembership(t, a, ops, "guest_2", false)
	a.expectQuiet(t)
	c.expectQuiet(t)
	if entry := listRooms(t, a, map[string]any{"room_id": ops, "members": true})["joined"].([]any)[0].(map[string]any); !reflect.DeepEqual(memberIDs(entry), []string{"guest_1"}) {
		t.Fatalf("retired guest is still a member: %#v", entry)
	}
	// history returns the room's memberships in log order.
	var changes []string
	for _, value := range records(t, historyPage(t, a, ops, map[string]any{}), "membership") {
		entry := value.(map[string]any)["members"].([]any)[0].(map[string]any)
		changes = append(changes, fmt.Sprint(entry["user"].(map[string]any)["user_id"], " ", entry["joined"]))
	}
	want := []string{"guest_1 true", "guest_2 true", "guest_2 false", "guest_2 true", "guest_2 false"}
	if !reflect.DeepEqual(changes, want) {
		t.Fatalf("ops memberships: %v, want %v", changes, want)
	}
}

// A thread's messages go only to its members; the parent's members get only
// its room record changes, as room_update updated (§3.4, §4.3.3).
func TestThreadMessagesReachOnlyThreadMembers(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 3)
	a, b, c := clients[0], clients[1], clients[2]
	save(t, a, "root", map[string]any{"body": map[string]any{"text": "Deploy"}})
	b.notification(t, "message")
	c.notification(t, "message")
	thread, record := saveRoom(t, a, "thread", map[string]any{"parent_room_id": "general", "description": "Deploy"})
	for _, member := range []*testClient{b, c} {
		if observed := roomUpdated(t, member, "updated"); !reflect.DeepEqual(observed, record) {
			t.Fatalf("parent member's new thread %#v differs from %#v", observed, record)
		}
	}
	joinRoom(t, b, thread)
	expectMembership(t, a, thread, "guest_2", true)
	c.expectQuiet(t)

	id, _ := save(t, a, "in-thread", map[string]any{"room_id": thread, "body": map[string]any{"text": "rolling out"}})
	b.notification(t, "message")
	react(t, b, "react", id, "👍")
	a.notification(t, "reactions")
	a.write(t, map[string]any{"method": "activity", "params": map[string]any{"room_id": thread, "typing": 3}})
	a.notification(t, "activity")
	b.notification(t, "activity")
	c.expectQuiet(t)

	// An edited thread record reaches the parent's members too.
	_, edited := saveRoom(t, b, "rename", map[string]any{"room_id": thread, "title": "Rollout"})
	for _, member := range []*testClient{a, c} {
		if observed := roomUpdated(t, member, "updated"); !reflect.DeepEqual(observed, edited) {
			t.Fatalf("thread edit %#v differs from %#v", observed, edited)
		}
	}
	for _, client := range clients {
		client.expectQuiet(t)
	}
}

func TestPrevLogIDLinksRecords(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	c := dialTestClient(t, httpServer)
	id, creation := save(t, c, "create", map[string]any{"body": map[string]any{"text": "one"}})
	if _, has := creation["prev_log_id"]; has {
		t.Fatalf("creation has prev_log_id: %#v", creation)
	}
	_, edit := save(t, c, "edit", map[string]any{"message_id": id, "body": map[string]any{"text": "two"}})
	if edit["prev_log_id"] != id {
		t.Fatalf("edit prev_log_id: %#v", edit)
	}
	room, record := saveRoom(t, c, "room", map[string]any{"title": "A"})
	_, updated := saveRoom(t, c, "update", map[string]any{"room_id": room, "title": "B"})
	if _, has := record["prev_log_id"]; has || updated["prev_log_id"] != record["log_id"] {
		t.Fatalf("room prev_log_id: %#v then %#v", record, updated)
	}
	// Only room records and message snapshots carry prev_log_id (§2).
	first := react(t, c, "r1", id, "👍")
	second := react(t, c, "r2", id, "🎉")
	if first["prev_log_id"] != nil || second["prev_log_id"] != nil {
		t.Fatalf("reactions prev_log_id: %#v then %#v", first, second)
	}
	leaveRoom(t, c, room)
	for _, value := range records(t, historyPage(t, c, room, map[string]any{}), "membership") {
		if _, has := value.(map[string]any)["prev_log_id"]; has {
			t.Fatalf("membership with prev_log_id: %#v", value)
		}
	}
}

func TestReadCursors(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	first, _ := save(t, a, "one", map[string]any{"body": map[string]any{"text": "one"}})
	b.notification(t, "message")
	second, _ := save(t, a, "two", map[string]any{"body": map[string]any{"text": "two"}})
	b.notification(t, "message")

	a.write(t, map[string]any{"method": "activity", "params": map[string]any{"room_id": "general", "read_message_id": second}})
	want := map[string]any{"room_id": "general", "from": map[string]any{"user_id": "guest_1"}, "read_message_id": second}
	for _, c := range []*testClient{a, b} {
		if cursor := c.notification(t, "activity"); !reflect.DeepEqual(cursor, want) {
			t.Fatalf("read cursor = %#v, want %#v", cursor, want)
		}
	}
	// A cursor that moves back is ignored.
	a.write(t, map[string]any{"method": "activity", "params": map[string]any{"room_id": "general", "read_message_id": first}})
	b.expectQuiet(t)
	a.expectQuiet(t)
	a.expectError(t, "activity", "unknown", map[string]any{"room_id": "general", "read_message_id": "999"}, codeInvalidParams)

	// Kept cursors follow a room_list result that lists the room: every
	// member's for a joined room, only the user's own for another.
	c := dialTestClient(t, httpServer)
	expectMembership(t, a, "general", c.userID, true)
	expectMembership(t, b, "general", c.userID, true)
	c.write(t, map[string]any{"method": "room_list", "id": "list", "params": map[string]any{"filter": "joined"}})
	frames := c.drain(t)
	if !reflect.DeepEqual(methods(frames), []string{"reply", "activity"}) || !reflect.DeepEqual(frames[1]["params"], any(want)) {
		t.Fatalf("room_list frames: %#v", frames)
	}
	thread, _ := saveRoom(t, b, "thread", map[string]any{"parent_room_id": "general"})
	roomUpdated(t, a, "updated")
	roomUpdated(t, c, "updated")
	b.write(t, map[string]any{"method": "activity", "params": map[string]any{"room_id": thread, "read_message_id": second}})
	b.notification(t, "activity")
	a.write(t, map[string]any{"method": "activity", "params": map[string]any{"room_id": thread, "read_message_id": first}})
	a.notification(t, "activity")
	b.notification(t, "activity")
	a.write(t, map[string]any{"method": "room_list", "id": "threads", "params": map[string]any{"parent_room_id": "general"}})
	frames = a.drain(t)
	if !reflect.DeepEqual(methods(frames), []string{"reply", "activity"}) || frames[1]["params"].(map[string]any)["from"].(map[string]any)["user_id"] != "guest_1" {
		t.Fatalf("unjoined room cursors: %#v", frames)
	}
}

func TestProfilesAndUserNotifications(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	ext := map[string]any{"example.org": map[string]any{"pronouns": "she/her"}}
	you := a.result(t, "me", "profile", map[string]any{"name": "  Ada  ", "avatar": "data:image/png;base64,iVBORw0KGgo=", "ext": ext})["you"]
	want := map[string]any{"user_id": "guest_1", "name": "Ada", "avatar": "data:image/png;base64,iVBORw0KGgo=", "ext": ext}
	if !reflect.DeepEqual(you, any(want)) {
		t.Fatalf("you = %#v", you)
	}
	if notice := b.notification(t, "user"); !reflect.DeepEqual(notice, map[string]any{"new": want}) {
		t.Fatalf("user notification: %#v", notice)
	}
	// Avatars and ext travel in profiles, not in every from.
	_, snapshot := save(t, a, "post", map[string]any{"body": map[string]any{"text": "hi"}})
	b.notification(t, "message")
	if !reflect.DeepEqual(snapshot["from"], map[string]any{"user_id": "guest_1", "name": "Ada"}) {
		t.Fatalf("from: %#v", snapshot["from"])
	}
	// room_list with members: true sends members bare, with complete
	// objects in users.
	if listed := listRooms(t, b, map[string]any{"filter": "joined"}); listed["users"] != nil {
		t.Fatalf("users without members: true: %#v", listed)
	}
	listed := listRooms(t, b, map[string]any{"filter": "joined", "members": true})
	if members := listed["joined"].([]any)[0].(map[string]any)["members"].([]any); !reflect.DeepEqual(members[0], map[string]any{"user_id": "guest_1"}) {
		t.Fatalf("members: %#v", members)
	}
	if users := listed["users"].([]any); !reflect.DeepEqual(users[0], any(want)) {
		t.Fatalf("users: %#v", users)
	}
	// An unchanged profile sends no notification; omitted fields stay.
	a.result(t, "me", "same", map[string]any{"name": "Ada"})
	if kept := a.result(t, "me", "keep", map[string]any{})["you"]; !reflect.DeepEqual(kept, any(want)) {
		t.Fatalf("empty me changed the profile: %#v", kept)
	}
	b.expectQuiet(t)
	// An empty value removes a field, announced as that empty value.
	you = a.result(t, "me", "clear", map[string]any{"ext": map[string]any{}, "avatar": ""})["you"]
	cleared := map[string]any{"user_id": "guest_1", "name": "Ada", "avatar": "", "ext": map[string]any{}}
	if !reflect.DeepEqual(you, any(cleared)) {
		t.Fatalf("removal result: %#v", you)
	}
	if notice := b.notification(t, "user"); !reflect.DeepEqual(notice, map[string]any{"new": cleared}) {
		t.Fatalf("removal notification: %#v", notice)
	}
	if users := listRooms(t, b, map[string]any{"room_id": "general", "members": true})["users"].([]any); !reflect.DeepEqual(users[0], map[string]any{"user_id": "guest_1", "name": "Ada"}) {
		t.Fatalf("profile after removal: %#v", users[0])
	}
	if you := a.result(t, "me", "clear-name", map[string]any{"name": ""})["you"]; !reflect.DeepEqual(you, map[string]any{"user_id": "guest_1", "name": ""}) {
		t.Fatalf("clearing the name: %#v", you)
	}
	b.notification(t, "user")
	a.expectError(t, "me", "bad-avatar", map[string]any{"avatar": "javascript:alert(1)"}, codeInvalidParams)
	a.expectError(t, "me", "bad-name", map[string]any{"name": 7}, codeInvalidParams)
	b.expectQuiet(t)
}

func TestNamesAreNormalized(t *testing.T) {
	for input, want := range map[string]string{
		"  Ada   Lovelace ":     "Ada Lovelace",
		"Ａｄａ":                   "Ada",
		"Ada\u202eecalevoL":     "AdaecalevoL",
		"Bob\x00\x07":           "Bob",
		"\u200b\u200b":          "",
		strings.Repeat("é", 80): strings.Repeat("é", maxNameRunes),
	} {
		if got := normalizeName(input); got != want {
			t.Errorf("normalizeName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestUploadsAreHostedWithOpenGraph(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	// The pending snapshot precedes the result carrying the write URL (§1).
	before, result := a.request(t, "message", "attach", map[string]any{"room_id": "general", "body": map[string]any{
		"text": "Before the fix:",
		"embeds": []any{
			map[string]any{"kind": "upload", "title": "dots.png", "url": "https://forged.example/x", "og": map[string]any{"image": map[string]any{"alt": "Three dots"}}},
			map[string]any{"kind": "iframe", "url": "https://backend.example/term", "height": 300},
		},
	}})
	if len(before) != 1 {
		t.Fatalf("frames before the upload result: %#v", before)
	}
	pending := notificationParams(t, before[0], "message")
	written := result["embeds"].([]any)
	if len(written) != 1 {
		t.Fatalf("written embeds: %#v", result)
	}
	write := written[0].(map[string]any)
	embedID, writeURL := write["embed_id"].(string), write["write_url"].(string)
	if write["kind"] != "upload" || !strings.HasPrefix(writeURL, httpServer.URL+writePath) {
		t.Fatalf("write: %#v", write)
	}
	b.notification(t, "message")
	embeds := embedsOf(t, pending)
	if !reflect.DeepEqual(embeds[0], map[string]any{"embed_id": embedID, "kind": "upload", "title": "dots.png"}) || embeds[1]["embed_id"] == nil || embeds[1]["url"] != "https://backend.example/term" {
		t.Fatalf("pending embeds: %#v", embeds)
	}

	image := testPNG(t)
	if status, _, _ := httpDo(t, http.MethodPut, writeURL, bytes.NewReader(image), "image/png"); status != http.StatusCreated {
		t.Fatalf("upload status %d", status)
	}
	completed := a.notification(t, "message")
	b.notification(t, "message")
	upload := embedsOf(t, completed)[0]
	fileURL, _ := upload["url"].(string)
	wantOG := map[string]any{"title": "dots.png", "image": map[string]any{"url": fileURL, "type": "image/png", "width": float64(3), "height": float64(2), "alt": "Three dots"}}
	if !strings.HasPrefix(fileURL, httpServer.URL+filePath+embedID+"/") || !reflect.DeepEqual(upload["og"], any(wantOG)) || completed["prev_log_id"] != pending["log_id"] {
		t.Fatalf("completed upload: %#v", completed)
	}
	status, header, content := httpDo(t, http.MethodGet, fileURL, nil, "")
	if status != http.StatusOK || !bytes.Equal(content, image) || header.Get("Content-Type") != "image/png" ||
		header.Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(header.Get("Content-Security-Policy"), "sandbox") ||
		!strings.HasPrefix(header.Get("Content-Disposition"), "inline") {
		t.Fatalf("file: %d %#v", status, header)
	}
	if status, _, _ := httpDo(t, http.MethodGet, httpServer.URL+filePath+embedID+"/wrong", nil, ""); status != http.StatusNotFound {
		t.Fatalf("wrong secret: %d", status)
	}
	if status, _, _ := httpDo(t, http.MethodPut, writeURL, strings.NewReader("again"), ""); status != http.StatusNotFound {
		t.Fatalf("reused write URL: %d", status)
	}

	// A save keeps an embed by embed_id; the server restores what it owns.
	messageID := result["message_id"].(string)
	kept := []any{
		map[string]any{"embed_id": embedID, "kind": "stream", "title": "renamed.png", "url": "https://forged.example/y"},
		embeds[1],
	}
	_, edited := save(t, a, "keep", map[string]any{"message_id": messageID, "body": map[string]any{"text": "edited", "embeds": kept}})
	b.notification(t, "message")
	restored := embedsOf(t, edited)[0]
	if restored["kind"] != "upload" || restored["url"] != fileURL || !reflect.DeepEqual(restored["og"], any(wantOG)) || restored["title"] != "renamed.png" {
		t.Fatalf("restored embed: %#v", restored)
	}
	a.expectError(t, "message", "unknown", map[string]any{"message_id": messageID, "room_id": "general", "body": map[string]any{"embeds": []any{map[string]any{"embed_id": "embed_999", "kind": "upload"}}}}, codeInvalidParams)

	// Removing the embed deletes its content.
	save(t, a, "remove", map[string]any{"message_id": messageID, "body": map[string]any{"text": "no file"}})
	b.notification(t, "message")
	if status, _, _ := httpDo(t, http.MethodGet, fileURL, nil, ""); status != http.StatusNotFound {
		t.Fatalf("removed file: %d", status)
	}
}

func TestUploadTypesThatCouldRunAreServedAsDownloads(t *testing.T) {
	for declared, want := range map[string]string{
		"image/png":        "image/png",
		"application/pdf":  "application/pdf",
		"text/html":        "text/plain",
		"text/ecmascript":  "text/plain",
		"text/css":         "text/plain",
		"text/xsl":         "text/plain",
		"application/wasm": "text/plain",
	} {
		if got := uploadContentType(declared, []byte("hello")); got != want {
			t.Errorf("%s: served as %s, want %s", declared, got, want)
		}
	}
	if got := uploadContentType("text/css", []byte("<html><script>")); got != "application/octet-stream" {
		t.Errorf("sniffed markup served as %s", got)
	}
}

func TestAvatarUploadsMustBeImages(t *testing.T) {
	check := func(contentType string, data []byte) bool {
		file, err := os.CreateTemp(t.TempDir(), "avatar")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		_, _ = file.Write(data)
		return validAvatarImage(contentType, file)
	}
	if !check("image/png", testPNG(t)) {
		t.Fatal("a PNG was refused")
	}
	webp, _ := base64.StdEncoding.DecodeString("UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA==")
	if !check("image/webp", webp) {
		t.Fatal("a WebP was refused")
	}
	if check("image/png", []byte("not an image")) || check("image/jpeg", testPNG(t)) || check("image/webp", []byte("RIFF\x00\x00\x00\x00WEBPVP8 ")) {
		t.Fatal("a mismatched avatar was accepted")
	}
}

func TestFailedAndExpiredWritesDropTheEmbed(t *testing.T) {
	config := DefaultConfig()
	config.MaxUploadBytes = 4
	config.UploadStartTimeout = 100 * time.Millisecond
	_, httpServer := newTestServer(t, config)
	a := dialTestClient(t, httpServer)
	result := postEmbeds(t, a, "two", map[string]any{"room_id": "general", "body": map[string]any{
		"embeds": []any{map[string]any{"kind": "upload"}, map[string]any{"kind": "stream"}},
	}})
	written := result["embeds"].([]any)
	large := written[0].(map[string]any)["write_url"].(string)
	if status, _, _ := httpDo(t, http.MethodPut, large, strings.NewReader("too large"), "text/plain"); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload: %d", status)
	}
	if left := embedsOf(t, a.notification(t, "message")); len(left) != 1 || left[0]["kind"] != "stream" {
		t.Fatalf("after failed upload: %#v", left)
	}
	// The stream's write URL is never used, so it expires.
	if expired := a.notification(t, "message"); len(embedsOf(t, expired)) != 0 {
		t.Fatalf("after expiry: %#v", expired)
	}
	if status, _, _ := httpDo(t, http.MethodPut, written[1].(map[string]any)["write_url"].(string), strings.NewReader("late"), ""); status != http.StatusNotFound {
		t.Fatalf("expired write URL: %d", status)
	}
}

func TestStreamsGrowLiveThenKeepTheirText(t *testing.T) {
	config := DefaultConfig()
	config.StreamKeepBytes = 16
	config.StreamMaxBytes = 64
	_, httpServer := newTestServer(t, config)
	a := dialTestClient(t, httpServer)
	result := postEmbeds(t, a, "stream", map[string]any{"room_id": "general", "body": map[string]any{
		"embeds": []any{map[string]any{"kind": "stream", "format": "terminal", "text": "forged"}},
	}})
	writeURL := result["embeds"].([]any)[0].(map[string]any)["write_url"].(string)
	live := embedsOf(t, result["snapshot"].(map[string]any))[0]
	streamURL, _ := live["url"].(string)
	if !strings.HasPrefix(streamURL, httpServer.URL+streamPath) || live["format"] != "terminal" || live["text"] != nil {
		t.Fatalf("live embed: %#v", live)
	}

	reader, err := http.Get(streamURL)
	if err != nil || reader.StatusCode != http.StatusOK {
		t.Fatalf("stream reader: %v %v", reader, err)
	}
	defer reader.Body.Close()
	body, pipe := io.Pipe()
	writerDone := make(chan int, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodPut, writeURL, body)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			writerDone <- 0
			return
		}
		response.Body.Close()
		writerDone <- response.StatusCode
	}()
	if _, err := pipe.Write([]byte("hello ")); err != nil {
		t.Fatal(err)
	}
	first := make([]byte, 6)
	if _, err := io.ReadFull(reader.Body, first); err != nil || string(first) != "hello " {
		t.Fatalf("live read %q: %v", first, err)
	}
	_, _ = pipe.Write([]byte("world, this is a long line\n"))
	_ = pipe.Close()
	// The second write overflows the 16 kept bytes before the reader catches
	// up, so the reader continues from the kept text.
	rest, _ := io.ReadAll(reader.Body)
	if string(rest) != " is a long line\n" {
		t.Fatalf("reader got %q then %q", first, rest)
	}
	if status := <-writerDone; status != http.StatusNoContent {
		t.Fatalf("writer status %d", status)
	}
	finished := embedsOf(t, a.notification(t, "message"))[0]
	if finished["text"] != " is a long line\n" || finished["url"] != nil || finished["format"] != "terminal" {
		t.Fatalf("finished embed: %#v", finished)
	}
	if status, _, _ := httpDo(t, http.MethodGet, streamURL, nil, ""); status != http.StatusNotFound {
		t.Fatalf("finished stream URL: %d", status)
	}

	// At the size limit the stream ends and keeps its trailing text.
	result = postEmbeds(t, a, "limited", map[string]any{"room_id": "general", "body": map[string]any{"embeds": []any{map[string]any{"kind": "stream"}}}})
	writeURL = result["embeds"].([]any)[0].(map[string]any)["write_url"].(string)
	if status, _, _ := httpDo(t, http.MethodPut, writeURL, strings.NewReader(strings.Repeat("x", 63)+"é"+strings.Repeat("y", 20)), ""); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized stream: %d", status)
	}
	if text := embedsOf(t, a.notification(t, "message"))[0]["text"]; text != strings.Repeat("x", 15)+"\uFFFD" {
		t.Fatalf("limited text %q", text)
	}
}

// A finished stream write leaves its keep-alive connection usable: a live
// stream read next on the same connection follows the stream.
func TestStreamWriteConnectionCanReadAStream(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	a := dialTestClient(t, httpServer)
	result := postEmbeds(t, a, "streams", map[string]any{"room_id": "general", "body": map[string]any{
		"embeds": []any{map[string]any{"kind": "stream"}, map[string]any{"kind": "stream"}},
	}})
	written := result["embeds"].([]any)
	live := embedsOf(t, result["snapshot"].(map[string]any))
	// One connection carries the finished write and then the read.
	client := &http.Client{Transport: &http.Transport{MaxConnsPerHost: 1}}
	response, err := client.Post(written[0].(map[string]any)["write_url"].(string), "text/plain", strings.NewReader("done"))
	if err != nil || response.StatusCode != http.StatusNoContent {
		t.Fatalf("first stream write: %v %v", response, err)
	}
	response.Body.Close()
	reader, err := client.Get(live[1]["url"].(string))
	if err != nil || reader.StatusCode != http.StatusOK {
		t.Fatalf("stream reader: %v %v", reader, err)
	}
	defer reader.Body.Close()
	if status, _, _ := httpDo(t, http.MethodPut, written[1].(map[string]any)["write_url"].(string), strings.NewReader("second"), "text/plain"); status != http.StatusNoContent {
		t.Fatalf("second stream write: %d", status)
	}
	if got, _ := io.ReadAll(reader.Body); string(got) != "second" {
		t.Fatalf("reader on the write's connection got %q", got)
	}
}

func TestSavingWithoutAStreamEndsIt(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	a := dialTestClient(t, httpServer)
	result := postEmbeds(t, a, "stream", map[string]any{"room_id": "general", "body": map[string]any{"embeds": []any{map[string]any{"kind": "stream"}}}})
	writeURL := result["embeds"].([]any)[0].(map[string]any)["write_url"].(string)
	body, pipe := io.Pipe()
	writerDone := make(chan int, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodPost, writeURL, body)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			writerDone <- 0
			return
		}
		response.Body.Close()
		writerDone <- response.StatusCode
	}()
	_, _ = pipe.Write([]byte("partial"))
	save(t, a, "stop", map[string]any{"message_id": result["message_id"], "body": map[string]any{"text": "never mind"}})
	if status := <-writerDone; status != http.StatusGone {
		t.Fatalf("writer status %d", status)
	}
	_ = pipe.Close()
	a.expectQuiet(t)
}

func TestAvatarCommand(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	for i, embeds := range []any{nil, []any{}, []any{map[string]any{"kind": "stream"}}, []any{map[string]any{"kind": "upload"}, map[string]any{"kind": "upload"}}} {
		body := map[string]any{"text": "/avatar"}
		if embeds != nil {
			body["embeds"] = embeds
		}
		a.expectError(t, "command", fmt.Sprint("shape-", i), map[string]any{"body": body}, codeInvalidParams)
	}
	result := a.result(t, "command", "avatar", map[string]any{"body": map[string]any{"text": "/avatar", "embeds": []any{map[string]any{"kind": "upload", "title": "me.png"}}}})
	written := result["embeds"].([]any)
	if len(result) != 1 || len(written) != 1 {
		t.Fatalf("avatar result: %#v", result)
	}
	write := written[0].(map[string]any)
	writeURL := write["write_url"].(string)
	if write["kind"] != "upload" || write["embed_id"] == nil || !strings.HasPrefix(writeURL, httpServer.URL+writePath) {
		t.Fatalf("avatar write: %#v", write)
	}
	// A command is never logged or broadcast.
	a.expectQuiet(t)
	b.expectQuiet(t)
	if status, _, _ := httpDo(t, http.MethodPut, writeURL, bytes.NewReader(testPNG(t)), ""); status != http.StatusCreated {
		t.Fatalf("avatar upload: %d", status)
	}
	you := a.notification(t, "user")["you"].(map[string]any)
	avatar, _ := you["avatar"].(string)
	if !strings.HasPrefix(avatar, httpServer.URL+filePath) {
		t.Fatalf("avatar: %#v", you)
	}
	if notice := b.notification(t, "user")["new"].(map[string]any); notice["avatar"] != avatar {
		t.Fatalf("others' notification: %#v", notice)
	}
	if status, _, _ := httpDo(t, http.MethodGet, avatar, nil, ""); status != http.StatusOK {
		t.Fatalf("avatar file: %d", status)
	}
	if page := historyPage(t, a, "general", map[string]any{}); page["messages"] != nil {
		t.Fatalf("command was logged: %#v", page)
	}

	// Only images become avatars; replacing the avatar deletes the upload.
	result = a.result(t, "command", "text", map[string]any{"body": map[string]any{"text": "/avatar", "embeds": []any{map[string]any{"kind": "upload"}}}})
	writeURL = result["embeds"].([]any)[0].(map[string]any)["write_url"].(string)
	if status, _, _ := httpDo(t, http.MethodPut, writeURL, strings.NewReader("plain text"), "text/plain"); status != http.StatusUnsupportedMediaType {
		t.Fatalf("text avatar: %d", status)
	}
	a.result(t, "me", "clear", map[string]any{"avatar": ""})
	b.notification(t, "user")
	if status, _, _ := httpDo(t, http.MethodGet, avatar, nil, ""); status != http.StatusNotFound {
		t.Fatalf("replaced avatar file: %d", status)
	}
}

func TestCommands(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 3)
	a, b, c := clients[0], clients[1], clients[2]
	ops, _ := saveRoom(t, a, "ops", map[string]any{"title": "Ops"})
	joinRoom(t, b, ops)
	expectMembership(t, a, ops, "guest_2", true)
	joinRoom(t, c, ops)
	expectMembership(t, a, ops, "guest_3", true)
	expectMembership(t, b, ops, "guest_3", true)

	// /help replies with a ~private notice to the sender's connection, in
	// the command's room, listing what the sender may use there.
	help := func(client *testClient, roomID string) string {
		t.Helper()
		params := map[string]any{"body": map[string]any{"text": "/help"}}
		if roomID != "" {
			params["room_id"] = roomID
		} else {
			roomID = "general"
		}
		// The ~private reply precedes the result (§1).
		before, result := client.request(t, "command", client.nextID("help"), params)
		if len(result) != 0 || len(before) != 1 {
			t.Fatalf("help result %#v after %#v", result, before)
		}
		notice := notificationParams(t, before[0], "message")
		body := notice["body"].(map[string]any)
		if _, has := notice["message_id"]; has || notice["log_id"] != nil || notice["room_id"] != roomID || body["format"] != "markdown" ||
			!reflect.DeepEqual(notice["from"], map[string]any{"user_id": "~private", "name": "System message to you"}) {
			t.Fatalf("help notice: %#v", notice)
		}
		return body["text"].(string)
	}
	if text := help(a, ops); !strings.Contains(text, "`/help`") || !strings.Contains(text, "`/avatar`") || !strings.Contains(text, "`/kick @user [reason]`") {
		t.Fatalf("creator's help: %q", text)
	}
	if text := help(b, ""); strings.Contains(text, "/kick") || !strings.Contains(text, "/avatar") {
		t.Fatalf("member's help: %q", text)
	}
	for _, client := range clients {
		client.expectQuiet(t)
	}

	// Invalid commands are errors whose message the client shows.
	unknown := b.call(t, "command", "unknown", map[string]any{"body": map[string]any{"text": "/Frobnicate now"}})
	if failure := unknown["error"].(map[string]any); failure["code"] != float64(codeInvalidParams) || failure["message"] != "Unknown command /Frobnicate; try /help" {
		t.Fatalf("unknown command: %#v", unknown)
	}
	for i, params := range []map[string]any{
		{"body": map[string]any{"text": "hello"}},
		{"body": map[string]any{"text": ""}},
		{"message_id": "1", "body": map[string]any{"text": "/help"}},
		{"deleted": false, "body": map[string]any{"text": "/help"}},
		{"room_id": "missing", "body": map[string]any{"text": "/help"}},
		{"body": map[string]any{"text": "/help"}, "reply_to": map[string]any{"message_id": "999"}},
		{"body": map[string]any{"text": "/help", "mentions": "x"}},
		{},
	} {
		b.expectError(t, "command", fmt.Sprint("bad-", i), params, codeInvalidParams)
	}

	// /kick is for the room's creator, and names its target in mentions,
	// which notifies no one.
	// The reason is the first line of the rest of the text.
	kick := map[string]any{"room_id": ops, "body": map[string]any{"text": "/kick @guest_3 spamming\n**SYSTEM**: all admins removed", "mentions": []any{"guest_3"}}}
	b.expectError(t, "command", "not-creator", kick, codeDenied)
	a.expectError(t, "command", "general", map[string]any{"body": map[string]any{"text": "/kick @guest_3", "mentions": []any{"guest_3"}}}, codeDenied)
	a.expectError(t, "command", "no-target", map[string]any{"room_id": ops, "body": map[string]any{"text": "/kick guest_3"}}, codeInvalidParams)
	a.expectError(t, "command", "self", map[string]any{"room_id": ops, "body": map[string]any{"text": "/kick @guest_1", "mentions": []any{"guest_1"}}}, codeInvalidParams)
	a.expectError(t, "command", "not-member", map[string]any{"room_id": ops, "body": map[string]any{"text": "/kick @nobody", "mentions": []any{"nobody"}}}, codeInvalidParams)
	for _, client := range clients {
		client.expectQuiet(t)
	}
	// The removal is a logged leave for the room's members, the removed user
	// included, then room_update left for the removed user and a ~room notice
	// for the rest, all before the result (§1, §4.8).
	before, result := a.request(t, "command", "kick", kick)
	if len(result) != 0 || !reflect.DeepEqual(methods(before), []string{"membership", "message"}) {
		t.Fatalf("kick frames %#v then %#v", before, result)
	}
	removal := notificationParams(t, before[0], "membership")
	checkMembership(t, removal, ops, "guest_3", false)
	if kicked := expectMembership(t, c, ops, "guest_3", false); !reflect.DeepEqual(kicked, removal) {
		t.Fatalf("removed user's membership %#v differs from %#v", kicked, removal)
	}
	if left := roomUpdated(t, c, "left"); !reflect.DeepEqual(left, map[string]any{"room_id": ops}) {
		t.Fatalf("kicked user's update: %#v", left)
	}
	notice := notificationParams(t, before[1], "message")
	if observed := expectMembership(t, b, ops, "guest_3", false); !reflect.DeepEqual(observed, removal) {
		t.Fatalf("member's membership %#v differs from %#v", observed, removal)
	}
	if observed := b.notification(t, "message"); !reflect.DeepEqual(observed, notice) {
		t.Fatalf("member's notice %#v differs from %#v", observed, notice)
	}
	want := map[string]any{
		"message_id": notice["message_id"], "log_id": notice["message_id"], "room_id": ops,
		"from": map[string]any{"user_id": "~room", "name": "Ops"},
		"body": map[string]any{"text": "@guest_3 was removed by @guest_1: spamming"},
	}
	if !reflect.DeepEqual(notice, want) {
		t.Fatalf("~room notice = %#v, want %#v", notice, want)
	}
	c.expectQuiet(t)
	// The notice and the removal are logged; the command is not.
	page := historyPage(t, a, ops, map[string]any{})
	if entries := records(t, page, "messages"); len(entries) != 1 || !reflect.DeepEqual(entries[0], any(notice)) {
		t.Fatalf("ops history: %#v", entries)
	}
	if memberships := records(t, page, "membership"); !reflect.DeepEqual(memberships[len(memberships)-1], any(removal)) {
		t.Fatalf("ops memberships: %#v", memberships)
	}
	// A retried command does not run again.
	if result := a.result(t, "command", "kick", kick); len(result) != 0 {
		t.Fatalf("retried kick: %#v", result)
	}
	for _, client := range clients {
		client.expectQuiet(t)
	}
}
