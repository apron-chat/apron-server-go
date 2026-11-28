package server

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"iter"
	"slices"
	"strings"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/internal/core"
)

// Fields are the members of a JSON object, such as a request's params,
// each kept raw, as it arrived, which a Decoder reads with the protocol's
// rules for each (§1.1). Get is the escape hatch for a param no Decoder
// method reads, such as an array of a backend's own: Get("wake") is the
// raw value, and whether the request has it. The zero Fields are an empty
// object.
//
// Fields read from a frame are parts of it, its names too, not copies:
// they are shared, and not changed, and one kept past the request, which
// keeps the whole frame, is kept as a copy (Map).
type Fields struct {
	members []core.Field
}

// ParseFields reads the members of a JSON object, as parts of it: object
// is kept as it is, and read with them. Its error is error/invalid_params
// for a value that is not an object, null included.
func ParseFields(object jsontext.Value) (Fields, error) {
	members, ok := core.Members(object)
	if !ok {
		return Fields{}, apron.InvalidParams("not a JSON object")
	}
	return Fields{members}, nil
}

// Get returns the raw value of a member, and whether the object has it,
// null included.
func (f Fields) Get(name string) (jsontext.Value, bool) {
	for _, m := range f.members {
		if m.Name == name {
			return m.Value, true
		}
	}
	return nil, false
}

// Len is how many members the object has.
func (f Fields) Len() int { return len(f.members) }

// All yields each member's name and raw value, in the order they arrived.
func (f Fields) All() iter.Seq2[string, jsontext.Value] {
	return func(yield func(string, jsontext.Value) bool) {
		for _, m := range f.members {
			if !yield(m.Name, m.Value) {
				return
			}
		}
	}
}

// Map returns the members as a map of copies, names and values, which
// may be kept and changed.
func (f Fields) Map() map[string]jsontext.Value {
	m := make(map[string]jsontext.Value, len(f.members))
	for _, member := range f.members {
		m[strings.Clone(member.Name)] = slices.Clone(member.Value)
	}
	return m
}

// MarshalJSONTo writes the object, its members sorted by name, as a map
// of them encodes (apron.Marshal).
func (f Fields) MarshalJSONTo(enc *jsontext.Encoder) error {
	sorted := slices.SortedFunc(slices.Values(f.members), func(a, b core.Field) int { return strings.Compare(a.Name, b.Name) })
	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	for _, m := range sorted {
		if err := enc.WriteToken(jsontext.String(m.Name)); err != nil {
			return err
		}
		if err := enc.WriteValue(m.Value); err != nil {
			return err
		}
	}
	return enc.WriteToken(jsontext.EndObject)
}

// UnmarshalJSONFrom reads a JSON object, its values copies.
func (f *Fields) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	raw, err := dec.ReadValue()
	if err != nil {
		return err
	}
	parsed, err := ParseFields(slices.Clone(raw))
	if err != nil {
		return err
	}
	*f = parsed
	return nil
}

// The readers of one field each, which the Decoder's methods read with: a
// field of the wrong type, or a null where a value is needed, is
// invalid_params. Each reports whether the object has the field at all, as
// the protocol tells a field sent empty from one left out (§3.3.1), and
// returns its error as an *Error, nil when the field is absent or well
// formed.

// Fields returns the request's params, by name, as they were decoded once
// when the frame was read: the escape hatch for a param no Decoder method
// reads. They are shared, parts of the frame as it arrived: a handler
// reads them, and does not change them, and one that keeps a value past
// the request, which keeps the whole frame, keeps a copy.
func (r *Request) Fields() Fields { return r.fields }

// stringParam reads a string param.
func (f Fields) stringParam(name string) (value string, present bool, _ error) {
	raw, ok := f.Get(name)
	if !ok {
		return "", false, nil
	}
	if core.IsNull(raw) || json.Unmarshal(raw, &value) != nil {
		return "", true, apron.InvalidParams(name + " must be a string")
	}
	return value, true, nil
}

// idParam reads a room_id or user_id param, which is not empty and has no
// whitespace or control characters (§2.4).
func (f Fields) idParam(name string) (value string, present bool, _ error) {
	value, present, err := f.stringParam(name)
	if err == nil && present && !apron.ValidID(value) {
		err = apron.InvalidParams(name + " must be an ID: not empty, without whitespace or control characters")
	}
	return value, present, err
}

// boolParam reads a boolean param.
func (f Fields) boolParam(name string) (value, present bool, _ error) {
	raw, ok := f.Get(name)
	if !ok {
		return false, false, nil
	}
	if core.IsNull(raw) || json.Unmarshal(raw, &value) != nil {
		return false, true, apron.InvalidParams(name + " must be a boolean")
	}
	return value, true, nil
}

// objectParam reads an object param, with its values raw.
func (f Fields) objectParam(name string) (value Fields, present bool, _ error) {
	raw, ok := f.Get(name)
	if !ok {
		return Fields{}, false, nil
	}
	members, ok := core.Members(raw)
	if !ok {
		return Fields{}, true, apron.InvalidParams(name + " must be an object")
	}
	return Fields{members}, true, nil
}

// extParam reads an ext param (§4.13): an object whose values are any JSON,
// kept as they arrived, compacted.
func (f Fields) extParam(name string) (apron.Ext, bool, error) {
	object, present, err := f.objectParam(name)
	if err != nil || !present {
		return nil, present, err
	}
	ext := make(apron.Ext, object.Len())
	for key, value := range object.All() {
		value = slices.Clone(value)
		if value.Compact() != nil {
			return nil, true, apron.InvalidParams(fmt.Sprintf("%s.%s is not valid JSON", name, key))
		}
		ext[strings.Clone(key)] = value
	}
	return ext, true, nil
}

// logIDParam reads a log_id param, such as a history bound (§2.1): a decimal
// string of a non-negative number below 2^53, as ParseLogID reads it.
func (f Fields) logIDParam(name string) (apron.LogID, bool, error) {
	raw, ok := f.Get(name)
	if !ok {
		return 0, false, nil
	}
	var value string
	if core.IsNull(raw) || json.Unmarshal(raw, &value) != nil {
		return 0, true, apron.InvalidParams(name + " must be a decimal string")
	}
	switch id, err := core.ParseLogID(value); err {
	case nil:
		return apron.LogID(id), true, nil
	case core.ErrLogIDRange:
		return 0, true, apron.InvalidParams(name + " must be below 2^53")
	}
	return 0, true, apron.InvalidParams(name + " must be a non-negative decimal string")
}

// messageIDParam reads a message_id param (§2.2): a log_id that is not 0.
func (f Fields) messageIDParam(name string) (apron.LogID, bool, error) {
	raw, ok := f.Get(name)
	if !ok {
		return 0, false, nil
	}
	id, err := messageID(name, raw)
	return id, true, err
}

// messageID reads the message_id raw, the value of the param name.
func messageID(name string, raw jsontext.Value) (apron.LogID, error) {
	var value string
	if core.IsNull(raw) || json.Unmarshal(raw, &value) != nil {
		return 0, apron.InvalidParams(name + " must be a positive decimal string")
	}
	switch id, err := core.ParseLogID(value); {
	case err == nil && id > 0:
		return apron.LogID(id), nil
	case err == core.ErrLogIDRange:
		return 0, apron.InvalidParams(name + " must be below 2^53")
	}
	return 0, apron.InvalidParams(name + " must be a positive decimal string")
}

// countParam reads a positive integer param, such as a limit.
func (f Fields) countParam(name string) (int, bool, error) {
	raw, ok := f.Get(name)
	if !ok {
		return 0, false, nil
	}
	var value int
	if json.Unmarshal(raw, &value) != nil || value <= 0 {
		return 0, true, apron.InvalidParams(name + " must be a positive integer")
	}
	return value, true, nil
}

// messageRefParam reads a reference to a message (§3.5.4), {"message_id": ...},
// and returns its message_id, as MessageID reads it.
func (f Fields) messageRefParam(name string) (apron.LogID, bool, error) {
	raw, present := f.Get(name)
	if !present {
		return 0, false, nil
	}
	members, ok := core.Members(raw)
	if !ok {
		return 0, true, apron.InvalidParams(name + ".message_id must be a positive decimal string")
	}
	raw, present = Fields{members}.Get("message_id")
	if !present {
		return 0, true, apron.InvalidParams(name + ".message_id must be a positive decimal string")
	}
	id, err := messageID(name+".message_id", raw)
	return id, true, err
}
