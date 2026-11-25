package aprond

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"regexp"
	"slices"
	"strconv"
	"testing"

	"github.com/apron-chat/apron-server-go/aprond/internal/roomlog"
	"github.com/apron-chat/apron-server-go/aprond/internal/rooms"
	"github.com/apron-chat/apron-server-go/store"
)

// testdata/stored-log.jsonl holds the record, room, and message entries a
// server stored before rooms, records, and messages had typed shapes
// (roomlog), after storedLogScenario. Each is stored again byte for byte.

func loadStoredLog(t *testing.T) []store.Entry {
	t.Helper()
	file, err := os.Open("testdata/stored-log.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var entries []store.Entry
	lines := bufio.NewScanner(file)
	lines.Buffer(nil, 1<<20)
	for lines.Scan() {
		var line struct{ Kind, ID, Value string }
		if err := json.Unmarshal(lines.Bytes(), &line, json.MatchCaseInsensitiveNames(true)); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, store.Entry{Kind: line.Kind, ID: line.ID, Value: []byte(line.Value)})
	}
	return entries
}

// storedLogScenario makes rooms, a private room and a thread, messages
// with mentions, ext, and a reply, reactions set and cleared, a read
// cursor, a rename, a move, a deletion, a room edit, a leave, and a
// message whose body carries embeds and members the server does not know.
func storedLogScenario(t *testing.T, app *Server, a, b *testClient) {
	n := 0
	do := func(c *testClient, method string, params map[string]any) map[string]any {
		n++
		_, result := c.request(t, method, "r"+strconv.Itoa(n), params)
		return result
	}
	ops := do(a, "room_set", map[string]any{"title": "Ops <&>", "description": "Ops talk", "ext": map[string]any{"x": map[string]any{"n": jsontext.Value("12345678901234567890")}}})["room_id"]
	do(b, "room_join", map[string]any{"room_id": ops})
	priv := do(a, "room_set", map[string]any{"title": "Secret", "private": true})["room_id"]
	thread := do(a, "room_set", map[string]any{"parent_room_id": ops, "description": "A thread\nsecond line"})["room_id"]
	first := do(a, "message", map[string]any{"room_id": ops, "body": map[string]any{"text": "hi @b", "mentions": []any{b.userID}, "format": "markdown"}, "ext": map[string]any{"y": true}})["message_id"]
	do(b, "message", map[string]any{"room_id": ops, "body": map[string]any{"text": "re"}, "reply_to": map[string]any{"message_id": first}})
	do(b, "reactions", map[string]any{"message_id": first, "emojis": []any{"👍", "❤️"}})
	do(a, "reactions", map[string]any{"message_id": first, "emojis": []any{"👍"}})
	do(a, "reactions", map[string]any{"message_id": first, "emojis": []any{}})
	b.write(t, map[string]any{"method": "activity", "params": map[string]any{"room_id": ops, "read_message_id": first}})
	do(b, "me", map[string]any{"name": "Bea"})
	do(b, "message", map[string]any{"room_id": ops, "body": map[string]any{"text": "named"}})
	do(a, "message", map[string]any{"message_id": first, "room_id": thread, "body": map[string]any{"text": "moved"}})
	m3 := do(a, "message", map[string]any{"room_id": priv, "body": map[string]any{"text": "secret"}})["message_id"]
	do(a, "message", map[string]any{"message_id": m3, "room_id": priv, "deleted": true})
	do(a, "room_set", map[string]any{"room_id": ops, "title": "Ops 2", "description": "Ops talk"})
	do(b, "room_leave", map[string]any{"room_id": ops})
	// A message body keeps what the server does not know, and the server
	// adds embed_ids to its embeds.
	media := do(a, "room_set", map[string]any{"title": "Media"})["room_id"]
	do(a, "message", map[string]any{"room_id": media, "body": map[string]any{
		"text": "see", "format": "plain", "mentions": []any{}, "color": "red", "zz": map[string]any{"b": 1, "a": []any{true, nil}},
		"embeds": []any{map[string]any{"kind": "link", "url": "https://example.com/", "title": "Example"}, map[string]any{"kind": "upload", "name": "a.txt"}},
	}, "ext": map[string]any{"b": "x", "a": jsontext.Value(`{"z":1,"y":2}`)}})
	if err := app.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// logIDPattern matches the log_ids in stored entries, which depend on
// when they were made.
var logIDPattern = regexp.MustCompile(`1[0-9]{12}`)

// normalizeLogIDs renames each log_id in entries by its rank, so that
// entries made at other times compare.
func normalizeLogIDs(entries []store.Entry) map[string]string {
	var ids []string
	for _, e := range entries {
		ids = append(ids, logIDPattern.FindAllString(e.ID+" "+string(e.Value), -1)...)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	rename := func(text string) string {
		return logIDPattern.ReplaceAllStringFunc(text, func(id string) string {
			i, _ := slices.BinarySearch(ids, id)
			return "#" + strconv.Itoa(i)
		})
	}
	normalized := make(map[string]string, len(entries))
	for _, e := range entries {
		normalized[e.Kind+"/"+rename(e.ID)] = rename(string(e.Value))
	}
	return normalized
}

func TestStoredLogIsUnchanged(t *testing.T) {
	want := normalizeLogIDs(loadStoredLog(t))
	mem := store.NewMemory()
	config := DefaultConfig()
	config.Store = mem
	app, httpServer := newTestServer(t, config)
	storedLogScenario(t, app, dialTestClient(t, httpServer), dialTestClient(t, httpServer))
	var entries []store.Entry
	_ = mem.Load(func(e store.Entry) error {
		switch e.Kind {
		case roomlog.EntryKind, entryRoom, entryMessage:
			entries = append(entries, e)
		}
		return nil
	})
	got := normalizeLogIDs(entries)
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s is stored as\n%s\nwant\n%s", key, got[key], value)
		}
	}
	if len(got) != len(want) {
		t.Errorf("%d entries stored, want %d", len(got), len(want))
	}
}

func TestStoredShapesRoundTrip(t *testing.T) {
	entries := loadStoredLog(t)
	records := make(map[string]jsontext.Value)
	stored := make(map[string]jsontext.Value)
	messages := make(map[string]jsontext.Value)
	for _, e := range entries {
		switch e.Kind {
		case roomlog.EntryKind:
			records[e.ID] = e.Value
		case entryRoom:
			stored[e.ID] = e.Value
		case entryMessage:
			messages[e.ID] = e.Value
		}
	}
	// Rooms are restored with every member they name, and stored again.
	log := roomlog.NewLog()
	table := rooms.NewTable[*userState](log, holdMessage)
	if err := table.Restore(stored); err != nil {
		t.Fatal(err)
	}
	table.RestoreMembers(func(_ *roomState, id string) (*userState, bool) { return newUserState(id, ""), true })
	for id, value := range stored {
		if again := rooms.Entry(table.Get(id)).Value; !bytes.Equal(again, value) {
			t.Errorf("room %s is stored again as\n%s\nwant\n%s", id, again, value)
		}
	}
	restored, err := log.Restore(records, func(string) *roomlog.Timeline { return nil })
	if err != nil {
		t.Fatal(err)
	}
	// Messages are restored with their records, and stored again.
	if err := table.RestoreMessages(messages, restored); err != nil {
		t.Fatal(err)
	}
	for id, value := range messages {
		m := table.Message(id)
		if m == nil {
			t.Fatalf("message %s was not restored", id)
		}
		if again := rooms.MessageEntry(m.Message).Value; !bytes.Equal(again, value) {
			t.Errorf("message %s is stored again as\n%s\nwant\n%s", id, again, value)
		}
	}
	for _, record := range restored {
		entry := roomlog.Entry(record)
		if !bytes.Equal(entry.Value, records[entry.ID]) {
			t.Errorf("record %s is stored again as\n%s\nwant\n%s", entry.ID, entry.Value, records[entry.ID])
		}
		var shape any
		switch record.Kind {
		case roomlog.KindRoom:
			shape = new(roomlog.RoomRecord)
		case roomlog.KindMembership:
			shape = new(roomlog.Membership)
		case roomlog.KindReactions:
			shape = new(roomlog.Reactions)
		case roomlog.KindMessage:
			shape = new(roomlog.Message)
		}
		if err := json.Unmarshal(record.Raw, shape); err != nil {
			t.Fatal(err)
		}
		if again := encodeJSON(shape); !bytes.Equal(again, record.Raw) {
			t.Errorf("record %d encodes again as\n%s\nwant\n%s", record.ID, again, record.Raw)
		}
	}
}
