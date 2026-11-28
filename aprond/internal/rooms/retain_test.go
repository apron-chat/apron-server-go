package rooms

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"strings"
	"testing"

	"github.com/apron-chat/apron-server-go/aprond/internal/roomlog"
	"github.com/apron-chat/apron-server-go/store"
)

// Retain discards the start of a room's log: history_log_id moves past it
// and never back, the latest record stays, and a part with memberships
// goes after a record of every member, joined.
func TestRetain(t *testing.T) {
	table := newTable()
	general := table.Create("general", nil, false, "", roomlog.RoomRecord{})
	ada, bob := &user{id: "ada"}, &user{id: "bob"}
	join(t, table, general, ada, bob)
	var posted []*roomlog.Record
	for i := range 5 {
		m := table.NewMessage(roomlog.FormatID(int64(1000+i)), ada.from(), "ada")
		record, _, _ := snapshot(table, m, general.ID(), "hi")
		posted = append(posted, record)
	}
	table.Entries()
	log := general.Log()
	created := log.HistoryID()
	before := posted[2].ID
	discarded := table.Retain(general, before)
	// The creation record, two joins, and two messages.
	if len(discarded) != 5 || log.HistoryID() != before || log.Retained() != before {
		t.Fatalf("discarded %d, history_log_id %d (created %d), want 5 and %d", len(discarded), log.HistoryID(), created, before)
	}
	latest := log.Since(0, 1)[0]
	if latest.Kind != roomlog.KindMembership || log.Latest() != latest.ID {
		t.Fatalf("the latest record is %s, want the members'", latest.Raw)
	}
	var members roomlog.Membership
	_ = json.Unmarshal(latest.Raw, &members)
	if len(members.Members) != 2 || members.Members[0].User.UserID != "ada" || !members.Members[0].Joined || !members.Members[1].Joined {
		t.Fatalf("the members' record is %s", latest.Raw)
	}
	if table.Retain(general, before-1) != nil || log.HistoryID() != before {
		t.Fatal("history_log_id moved back")
	}
	// A part without memberships goes without another members' record,
	// and the latest record always stays.
	n := log.Records()
	discarded = table.Retain(general, log.Latest()+100)
	if len(discarded) != n-1 || log.Records() != 1 || log.HistoryID() != latest.ID {
		t.Fatalf("discarded %d of %d, %d left from %d", len(discarded), n, log.Records(), log.HistoryID())
	}
}

// Forget takes out of the log, and the store, the records no room holds,
// a move snapshot once both rooms let it go, and the messages none of
// whose snapshots is left.
func TestForget(t *testing.T) {
	table := newTable()
	a := table.Create("a", nil, false, "", roomlog.RoomRecord{})
	b := table.Create("b", nil, false, "", roomlog.RoomRecord{})
	ada := &user{id: "ada"}
	stays := table.NewMessage("1", ada.from(), "ada")
	snapshot(table, stays, a.ID(), "first")
	goes := table.NewMessage("2", ada.from(), "ada")
	snapshot(table, goes, a.ID(), "gone")
	_, _, moved := snapshot(table, stays, b.ID(), "moved")
	if !moved {
		t.Fatal("no move")
	}
	table.Entries()
	table.log.Entries()
	// a lets go of everything but its latest record, the move.
	gone := table.Forget(table.Retain(a, a.Log().Latest()))
	if len(gone) != 1 || gone[0] != goes || table.Message("2") != nil {
		t.Fatalf("forgot %v, want message 2", gone)
	}
	if kept := table.Message("1"); kept == nil || len(kept.Records()) != 1 {
		t.Fatalf("message 1 kept %v", kept)
	}
	deleted := func(entries []store.Entry, kind string) []string {
		var ids []string
		for _, e := range entries {
			if e.Kind == kind && e.Value == nil {
				ids = append(ids, e.ID)
			}
		}
		slices.Sort(ids)
		return ids
	}
	if got := deleted(table.Entries(), MessageEntryKind); !slices.Equal(got, []string{"2"}) {
		t.Errorf("deleted messages %v, want 2", got)
	}
	// Room a's creation record and both first snapshots go; the move stays
	// in a, and b's creation record in b.
	if got := deleted(table.log.Entries(), roomlog.EntryKind); len(got) != 3 {
		t.Errorf("deleted records %v, want three", got)
	}
	// b lets go of all but its latest record, the move: still held by a,
	// which keeps its own latest, it stays.
	table.Forget(table.Retain(b, b.Log().Latest()))
	if table.Message("1") == nil || b.Log().Records() != 1 {
		t.Fatal("the move snapshot went while a room held it")
	}
}

// A room's entry keeps where its retained history starts, and an entry
// without it, a room that never discarded, restores as before.
func TestRetainedStored(t *testing.T) {
	table := newTable()
	general := table.Create("general", nil, false, "", roomlog.RoomRecord{})
	ada := &user{id: "ada"}
	join(t, table, general, ada)
	m := table.NewMessage("1", ada.from(), "ada")
	record, _, _ := snapshot(table, m, general.ID(), "hi")
	if strings.Contains(string(Entry(general).Value), "retained_id") {
		t.Fatal("a room that discarded nothing stores retained_id")
	}
	table.Retain(general, record.ID)
	restored := newTable()
	if err := restored.Restore(map[string]jsontext.Value{"general": Entry(general).Value}); err != nil {
		t.Fatal(err)
	}
	if got := restored.Get("general").Log().HistoryID(); got != record.ID {
		t.Fatalf("restored history_log_id %d, want %d", got, record.ID)
	}
}
