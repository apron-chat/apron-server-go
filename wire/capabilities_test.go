package wire

import (
	"encoding/json/jsontext"
	"strconv"
	"testing"
	"time"
)

func logOf(ids ...int64) []Record {
	var log []Record
	for _, id := range ids {
		s := strconv.FormatInt(id, 10)
		log = append(log, Record{LogID: id, Message: &Message{MessageID: s, LogID: s}})
	}
	return log
}

func TestPage(t *testing.T) {
	log := logOf(10, 20, 30, 40, 50)
	cases := []struct {
		name        string
		p           HistoryParams
		limit       int
		lower       int64
		first, last string
		more        bool
		count       int
	}{
		{"newest without after", HistoryParams{}, 2, 10, "40", "50", true, 2},
		{"oldest with after", HistoryParams{After: 15}, 2, 10, "20", "30", true, 2},
		{"inclusive bounds", HistoryParams{After: 20, Before: 40}, 10, 10, "20", "40", false, 3},
		{"one record", HistoryParams{After: 30, Before: 30}, 10, 10, "30", "30", false, 1},
		{"client limit under the server's", HistoryParams{Limit: 1}, 10, 10, "50", "50", true, 1},
		{"below the lower bound", HistoryParams{Before: 25}, 10, 30, "", "", false, 0},
		{"empty", HistoryParams{After: 60}, 10, 10, "", "", false, 0},
	}
	for _, c := range cases {
		r := Page(log, c.p, c.limit, c.lower, 50)
		if r.FirstLogID != c.first || r.LastLogID != c.last || r.More != c.more || len(r.Messages) != c.count {
			t.Errorf("%s: got first %q last %q more %v count %d", c.name, r.FirstLogID, r.LastLogID, r.More, len(r.Messages))
		}
		if r.LatestLogID != "50" || r.HistoryLogID == nil || *r.HistoryLogID != strconv.FormatInt(c.lower, 10) {
			t.Errorf("%s: bounds %q %v", c.name, r.LatestLogID, r.HistoryLogID)
		}
	}
	if r := Page(nil, HistoryParams{}, 10, 0, 0); r.HistoryLogID != nil || r.LatestLogID != "" || r.More {
		t.Errorf("empty log: %+v", r)
	}
}

func TestIDs(t *testing.T) {
	clock := &Clock{}
	ids := &IDs[string]{Clock: clock, Size: 2}
	past := time.UnixMilli(1000)
	a := ids.ID("a", past)
	if a != "1000" {
		t.Fatalf("an ID is derived from its time: %s", a)
	}
	// The same time again: unique, and still increasing.
	if b := ids.ID("b", past); b != "1001" {
		t.Fatalf("b: %s", b)
	}
	if again := ids.ID("a", time.Now()); again != a {
		t.Fatalf("the same key keeps its ID: %s", again)
	}
	if key, ok := ids.Key(a); !ok || key != "a" {
		t.Fatalf("Key: %q %v", key, ok)
	}
	// Past Size, the oldest is forgotten.
	ids.ID("c", past)
	if _, ok := ids.Lookup("a"); ok {
		t.Fatal("a was kept past Size")
	}
	if _, ok := ids.Key(a); ok {
		t.Fatal("a's ID was kept past Size")
	}
	if clock.Last() != "1002" {
		t.Fatalf("Last: %s", clock.Last())
	}
}

func TestParseStatus(t *testing.T) {
	p, err := parseStatus(jsontext.Value(`{"idle": false, "mute": 3600, "room_id": "general"}`))
	if err != nil || p.Idle == nil || !p.Idle.Attended || p.Mute == nil || !p.Mute.On || p.Mute.Seconds != 3600 || p.RoomID != "general" {
		t.Fatalf("got %+v %v", p, err)
	}
	p, err = parseStatus(jsontext.Value(`{"idle": 300}`))
	if err != nil || p.Idle.Seconds != 300 || p.Mute != nil {
		t.Fatalf("got %+v %v", p, err)
	}
	for _, bad := range []string{`{"idle": true}`, `{"idle": -1}`, `{"mute": 0}`, `{"mute": "x"}`} {
		if _, err := parseStatus(jsontext.Value(bad)); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	for _, m := range []Mute{{}, {On: true}, {On: true, Seconds: 60}} {
		got, _ := m.MarshalJSON()
		want := map[Mute]string{{}: "false", {On: true}: "true", {On: true, Seconds: 60}: "60"}[m]
		if string(got) != want {
			t.Errorf("%+v encodes as %s", m, got)
		}
	}
}

func TestParseHistory(t *testing.T) {
	p, err := parseHistory(jsontext.Value(`{"room_id": "general", "after": "10", "before": "20", "limit": 5}`))
	if err != nil || p.After != 10 || p.Before != 20 || p.Limit != 5 {
		t.Fatalf("got %+v %v", p, err)
	}
	for _, bad := range []string{`{"after": "x"}`, `{"limit": 0}`, `{"room_id": "a b"}`} {
		if _, err := parseHistory(jsontext.Value(bad)); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
}
