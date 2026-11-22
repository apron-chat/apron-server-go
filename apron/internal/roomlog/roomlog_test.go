package roomlog

import (
	"encoding/json/v2"
	"slices"
	"testing"
)

func ids(records []*Record) []int64 {
	var out []int64
	for _, r := range records {
		out = append(out, r.ID)
	}
	return out
}

// twoRooms logs records 1–6 in rooms a and b; 4 is a move snapshot in both.
func twoRooms() (*Log, *Timeline, *Timeline) {
	l := NewLog()
	a, b := NewTimeline("a", 1, 0), NewTimeline("b", 2, 0)
	for _, step := range []struct {
		id    int64
		rooms []*Timeline
	}{{1, []*Timeline{a}}, {2, []*Timeline{b}}, {3, []*Timeline{a}}, {4, []*Timeline{a, b}}, {5, []*Timeline{b}}, {6, []*Timeline{a}}} {
		l.Append(New(step.id, KindMessage, map[string]any{"log_id": FormatID(step.id)}), step.rooms...)
	}
	return l, a, b
}

func TestPage(t *testing.T) {
	_, a, b := twoRooms()
	for _, tc := range []struct {
		name  string
		rooms []*Timeline
		q     Query
		want  []int64
		more  bool
	}{
		{"latest", []*Timeline{a, b}, Query{Limit: 3, MaxBytes: 1 << 20}, []int64{4, 5, 6}, true},
		{"all once", []*Timeline{a, b}, Query{Limit: 10, MaxBytes: 1 << 20}, []int64{1, 2, 3, 4, 5, 6}, false},
		{"after", []*Timeline{a, b}, Query{After: 2, HasAfter: true, Limit: 2, MaxBytes: 1 << 20}, []int64{2, 3}, true},
		{"before", []*Timeline{a}, Query{Before: 4, HasBefore: true, Limit: 2, MaxBytes: 1 << 20}, []int64{3, 4}, true},
		{"one log_id", []*Timeline{b}, Query{After: 4, HasAfter: true, Before: 4, HasBefore: true, Limit: 10, MaxBytes: 1 << 20}, []int64{4}, false},
		{"bytes keep one", []*Timeline{a, b}, Query{Limit: 10, MaxBytes: 1}, []int64{6}, true},
		{"bytes from after", []*Timeline{a, b}, Query{After: 0, HasAfter: true, Limit: 10, MaxBytes: 1}, []int64{1}, true},
	} {
		page, more := Page(tc.rooms, tc.q)
		if got := ids(page); !slices.Equal(got, tc.want) || more != tc.more {
			t.Errorf("%s: page %v more %v, want %v %v", tc.name, got, more, tc.want, tc.more)
		}
	}
	if a.Latest() != 6 || b.Latest() != 5 || a.Created() != 1 {
		t.Fatalf("latest %d %d, created %d", a.Latest(), b.Latest(), a.Created())
	}
	if got := ids(a.Since(3, 10)); !slices.Equal(got, []int64{4, 6}) {
		t.Fatalf("Since(3) = %v", got)
	}
	if got := ids(a.Since(0, 2)); !slices.Equal(got, []int64{4, 6}) {
		t.Fatalf("Since(0, 2) = %v", got)
	}
}

func TestFeedAndEntries(t *testing.T) {
	l, _, _ := twoRooms()
	if l.Last() != 6 {
		t.Fatalf("Last = %d", l.Last())
	}
	records, changed := l.After(2, 2)
	if got := ids(records); !slices.Equal(got, []int64{3, 4}) {
		t.Fatalf("After(2, 2) = %v", got)
	}
	if got := l.records[3].Rooms; !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("a move snapshot's rooms are %v", got)
	}
	if entries := l.Entries(); len(entries) != 6 || len(l.Entries()) != 0 {
		t.Fatalf("Entries gave %d, then more", len(entries))
	}
	l.Append(New(7, KindRoom, RoomRecord{RoomID: "a", LogID: "7"}))
	select {
	case <-changed:
	default:
		t.Fatal("an append left followers waiting")
	}
	if entries := l.Entries(); len(entries) != 1 || string(entries[0].Value) != `{"kind":0,"raw":{"log_id":"7","room_id":"a"},"rooms":null}` {
		t.Fatalf("entries %v", entries)
	}
}

func TestRedact(t *testing.T) {
	l := NewLog()
	r := New(5, KindMessage, map[string]any{
		"message_id": "5", "log_id": "5", "room_id": "a", "from": Author{UserID: "ada", Name: "Ada"},
		"body": map[string]any{"text": "oops"}, "ext": map[string]any{"x": 1}, "reply_to": map[string]any{"message_id": "3"},
	})
	l.Redact(r)
	const want = `{"deleted":true,"from":{"name":"Ada","user_id":"ada"},"log_id":"5","message_id":"5","reply_to":{"message_id":"3"},"room_id":"a"}`
	if string(r.Raw) != want {
		t.Fatalf("redacted %s, want %s", r.Raw, want)
	}
	if len(l.Entries()) != 1 {
		t.Fatal("a redaction is not stored")
	}
}

func TestShapesKeepUnknownMembers(t *testing.T) {
	const stored = `{"log_id":"1","room_id":"a","title":"A","topic":"old"}`
	var room RoomRecord
	if err := json.Unmarshal([]byte(stored), &room); err != nil {
		t.Fatal(err)
	}
	if room.Title != "A" || string(encode(room)) != stored {
		t.Fatalf("decoded %+v, encodes as %s", room, encode(room))
	}
	var author Author
	if err := json.Unmarshal([]byte(`{"avatar":"x","user_id":"ada"}`), &author); err != nil {
		t.Fatal(err)
	}
	if got := string(encode(author)); got != `{"user_id":"ada","avatar":"x"}` {
		t.Fatalf("author encodes as %s", got)
	}
	m := NewMembership(9, "a", Author{UserID: "ada"}, false)
	if string(m.Raw) != `{"log_id":"9","members":[{"joined":false,"user":{"user_id":"ada"}}],"room_id":"a"}` {
		t.Fatalf("membership %s", m.Raw)
	}
}
