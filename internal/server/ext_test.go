package server

import (
	"encoding/json/jsontext"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// bigNumber is above 2^53, so a float64 cannot hold it exactly.
const bigNumber = "12345678901234567891"

// Writes merge ext one level deep (§4.12): each key a write carries replaces
// the kept value whole, an empty value ("", [], {}) removes the key, keys it
// leaves out stay, null is an ordinary value, and "ext": {} changes nothing.
// A message save leaves ext out to keep it.
func TestMessageSavesMergeExt(t *testing.T) {
	app, httpServer := newTestServer(t, DefaultConfig())
	a := dialTestClient(t, httpServer)

	id, created := save(t, a, "create", map[string]any{
		"body": map[string]any{"text": "hi"},
		"ext":  map[string]any{"irc": map[string]any{"nick": "ada_"}, "big": jsontext.Value(bigNumber), "gone": ""},
	})
	if want := map[string]any{"irc": map[string]any{"nick": "ada_"}, "big": 12345678901234567891.0}; !reflect.DeepEqual(created["ext"], any(want)) {
		t.Fatalf("created ext: %#v", created["ext"])
	}
	steps := []struct {
		ext  any // nil: the save leaves ext out
		want map[string]any
	}{
		{nil, map[string]any{"irc": map[string]any{"nick": "ada_"}, "big": 12345678901234567891.0}},
		{map[string]any{}, map[string]any{"irc": map[string]any{"nick": "ada_"}, "big": 12345678901234567891.0}},
		{map[string]any{"irc": map[string]any{"msgid": "a1"}, "matrix": nil}, map[string]any{"irc": map[string]any{"msgid": "a1"}, "big": 12345678901234567891.0, "matrix": nil}},
		{map[string]any{"irc": "", "matrix": []any{}, "never": map[string]any{}}, map[string]any{"big": 12345678901234567891.0}},
		{map[string]any{"big": map[string]any{}}, nil},
	}
	for i, step := range steps {
		params := map[string]any{"message_id": id, "body": map[string]any{"text": fmt.Sprint("edit ", i)}}
		if step.ext != nil {
			params["ext"] = step.ext
		}
		_, snapshot := save(t, a, fmt.Sprint("edit-", i), params)
		got, has := snapshot["ext"]
		if step.want == nil {
			if has {
				t.Fatalf("step %d: ext %#v, want none", i, got)
			}
			continue
		}
		if !reflect.DeepEqual(got, any(step.want)) {
			t.Fatalf("step %d: ext %#v, want %#v", i, got, step.want)
		}
		// Values are kept as they arrived, integers past 2^53 included.
		app.mu.RLock()
		raw := string(app.messages[id].currentRaw())
		app.mu.RUnlock()
		if !strings.Contains(raw, `"big":`+bigNumber) {
			t.Fatalf("step %d: stored snapshot lost the exact number: %s", i, raw)
		}
	}

	// The limit applies to the merged ext: keys that fit one at a time are
	// too_large together, and the refused save changes nothing.
	half := strings.Repeat("x", maxMessageExtBytes/2)
	save(t, a, "first-half", map[string]any{"message_id": id, "body": map[string]any{"text": "x"}, "ext": map[string]any{"a": half}})
	a.expectError(t, "message", "second-half", map[string]any{"message_id": id, "room_id": "general", "body": map[string]any{"text": "x"}, "ext": map[string]any{"b": half}}, codeTooLarge)
	_, kept := save(t, a, "after", map[string]any{"message_id": id, "body": map[string]any{"text": "x"}})
	if ext := kept["ext"].(map[string]any); len(ext) != 1 || ext["a"] != half {
		t.Fatalf("a refused save changed ext: %v keys", len(ext))
	}
	a.expectError(t, "message", "new-too-large", map[string]any{"room_id": "general", "body": map[string]any{"text": "x"}, "ext": map[string]any{"a": half, "b": half}}, codeTooLarge)
	a.expectError(t, "message", "bad-ext", map[string]any{"room_id": "general", "body": map[string]any{"text": "x"}, "ext": []any{}}, codeInvalidParams)
	a.expectQuiet(t)

	// A tombstone carries no ext, and a save of a tombstone merges into an
	// empty ext (§4.12).
	_, tombstone := save(t, a, "delete", map[string]any{"message_id": id, "deleted": true, "ext": map[string]any{"irc": "x"}})
	if _, has := tombstone["ext"]; has {
		t.Fatalf("tombstone: %#v", tombstone)
	}
	_, restored := save(t, a, "restore", map[string]any{"message_id": id, "body": map[string]any{"text": "back"}, "ext": map[string]any{"irc": "x"}})
	if want := map[string]any{"irc": "x"}; !reflect.DeepEqual(restored["ext"], any(want)) {
		t.Fatalf("a save of a tombstone merged into %#v, want an empty ext", restored["ext"])
	}
}

// Concurrent saves of different ext keys of one message, from two
// connections of its author, both survive: each merges into the snapshot
// current when the server applies it (§4.4).
func TestConcurrentSavesOfDifferentExtKeysBothSurvive(t *testing.T) {
	_, httpServer := passkeyTestServer(t)
	first, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	registered := registerTestPasskey(t, first, newTestAuthenticator(t))
	second, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	second.result(t, "auth", "resume", map[string]any{"scheme": "token", "token": registered["token"]})
	first.drain(t)
	second.drain(t)
	id, _ := save(t, first, "create", map[string]any{"body": map[string]any{"text": "hi"}})
	second.notification(t, "message")

	const rounds = 20
	clients := map[string]*testClient{"left": first, "right": second}
	for i := range rounds {
		for key, c := range clients {
			c.write(t, map[string]any{"method": "message", "id": fmt.Sprint(key, i), "params": map[string]any{
				"message_id": id, "room_id": "general", "body": map[string]any{"text": "hi"},
				"ext": map[string]any{key: i},
			}})
		}
	}
	// Each connection receives every snapshot, then its own replies; the
	// last snapshot either receives carries both keys at their last values.
	want := map[string]any{"left": float64(rounds - 1), "right": float64(rounds - 1)}
	for _, c := range clients {
		var last map[string]any
		snapshots, replies := 0, 0
		for snapshots < 2*rounds || replies < rounds {
			frame := c.read(t)
			switch {
			case frame["method"] == "message":
				last = frame["params"].(map[string]any)
				snapshots++
			case frame["result"] != nil:
				replies++
			default:
				t.Fatalf("unexpected frame: %#v", frame)
			}
		}
		if !reflect.DeepEqual(last["ext"], any(want)) {
			t.Fatalf("last snapshot's ext: %#v, want %#v", last["ext"], want)
		}
	}
}

// room_set replaces the client fields of a room, but merges ext (§4.3.4,
// §4.12); the limit applies to the merged ext.
func TestRoomSetMergesExt(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	a := dialTestClient(t, httpServer)
	roomID, created := saveRoom(t, a, "create", map[string]any{"title": "Ops", "ext": map[string]any{"irc": map[string]any{"channel": "#ops"}, "none": []any{}}})
	if want := map[string]any{"irc": map[string]any{"channel": "#ops"}}; !reflect.DeepEqual(created["ext"], any(want)) {
		t.Fatalf("created ext: %#v", created["ext"])
	}
	_, kept := saveRoom(t, a, "rename", map[string]any{"room_id": roomID, "title": "Ops 2"})
	if want := map[string]any{"irc": map[string]any{"channel": "#ops"}}; !reflect.DeepEqual(kept["ext"], any(want)) || kept["title"] != "Ops 2" {
		t.Fatalf("an edit without ext: %#v", kept)
	}
	_, merged := saveRoom(t, a, "merge", map[string]any{"room_id": roomID, "title": "Ops 2", "ext": map[string]any{"matrix": nil}})
	if want := map[string]any{"irc": map[string]any{"channel": "#ops"}, "matrix": nil}; !reflect.DeepEqual(merged["ext"], any(want)) {
		t.Fatalf("merged ext: %#v", merged["ext"])
	}
	_, cleared := saveRoom(t, a, "clear", map[string]any{"room_id": roomID, "ext": map[string]any{"irc": "", "matrix": map[string]any{}}})
	if _, has := cleared["ext"]; has || cleared["title"] != nil {
		t.Fatalf("cleared: %#v", cleared)
	}
	half := strings.Repeat("x", maxRoomExtBytes/2)
	saveRoom(t, a, "half", map[string]any{"room_id": roomID, "ext": map[string]any{"a": half}})
	a.expectError(t, "room_set", "over", map[string]any{"room_id": roomID, "ext": map[string]any{"b": half}}, codeTooLarge)
	a.expectQuiet(t)
}

// A profile's ext limit applies to the merged ext, and a refused `me`
// changes nothing.
func TestProfileExtLimitAppliesToTheMergedExt(t *testing.T) {
	app, httpServer := newTestServer(t, DefaultConfig())
	a := dialTestClient(t, httpServer)
	half := strings.Repeat("x", maxProfileExtBytes/2)
	a.result(t, "me", "a", map[string]any{"ext": map[string]any{"a": half, "big": jsontext.Value(bigNumber)}})
	a.expectError(t, "me", "b", map[string]any{"name": "Ada", "ext": map[string]any{"b": half}}, codeTooLarge)
	you := a.result(t, "me", "same", map[string]any{})["you"].(map[string]any)
	if ext := you["ext"].(map[string]any); len(ext) != 2 || you["name"] != nil {
		t.Fatalf("a refused me changed the profile: %#v", you)
	}
	app.mu.RLock()
	raw := string(app.users[a.userID].ext["big"])
	app.mu.RUnlock()
	if raw != bigNumber {
		t.Fatalf("stored ext value %s, want %s", raw, bigNumber)
	}
}

// Clients send every request with an id, and a server may ignore a request
// method sent without one (§1.1): this one does, before sign-in and after,
// whatever it is.
func TestRequestsWithoutAnIDAreIgnored(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	for _, frame := range []map[string]any{
		{"method": "me", "params": map[string]any{"name": "Nobody"}},
		{"method": "message", "params": map[string]any{"room_id": "general", "body": map[string]any{"text": "hi"}}},
		{"method": "room_set", "params": map[string]any{"title": "Nothing"}},
		{"method": "room_join", "params": map[string]any{"room_id": "general"}},
		{"method": "status", "params": map[string]any{"mute": true}},
		{"method": "auth", "params": map[string]any{"scheme": "guest"}},
	} {
		a.write(t, frame)
	}
	a.expectQuiet(t)
	b.expectQuiet(t)
	if rooms := roomIDs(t, listRooms(t, a, map[string]any{})["joined"]); len(rooms) != 1 {
		t.Fatalf("rooms after ignored requests: %v", rooms)
	}
}

// Every notification a sign-in causes on its connection comes after the
// auth result (§3.2): the new guest's join, then the statuses of those who
// share a room (§4.5).
func TestSignInNotificationsFollowTheResult(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	a := dialTestClient(t, httpServer)
	b, _ := dialRaw(t, httpServer)
	watching(b)
	b.write(t, map[string]any{"method": "auth", "id": "auth", "params": map[string]any{"scheme": "guest"}})
	frames := []map[string]any{b.read(t), b.read(t), b.read(t)}
	if got := methods(frames); !reflect.DeepEqual(got, []string{"reply", "room_update", "user"}) {
		t.Fatalf("sign-in frames: %v", got)
	}
	b.userID = frames[0]["result"].(map[string]any)["you"].(map[string]any)["user_id"].(string)
	checkMembership(t, membershipOnly(t, frames[1]), "general", b.userID, true)
	checkStatus(t, frames[2], a.userID, "online")
	b.expectQuiet(t)
}
