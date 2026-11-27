package server

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/internal/core"
)

// A reply written around its result is the reply encoded whole.
func TestResultFrame(t *testing.T) {
	results := []any{
		apron.MessageResult{MessageID: 1724803200042},
		apron.HistoryResult{Messages: []apron.Message{{MessageID: 1, LogID: 2, From: apron.User{UserID: "a", Name: "é <&>"}, Body: &apron.Body{Text: "\"hi\"\n"}}}, More: true},
		map[string]any{"z": []int{1}, "a": nil},
		nil,
	}
	for _, id := range []string{"k3J9xQ2mZ8aB-17", "", " \"é\\"} {
		for _, hasID := range []bool{true, false} {
			for _, result := range results {
				r := &Request{Method: "x", ID: id, hasID: hasID}
				frame, encoded, e := r.encodeReply(result, nil)
				if e != nil {
					t.Fatal(e)
				}
				reply := core.ReplyFrame{Result: encoded}
				if hasID {
					reply.ID = &id
				}
				want, err := core.Marshal(&reply)
				if err != nil {
					t.Fatal(err)
				}
				if string(frame) != string(want) {
					t.Errorf("%q: %s, want %s", id, frame, want)
				}
			}
		}
	}
	// A raw result is encoded within the reply, as it always was.
	r := &Request{Method: "x", ID: "1", hasID: true}
	if frame, _, _ := r.encodeReply(jsontext.Value(`{ "b" : 1 }`), nil); string(frame) != `{"id":"1","result":{"b":1}}` {
		t.Errorf("a raw result: %s", frame)
	}
}
