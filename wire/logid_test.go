package wire

import (
	"encoding/json/v2"
	"testing"
	"time"
)

func TestParseLogID(t *testing.T) {
	for s, want := range map[string]LogID{"0": 0, "1": 1, "1724803200042": 1724803200042, "9223372036854775807": 1<<63 - 1} {
		if got, err := ParseLogID(s); err != nil || got != want {
			t.Errorf("ParseLogID(%q) = %v, %v", s, got, err)
		}
	}
	for _, s := range []string{"", "-1", "+1", "01", "00", "1.5", "1e3", " 1", "1 ", "0x10", "9223372036854775808"} {
		if got, err := ParseLogID(s); err == nil || CodeOf(err) != CodeInvalidParams {
			t.Errorf("ParseLogID(%q) = %v, %v", s, got, err)
		}
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
	for _, bad := range []string{`5`, `"x"`, `"-1"`, `"01"`, `true`, `{}`} {
		var id LogID
		if err := json.Unmarshal([]byte(bad), &id); err == nil {
			t.Errorf("%s decoded as %v", bad, id)
		}
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

	p, err := parseMessage(req(`{"message_id":"7","body":{"text":"x"},"reply_to":{"message_id":"3"}}`))
	if err != nil || p.MessageID != 7 || p.ReplyTo == nil || p.ReplyTo.MessageID != 3 {
		t.Errorf("message: %+v %v", p, err)
	}
	for _, bad := range []string{`{"message_id":"0"}`, `{"message_id":"07"}`, `{"message_id":7}`, `{"reply_to":{"message_id":"0"}}`, `{"reply_to":{}}`, `{"reply_to":"3"}`} {
		if _, err := parseMessage(req(bad)); CodeOf(err) != CodeInvalidParams {
			t.Errorf("message %s: %v", bad, err)
		}
	}
	if p, err := parseReact(req(`{"message_id":"9","emojis":[]}`)); err != nil || p.MessageID != 9 {
		t.Errorf("reactions: %+v %v", p, err)
	}
	if _, err := parseReact(req(`{"message_id":"x","emojis":[]}`)); CodeOf(err) != CodeInvalidParams {
		t.Errorf("reactions with a bad message_id: %v", err)
	}
	if p, err := parseActivity(req(`{"read_message_id":"11"}`)); err != nil || p.ReadMessageID == nil || *p.ReadMessageID != 11 {
		t.Errorf("activity: %+v %v", p, err)
	}
	if _, err := parseActivity(req(`{"read_message_id":""}`)); err == nil {
		t.Error("activity with an empty read_message_id was accepted")
	}
	if p, err := parseRoomList(req(`{"latest_log_id":"12"}`)); err != nil || p.LatestLogID == nil || *p.LatestLogID != 12 {
		t.Errorf("room_list: %+v %v", p, err)
	}
}
