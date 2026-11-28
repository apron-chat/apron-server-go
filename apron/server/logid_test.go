package server

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
)

// log_ids are below 2^53 (§2.1): every param that reads one refuses a
// greater one as error/invalid_params, and a Clock never issues one.
func TestLogIDBound(t *testing.T) {
	const atMax, past = `"9007199254740991"`, `"9007199254740992"`
	f, _ := ParseFields(jsontext.Value(`{"max":` + atMax + `,"past":` + past + `,"huge":"18446744073709551616","ref":{"message_id":` + past + `}}`))
	for _, read := range []func(string) (apron.LogID, bool, error){f.logIDParam, f.messageIDParam} {
		if id, _, err := read("max"); id != apron.MaxLogID || err != nil {
			t.Errorf("max: %v, %v", id, err)
		}
		for _, name := range []string{"past", "huge"} {
			if id, present, err := read(name); id != 0 || !present || apron.CodeOf(err) != apron.CodeInvalidParams || !strings.Contains(err.Error(), "2^53") {
				t.Errorf("%s: %v %v %v", name, id, present, err)
			}
		}
	}
	if _, _, err := f.messageRefParam("ref"); apron.CodeOf(err) != apron.CodeInvalidParams || !strings.Contains(err.Error(), "ref.message_id must be below 2^53") {
		t.Errorf("ref: %v", err)
	}
	for _, ref := range []string{`null`, `{}`, `{"message_id":"0"}`, `{"message_id":"x"}`, `{"message_id":5}`} {
		fields, _ := ParseFields(jsontext.Value(`{"ref":` + ref + `}`))
		if _, _, err := fields.messageRefParam("ref"); err == nil || err.(*apron.Error).Message != "ref.message_id must be a positive decimal string" {
			t.Errorf("ref %s: %v", ref, err)
		}
	}
	for _, params := range []string{
		`{"message_id":` + past + `,"body":{"text":"x"}}`,
		`{"body":{"text":"x"},"reply_to":{"message_id":` + past + `}}`,
	} {
		if _, err := parseMessageFields(req(params)); apron.CodeOf(err) != apron.CodeInvalidParams {
			t.Errorf("message %s: %v", params, err)
		}
	}
	if _, err := parseRoomList(req(`{"latest_log_id":` + past + `}`)); apron.CodeOf(err) != apron.CodeInvalidParams {
		t.Errorf("room_list: %v", err)
	}
	if _, err := parseActivity(req(`{"read_message_id":` + past + `}`)); apron.CodeOf(err) != apron.CodeInvalidParams {
		t.Errorf("activity: %v", err)
	}
	var m apron.Message
	if err := json.Unmarshal([]byte(`{"from":{"user_id":"a"},"log_id":`+past+`}`), &m); err == nil {
		t.Errorf("a message with log_id %s decoded as %+v", past, m)
	}

	// A Clock issues MaxLogID, and then panics rather than issue more; a t
	// past MaxLogID counts as not later.
	var c Clock
	c.Restore(apron.MaxLogID - 2)
	if id := c.At(time.UnixMilli(int64(apron.MaxLogID) + 5)); id != apron.MaxLogID-1 {
		t.Errorf("At past the bound: %v", id)
	}
	if id := c.Next(); id != apron.MaxLogID {
		t.Errorf("last log_id: %v", id)
	}
	for name, f := range map[string]func(){
		"Next":    func() { c.Next() },
		"Restore": func() { new(Clock).Restore(apron.MaxLogID + 1) },
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
	if c.Last() != apron.MaxLogID {
		t.Errorf("Last after the panic: %v", c.Last())
	}
}

func TestClockRestore(t *testing.T) {
	var c Clock
	future := apron.LogID(time.Now().Add(time.Hour).UnixMilli())
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
	if id, present, err := f.logIDParam("zero"); id != 0 || !present || err != nil {
		t.Errorf("LogID zero: %v %v %v", id, present, err)
	}
	if id, present, err := f.messageIDParam("id"); id != 42 || !present || err != nil {
		t.Errorf("MessageID: %v %v %v", id, present, err)
	}
	if id, present, err := f.messageRefParam("ref"); id != 42 || !present || err != nil {
		t.Errorf("MessageRef: %v %v %v", id, present, err)
	}
	if _, present, err := f.messageIDParam("absent"); present || err != nil {
		t.Errorf("absent: %v %v", present, err)
	}
	if _, _, err := f.messageIDParam("zero"); err == nil {
		t.Error("a message_id of 0 was accepted")
	}

	p, err := parseMessageFields(req(`{"message_id":"7","body":{"text":"x"},"reply_to":{"message_id":"3"}}`))
	if err != nil || p.messageID != 7 || p.replyTo == nil || p.replyTo.MessageID != 3 {
		t.Errorf("message: %+v %v", p, err)
	}
	for _, bad := range []string{`{"message_id":"0"}`, `{"message_id":"07"}`, `{"message_id":7}`, `{"reply_to":{"message_id":"0"}}`, `{"reply_to":{}}`, `{"reply_to":"3"}`} {
		if _, err := parseMessageFields(req(bad)); apron.CodeOf(err) != apron.CodeInvalidParams {
			t.Errorf("message %s: %v", bad, err)
		}
	}
	if p, err := parseReact(req(`{"message_id":"9","emojis":[]}`)); err != nil || p.MessageID != 9 {
		t.Errorf("reactions: %+v %v", p, err)
	}
	if _, err := parseReact(req(`{"message_id":"x","emojis":[]}`)); apron.CodeOf(err) != apron.CodeInvalidParams {
		t.Errorf("reactions with a bad message_id: %v", err)
	}
	if p, err := parseActivity(req(`{"read_message_id":"11"}`)); err != nil || p.ReadMessageID != 11 {
		t.Errorf("activity: %+v %v", p, err)
	}
	if _, err := parseActivity(req(`{"read_message_id":""}`)); err == nil {
		t.Error("activity with an empty read_message_id was accepted")
	}
	if p, err := parseRoomList(req(`{"latest_log_id":"12"}`)); err != nil || p.LatestLogID == nil || *p.LatestLogID != 12 {
		t.Errorf("room_list: %+v %v", p, err)
	}
}
