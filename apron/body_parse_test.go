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

// A history result with its records raw is the result with them decoded
// into its arrays.
func TestHistoryResultRaw(t *testing.T) {
	typed := HistoryResult{
		Rooms:      []Room{{RoomID: "general", LogID: 5}},
		Messages:   []Message{{MessageID: 6, LogID: 7, RoomID: "general", From: User{UserID: "a", Name: "é"}, Body: &Body{Text: "hi"}}},
		FirstLogID: 5, LastLogID: 7, More: true, LatestLogID: 9, HistoryLogID: 1,
	}
	raw := HistoryResult{FirstLogID: 5, LastLogID: 7, More: true, LatestLogID: 9, HistoryLogID: 1, Raw: &RawRecords{}}
	for _, r := range typed.Rooms {
		encoded, _ := Marshal(r)
		raw.Raw.Rooms = append(raw.Raw.Rooms, encoded)
	}
	for _, m := range typed.Messages {
		encoded, _ := Marshal(m)
		raw.Raw.Messages = append(raw.Raw.Messages, encoded)
	}
	want, err := Marshal(typed)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Marshal(raw)
	if err != nil || string(got) != string(want) {
		t.Errorf("raw: %s, %v; want %s", got, err, want)
	}
	if empty, _ := Marshal(HistoryResult{Raw: &RawRecords{}}); string(empty) != `{"more":false,"history_log_id":null}` {
		t.Errorf("empty: %s", empty)
	}
}

// decodeMembers decodes a body as Body.UnmarshalJSONFrom did before it
// read tokens: its members raw, then each it knows.
func decodeMembers(raw []byte) (Body, error) {
	var in bodyMembers
	if err := json.Unmarshal(raw, &in); err != nil {
		return Body{}, err
	}
	body := Body{Unknown: in.Unknown, sent: bodyDecoded}
	for _, field := range []struct {
		raw  jsontext.Value
		into any
		bit  bodyFields
	}{
		{in.Text, &body.Text, bodyText},
		{in.Format, &body.Format, bodyFormat},
		{in.Mentions, &body.Mentions, bodyMentions},
		{in.Embeds, &body.Embeds, bodyEmbeds},
	} {
		if field.raw == nil {
			continue
		}
		body.sent |= field.bit
		if err := json.Unmarshal(field.raw, field.into); err != nil {
			return Body{}, err
		}
	}
	return body, nil
}

// A body read token by token is the body its members decode as: present
// when null, unknown members kept as they arrived, escaped names known,
// and the same values refused.
func TestBodyUnmarshal(t *testing.T) {
	for _, raw := range []string{
		`{}`, `null`, `{"text":"hi"}`, `{"text":null}`, `{"text":null,"format":null,"mentions":null,"embeds":null}`,
		`{"text":"","mentions":[],"embeds":[]}`,
		` { "x" : { "y" : [ 1 , 2 ] } , "text" : "a" , "z" : null } `,
		`{"\u0074ext":"escaped name","t\u0065xt2":1}`,
		`{"text":"\u00e9\ud83d\ude00\n\"","format":"markdown","mentions":["a","b"],"embeds":[{"kind":"image"},{ "kind" : "x" }]}`,
		`{"a":1,"b":2,"c":3}`,
	} {
		var got Body
		gotErr := json.Unmarshal([]byte(raw), &got)
		want, wantErr := decodeMembers([]byte(raw))
		if gotErr != nil || wantErr != nil {
			t.Errorf("%s: %v, want %v", raw, gotErr, wantErr)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got %#v\nwant %#v", raw, got, want)
		}
	}
	for _, raw := range []string{
		`"x"`, `[]`, `{"text":5}`, `{"format":true}`, `{"mentions":"a"}`, `{"embeds":{}}`,
		"{\"text\":\"\xff\"}", "{\"x\":\"\xff\"}", `{"text":"a","text":"b"}`,
	} {
		var got Body
		gotErr := json.Unmarshal([]byte(raw), &got)
		_, wantErr := decodeMembers([]byte(raw))
		if (gotErr == nil) != (wantErr == nil) {
			t.Errorf("%s: error %v, want %v", raw, gotErr, wantErr)
		}
	}
	// A message's body null is no body, as before.
	var m Message
	if err := json.Unmarshal([]byte(`{"from":{"user_id":"a"},"body":null}`), &m); err != nil || m.Body != nil {
		t.Errorf("body null: %+v, %v", m.Body, err)
	}
}
