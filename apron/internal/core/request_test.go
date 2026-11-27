package core

import "testing"

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
