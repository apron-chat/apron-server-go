package archive_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/archive"
)

// example is ARCHIVE.md's example, as it is written there.
const example = `{"apron_archive":1,"server":"wss://chat.example/","exported_at":"2026-10-27T12:00:00.000Z","you":{"user_id":"ada","name":"Ada"},"media":["https://chat.example"]}
{"joined":[{"room_id":"general","log_id":"1724800000000","title":"General","latest_log_id":"1724803500000","history_log_id":"1724803200042","members":[{"user_id":"ada"},{"user_id":"bob"}]}],"not_joined":[],"users":[{"user_id":"ada","name":"Ada"},{"user_id":"bob","name":"Bob"}]}
{"messages":[{"message_id":"1724803200042","log_id":"1724803200042","room_id":"general","from":{"user_id":"bob","name":"Bob"},"body":{"text":"Hi!"}}]}
{"reactions":[{"log_id":"1724803312011","message_id":"1724803200042","room_id":"general","reactions":[{"from":{"user_id":"ada","name":"Ada"},"emojis":["👍"]}]}]}
`

func TestReadExample(t *testing.T) {
	a, err := archive.Read(strings.NewReader(example))
	if err != nil {
		t.Fatal(err)
	}
	if a.Header.Server != "wss://chat.example/" || a.Header.You == nil || a.Header.You.UserID != "ada" || len(a.Header.Media) != 1 || a.Skipped != 0 {
		t.Fatalf("header %+v, skipped %d", a.Header, a.Skipped)
	}
	if l := a.Rooms["general"]; !l.Joined || l.Title != "General" || len(l.Members) != 2 {
		t.Fatalf("listing %+v", l)
	}
	if start, end, ok := a.Stretch("general"); !ok || start != 1724803200042 || end != 1724803500000 {
		t.Fatalf("stretch %v %v %v", start, end, ok)
	}
	if got := a.Messages("general"); len(got) != 1 || got[0].Body.Text != "Hi!" {
		t.Fatalf("messages %+v", got)
	}
	if got := a.Log.Reactions(1724803200042); len(got) != 1 || got[0].Emojis[0] != "👍" {
		t.Fatalf("reactions %+v", got)
	}
	if a.Users["bob"].Name != "Bob" {
		t.Fatalf("users %+v", a.Users)
	}
}

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	you := apron.User{UserID: "ada", Name: "Ada", Status: "online", Seen: "1724800000000"}
	w, err := archive.NewWriter(&buf, archive.Header{Server: "wss://chat.example/ws", ExportedAt: archive.FormatTime(time.Unix(1724803500, 0)), You: &you, Media: []string{"https://chat.example"}})
	if err != nil {
		t.Fatal(err)
	}
	from := apron.User{UserID: "ada", Name: "Ada"}
	start, none := apron.LogID(10), apron.LogID(0)
	page := apron.HistoryResult{
		Rooms:       []apron.Room{{RoomID: "general", LogID: 10, Title: "General"}},
		Messages:    []apron.Message{{MessageID: 11, LogID: 11, RoomID: "general", From: from, Body: &apron.Body{Text: "hi", Format: apron.Markdown}}},
		Memberships: []apron.Membership{{LogID: 12, RoomID: "general", Members: []apron.Member{{User: apron.User{UserID: "ada"}, Joined: true}}}},
		More:        true, FirstLogID: 10, LastLogID: 12,
	}
	for _, p := range []archive.Part{
		archive.HistoryPart(page),
		{}, // writes nothing
		{Messages: []apron.Message{{MessageID: 5, LogID: 5, RoomID: "general", From: from, Body: &apron.Body{Text: "a reference"}}}},
		{Joined: []apron.RoomView{{Room: page.Rooms[0], LatestLogID: 12, HistoryLogID: &start,
			Members: []apron.CurrentUser{{User: apron.User{UserID: "ada", Status: "online"}, Cleared: []string{"seen"}}}}}},
		{NotJoined: []apron.RoomView{{Room: apron.Room{RoomID: "random"}, LatestLogID: 3, HistoryLogID: &none}}},
		{Users: []apron.User{you}},
	} {
		if err := w.Write(p); err != nil {
			t.Fatal(err)
		}
	}
	if lines := strings.Count(buf.String(), "\n"); lines != 6 {
		t.Fatalf("%d lines:\n%s", lines, buf.String())
	}
	if strings.Contains(buf.String(), "online") || strings.Contains(buf.String(), "seen") {
		t.Fatalf("status or seen written:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), `"history_log_id":null`) || !strings.Contains(buf.String(), `"exported_at":"2024-08-28T00:05:00.000Z"`) {
		t.Fatalf("archive:\n%s", buf.String())
	}

	a, err := archive.Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if a.Skipped != 0 || a.Header.Archive != archive.Version || a.Header.You.Status != "" || a.Users["ada"].Name != "Ada" {
		t.Fatalf("read %+v", a)
	}
	if s, e, ok := a.Stretch("general"); !ok || s != 10 || e != 12 {
		t.Fatalf("general's stretch %v %v %v", s, e, ok)
	}
	// random is listed with history_log_id null: no stretch.
	if l, listed := a.Rooms["random"]; !listed || l.Joined || l.HistoryLogID == nil || *l.HistoryLogID != 0 {
		t.Fatalf("random %+v", l)
	}
	if _, _, ok := a.Stretch("random"); ok {
		t.Fatal("random has a stretch")
	}
	// The message below the stretch is a reference, which is kept but not
	// shown in its room.
	if !a.Reference("general", 5) || a.Reference("general", 11) || !a.Reference("elsewhere", 11) {
		t.Fatal("Reference")
	}
	if got := a.Messages("general"); len(got) != 1 || got[0].MessageID != 11 || got[0].Body.Format != apron.Markdown {
		t.Fatalf("messages %+v", got)
	}
	if m, ok := a.Log.Message(5); !ok || m.Body.Text != "a reference" {
		t.Fatal("the reference is not kept")
	}
	if got := a.Log.Members("general"); len(got) != 1 {
		t.Fatalf("members %+v", got)
	}
}

// A stretch without history_log_id starts at the room's oldest record.
func TestStretchAbsent(t *testing.T) {
	a, err := archive.Read(strings.NewReader(`{"apron_archive":1}
{"joined":[{"room_id":"r","latest_log_id":"30"}]}
{"messages":[{"message_id":"20","log_id":"20","room_id":"r","from":{"user_id":"u"},"body":{"text":"x"}}]}
{"rooms":[{"room_id":"r","log_id":"15"}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	if s, e, ok := a.Stretch("r"); !ok || s != 15 || e != 30 {
		t.Fatalf("stretch %v %v %v", s, e, ok)
	}
}

// A reader skips what is not valid, line by line and record by record,
// and the order of listings and users matters.
func TestReadSkips(t *testing.T) {
	input := strings.Join([]string{
		`{"apron_archive":1,"server":7,"you":"nobody"}`,
		``,
		`not json`,
		`["an","array"]`,
		`{"messages":"not an array","users":[{"user_id":"ada","name":"Ada"}]}`,
		`{"messages":[{"message_id":"x","log_id":"1","room_id":"r","from":{"user_id":"u"}},{"message_id":"2","log_id":"2","room_id":"","from":{"user_id":"u"}},{"message_id":"3","log_id":"3","room_id":"r","from":{"user_id":"u"},"body":{"text":"ok"}}]}`,
		`{"reactions":[{"log_id":"4","message_id":"3","room_id":"r","reactions":[{"from":{"user_id":""},"emojis":["x"]}]}]}`,
		`{"memberships":[{"log_id":"9007199254740993","room_id":"r","members":[]}]}`,
		`{"joined":[{"room_id":"r","title":"First"}],"unknown":[1]}`,
		`{"not_joined":[{"room_id":"r","title":"Second"},{"room_id":"has space"}],"users":[{"user_id":"ada","name":"Ada L"}]}`,
	}, "\r\n")
	a, err := archive.Read(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if a.Header.Server != "" || a.Header.You != nil {
		t.Fatalf("header %+v", a.Header)
	}
	// not json, the array, "not an array", two messages, the reactions,
	// the membership, and the room with a space.
	if a.Skipped != 8 {
		t.Fatalf("skipped %d", a.Skipped)
	}
	if got := a.Log.Messages(""); len(got) != 1 || got[0].MessageID != 3 {
		t.Fatalf("messages %+v", got)
	}
	if l := a.Rooms["r"]; l.Title != "Second" || l.Joined || a.Users["ada"].Name != "Ada L" {
		t.Fatalf("latest listing %+v, users %+v", l, a.Users)
	}
}

func TestReadRefuses(t *testing.T) {
	for input, want := range map[string]error{
		``:                      archive.ErrNotArchive,
		"\n\n":                  archive.ErrNotArchive,
		`{"server":"x"}`:        archive.ErrNotArchive,
		`[1]`:                   archive.ErrNotArchive,
		`{"apron_archive":"1"}`: archive.ErrNotArchive,
		`{"apron_archive":2}`:   archive.ErrVersion,
	} {
		if _, err := archive.Read(strings.NewReader(input)); !errors.Is(err, want) {
			t.Errorf("%q: %v, want %v", input, err, want)
		}
	}
	// A header alone is an empty archive.
	r, err := archive.NewReader(strings.NewReader(`{"apron_archive":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("next: %v", err)
	}
}
