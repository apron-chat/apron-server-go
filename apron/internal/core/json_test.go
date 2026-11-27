package core

import (
	"encoding/json/jsontext"
	"testing"
)

// The envelopes encode as the maps of their members do, byte for byte.
func TestEnvelopes(t *testing.T) {
	id, empty := "k3J9xQ2mZ8aB-17  \"é", ""
	params := map[string]any{"z": 1, "a": []string{"<&>"}, "m": nil}
	raw := jsontext.Value(`{ "b" : 1, "a":"é", "n": 1.0e2 }`)
	type errorLike struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	e := &errorLike{"denied", "No <b>"}
	for _, tc := range []struct {
		name     string
		envelope any
		members  map[string]any
	}{
		{"notification", &NotificationFrame{Method: "message", Params: params}, map[string]any{"method": "message", "params": params}},
		{"notification of nil", &NotificationFrame{Method: "x"}, map[string]any{"method": "x", "params": nil}},
		{"request", &RequestFrame{ID: &id, Method: "message", Params: params}, map[string]any{"id": id, "method": "message", "params": params}},
		{"request with an empty id", &RequestFrame{ID: &empty, Method: "me"}, map[string]any{"id": "", "method": "me"}},
		{"notification sent", &RequestFrame{Method: "activity", Params: raw}, map[string]any{"method": "activity", "params": raw}},
		{"result", &ReplyFrame{ID: &id, Result: raw}, map[string]any{"id": id, "result": raw}},
		{"result to an empty id", &ReplyFrame{ID: &empty, Result: jsontext.Value(`{}`)}, map[string]any{"id": "", "result": jsontext.Value(`{}`)}},
		{"error", &ReplyFrame{ID: &id, Error: e}, map[string]any{"id": id, "error": e}},
		{"error without an id", &ReplyFrame{Error: e}, map[string]any{"error": e}},
	} {
		got, err := Marshal(tc.envelope)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		want, err := Marshal(tc.members)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s: %s, want %s", tc.name, got, want)
		}
	}
}
