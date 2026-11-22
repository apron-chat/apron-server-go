package wire

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"
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
	for _, bad := range []string{`5`, `"x"`, `"-1"`, `"01"`, `true`, `{}`, `"9007199254740992"`} {
		var id LogID
		if err := json.Unmarshal([]byte(bad), &id); err == nil {
			t.Errorf("%s decoded as %v", bad, id)
		}
	}
}

// log_ids are below 2^53 (§2.1): every param that reads one refuses a
// greater one as error/invalid_params, and a Clock never issues one.
func TestLogIDBound(t *testing.T) {
	const atMax, past = `"9007199254740991"`, `"9007199254740992"`
	f := Fields{"max": jsontext.Value(atMax), "past": jsontext.Value(past), "huge": jsontext.Value(`"18446744073709551616"`),
		"ref": jsontext.Value(`{"message_id":` + past + `}`)}
	for _, read := range []func(string) (LogID, bool, error){f.LogID, f.MessageID} {
		if id, _, err := read("max"); id != MaxLogID || err != nil {
			t.Errorf("max: %v, %v", id, err)
		}
		for _, name := range []string{"past", "huge"} {
			if id, present, err := read(name); id != 0 || !present || CodeOf(err) != CodeInvalidParams || !strings.Contains(err.Error(), "2^53") {
				t.Errorf("%s: %v %v %v", name, id, present, err)
			}
		}
	}
	if _, _, err := f.MessageRef("ref"); CodeOf(err) != CodeInvalidParams {
		t.Errorf("ref: %v", err)
	}
	for _, params := range []string{
		`{"message_id":` + past + `,"body":{"text":"x"}}`,
		`{"body":{"text":"x"},"reply_to":{"message_id":` + past + `}}`,
	} {
		if _, err := parseMessageFields(req(params)); CodeOf(err) != CodeInvalidParams {
			t.Errorf("message %s: %v", params, err)
		}
	}
	if _, err := ParseRoomList(req(`{"latest_log_id":` + past + `}`)); CodeOf(err) != CodeInvalidParams {
		t.Errorf("room_list: %v", err)
	}
	if _, err := ParseActivity(req(`{"read_message_id":` + past + `}`)); CodeOf(err) != CodeInvalidParams {
		t.Errorf("activity: %v", err)
	}
	var m Message
	if err := json.Unmarshal([]byte(`{"from":{"user_id":"a"},"log_id":`+past+`}`), &m); err == nil {
		t.Errorf("a message with log_id %s decoded as %+v", past, m)
	}

	// A Clock issues MaxLogID, and then panics rather than issue more; a t
	// past MaxLogID counts as not later.
	var c Clock
	c.Restore(MaxLogID - 2)
	if id := c.At(time.UnixMilli(int64(MaxLogID) + 5)); id != MaxLogID-1 {
		t.Errorf("At past the bound: %v", id)
	}
	if id := c.Next(); id != MaxLogID {
		t.Errorf("last log_id: %v", id)
	}
	for name, f := range map[string]func(){
		"Next":    func() { c.Next() },
		"Restore": func() { new(Clock).Restore(MaxLogID + 1) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s past the bound did not panic", name)
				}
			}()
			f()
		}()
	}
	if c.Last() != MaxLogID {
		t.Errorf("Last after the panic: %v", c.Last())
	}
}

func TestClockRestore(t *testing.T) {
	var c Clock
	future := LogID(time.Now().Add(time.Hour).UnixMilli())
	c.Restore(future)
	if c.Last() != future {
		t.Fatalf("Last after Restore: %v", c.Last())
	}
	if next := c.Next(); next != future+1 {
		t.Fatalf("Next after Restore: %v, want %v", next, future+1)
	}
	c.Restore(10)
	if c.Last() != future+1 {
		t.Fatalf("Restore moved the clock back to %v", c.Last())
	}
	if at := c.At(time.UnixMilli(5)); at != future+2 {
		t.Fatalf("At in the past: %v", at)
	}
}

// Fields read log_ids from decimal strings, and message_ids are not 0.
func TestLogIDParams(t *testing.T) {
	f := Fields{}
	if err := json.Unmarshal([]byte(`{"zero":"0","id":"42","ref":{"message_id":"42"}}`), &f); err != nil {
		t.Fatal(err)
	}
	if id, present, err := f.LogID("zero"); id != 0 || !present || err != nil {
		t.Errorf("LogID zero: %v %v %v", id, present, err)
	}
	if id, present, err := f.MessageID("id"); id != 42 || !present || err != nil {
		t.Errorf("MessageID: %v %v %v", id, present, err)
	}
	if id, present, err := f.MessageRef("ref"); id != 42 || !present || err != nil {
		t.Errorf("MessageRef: %v %v %v", id, present, err)
	}
	if _, present, err := f.MessageID("absent"); present || err != nil {
		t.Errorf("absent: %v %v", present, err)
	}
	if _, _, err := f.MessageID("zero"); err == nil {
		t.Error("a message_id of 0 was accepted")
	}

	p, err := parseMessageFields(req(`{"message_id":"7","body":{"text":"x"},"reply_to":{"message_id":"3"}}`))
	if err != nil || p.messageID != 7 || p.replyTo == nil || p.replyTo.MessageID != 3 {
		t.Errorf("message: %+v %v", p, err)
	}
	for _, bad := range []string{`{"message_id":"0"}`, `{"message_id":"07"}`, `{"message_id":7}`, `{"reply_to":{"message_id":"0"}}`, `{"reply_to":{}}`, `{"reply_to":"3"}`} {
		if _, err := parseMessageFields(req(bad)); CodeOf(err) != CodeInvalidParams {
			t.Errorf("message %s: %v", bad, err)
		}
	}
	if p, err := ParseReact(req(`{"message_id":"9","emojis":[]}`)); err != nil || p.MessageID != 9 {
		t.Errorf("reactions: %+v %v", p, err)
	}
	if _, err := ParseReact(req(`{"message_id":"x","emojis":[]}`)); CodeOf(err) != CodeInvalidParams {
		t.Errorf("reactions with a bad message_id: %v", err)
	}
	if p, err := ParseActivity(req(`{"read_message_id":"11"}`)); err != nil || p.ReadMessageID == nil || *p.ReadMessageID != 11 {
		t.Errorf("activity: %+v %v", p, err)
	}
	if _, err := ParseActivity(req(`{"read_message_id":""}`)); err == nil {
		t.Error("activity with an empty read_message_id was accepted")
	}
	if p, err := ParseRoomList(req(`{"latest_log_id":"12"}`)); err != nil || p.LatestLogID == nil || *p.LatestLogID != 12 {
		t.Errorf("room_list: %+v %v", p, err)
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
			{Room: Room{RoomID: "empty", LogID: 150}, LatestLogID: 150, HistoryLogID: ptr(0)},
		}},
			`{"joined":[{"room_id":"general","log_id":"100","latest_log_id":"200","history_log_id":"100"},{"room_id":"empty","log_id":"150","latest_log_id":"150","history_log_id":null}]}`},
		{"room_update room", RoomUpdate{
			Joined:  []RoomView{{Room: room, LatestLogID: 200, HistoryLogID: ptr(0), Members: []User{{UserID: "a"}}}},
			Updated: []RoomView{{Room: Room{RoomID: "general", LogID: 300, PrevLogID: 100, Title: "General"}}},
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
