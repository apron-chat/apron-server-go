package rooms

import (
	"bytes"
	"encoding/json/jsontext"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/aprond/internal/roomlog"
	"github.com/apron-chat/apron-server-go/store"
)

func snapshot(t *Table[*user, *Message], m *Message, room, text string) (*roomlog.Record, []*Room[*user], bool) {
	logID := t.log.NextID()
	return t.Snapshot(m, &roomlog.Message{
		MessageID: m.ID(), LogID: roomlog.FormatID(logID), RoomID: room, From: m.From(),
		Body: &roomlog.Body{Text: text},
	}, logID)
}

// A snapshot names the previous one, and a move is logged in both rooms;
// it names the room it left only to those who may see that room.
func TestSnapshots(t *testing.T) {
	table := newTable()
	ada, bob := &user{id: "ada"}, &user{id: "bob"}
	general := table.Create("general", nil, false, "", roomlog.RoomRecord{})
	ops := table.Create("", nil, false, "", roomlog.RoomRecord{})
	secret := table.Create("", nil, true, "", roomlog.RoomRecord{})
	join(t, table, general, ada, bob)
	join(t, table, secret, ada)
	m := table.NewMessage("1", ada.from(), "ada")
	first, rooms, moved := snapshot(table, m, general.ID(), "hi")
	if moved || len(rooms) != 1 || strings.Contains(string(first.Raw), "prev_") || first.Message != "1" {
		t.Fatalf("first snapshot %s in %d rooms, moved %v", first.Raw, len(rooms), moved)
	}
	if general.Active("ada") != first.ID {
		t.Error("posting did not make the sender active")
	}
	second, rooms, moved := snapshot(table, m, ops.ID(), "moved")
	if !moved || len(rooms) != 2 || m.RoomID() != ops.ID() || m.Arrival(ops.ID()) != second.ID || m.Arrival(general.ID()) != first.ID {
		t.Fatalf("move %s in %d rooms, moved %v", second.Raw, len(rooms), moved)
	}
	current := m.Current()
	if current.PrevLogID != roomlog.FormatID(first.ID) || current.PrevRoomID != general.ID() || current.Body.Text != "moved" {
		t.Errorf("move snapshot %s", second.Raw)
	}
	// Out of a private room into one more people see, the snapshot does not
	// name the private room.
	hidden := table.NewMessage("2", ada.from(), "ada")
	snapshot(table, hidden, secret.ID(), "secret")
	if table.Message("2") != hidden || table.VisibleMessage("2", ada) != hidden || table.VisibleMessage("2", bob) != nil || table.VisibleMessage("3", ada) != nil {
		t.Error("a message in a private room is visible to a non-member, or not kept")
	}
	if record, _, _ := snapshot(table, hidden, general.ID(), "out"); hidden.Current().PrevRoomID != "" {
		t.Errorf("a move out of a private room names it: %s", record.Raw)
	}
	table.Redact(hidden)
	for _, record := range hidden.Records() {
		if !strings.Contains(string(record.Raw), `"deleted":true`) || strings.Contains(string(record.Raw), "body") {
			t.Errorf("redacted snapshot %s", record.Raw)
		}
	}
}

// Reaction sets replace each other, are logged in the message's room, and
// are stored with the message.
func TestReactions(t *testing.T) {
	table := newTable()
	ada, bob := &user{id: "ada"}, &user{id: "bob"}
	general := table.Create("general", nil, false, "", roomlog.RoomRecord{})
	m := table.NewMessage("1", ada.from(), "ada")
	snapshot(table, m, general.ID(), "hi")
	table.Entries()
	record, r := table.React(m, bob.from(), []string{"👍", "❤️"})
	if r != general || record.Kind != roomlog.KindReactions || general.Log().Latest() != record.ID {
		t.Fatalf("reactions %s in %v", record.Raw, r)
	}
	if !m.SameEmojis("bob", []string{"❤️", "👍"}) || m.SameEmojis("bob", []string{"👍"}) {
		t.Error("SameEmojis is not set equality")
	}
	table.React(m, ada.from(), []string{"👍"})
	table.React(m, ada.from(), nil)
	if got := m.Reactions(); len(got) != 1 || got[0].From.UserID != "bob" || !slices.Equal(m.Emojis("bob"), []string{"👍", "❤️"}) {
		t.Errorf("reactions %+v", got)
	}
	entry := MessageEntry(m)
	// The room's entry changes too, with its latest record.
	entries := slices.DeleteFunc(table.Entries(), func(e store.Entry) bool { return e.Kind != MessageEntryKind })
	if len(entries) != 1 || !bytes.Equal(entries[0].Value, entry.Value) || table.Entries() != nil {
		t.Errorf("reacting stored %d messages, want the message once", len(entries))
	}
	records := map[int64]*roomlog.Record{}
	for _, record := range m.Records() {
		records[record.ID] = record
	}
	again := newTable()
	if err := again.RestoreMessages(map[string]jsontext.Value{m.ID(): entry.Value}, records); err != nil {
		t.Fatal(err)
	}
	restored := again.Message(m.ID())
	if restored == nil {
		t.Fatal("the message was not restored")
	}
	if stored := MessageEntry(restored); !bytes.Equal(stored.Value, entry.Value) {
		t.Errorf("message is stored again as\n%s\nwant\n%s", stored.Value, entry.Value)
	}
	gone := newTable()
	if err := gone.RestoreMessages(map[string]jsontext.Value{m.ID(): entry.Value}, nil); err != nil || gone.Message(m.ID()) != nil {
		t.Error("a message without its records was restored")
	}
}

// A message with many reaction sets stores its entry for reactions at most
// once every EntryDelay; restored from an entry that lags behind, it has
// the reactions its records logged since.
func TestManyReactionsLag(t *testing.T) {
	table := newTable()
	general := table.Create("general", nil, false, "", roomlog.RoomRecord{})
	ada := &user{id: "ada"}
	m := table.NewMessage("1", ada.from(), "ada")
	snapshot(table, m, general.ID(), "hot")
	var reactors []*user
	for i := range LaterReactions + 2 {
		reactors = append(reactors, &user{id: "u" + strings.Repeat("x", i)})
	}
	for _, u := range reactors[:LaterReactions] {
		table.React(m, u.from(), []string{"👍"})
	}
	messages := func(entries []store.Entry) []store.Entry {
		return slices.DeleteFunc(entries, func(e store.Entry) bool { return e.Kind != MessageEntryKind })
	}
	start := time.Now()
	// Up to LaterReactions sets, each reaction stores the entry at once.
	if got := messages(table.EntriesDue(start)); len(got) != 1 {
		t.Fatalf("stored %d message entries, want 1", len(got))
	}
	stale := MessageEntry(m)
	table.React(m, reactors[LaterReactions].from(), []string{"🎉"})
	table.React(m, reactors[0].from(), nil)
	table.React(m, reactors[1].from(), []string{"❤️"})
	if got := messages(table.EntriesDue(start)); len(got) != 0 {
		t.Fatalf("stored %d message entries at once, want none", len(got))
	}
	if got := messages(table.EntriesDue(start.Add(EntryDelay))); len(got) != 1 {
		t.Fatalf("stored %d message entries after the delay, want 1", len(got))
	}
	want := m.Reactions()
	records := map[int64]*roomlog.Record{}
	for _, r := range general.Log().Since(0, 1<<30) {
		records[r.ID] = r
	}
	for name, entry := range map[string]jsontext.Value{"lagging": stale.Value, "current": MessageEntry(m).Value} {
		restored := newTable()
		if err := restored.RestoreMessages(map[string]jsontext.Value{"1": entry}, records); err != nil {
			t.Fatal(err)
		}
		if got := restored.Message("1").Reactions(); !reflect.DeepEqual(got, want) {
			t.Errorf("restored from the %s entry: %d sets, want %d", name, len(got), len(want))
		}
	}
}
