package apron

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"
)

// A body ParseBody takes is the Body it decodes as (Body.UnmarshalJSONFrom),
// the fields it carried and the members it does not know included.
func TestParseBodyDecodes(t *testing.T) {
	for _, raw := range []string{
		`{}`,
		`{"text":"hi"}`,
		` { "text" : "", "format" : "markdown", "mentions": [], "x": {"y": [1, 2]} } `,
		`{"mentions":["a","b"],"embeds":[{"kind":"image","url":"u"}],"z":null,"text":"é"}`,
		`{"embeds":[],"format":"plain"}`,
	} {
		got, err := ParseBody(jsontext.Value(raw))
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		var want Body
		if err := json.Unmarshal([]byte(raw), &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(*got, want) {
			t.Errorf("%s: parsed %#v, decodes as %#v", raw, *got, want)
		}
	}
}

// A body is written token by token as it is encoded as a bodyJSON.
func TestBodyMarshal(t *testing.T) {
	bodies := []Body{
		{}, {Text: "hi"}, {Text: "é <&>\"\n", Format: Markdown, Mentions: []string{"a", "b"}},
		{Embeds: []jsontext.Value{jsontext.Value(`{ "kind" : "image", "url": "é" }`)}},
	}
	for _, raw := range []string{`{}`, `{"text":"","mentions":[],"embeds":[]}`, `{"format":"plain"}`, `{"text":null}`} {
		var b Body
		if err := json.Unmarshal([]byte(raw), &b); err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, b)
	}
	for _, b := range bodies {
		got, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		enc := jsontext.NewEncoder(new(bytes.Buffer))
		var want bytes.Buffer
		enc.Reset(&want)
		if err := b.marshalAll(enc); err != nil {
			t.Fatal(err)
		}
		if string(got) != strings.TrimSuffix(want.String(), "\n") {
			t.Errorf("%#v: %s, want %s", b, got, want.String())
		}
	}
}
