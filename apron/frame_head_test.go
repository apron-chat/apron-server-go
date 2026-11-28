package apron

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"reflect"
	"testing"
)

// A frame's head is what decoding the frame into a FrameHead gives, and
// the frames decoding refuses, Head refuses.
func TestFrameHead(t *testing.T) {
	for _, raw := range []string{
		`{"method":"message","params":{"room_id":"general"}}`,
		`{"id":"a-1","result":{"message_id":"5"}}`,
		`{"error":{"code":"denied"},"id":"a-2"}`,
		` { "method" : null , "id" : null , "x" : [1] , "params" : { } } `,
		`{"\u006dethod":"ext:x","params":[1,2]}`,
		`{"error":{"code":"denied","message":"no"}}`,
		`{}`,
	} {
		got, err := Frame(raw).Head()
		var want struct {
			Method string         `json:"method"`
			ID     jsontext.Value `json:"id"`
			Params jsontext.Value `json:"params"`
			Result jsontext.Value `json:"result"`
			Error  jsontext.Value `json:"error"`
		}
		werr := json.Unmarshal([]byte(raw), &want)
		if err != nil || werr != nil {
			t.Fatalf("%s: %v, %v", raw, err, werr)
		}
		if !reflect.DeepEqual(got, FrameHead{want.Method, want.ID, want.Params, want.Result, want.Error}) {
			t.Errorf("%s:\n got %#v\nwant %#v", raw, got, want)
		}
	}
	for _, raw := range []string{`[]`, `null`, `"x"`, `{"method":1}`, `{"method":"a","method":"b"}`, `{`, `{} {}`, "{\"method\":\"\xff\"}"} {
		if _, err := Frame(raw).Head(); err == nil {
			t.Errorf("%s: no error", raw)
		}
	}
	frame := Frame(`{"method":"message","params":{"a":1}}`)
	h, _ := frame.Head()
	if &h.Params[0] != &frame[len(`{"method":"message","params":`)] {
		t.Error("params are a copy")
	}
}
