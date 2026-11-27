package apron

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"

	"github.com/apron-chat/apron-server-go/apron/internal/core"
)

// Body is a message's content (§3.5).
//
// A Body encodes as it was decoded: a field the JSON left out stays out,
// and one it carried empty, such as "mentions": [], is sent empty again,
// so that a backend that keeps a Body it decoded sends what the client
// sent. A field that is not empty is always sent, so a decoded Body that a
// backend changes, such as by adding mentions, sends the change. A Body
// built in Go sends text, "" too, and the other fields only when they are
// not empty. A null is read as the field's empty value.
type Body struct {
	// Text defaults to "" (§3.5.1).
	Text string `json:"text"`
	// Format is Plain, Markdown, or "", which is Plain.
	Format string `json:"format"`
	// Mentions are the user_ids the message mentions (§3.5.5).
	Mentions []string `json:"mentions"`
	// Embeds are the message's embeds (§4.8), as their objects.
	Embeds []jsontext.Value `json:"embeds"`
	// Unknown holds the members this package has no field for, as they
	// arrived, such as a newer protocol's, and is sent after the others:
	// a backend that keeps a Body keeps them.
	Unknown jsontext.Value `json:",embed"`

	// sent records the fields a decoded Body carried, and that it was
	// decoded; it is zero for a Body built in Go.
	sent bodyFields
}

// bodyFields is a set of a Body's fields.
type bodyFields uint8

const (
	bodyText bodyFields = 1 << iota
	bodyFormat
	bodyMentions
	bodyEmbeds
	// bodyDecoded marks a Body that was decoded, whose text is sent only
	// when it carried one, or has one now.
	bodyDecoded
)

// Empty reports whether a body has no text and no embeds: a new message
// with such a body is invalid_params (§3.5.1).
func (b *Body) Empty() bool { return b == nil || b.Text == "" && len(b.Embeds) == 0 }

// Formats of a Body.
const (
	Plain    = "plain"
	Markdown = "markdown"
)

// bodyJSON is a Body as JSON: a field that is nil is left out.
type bodyJSON struct {
	Text     *string           `json:"text,omitzero"`
	Format   *string           `json:"format,omitzero"`
	Mentions *[]string         `json:"mentions,omitzero"`
	Embeds   *[]jsontext.Value `json:"embeds,omitzero"`
	Unknown  jsontext.Value    `json:",embed"`
}

// MarshalJSONTo writes the body: each field that is not empty, or that
// the decoded body carried, and text for a body built in Go.
func (b Body) MarshalJSONTo(enc *jsontext.Encoder) error {
	if len(b.Unknown) > 0 {
		return b.marshalAll(enc)
	}
	// Written token by token, as encoding a bodyJSON writes it, without
	// what that allocates: every message has a body.
	text := b.Text != "" || b.sent&bodyText != 0 || b.sent&bodyDecoded == 0
	format := b.Format != "" || b.sent&bodyFormat != 0
	mentions := len(b.Mentions) > 0 || b.sent&bodyMentions != 0
	embeds := len(b.Embeds) > 0 || b.sent&bodyEmbeds != 0
	err := enc.WriteToken(jsontext.BeginObject)
	if text && err == nil {
		err = writeMember(enc, "text", jsontext.String(b.Text))
	}
	if format && err == nil {
		err = writeMember(enc, "format", jsontext.String(b.Format))
	}
	if mentions && err == nil {
		err = writeMember(enc, "mentions", jsontext.BeginArray)
		for _, id := range b.Mentions {
			if err == nil {
				err = enc.WriteToken(jsontext.String(id))
			}
		}
		if err == nil {
			err = enc.WriteToken(jsontext.EndArray)
		}
	}
	if embeds && err == nil {
		err = writeMember(enc, "embeds", jsontext.BeginArray)
		for _, embed := range b.Embeds {
			if err == nil {
				err = enc.WriteValue(embed)
			}
		}
		if err == nil {
			err = enc.WriteToken(jsontext.EndArray)
		}
	}
	if err != nil {
		return err
	}
	return enc.WriteToken(jsontext.EndObject)
}

// writeMember writes a member's name, and its value, a token.
func writeMember(enc *jsontext.Encoder, name string, value jsontext.Token) error {
	if err := enc.WriteToken(jsontext.String(name)); err != nil {
		return err
	}
	return enc.WriteToken(value)
}

// marshalAll writes the body as a bodyJSON, its unknown members and all.
func (b Body) marshalAll(enc *jsontext.Encoder) error {
	out := bodyJSON{Unknown: b.Unknown}
	if b.Text != "" || b.sent&bodyText != 0 || b.sent&bodyDecoded == 0 {
		out.Text = &b.Text
	}
	if b.Format != "" || b.sent&bodyFormat != 0 {
		out.Format = &b.Format
	}
	if len(b.Mentions) > 0 || b.sent&bodyMentions != 0 {
		out.Mentions = &b.Mentions
		if b.Mentions == nil {
			out.Mentions = &[]string{}
		}
	}
	if len(b.Embeds) > 0 || b.sent&bodyEmbeds != 0 {
		out.Embeds = &b.Embeds
		if b.Embeds == nil {
			out.Embeds = &[]jsontext.Value{}
		}
	}
	return json.MarshalEncode(enc, out)
}

// UnmarshalJSONFrom reads a body, and notes which of its fields it
// carried, so that it encodes as it arrived.
func (b *Body) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	// Read token by token, each member once: a member this package knows
	// is present even when null, and the others are kept as they arrived,
	// as decoding a bodyMembers keeps them.
	tok, err := dec.ReadToken()
	if err != nil {
		return err
	}
	switch tok.Kind() {
	case 'n':
		*b = Body{sent: bodyDecoded}
		return nil
	case '{':
	default:
		return fmt.Errorf("apron: a body is a JSON object, not %v", tok.Kind())
	}
	body := Body{sent: bodyDecoded}
	for dec.PeekKind() != '}' {
		quoted, err := dec.ReadValue()
		if err != nil {
			return err
		}
		switch bodyMember(quoted) {
		case "text":
			body.sent |= bodyText
			err = readString(dec, &body.Text)
		case "format":
			body.sent |= bodyFormat
			err = readString(dec, &body.Format)
		case "mentions":
			body.sent |= bodyMentions
			err = json.UnmarshalDecode(dec, &body.Mentions)
		case "embeds":
			body.sent |= bodyEmbeds
			err = json.UnmarshalDecode(dec, &body.Embeds)
		default:
			// quoted is the decoder's until its next read.
			if body.Unknown == nil {
				body.Unknown = append(jsontext.Value{'{'}, quoted...)
			} else {
				body.Unknown = append(append(body.Unknown[:len(body.Unknown)-1], ','), quoted...)
			}
			body.Unknown = append(body.Unknown, ':')
			var value jsontext.Value
			if value, err = dec.ReadValue(); err == nil {
				body.Unknown = append(append(body.Unknown, value...), '}')
			}
		}
		if err != nil {
			return err
		}
	}
	if _, err := dec.ReadToken(); err != nil {
		return err
	}
	*b = body
	return nil
}

// bodyMember returns the name of a body's member, quoted as it arrived,
// when it is one this package knows, and else "".
func bodyMember(quoted jsontext.Value) string {
	name := quoted
	if bytes.IndexByte(quoted, '\\') >= 0 {
		unquoted, err := jsontext.AppendUnquote(nil, quoted)
		if err != nil {
			return ""
		}
		name = unquoted
	}
	switch string(name) {
	case `"text"`, "text":
		return "text"
	case `"format"`, "format":
		return "format"
	case `"mentions"`, "mentions":
		return "mentions"
	case `"embeds"`, "embeds":
		return "embeds"
	}
	return ""
}

// readString reads a string, or null as "", as decoding a string does.
func readString(dec *jsontext.Decoder, into *string) error {
	tok, err := dec.ReadToken()
	if err != nil {
		return err
	}
	switch tok.Kind() {
	case 'n':
		*into = ""
	case '"':
		*into = tok.String()
	default:
		return fmt.Errorf("apron: a body's text and format are strings, not %v", tok.Kind())
	}
	return nil
}

// bodyMembers are a body's members as they arrived: those this package
// knows raw, each present when not nil, null too, and the others.
type bodyMembers struct {
	Text     jsontext.Value `json:"text,omitzero"`
	Format   jsontext.Value `json:"format,omitzero"`
	Mentions jsontext.Value `json:"mentions,omitzero"`
	Embeds   jsontext.Value `json:"embeds,omitzero"`
	Unknown  jsontext.Value `json:",embed"`
}

// parseBody reads a message body (§3.5) by its members: each this package
// knows is checked, and the others are kept in Unknown. The Body is what
// decoding the body makes of it (UnmarshalJSONFrom), once every member is
// checked.
func parseBody(in bodyMembers) (*Body, error) {
	body := &Body{Unknown: in.Unknown, sent: bodyDecoded}
	if raw := in.Text; raw != nil {
		if core.IsNull(raw) || json.Unmarshal(raw, &body.Text) != nil {
			return nil, InvalidParams("body.text must be a string")
		}
		body.sent |= bodyText
	}
	if raw := in.Format; raw != nil {
		if core.IsNull(raw) || json.Unmarshal(raw, &body.Format) != nil || (body.Format != Plain && body.Format != Markdown) {
			return nil, InvalidParams("body.format must be plain or markdown")
		}
		body.sent |= bodyFormat
	}
	if raw := in.Mentions; raw != nil {
		if core.IsNull(raw) || json.Unmarshal(raw, &body.Mentions) != nil || slices.ContainsFunc(body.Mentions, func(id string) bool { return !ValidID(id) }) {
			return nil, InvalidParams("body.mentions must be an array of user_id strings")
		}
		body.sent |= bodyMentions
	}
	if raw := in.Embeds; raw != nil {
		if core.IsNull(raw) || json.Unmarshal(raw, &body.Embeds) != nil {
			return nil, InvalidParams("body.embeds must be an array")
		}
		for i, embed := range body.Embeds {
			var object map[string]jsontext.Value
			if core.IsNull(embed) || json.Unmarshal(embed, &object) != nil {
				return nil, InvalidParams(fmt.Sprintf("body.embeds[%d] must be an object", i))
			}
			var kind string
			if raw, ok := object["kind"]; !ok || core.IsNull(raw) || json.Unmarshal(raw, &kind) != nil || kind == "" {
				return nil, InvalidParams(fmt.Sprintf("body.embeds[%d].kind must be a non-empty string", i))
			}
		}
		body.sent |= bodyEmbeds
	}
	return body, nil
}

// ParseBody reads a message body (§3.5) as the message requests' parsers
// read one, such as parseMessage: text a string, format plain or
// markdown, mentions user_ids, and embeds objects with a kind, with the
// members this package does not know kept in Unknown. A backend checks
// with it a body that came from elsewhere, such as one its filter
// rewrote, as apron/server checks a client's. Its error is always an *Error,
// error/invalid_params.
func ParseBody(raw jsontext.Value) (*Body, error) {
	var in bodyMembers
	if raw.Kind() != '{' || json.Unmarshal(raw, &in) != nil {
		return nil, InvalidParams("body must be an object")
	}
	return parseBody(in)
}
