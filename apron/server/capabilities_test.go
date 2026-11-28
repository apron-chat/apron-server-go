package server

import (
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
)

func ptr(n apron.LogID) *apron.LogID { return &n }

func req(params string) *Request {
	r, err := detachedRequest("", jsontext.Value(params), nil)
	if err != nil {
		panic(err)
	}
	return r
}

func logOf(ids ...apron.LogID) []apron.Record {
	var log []apron.Record
	for _, id := range ids {
		log = append(log, apron.Record{LogID: id, Message: &apron.Message{MessageID: id, LogID: id}})
	}
	return log
}

func TestPage(t *testing.T) {
	records := logOf(10, 20, 30, 40, 50)
	cases := []struct {
		name        string
		p           apron.HistoryParams
		limit       int
		lower       apron.LogID
		first, last apron.LogID
		more        bool
		count       int
	}{
		{"newest without after", apron.HistoryParams{}, 2, 10, 40, 50, true, 2},
		{"oldest with after", apron.HistoryParams{After: ptr(15)}, 2, 10, 20, 30, true, 2},
		{"inclusive bounds", apron.HistoryParams{After: ptr(20), Before: ptr(40)}, 10, 10, 20, 40, false, 3},
		{"one record", apron.HistoryParams{After: ptr(30), Before: ptr(30)}, 10, 10, 30, 30, false, 1},
		{"client limit under the server's", apron.HistoryParams{Limit: 1}, 10, 10, 50, 50, true, 1},
		{"below the lower bound", apron.HistoryParams{Before: ptr(25)}, 10, 30, 0, 0, false, 0},
		{"empty", apron.HistoryParams{After: ptr(60)}, 10, 10, 0, 0, false, 0},
		{"a limit of 0 is 1", apron.HistoryParams{}, 0, 10, 50, 50, true, 1},
		{"a negative limit is 1", apron.HistoryParams{After: ptr(10)}, -5, 10, 10, 10, true, 1},
	}
	for _, c := range cases {
		r := Page(Log{Records: records, Lower: c.lower, Latest: 50}, c.p, c.limit)
		if r.FirstLogID != c.first || r.LastLogID != c.last || r.More != c.more || len(r.Messages) != c.count {
			t.Errorf("%s: got first %v last %v more %v count %d", c.name, r.FirstLogID, r.LastLogID, r.More, len(r.Messages))
		}
		if r.LatestLogID != 50 || r.HistoryLogID != c.lower {
			t.Errorf("%s: bounds %v %v", c.name, r.LatestLogID, r.HistoryLogID)
		}
	}
	if r := Page(Log{}, apron.HistoryParams{}, 10); r.HistoryLogID != 0 || r.LatestLogID != 0 || r.More {
		t.Errorf("empty log: %+v", r)
	}
}

func TestIDs(t *testing.T) {
	clock := &Clock{}
	ids := &MessageIDs[string]{Clock: clock, Size: 2}
	past := time.UnixMilli(1000)
	a := ids.ID("a", past)
	if a != 1000 {
		t.Fatalf("an ID is derived from its time: %s", a)
	}
	// The same time again: unique, and still increasing.
	if b := ids.ID("b", past); b != 1001 {
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
	if clock.Last() != 1002 {
		t.Fatalf("Last: %s", clock.Last())
	}
}

func TestParseStatus(t *testing.T) {
	p, err := parseStatus(req(`{"idle": false, "mute": 3600, "room_id": "general"}`))
	if err != nil || p.Idle == nil || !p.Idle.Attended || p.Mute == nil || !p.Mute.On || p.Mute.Seconds != 3600 || p.RoomID != "general" {
		t.Fatalf("got %+v %v", p, err)
	}
	p, err = parseStatus(req(`{"idle": 300}`))
	if err != nil || p.Idle.Seconds != 300 || p.Mute != nil {
		t.Fatalf("got %+v %v", p, err)
	}
	for _, bad := range []string{`{"room_id": "general", "idle": 5}`, `{"idle": true}`, `{"idle": -1}`, `{"mute": 0}`, `{"mute": "x"}`} {
		if _, err := parseStatus(req(bad)); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	for _, m := range []apron.Mute{{}, {On: true}, {On: true, Seconds: 60}} {
		got, _ := m.MarshalJSON()
		want := map[apron.Mute]string{{}: "false", {On: true}: "true", {On: true, Seconds: 60}: "60"}[m]
		if string(got) != want {
			t.Errorf("%+v encodes as %s", m, got)
		}
	}
}

func TestParseHistory(t *testing.T) {
	p, err := parseHistory(req(`{"room_id": "general", "after": "10", "before": "20", "limit": 5}`))
	if err != nil || p.After == nil || *p.After != 10 || p.Before == nil || *p.Before != 20 || p.Limit != 5 {
		t.Fatalf("got %+v %v", p, err)
	}
	for _, bad := range []string{`{"after": "x"}`, `{"after": 10}`, `{"after": null}`, `{"before": "-1"}`, `{"before": "+1"}`, `{"after": "010"}`, `{"limit": 0}`, `{"room_id": "a b"}`} {
		if _, err := parseHistory(req(bad)); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
}

// A room_list without members sends none, raw ones neither.
func TestNormalizeRoomListRaw(t *testing.T) {
	raw := []jsontext.Value{jsontext.Value(`{"user_id":"a"}`)}
	result := normalizeRoomList(apron.RoomListParams{Filter: apron.RoomsJoined}, apron.RoomListResult{
		Joined: []apron.RoomView{{Room: apron.Room{RoomID: "r"}, MembersRaw: raw, MemberCount: 1}}, UsersRaw: raw,
	})
	if result.UsersRaw != nil || result.Joined[0].MembersRaw != nil || result.Joined[0].MemberCount != 0 {
		t.Fatalf("members left in %+v", result)
	}
}
