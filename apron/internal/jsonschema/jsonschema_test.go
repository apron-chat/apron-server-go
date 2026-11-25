package jsonschema

import (
	"encoding/json/v2"
	"os"
	"strings"
	"testing"
)

func decode(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("%s: %v", s, err)
	}
	return v
}

// Each keyword accepts what matches it and rejects what does not, as
// draft 2020-12 has it.
func TestKeywords(t *testing.T) {
	for _, c := range []struct {
		schema string
		valid  []string
		not    []string
	}{
		{`{"type":"string"}`, []string{`""`, `"x"`}, []string{`1`, `null`, `true`, `[]`, `{}`}},
		{`{"type":"integer"}`, []string{`0`, `-3`, `1.0`, `1e3`}, []string{`1.5`, `"1"`, `true`, `null`}},
		{`{"type":"number"}`, []string{`0`, `1.5`, `-2e-3`}, []string{`"1"`, `false`, `null`}},
		{`{"type":"boolean"}`, []string{`true`, `false`}, []string{`0`, `"true"`, `null`}},
		{`{"type":"null"}`, []string{`null`}, []string{`0`, `false`, `""`}},
		{`{"type":"array"}`, []string{`[]`, `[1]`}, []string{`{}`, `"[]"`}},
		{`{"type":"object"}`, []string{`{}`, `{"a":1}`}, []string{`[]`, `null`}},
		{`{"type":["string","null"]}`, []string{`"x"`, `null`}, []string{`1`}},
		{`{"const":false}`, []string{`false`}, []string{`0`, `null`, `true`, `"false"`}},
		{`{"const":1}`, []string{`1`, `1.0`}, []string{`true`, `"1"`, `2`}},
		{`{"const":{"a":[1,"x"]}}`, []string{`{"a":[1,"x"]}`, `{"a":[1.0,"x"]}`}, []string{`{"a":[1]}`, `{"a":[1,"x"],"b":0}`, `{"a":["x",1]}`}},
		{`{"enum":["plain","markdown"]}`, []string{`"plain"`, `"markdown"`}, []string{`"html"`, `null`, `0`}},
		{`{"enum":[1,true]}`, []string{`1`, `true`}, []string{`false`, `0`}},
		{`{"minimum":1}`, []string{`1`, `2.5`, `"0"`, `null`}, []string{`0`, `0.999`, `-1`}},
		{`{"exclusiveMinimum":0}`, []string{`0.001`, `1`, `"x"`}, []string{`0`, `-1`}},
		{`{"maxLength":2}`, []string{`""`, `"ab"`, `"éé"`, `"👍👍"`, `123`}, []string{`"abc"`, `"ééé"`}},
		{`{"required":["a","b"]}`, []string{`{"a":1,"b":null}`, `[]`, `"x"`}, []string{`{}`, `{"a":1}`, `{"b":1}`}},
		{`{"properties":{"a":{"type":"string"}}}`, []string{`{}`, `{"a":"x"}`, `{"b":1}`, `5`}, []string{`{"a":1}`, `{"a":null}`}},
		{`{"properties":{"a":{}},"additionalProperties":{"type":"string"}}`, []string{`{"a":1,"b":"x"}`, `{}`}, []string{`{"b":1}`, `{"a":1,"c":null}`}},
		{`{"propertyNames":{"not":{"pattern":"^ext:"}}}`, []string{`{}`, `{"x":1,"text:":2}`, `[]`}, []string{`{"ext:x":1}`}},
		{`{"propertyNames":{"const":"method"}}`, []string{`{"method":"ping"}`, `{}`}, []string{`{"method":"ping","params":{}}`}},
		{`{"dependentRequired":{"room_id":["mute"]}}`, []string{`{}`, `{"mute":true}`, `{"room_id":"r","mute":true}`}, []string{`{"room_id":"r"}`}},
		{`{"dependentSchemas":{"seen":{"properties":{"status":{"const":"offline"}},"required":["status"]}}}`,
			[]string{`{}`, `{"status":"online"}`, `{"seen":"1","status":"offline"}`}, []string{`{"seen":"1"}`, `{"seen":"1","status":"online"}`}},
		{`{"items":{"type":"integer"}}`, []string{`[]`, `[1,2]`, `{}`}, []string{`[1,"2"]`, `[null]`}},
		{`{"minItems":1}`, []string{`["x"]`, `{}`}, []string{`[]`}},
		{`{"contains":{"const":"a"}}`, []string{`["a"]`, `["b","a"]`}, []string{`[]`, `["b"]`}},
		{`{"contains":{"properties":{"kind":{"const":"append"}},"required":["kind"]},"minContains":0,"maxContains":1}`,
			[]string{`[]`, `[{"kind":"upload"}]`, `[{"kind":"append"},{"kind":"upload"}]`}, []string{`[{"kind":"append"},{"kind":"append"}]`}},
		{`{"allOf":[{"type":"string"},{"maxLength":1}]}`, []string{`"a"`}, []string{`"ab"`, `1`}},
		{`{"anyOf":[{"const":false},{"type":"integer","minimum":0}]}`, []string{`false`, `0`, `7`}, []string{`true`, `-1`, `1.5`, `null`}},
		{`{"oneOf":[{"type":"integer"},{"minimum":0}]}`, []string{`-1`, `0.5`, `"x"`}, []string{`1`, `-0.5`}},
		{`{"not":{"required":["id"]}}`, []string{`{}`, `{"method":"x"}`}, []string{`{"id":"1"}`}},
		{`{"if":{"properties":{"method":{"const":"server"}},"required":["method"]},"then":{"required":["params"]}}`,
			[]string{`{"method":"server","params":{}}`, `{"method":"user"}`, `{}`}, []string{`{"method":"server"}`}},
		{`{"if":{"type":"string"},"then":{"maxLength":1},"else":{"type":"integer"}}`, []string{`"a"`, `3`}, []string{`"ab"`, `1.5`}},
		{`true`, []string{`null`, `{}`}, nil},
		{`false`, nil, []string{`null`, `{}`}},
		{`{"$defs":{"Id":{"type":"string"}},"properties":{"id":{"$ref":"#/$defs/Id"}}}`, []string{`{"id":"x"}`}, []string{`{"id":1}`}},
		// $ref applies alongside its siblings (draft 2020-12).
		{`{"$defs":{"S":{"type":"string"}},"$ref":"#/$defs/S","maxLength":1}`, []string{`"a"`}, []string{`"ab"`, `1`}},
		// A recursive reference.
		{`{"$defs":{"T":{"type":"array","items":{"$ref":"#/$defs/T"}}},"$ref":"#/$defs/T"}`, []string{`[]`, `[[],[[]]]`}, []string{`[[1]]`}},
	} {
		s, err := Compile([]byte(c.schema))
		if err != nil {
			t.Errorf("%s: %v", c.schema, err)
			continue
		}
		for _, v := range c.valid {
			if errs := s.Validate(decode(t, v), ""); len(errs) != 0 {
				t.Errorf("%s: %s is valid, got %v", c.schema, v, errs)
			}
		}
		for _, v := range c.not {
			if errs := s.Validate(decode(t, v), ""); len(errs) == 0 {
				t.Errorf("%s: %s is not valid, got no error", c.schema, v)
			}
		}
	}
}

// Patterns are ECMA-262's: \s is its white space, \uXXXX a code point, $
// the end of the string, and . anything but a line terminator; the match
// is anywhere in the string.
func TestPattern(t *testing.T) {
	for _, c := range []struct {
		pattern string
		valid   []string
		not     []string
	}{
		{`^[^\\s\\u0000-\\u001f\\u007f-\\u009f]+$`, []string{"ada", "a.b-c_d", "ünï", "👍"},
			[]string{"", "a b", "a\tb", "a\nb", "ada\n", "a\u00a0b", "a\u2028b", "a\u3000b", "a\ufeffb", "a\u0000", "a\u007f", "a\u0085", "a\u009f"}},
		{`^[1-9][0-9]*$`, []string{"1", "1724803200042"}, []string{"", "0", "01", "-1", "1.5", "12\n", " 1"}},
		{`^[A-Za-z0-9_-]{1,64}$`, []string{"a", "A-b_9", strings.Repeat("x", 64)}, []string{"", "a b", "a.b", strings.Repeat("x", 65)}},
		{`^(https://|http://(([a-z0-9-]+\\.)*localhost|127(\\.[0-9]{1,3}){3}|\\[::1\\])([:/?]|$))`,
			[]string{"https://x", "http://localhost", "http://a.localhost:8080/", "http://127.0.0.1/x", "http://[::1]:1"},
			[]string{"http://x.example", "http://localhostx", "ftp://x", "http://127.0.0/", " https://x"}},
		{`^ext:`, []string{"ext:x"}, []string{"text:", "x"}},
		{`b`, []string{"abc"}, []string{"a"}},
		{`^a.c$`, []string{"abc", "a c"}, []string{"a\nc", "a\rc", "a\u2028c", "ac"}},
		{`^\\d\\D$`, []string{"1a"}, []string{"11", "a1", "١a"}},
		{`^\\S$`, []string{"a"}, []string{" ", "\ufeff"}},
	} {
		s, err := Compile([]byte(`{"pattern":"` + c.pattern + `"}`))
		if err != nil {
			t.Errorf("%s: %v", c.pattern, err)
			continue
		}
		for _, v := range c.valid {
			if errs := s.Validate(v, ""); len(errs) != 0 {
				t.Errorf("%s: %q matches, got %v", c.pattern, v, errs)
			}
		}
		for _, v := range c.not {
			if errs := s.Validate(v, ""); len(errs) == 0 {
				t.Errorf("%s: %q does not match, got no error", c.pattern, v)
			}
		}
	}
}

// A schema that uses what this package does not know is refused, rather
// than checked short of what it says.
func TestCompileRefuses(t *testing.T) {
	for _, schema := range []string{
		`{"unevaluatedProperties":false}`,
		`{"properties":{"a":{"format":"uri"}}}`,
		`{"prefixItems":[{}]}`,
		`{"$ref":"other.json#/x"}`,
		`{"$ref":"#/$defs/Missing"}`,
		`{"pattern":"^(?=a)"}`,
		`{"pattern":"(a)\\1"}`,
		`{"pattern":"[\\S]"}`,
		`{"type":"date"}`,
		`{"anyOf":[]}`,
		`{"required":"a"}`,
		`{"maxLength":-1}`,
		`5`,
		`not json`,
	} {
		if _, err := Compile([]byte(schema)); err == nil {
			t.Errorf("%s compiled", schema)
		}
	}
}

// Errors say where in the value, and a definition is validated by name.
func TestErrors(t *testing.T) {
	s, err := Compile([]byte(`{"$defs":{
		"Frame":{"type":"object","required":["params"],"properties":{"params":{"$ref":"#/$defs/Params"}}},
		"Params":{"type":"object","properties":{"users":{"type":"array","items":{"$ref":"#/$defs/User"}}}},
		"User":{"type":"object","required":["user_id"],"properties":{"user_id":{"type":"string"}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Has("User") || s.Has("Nope") {
		t.Fatal("Has")
	}
	if errs := s.Validate(decode(t, `{"x":1}`), "Nope"); errs != nil {
		t.Fatalf("an undefined name: %v", errs)
	}
	errs := s.Validate(decode(t, `{"params":{"users":[{"user_id":"a"},{"user_id":5},{}]}}`), "Frame")
	if len(errs) != 2 || errs[0].Path != "/params/users/1/user_id" || errs[1].Path != "/params/users/2" ||
		!strings.Contains(errs[0].Message, "not of type string") || !strings.Contains(errs[1].Message, `"user_id" is a required property`) {
		t.Fatalf("errors: %v", errs)
	}
}

// The protocol's schema compiles, and checks frames: a valid one of each
// kind passes, and each broken one fails.
func TestApronSchema(t *testing.T) {
	doc, err := os.ReadFile("../../conformance/apron.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Compile(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{
		`{"method":"server","params":{"apron":10,"auth":["guest"],"capabilities":[],"ping":30}}`,
		`{"method":"message","params":{"message_id":"1","log_id":"1","from":{"user_id":"ada"},"body":{"text":"hi"}}}`,
		`{"method":"pong"}`,
		`{"method":"x_custom","params":{}}`,
		`{"id":"1","result":{}}`,
		`{"id":"1","error":{"code":-32602,"message":"no"}}`,
		`{"error":{"code":1,"message":"no","data":[1]}}`,
		`{"method":"user","params":{"new":{"user_id":"ada","status":"offline","seen":"5"}}}`,
	} {
		if errs := s.Validate(decode(t, f), "ServerFrame"); len(errs) != 0 {
			t.Errorf("%s: %v", f, errs)
		}
	}
	for _, f := range []string{
		`{}`,
		`{"method":"server"}`,
		`{"method":"server","params":{"apron":0,"auth":["guest"],"capabilities":[],"ping":30}}`,
		`{"method":"message","params":{"message_id":"1","log_id":"01","from":{"user_id":"ada"},"body":{}}}`,
		`{"method":"message","params":{"message_id":"1","log_id":"1","from":{"user_id":"a b"},"body":{}}}`,
		`{"method":"message","params":{"message_id":"1","log_id":"1","from":{"user_id":"ada"},"body":{"format":"html"}}}`,
		`{"method":"pong","id":"1"}`,
		`{"method":"auth","params":{}}`,
		`{"id":"1","result":{},"error":{"code":1,"message":"x"}}`,
		`{"id":1,"result":{}}`,
		`{"id":"1","result":[]}`,
		`{"error":{"code":1.5,"message":"no"}}`,
		`{"method":"user","params":{"new":{"user_id":"ada","status":"online","seen":"5"}}}`,
		`{"method":"message","params":{"ext":{"ext:x":1}}}`,
	} {
		if errs := s.Validate(decode(t, f), "ServerFrame"); len(errs) == 0 {
			t.Errorf("%s: no error", f)
		}
	}
	if errs := s.Validate(decode(t, `{"message_id":5}`), "MessageResult"); len(errs) == 0 {
		t.Error("a message result with a number message_id: no error")
	}
}
