package apron

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"reflect"
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
