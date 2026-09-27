package server

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
			req, err := parseRequest([]byte(tc.input))
			if err == nil || err.Code != tc.code || req.id != tc.id {
				t.Fatalf("parse result = %#v, %#v", req, err)
			}
		})
	}
}

// Unknown envelope keys are ignored (PROTOCOL.md §1), jsonrpc included, so a
// JSON-RPC 2.0 client's frames parse whatever version they name.
func TestUnknownEnvelopeKeysAreIgnored(t *testing.T) {
	for _, input := range []string{
		`{"jsonrpc":"2.0","method":"history","id":"x"}`,
		`{"jsonrpc":"1.0","method":"history","id":"x"}`,
		`{"jsonrpc":2,"method":"history","id":"x","extra":{}}`,
	} {
		req, err := parseRequest([]byte(input))
		if err != nil || req.method != "history" || req.id != "x" {
			t.Fatalf("%s: %#v, %#v", input, req, err)
		}
	}
}
