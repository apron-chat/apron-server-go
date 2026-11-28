package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/archive"
	"github.com/apron-chat/apron-server-go/aprond"
	"github.com/apron-chat/apron-server-go/aprond/aprondtest"
)

// serverState replays what the server holds of rooms, each up to the
// latest_log_id that an archive lists it with.
func serverState(t *testing.T, server *aprondtest.Server, token string, a *archive.Archive) *apron.Replay {
	t.Helper()
	c := conn(t, server, apron.AuthParams{Scheme: "token", Token: token})
	var state apron.Replay
	for room, listing := range a.Rooms {
		start, end, ok := a.Stretch(room)
		if !ok {
			continue
		}
		for page, err := range c.HistoryPages(t.Context(), apron.HistoryParams{RoomID: room, After: &start, Before: &end}) {
			if err != nil {
				t.Fatal(err)
			}
			if page.LowerBound() != *listing.HistoryLogID {
				t.Errorf("%s: the archive's stretch starts at %v, the server's history at %v", room, *listing.HistoryLogID, page.LowerBound())
			}
			state.ApplyHistory(page)
		}
	}
	return &state
}

// sameState fails the test unless two replays hold the same records.
func sameState(t *testing.T, got, want *apron.Replay) {
	t.Helper()
	a, err := apron.Marshal(got.Records())
	if err != nil {
		t.Fatal(err)
	}
	b, err := apron.Marshal(want.Records())
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("the archive holds\n%s\nand the server\n%s", a, b)
	}
}

func readFile(t *testing.T, path string) *archive.Archive {
	t.Helper()
	a, err := readArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	if a.Skipped != 0 {
		t.Fatalf("%s: %d lines or records not valid", path, a.Skipped)
	}
	return a
}

func TestExport(t *testing.T) {
	server := aprondtest.Serve(t, aprond.DefaultConfig())
	ada, bob := server.Member("ada"), server.Member("bob")
	first, err := ada.Post("general", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bob.Post("general", "hi *ada*", aprond.ReplyTo(first), aprond.Markdown()); err != nil {
		t.Fatal(err)
	}
	if err := bob.Call("reactions", map[string]any{"message_id": first, "emojis": []string{"👍"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := ada.Edit("general", first, "hello, all"); err != nil {
		t.Fatal(err)
	}
	var thread apron.RoomSetResult
	if err := ada.Call("room_set", map[string]any{"parent_room_id": "general", "title": "Deploys"}, &thread); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"one", "two", "three", "four"} {
		if _, err := ada.Post(thread.RoomID, text); err != nil {
			t.Fatal(err)
		}
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "first.jsonl")
	// Pages of 3 changes: several for each room.
	out, errOut, err := aproncli(t, server, "", "--token", ada.Token, "export", "--limit", "3", "--rate", "0")
	if err != nil {
		t.Fatalf("export: %v\n%s", err, errOut)
	}
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	a := readFile(t, path)
	if a.Header.Server != server.WebSocketURL() || a.Header.You.UserID == "" || a.Header.You.UserID != "ada" || a.Header.ExportedAt == "" ||
		len(a.Header.Media) != 1 || a.Header.Media[0] != server.URL {
		t.Fatalf("header %+v", a.Header)
	}
	for _, room := range []string{"general", thread.RoomID} {
		if l, ok := a.Rooms[room]; !ok || !l.Joined || len(l.Members) == 0 {
			t.Fatalf("listing of %s: %+v", room, l)
		}
	}
	if a.Users["bob"].Name != "bob" {
		t.Fatalf("users %+v", a.Users)
	}
	sameState(t, &a.Log, serverState(t, server, ada.Token, a))
	if got := a.Messages(thread.RoomID); len(got) != 4 || got[3].Body.Text != "four" {
		t.Fatalf("the thread's messages %+v", got)
	}
	if got := a.Log.Reactions(mustID(t, first)); len(got) != 1 {
		t.Fatalf("reactions %+v", got)
	}

	// More happens, and the export resumes: the old archive's parts come
	// first, as they were, then only what is new.
	if _, err := bob.Post("general", "anything new?"); err != nil {
		t.Fatal(err)
	}
	if err := ada.Edit("general", first, "hello, everyone"); err != nil {
		t.Fatal(err)
	}
	if _, err := ada.Post(thread.RoomID, "five"); err != nil {
		t.Fatal(err)
	}
	resumed, errOut, err := aproncli(t, server, "", "--token", ada.Token, "export", "--resume", path, "--limit", "2", "--rate", "0")
	if err != nil {
		t.Fatalf("export --resume: %v\n%s", err, errOut)
	}
	oldLines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	newLines := strings.Split(strings.TrimSuffix(resumed, "\n"), "\n")
	if len(newLines) <= len(oldLines) || strings.Join(newLines[1:len(oldLines)], "\n") != strings.Join(oldLines[1:], "\n") {
		t.Fatalf("the resumed archive does not start with the old one's parts:\n%s", resumed)
	}
	added, err := archive.NewReader(strings.NewReader(newLines[0] + "\n" + strings.Join(newLines[len(oldLines):], "\n")))
	if err != nil {
		t.Fatal(err)
	}
	records := 0
	for {
		p, err := added.Next()
		if err != nil {
			break
		}
		h := p.History()
		check := func(room string, id apron.LogID) {
			records++
			if _, end, _ := a.Stretch(room); id <= end {
				t.Errorf("the resumed export wrote %s's record %v again: the old stretch ends at %v", room, id, end)
			}
		}
		for _, m := range h.Messages {
			check(m.RoomID, m.LogID)
		}
		for _, r := range h.Rooms {
			check(r.RoomID, r.LogID)
		}
		for _, r := range h.Reactions {
			check(r.RoomID, r.LogID)
		}
		for _, r := range h.Memberships {
			check(r.RoomID, r.LogID)
		}
	}
	if records < 3 {
		t.Fatalf("the resumed export added %d records", records)
	}
	path2 := filepath.Join(dir, "resumed.jsonl")
	if err := os.WriteFile(path2, []byte(resumed), 0o600); err != nil {
		t.Fatal(err)
	}
	b := readFile(t, path2)
	sameState(t, &b.Log, serverState(t, server, ada.Token, b))
	if start, _, _ := b.Stretch("general"); start != *a.Rooms["general"].HistoryLogID {
		t.Fatalf("the resumed stretch starts at %v, the first at %v", start, *a.Rooms["general"].HistoryLogID)
	}
	if m, _ := b.Log.Message(mustID(t, first)); m.Body.Text != "hello, everyone" {
		t.Fatalf("the edited message %+v", m.Body)
	}

	// An archive of another server does not resume.
	other := aprondtest.Serve(t, aprond.DefaultConfig())
	if _, _, err := aproncli(t, other, "", "--guest", "Ada", "export", "--resume", path); err == nil || !strings.Contains(err.Error(), "not of") {
		t.Fatalf("resuming another server's archive: %v", err)
	}
}

// export --room exports that room, and lists its parent without its log.
func TestExportRoom(t *testing.T) {
	server := aprondtest.Serve(t, aprond.DefaultConfig())
	ada := server.Member("ada")
	var thread apron.RoomSetResult
	if err := ada.Call("room_set", map[string]any{"parent_room_id": "general", "title": "Deploys"}, &thread); err != nil {
		t.Fatal(err)
	}
	if _, err := ada.Post(thread.RoomID, "in the thread"); err != nil {
		t.Fatal(err)
	}
	if _, err := ada.Post("general", "not exported"); err != nil {
		t.Fatal(err)
	}
	out, errOut, err := aproncli(t, server, "", "--token", ada.Token, "export", "--room", thread.RoomID)
	if err != nil {
		t.Fatalf("export: %v\n%s", err, errOut)
	}
	a, err := archive.Read(strings.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := a.Stretch(thread.RoomID); !ok {
		t.Fatal("no stretch of the thread")
	}
	if l, ok := a.Rooms["general"]; !ok || l.HistoryLogID == nil || *l.HistoryLogID != 0 {
		t.Fatalf("the parent's listing %+v", l)
	}
	if got := a.Log.Messages("general"); len(got) != 0 {
		t.Fatalf("the parent's messages were exported: %+v", got)
	}
	if _, _, err := aproncli(t, server, "", "--token", ada.Token, "export", "--room", "nowhere"); err == nil {
		t.Fatal("exported a room that is not")
	}
}

func mustID(t *testing.T, s string) apron.LogID {
	t.Helper()
	id, err := apron.ParseLogID(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
