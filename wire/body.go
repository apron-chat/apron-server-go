package wire

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
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
	var in struct {
		Text     jsontext.Value `json:"text,omitzero"`
		Format   jsontext.Value `json:"format,omitzero"`
		Mentions jsontext.Value `json:"mentions,omitzero"`
		Embeds   jsontext.Value `json:"embeds,omitzero"`
		Unknown  jsontext.Value `json:",embed"`
	}
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
