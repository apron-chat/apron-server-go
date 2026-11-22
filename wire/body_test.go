package wire_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/apron-chat/apron-server-go/wire"
)

// A decoded Body encodes as it arrived: each field it left out stays out,
// and each it carried empty is sent empty, beside the members wire has no
// field for.
func TestBodyRoundTrip(t *testing.T) {
	for _, in := range []string{
		`{}`,
		`{"text":""}`,
		`{"text":"hi"}`,
		`{"format":""}`,
		`{"format":"markdown"}`,
		`{"mentions":[]}`,
		`{"mentions":["ada","bob"]}`,
		`{"embeds":[]}`,
		`{"embeds":[{"kind":"og","url":"https://example.com"}]}`,
		`{"text":"","format":"","mentions":[],"embeds":[]}`,
		`{"text":"hi","format":"plain","mentions":["ada"],"embeds":[{"kind":"x"}]}`,
		`{"color":"red"}`,
		`{"text":"hi","mentions":[],"color":"red","rich":{"a":[1,2]}}`,
	} {
		var body wire.Body
		if err := json.Unmarshal([]byte(in), &body); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		for name, encode := range map[string]func() ([]byte, error){
			"json.Marshal":     func() ([]byte, error) { return json.Marshal(body) },
			"json.Marshal ptr": func() ([]byte, error) { return json.Marshal(&body) },
			"wire.Marshal":     func() ([]byte, error) { return wire.Marshal(body) },
		} {
			if out, err := encode(); err != nil || string(out) != in {
				t.Errorf("%s: %s encodes as %s, %v", name, in, out, err)
			}
		}
		// In a message too, and again after a second round trip.
		var m wire.Message
		if err := json.Unmarshal([]byte(`{"from":{"user_id":"ada"},"body":`+in+`}`), &m); err != nil {
			t.Fatalf("message with %s: %v", in, err)
		}
		out, err := wire.Marshal(m)
		if want := `{"from":{"user_id":"ada"},"body":` + in + `}`; err != nil || string(out) != want {
			t.Errorf("message encodes as %s, %v; want %s", out, err, want)
		}
		var again wire.Message
		if err := json.Unmarshal(out, &again); err != nil {
			t.Fatal(err)
		}
		if twice, _ := wire.Marshal(again); string(twice) != string(out) {
			t.Errorf("second round trip: %s, want %s", twice, out)
		}
	}
}

// A Body built in Go sends text, "" too, and each other field only when it
// is not empty.
func TestBodyBuilt(t *testing.T) {
	for _, c := range []struct {
		body wire.Body
		want string
	}{
		{wire.Body{}, `{"text":""}`},
		{wire.Body{Text: "hi"}, `{"text":"hi"}`},
		{wire.Body{Mentions: []string{}, Embeds: []jsontext.Value{}}, `{"text":""}`},
		{wire.Body{Format: wire.Markdown, Mentions: []string{"ada"}}, `{"text":"","format":"markdown","mentions":["ada"]}`},
		{wire.Body{Embeds: []jsontext.Value{jsontext.Value(`{"kind":"x"}`)}}, `{"text":"","embeds":[{"kind":"x"}]}`},
		{wire.Body{Text: "hi", Unknown: jsontext.Value(`{"color":"red"}`)}, `{"text":"hi","color":"red"}`},
	} {
		if out, err := wire.Marshal(c.body); err != nil || string(out) != c.want {
			t.Errorf("%+v encodes as %s, %v; want %s", c.body, out, err, c.want)
		}
	}
}

// A decoded Body that a backend changes sends what it has now: a field it
// set that the client left out, and a field it emptied that the client
// sent.
func TestBodyChanged(t *testing.T) {
	var body wire.Body
	if err := json.Unmarshal([]byte(`{"text":"hi @ada","mentions":["ada"],"x":1}`), &body); err != nil {
		t.Fatal(err)
	}
	body.Text, body.Mentions, body.Format = "", nil, wire.Markdown
	body.Embeds = []jsontext.Value{jsontext.Value(`{"kind":"og"}`)}
	if out, _ := wire.Marshal(body); string(out) != `{"text":"","format":"markdown","mentions":[],"embeds":[{"kind":"og"}],"x":1}` {
		t.Errorf("changed body encodes as %s", out)
	}
	// Decoding replaces what a Body held, what it carried included.
	if err := json.Unmarshal([]byte(`{"embeds":[]}`), &body); err != nil {
		t.Fatal(err)
	}
	if body.Text != "" || body.Format != "" || body.Mentions != nil || body.Unknown != nil {
		t.Errorf("decoding kept %+v", body)
	}
	if out, _ := wire.Marshal(body); string(out) != `{"embeds":[]}` {
		t.Errorf("redecoded body encodes as %s", out)
	}
}

// A null field is read as its empty value, and sent as it; a field of the
// wrong type is an error.
func TestBodyDecode(t *testing.T) {
	var body wire.Body
	if err := json.Unmarshal([]byte(`{"text":null,"mentions":null}`), &body); err != nil {
		t.Fatal(err)
	}
	if out, _ := wire.Marshal(body); string(out) != `{"text":"","mentions":[]}` {
		t.Errorf("nulls encode as %s", out)
	}
	for _, bad := range []string{`{"text":1}`, `{"mentions":"ada"}`, `{"embeds":{}}`, `[]`, `"hi"`, `{"text":"a","text":"b"}`} {
		if err := json.Unmarshal([]byte(bad), &body); err == nil {
			t.Errorf("%s decoded as %+v", bad, body)
		}
	}
}
