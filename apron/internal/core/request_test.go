package core

import (
	"bytes"
	"reflect"
	"testing"
)

func TestMalformedFrames(t *testing.T) {
	for _, tc := range []struct {
		input string
		code  int
		id    string
	}{
		{`{`, codeParseError, ""},
		{`[]`, codeInvalidRequest, ""},
		{`null`, codeInvalidRequest, ""},
		{`{"method":"message","id":"x","params":[]}`, codeInvalidParams, "x"},
		{`{"method":"message","id":1}`, codeInvalidRequest, ""},
		// Frames are parsed as RFC 7493 I-JSON: a repeated key or invalid
		// UTF-8 is a parse error rather than silently resolved.
		{`{"method":"message","id":"x","method":"history"}`, codeParseError, ""},
		{"{\"method\":\"message\",\"params\":{\"body\":{\"text\":\"\xff\"}}}", codeParseError, ""},
	} {
		t.Run(tc.input, func(t *testing.T) {
			req, err := ParseRequest([]byte(tc.input))
			if err == nil || err.Code != tc.code || req.ID != tc.id {
				t.Fatalf("parse result = %#v, %#v", req, err)
			}
		})
	}
}

// Unknown envelope keys are ignored (§1), jsonrpc included, so a
// JSON-RPC 2.0 client's frames parse whatever version they name.
func TestUnknownEnvelopeKeysAreIgnored(t *testing.T) {
	for _, input := range []string{
		`{"jsonrpc":"2.0","method":"history","id":"x"}`,
		`{"jsonrpc":"1.0","method":"history","id":"x"}`,
		`{"jsonrpc":2,"method":"history","id":"x","extra":{}}`,
	} {
		req, err := ParseRequest([]byte(input))
		if err != nil || req.Method != "history" || req.ID != "x" {
			t.Fatalf("%s: %#v, %#v", input, req, err)
		}
	}
}

// A request's fingerprint is the same however its params are written,
// and differs for other params, or another method.
func TestFingerprint(t *testing.T) {
	same := []string{
		`{"room_id":"general","body":{"text":"é","mentions":["a"]},"n":100}`,
		`{ "body" : { "mentions" : [ "a" ], "text" : "é" }, "n": 1e2, "room_id": "general" }`,
	}
	for _, params := range same[1:] {
		if Fingerprint("message", []byte(params)) != Fingerprint("message", []byte(same[0])) {
			t.Errorf("%s has another fingerprint than %s", params, same[0])
		}
	}
	for _, other := range []struct{ method, params string }{
		{"message", `{"room_id":"general","body":{"text":"e","mentions":["a"]},"n":100}`},
		{"message", `{"room_id":"general","body":{"text":"é","mentions":["a"]},"n":101}`},
		{"command", same[0]},
	} {
		if Fingerprint(other.method, []byte(other.params)) == Fingerprint("message", []byte(same[0])) {
			t.Errorf("%s %s has the fingerprint of message %s", other.method, other.params, same[0])
		}
	}
}

// A well-formed frame parses in one pass as it does member by member.
func TestParseWellFormed(t *testing.T) {
	for _, frame := range []string{
		`{"method":"message","id":"x","params":{"body":{"text":"hi"},"room_id":"general"}}`,
		` { "params" : { } , "method" : "me" , "id" : "" , "jsonrpc": "2.0" } `,
		`{"method":"history","id":"1"}`,
		`{"method":"activity","id":7,"params":{"typing":5}}`,
		`{"method":"ping"}`,
		`{"id":"x","method":"nonesuch","params":{"a":[1,{"b":null}]}}`,
	} {
		fast, ok := parseWellFormed([]byte(frame))
		if !ok {
			t.Errorf("%s is not well formed", frame)
			continue
		}
		slow, err := parseAny([]byte(frame))
		if err != nil || !reflect.DeepEqual(fast, slow) {
			t.Errorf("%s parses as %#v, and member by member as %#v, %v", frame, fast, slow, err)
		}
	}
	for _, frame := range []string{`null`, `[]`, `{"id":"x"}`, `{"method":""}`, `{"method":1}`, `{"method":"m","id":null}`, `{"method":"m","id":1}`, `{"method":"m","params":null}`, `{"method":"m","params":[]}`} {
		if _, ok := parseWellFormed([]byte(frame)); ok {
			t.Errorf("%s is well formed", frame)
		}
	}
}

// A well-formed request's params and their values are the frame's own
// bytes, not copies, which an append to one does not write over.
func TestParseInPlace(t *testing.T) {
	payload := []byte(`{"id":"1","method":"message","params":{"room_id":"general","body":{"text":"hi"}},"z":1}`)
	req, perr := ParseRequest(payload)
	if perr != nil {
		t.Fatal(perr)
	}
	within := func(v []byte) bool {
		return len(v) > 0 && &v[0] == &payload[bytes.Index(payload, v)]
	}
	if !within(req.Params) || !within(req.Fields["body"]) || !within(req.Fields["room_id"]) {
		t.Fatalf("params %s, fields %v: not the frame's own bytes", req.Params, req.Fields)
	}
	if string(req.Fields["room_id"]) != `"general"` || string(req.Params) != `{"room_id":"general","body":{"text":"hi"}}` {
		t.Fatalf("params %s, fields %v", req.Params, req.Fields)
	}
	_ = append(req.Fields["room_id"], 'x')
	_ = append(req.Params, 'x')
	if string(payload) != `{"id":"1","method":"message","params":{"room_id":"general","body":{"text":"hi"}},"z":1}` {
		t.Fatalf("an append wrote over the frame: %s", payload)
	}
}

// ParseReply reads the replies a Server writes, in place, and leaves any
// other frame to be decoded whole.
func TestParseReply(t *testing.T) {
	for _, tc := range []struct {
		frame, id, result, err string
	}{
		{`{"id":"a-1","result":{"message_id":"5"}}`, "a-1", `{"message_id":"5"}`, ""},
		{`{"error":{"code":"denied","message":"no"},"id":"a-2"}`, "a-2", "", `{"code":"denied","message":"no"}`},
		{` { "id" : "a-3" , "result" : { } } `, "a-3", `{ }`, ""},
	} {
		id, result, rawError, ok := ParseReply([]byte(tc.frame))
		if !ok || string(id) != tc.id || string(result) != tc.result || string(rawError) != tc.err {
			t.Errorf("%s: %q %s %s %v", tc.frame, id, result, rawError, ok)
		}
	}
	for _, frame := range []string{
		`{"method":"message","params":{}}`, `{"id":"\u0061","result":{}}`, `{"id":7,"result":{}}`,
		`{"id":null,"result":{}}`, `{"result":{}}`, `{"id":"a","result":{},"jsonrpc":"2.0"}`,
		`{"error":"x","id":"a"}`, `{"id":"a","id":"b"}`, `{`, `[]`,
	} {
		if _, _, _, ok := ParseReply([]byte(frame)); ok {
			t.Errorf("%s read as a reply", frame)
		}
	}
}
