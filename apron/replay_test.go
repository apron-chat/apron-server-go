package apron_test

import (
	"encoding/json/v2"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/apron-chat/apron-server-go/apron"
)

func user(id string) apron.User { return apron.User{UserID: id, Name: id} }

func snapshot(messageID, logID apron.LogID, text string) apron.Message {
	return apron.Message{MessageID: messageID, LogID: logID, RoomID: "general", From: user("ada"), Body: apron.Body{Text: text}}
}

// replayLog is a log with every kind of record, and keys that change.
func replayLog() []apron.Record {
	edited := snapshot(10, 13, "hello, world")
	edited.PrevLogID = 10
	tombstone := apron.Message{MessageID: 11, LogID: 15, RoomID: "general", From: user("bob"), Deleted: true}
	renamed := apron.Room{RoomID: "general", LogID: 14, PrevLogID: 1, Title: "General (ops)"}
	return []apron.Record{
		{LogID: 1, Room: &apron.Room{RoomID: "general", LogID: 1, Title: "General"}},
		{LogID: 2, Membership: &apron.Membership{LogID: 2, RoomID: "general", Members: []apron.Member{{User: user("ada"), Joined: true}}}},
		{LogID: 3, Membership: &apron.Membership{LogID: 3, RoomID: "general", Members: []apron.Member{{User: user("bob"), Joined: true}}}},
		{LogID: 10, Message: new(snapshot(10, 10, "hello"))},
		{LogID: 11, Message: new(snapshot(11, 11, "hi"))},
		{LogID: 12, Reactions: &apron.Reactions{LogID: 12, MessageID: 10, RoomID: "general", Reactions: []apron.Reaction{{From: user("bob"), Emojis: []string{"👍"}}}}},
		{LogID: 13, Message: &edited},
		{LogID: 14, Room: &renamed},
		{LogID: 15, Message: &tombstone},
		{LogID: 16, Reactions: &apron.Reactions{LogID: 16, MessageID: 10, RoomID: "general", Reactions: []apron.Reaction{{From: user("carol"), Emojis: []string{"🎉"}}}}},
		{LogID: 17, Membership: &apron.Membership{LogID: 17, RoomID: "general", Members: []apron.Member{{User: user("bob"), Joined: false}}}},
		{LogID: 18, Reactions: &apron.Reactions{LogID: 18, MessageID: 10, RoomID: "general", Reactions: []apron.Reaction{{From: user("bob"), Emojis: []string{}}}}},
	}
}

func checkReplayed(t *testing.T, r *apron.Replay) {
	t.Helper()
	if room, ok := r.Room("general"); !ok || room.Title != "General (ops)" {
		t.Errorf("room %+v, %v", room, ok)
	}
	if m, ok := r.Message(10); !ok || m.Body.Text != "hello, world" {
		t.Errorf("message 10 %+v, %v", m, ok)
	}
	if m, ok := r.Message(11); !ok || !m.Deleted {
		t.Errorf("message 11 %+v, %v", m, ok)
	}
	if got := r.Messages("general"); len(got) != 2 || got[0].MessageID != 10 || got[1].MessageID != 11 {
		t.Errorf("messages %+v", got)
	}
	if got := r.Reactions(10); len(got) != 1 || got[0].From.UserID != "carol" {
		t.Errorf("reactions %+v", got)
	}
	if got := r.Members("general"); len(got) != 1 || got[0].UserID != "ada" {
		t.Errorf("members %+v", got)
	}
	if r.Latest() != 18 {
		t.Errorf("latest %v", r.Latest())
	}
}

// Replay ends in the same state whatever the order of the records, and
// applying them again changes nothing. Its records are a compacted log,
// which leaves the same state, and encode alike whatever order the Replay
// was built in.
func TestReplayRecords(t *testing.T) {
	log := replayLog()
	for range 20 {
		var r apron.Replay
		for _, i := range rand.Perm(len(log)) {
			r.Apply(log[i])
		}
		checkReplayed(t, &r)
		for _, rec := range log {
			if r.Apply(rec) {
				t.Fatalf("applied %+v again", rec)
			}
		}
	}
	var forward, backward apron.Replay
	for i := range log {
		forward.Apply(log[i])
		backward.Apply(log[len(log)-1-i])
	}
	records := forward.Records()
	if !slices.IsSortedFunc(records, func(a, b apron.Record) int { return int(a.LogID - b.LogID) }) {
		t.Fatalf("records out of order: %+v", records)
	}
	a, _ := json.Marshal(records)
	b, _ := json.Marshal(backward.Records())
	if string(a) != string(b) {
		t.Fatalf("records differ:\n%s\n%s", a, b)
	}
	var again apron.Replay
	for _, rec := range records {
		again.Apply(rec)
	}
	checkReplayed(t, &again)
	// The page of the whole log applies as its records do.
	var paged apron.Replay
	page := apron.HistoryResult{}
	for _, rec := range log {
		switch {
		case rec.Room != nil:
			page.Rooms = append(page.Rooms, *rec.Room)
		case rec.Message != nil:
			page.Messages = append(page.Messages, *rec.Message)
		case rec.Reactions != nil:
			page.Reactions = append(page.Reactions, *rec.Reactions)
		case rec.Membership != nil:
			page.Memberships = append(page.Memberships, *rec.Membership)
		}
	}
	if n := paged.ApplyHistory(page); n != len(log) {
		t.Fatalf("applied %d of %d", n, len(log))
	}
	checkReplayed(t, &paged)
}

// At an equal log_id, a tombstone wins (§2.3), as a redaction rewrites an
// earlier snapshot.
func TestReplayTombstoneWins(t *testing.T) {
	live := snapshot(10, 10, "secret")
	redacted := apron.Message{MessageID: 10, LogID: 10, RoomID: "general", From: user("ada"), Deleted: true}
	for _, order := range [][]apron.Message{{live, redacted}, {redacted, live}} {
		var r apron.Replay
		for _, m := range order {
			r.ApplyMessage(m)
		}
		if m, _ := r.Message(10); !m.Deleted {
			t.Fatalf("kept %+v", m)
		}
	}
	if !redacted.Replaces(live) || live.Replaces(redacted) || live.Replaces(live) {
		t.Fatal("Replaces")
	}
	// A notice is never installed.
	var r apron.Replay
	if r.ApplyMessage(apron.Notice("general", "hi")) || len(r.Messages("")) != 0 {
		t.Fatal("installed a notice")
	}
}

// Snapshots applied raw, some or all, in any order with the rest, leave
// the state they leave decoded, and decode as they were when read.
func TestReplayRaw(t *testing.T) {
	log := replayLog()
	for i := range 40 {
		var r apron.Replay
		for _, j := range rand.Perm(len(log)) {
			rec := log[j]
			if rec.Message != nil && (i%2 == 0 || j%2 == 0) {
				raw, err := apron.Marshal(rec.Message)
				if err != nil {
					t.Fatal(err)
				}
				r.ApplyRawMessage(raw)
				continue
			}
			r.Apply(rec)
		}
		checkReplayed(t, &r)
		for _, rec := range log {
			if rec.Message == nil {
				continue
			}
			raw, _ := apron.Marshal(rec.Message)
			if r.ApplyRawMessage(raw) || r.Apply(rec) {
				t.Fatalf("applied %+v again", rec.Message)
			}
		}
		var typed apron.Replay
		for _, rec := range log {
			typed.Apply(rec)
		}
		got, _ := apron.Marshal(r.Records())
		want, _ := apron.Marshal(typed.Records())
		if string(got) != string(want) {
			t.Fatalf("records\n got %s\nwant %s", got, want)
		}
	}
	var r apron.Replay
	for _, bad := range []string{`{`, `{"log_id":"5"}`, `{"message_id":"5"}`, `[]`} {
		if r.ApplyRawMessage([]byte(bad)) {
			t.Errorf("applied %s", bad)
		}
	}
}
