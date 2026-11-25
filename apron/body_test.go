package apron_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/apron-chat/apron-server-go/apron"
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
		var body apron.Body
		if err := json.Unmarshal([]byte(in), &body); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		for name, encode := range map[string]func() ([]byte, error){
			"json.Marshal":     func() ([]byte, error) { return json.Marshal(body) },
			"json.Marshal ptr": func() ([]byte, error) { return json.Marshal(&body) },
			"apron.Marshal":    func() ([]byte, error) { return apron.Marshal(body) },
		} {
			if out, err := encode(); err != nil || string(out) != in {
				t.Errorf("%s: %s encodes as %s, %v", name, in, out, err)
			}
		}
		// In a message too, and again after a second round trip.
		var m apron.Message
		if err := json.Unmarshal([]byte(`{"from":{"user_id":"ada"},"body":`+in+`}`), &m); err != nil {
			t.Fatalf("message with %s: %v", in, err)
		}
		out, err := apron.Marshal(m)
		if want := `{"from":{"user_id":"ada"},"body":` + in + `}`; err != nil || string(out) != want {
			t.Errorf("message encodes as %s, %v; want %s", out, err, want)
		}
		var again apron.Message
		if err := json.Unmarshal(out, &again); err != nil {
			t.Fatal(err)
		}
		if twice, _ := apron.Marshal(again); string(twice) != string(out) {
			t.Errorf("second round trip: %s, want %s", twice, out)
		}
	}
}

// A Body built in Go sends text, "" too, and each other field only when it
// is not empty.
func TestBodyBuilt(t *testing.T) {
	for _, c := range []struct {
		body apron.Body
		want string
	}{
		{apron.Body{}, `{"text":""}`},
		{apron.Body{Text: "hi"}, `{"text":"hi"}`},
		{apron.Body{Mentions: []string{}, Embeds: []jsontext.Value{}}, `{"text":""}`},
		{apron.Body{Format: apron.Markdown, Mentions: []string{"ada"}}, `{"text":"","format":"markdown","mentions":["ada"]}`},
		{apron.Body{Embeds: []jsontext.Value{jsontext.Value(`{"kind":"x"}`)}}, `{"text":"","embeds":[{"kind":"x"}]}`},
		{apron.Body{Text: "hi", Unknown: jsontext.Value(`{"color":"red"}`)}, `{"text":"hi","color":"red"}`},
	} {
		if out, err := apron.Marshal(c.body); err != nil || string(out) != c.want {
			t.Errorf("%+v encodes as %s, %v; want %s", c.body, out, err, c.want)
		}
	}
}

// A decoded Body that a backend changes sends what it has now: a field it
// set that the client left out, and a field it emptied that the client
// sent.
func TestBodyChanged(t *testing.T) {
	var body apron.Body
	if err := json.Unmarshal([]byte(`{"text":"hi @ada","mentions":["ada"],"x":1}`), &body); err != nil {
		t.Fatal(err)
	}
	body.Text, body.Mentions, body.Format = "", nil, apron.Markdown
	body.Embeds = []jsontext.Value{jsontext.Value(`{"kind":"og"}`)}
	if out, _ := apron.Marshal(body); string(out) != `{"text":"","format":"markdown","mentions":[],"embeds":[{"kind":"og"}],"x":1}` {
		t.Errorf("changed body encodes as %s", out)
	}
	// Decoding replaces what a Body held, what it carried included.
	if err := json.Unmarshal([]byte(`{"embeds":[]}`), &body); err != nil {
		t.Fatal(err)
	}
	if body.Text != "" || body.Format != "" || body.Mentions != nil || body.Unknown != nil {
		t.Errorf("decoding kept %+v", body)
	}
	if out, _ := apron.Marshal(body); string(out) != `{"embeds":[]}` {
		t.Errorf("redecoded body encodes as %s", out)
	}
}

// A null field is read as its empty value, and sent as it; a field of the
// wrong type is an error.
func TestBodyDecode(t *testing.T) {
	var body apron.Body
	if err := json.Unmarshal([]byte(`{"text":null,"mentions":null}`), &body); err != nil {
		t.Fatal(err)
	}
	if out, _ := apron.Marshal(body); string(out) != `{"text":"","mentions":[]}` {
		t.Errorf("nulls encode as %s", out)
	}
	for _, bad := range []string{`{"text":1}`, `{"mentions":"ada"}`, `{"embeds":{}}`, `[]`, `"hi"`, `{"text":"a","text":"b"}`} {
		if err := json.Unmarshal([]byte(bad), &body); err == nil {
			t.Errorf("%s decoded as %+v", bad, body)
		}
	}
}

// ParseBody checks a body as the message requests' parsers do.
func TestParseBody(t *testing.T) {
	for in, want := range map[string]string{
		`null`:                         "body must be an object",
		`[]`:                           "body must be an object",
		`{"text":null}`:                "body.text must be a string",
		`{"text":5}`:                   "body.text must be a string",
		`{"format":"html"}`:            "body.format must be plain or markdown",
		`{"mentions":"ada"}`:           "body.mentions must be an array of user_id strings",
		`{"mentions":["a b"]}`:         "body.mentions must be an array of user_id strings",
		`{"embeds":{}}`:                "body.embeds must be an array",
		`{"embeds":[null]}`:            "body.embeds[0] must be an object",
		`{"embeds":[{"kind":"x"},[]]}`: "body.embeds[1] must be an object",
		`{"embeds":[{}]}`:              "body.embeds[0].kind must be a non-empty string",
		`{"embeds":[{"kind":""}]}`:     "body.embeds[0].kind must be a non-empty string",
		`{"embeds":[{"kind":5}]}`:      "body.embeds[0].kind must be a non-empty string",
		`{"embeds":[{"kind":null}]}`:   "body.embeds[0].kind must be a non-empty string",
	} {
		body, err := apron.ParseBody(jsontext.Value(in))
		if e, ok := err.(*apron.Error); !ok || e.Code != apron.CodeInvalidParams || e.Message != want {
			t.Errorf("ParseBody(%s) = %v, %v; want %q", in, body, err, want)
		}
	}
	const in = `{"text":"hi","format":"markdown","mentions":["ada"],"embeds":[{"kind":"upload"}],"color":"red"}`
	body, err := apron.ParseBody(jsontext.Value(in))
	if err != nil || body.Text != "hi" || len(body.Mentions) != 1 || len(body.Embeds) != 1 || string(body.Unknown) != `{"color":"red"}` {
		t.Fatalf("ParseBody(%s) = %+v, %v", in, body, err)
	}
	if out, _ := json.Marshal(body); string(out) != in {
		t.Fatalf("parsed body encodes as %s", out)
	}
}
