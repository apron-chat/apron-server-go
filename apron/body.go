package apron

import (
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
	// Each known member is read raw first, so that a null one is present.
	var in bodyMembers
	if err := json.UnmarshalDecode(dec, &in); err != nil {
		return err
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
		if err := json.Unmarshal(field.raw, field.into, dec.Options()); err != nil {
			return err
		}
	}
	*b = body
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
