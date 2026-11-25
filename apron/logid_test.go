package apron

import (
	"encoding/json/v2"
	"strings"
	"testing"
)

func TestParseLogID(t *testing.T) {
	for s, want := range map[string]LogID{"0": 0, "1": 1, "1724803200042": 1724803200042, "9007199254740991": MaxLogID} {
		if got, err := ParseLogID(s); err != nil || got != want {
			t.Errorf("ParseLogID(%q) = %v, %v", s, got, err)
		}
	}
	for _, s := range []string{"", "-1", "+1", "01", "00", "1.5", "1e3", " 1", "1 ", "0x10",
		"9007199254740992", "9223372036854775807", "9223372036854775808", "99999999999999999999"} {
		if got, err := ParseLogID(s); err == nil || CodeOf(err) != CodeInvalidParams {
			t.Errorf("ParseLogID(%q) = %v, %v", s, got, err)
		}
	}
	if MaxLogID != 1<<53-1 {
		t.Errorf("MaxLogID: %v", MaxLogID)
	}
	if s := LogID(1724803200042).String(); s != "1724803200042" {
		t.Errorf("String: %s", s)
	}
}

// A LogID is a decimal string in JSON, and 0 is null, both ways.
func TestLogIDJSON(t *testing.T) {
	type fields struct {
		Required LogID  `json:"required"`
		Optional LogID  `json:"optional,omitzero"`
		Pointer  *LogID `json:"pointer,omitzero"`
	}
	for _, c := range []struct {
		value fields
		json  string
	}{
		{fields{}, `{"required":null}`},
		{fields{Required: 5, Optional: 6, Pointer: ptr(7)}, `{"required":"5","optional":"6","pointer":"7"}`},
		{fields{Pointer: ptr(0)}, `{"required":null,"pointer":null}`},
	} {
		got, err := marshal(c.value)
		if err != nil || string(got) != c.json {
			t.Errorf("%+v encodes as %s, %v; want %s", c.value, got, err, c.json)
		}
	}
	var got fields
	if err := json.Unmarshal([]byte(`{"required":"\u0031724803200042","optional":null,"pointer":"3"}`), &got); err != nil ||
		got.Required != 1724803200042 || got.Optional != 0 || got.Pointer == nil || *got.Pointer != 3 {
		t.Fatalf("decoded %+v, %v", got, err)
	}
	for _, id := range []LogID{0, 1, 1724803200042} {
		var back LogID
		if data, err := json.Marshal(id); err != nil || json.Unmarshal(data, &back) != nil || back != id {
			t.Errorf("%v round-trips as %s to %v", id, data, back)
		}
	}
	// Only a log_id is encoded: a negative LogID, or one past MaxLogID, is
	// an encoding error, as it is no log_id (§2.1).
	if got, err := marshal(MaxLogID); err != nil || string(got) != `"9007199254740991"` {
		t.Errorf("MaxLogID encodes as %s, %v", got, err)
	}
	for _, id := range []LogID{-1, -1724803200042, MaxLogID + 1, 1<<63 - 1} {
		if got, err := marshal(fields{Required: 1, Optional: id}); err == nil || !strings.Contains(err.Error(), id.String()) {
			t.Errorf("%v encodes as %s, %v; want an error that names it", id, got, err)
		}
	}
	for _, bad := range []string{`5`, `"x"`, `"-1"`, `"01"`, `true`, `{}`, `"9007199254740992"`} {
		var id LogID
		if err := json.Unmarshal([]byte(bad), &id); err == nil {
			t.Errorf("%s decoded as %v", bad, id)
		}
	}
}

// history_log_id is in every history result, null for none; a RoomView
// sends it when set, null for none; a logged Room never has it (§4.3.3).
func TestHistoryLogIDJSON(t *testing.T) {
	room := Room{RoomID: "general", LogID: 100}
	for _, c := range []struct {
		name  string
		value any
		json  string
	}{
		{"history result", HistoryResult{LatestLogID: 200, HistoryLogID: 100},
			`{"more":false,"latest_log_id":"200","history_log_id":"100"}`},
		{"history result without history", HistoryResult{LatestLogID: 200},
			`{"more":false,"latest_log_id":"200","history_log_id":null}`},
		{"history record room", HistoryResult{Rooms: []Room{room}, FirstLogID: 100, LastLogID: 100, LatestLogID: 200, HistoryLogID: 100},
			`{"rooms":[{"room_id":"general","log_id":"100"}],"first_log_id":"100","last_log_id":"100","more":false,"latest_log_id":"200","history_log_id":"100"}`},
		{"room_list room", RoomListResult{Joined: []RoomView{
			{Room: room, LatestLogID: 200, HistoryLogID: ptr(100)},
			{RoomID: "empty", LogID: 150, LatestLogID: 150, HistoryLogID: ptr(0)},
		}},
			`{"joined":[{"room_id":"general","log_id":"100","latest_log_id":"200","history_log_id":"100"},{"room_id":"empty","log_id":"150","latest_log_id":"150","history_log_id":null}]}`},
		{"room_update room", RoomUpdate{
			Joined:  []RoomView{{Room: room, LatestLogID: 200, HistoryLogID: ptr(0), Members: []CurrentUser{{UserID: "a"}}}},
			Updated: []RoomView{{RoomID: "general", LogID: 300, PrevLogID: 100, Title: "General"}},
		},
			`{"joined":[{"room_id":"general","log_id":"100","latest_log_id":"200","history_log_id":null,"members":[{"user_id":"a"}]}],"updated":[{"room_id":"general","log_id":"300","prev_log_id":"100","title":"General"}]}`},
	} {
		got, err := marshal(c.value)
		if err != nil || string(got) != c.json {
			t.Errorf("%s:\n got %s, %v\nwant %s", c.name, got, err, c.json)
		}
	}

	// A RoomView decodes into its Room and its delivery fields.
	var view RoomView
	if err := json.Unmarshal([]byte(`{"room_id":"general","log_id":"100","latest_log_id":"200","history_log_id":"100","member_count":3}`), &view); err != nil ||
		view.RoomID != "general" || view.LogID != 100 || view.LatestLogID != 200 || view.HistoryLogID == nil || *view.HistoryLogID != 100 || view.MemberCount != 3 {
		t.Fatalf("decoded %+v, %v", view, err)
	}
}

func ptr(n LogID) *LogID { return &n }
