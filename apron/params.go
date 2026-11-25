package apron

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
)

// Fields are the fields of a JSON object, such as a request's params, each
// kept raw, which a Decoder reads with the protocol's rules for each
// (§1.1). Its map is the escape hatch for a param no Decoder method
// reads, such as an array of a backend's own: Fields()["wake"] is the raw
// value, and whether the request has it.
type Fields map[string]jsontext.Value

// The readers of one field each, which the Decoder's methods read with: a
// field of the wrong type, or a null where a value is needed, is
// invalid_params. Each reports whether the object has the field at all, as
// the protocol tells a field sent empty from one left out (§3.3.1), and
// returns its error as an *Error, nil when the field is absent or well
// formed.

// Fields returns the request's params, by name, as they were decoded once
// when the frame was read: the escape hatch for a param no Decoder method
// reads. They are shared: a handler reads them, and does not change them.
func (r *Request) Fields() Fields { return r.fields }

// Missing is the error of a request that lacks a required param.
func Missing(name string) *Error { return InvalidParams("Missing " + name) }

func isNull(raw jsontext.Value) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

// stringParam reads a string param.
func (f Fields) stringParam(name string) (value string, present bool, _ error) {
	raw, ok := f[name]
	if !ok {
		return "", false, nil
	}
	if isNull(raw) || json.Unmarshal(raw, &value) != nil {
		return "", true, InvalidParams(name + " must be a string")
	}
	return value, true, nil
}

// idParam reads a room_id or user_id param, which is not empty and has no
// whitespace or control characters (§2.4).
func (f Fields) idParam(name string) (value string, present bool, _ error) {
	value, present, err := f.stringParam(name)
	if err == nil && present && !ValidID(value) {
		err = InvalidParams(name + " must be an ID: not empty, without whitespace or control characters")
	}
	return value, present, err
}

// boolParam reads a boolean param.
func (f Fields) boolParam(name string) (value, present bool, _ error) {
	raw, ok := f[name]
	if !ok {
		return false, false, nil
	}
	if isNull(raw) || json.Unmarshal(raw, &value) != nil {
		return false, true, InvalidParams(name + " must be a boolean")
	}
	return value, true, nil
}

// objectParam reads an object param, with its values raw.
func (f Fields) objectParam(name string) (value Fields, present bool, _ error) {
	raw, ok := f[name]
	if !ok {
		return nil, false, nil
	}
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return nil, true, InvalidParams(name + " must be an object")
	}
	return value, true, nil
}

// extParam reads an ext param (§4.13): an object whose values are any JSON,
// kept as they arrived, compacted.
func (f Fields) extParam(name string) (Ext, bool, error) {
	object, present, err := f.objectParam(name)
	if err != nil || !present {
		return nil, present, err
	}
	ext := make(Ext, len(object))
	for key, value := range object {
		value = slices.Clone(value)
		if value.Compact() != nil {
			return nil, true, InvalidParams(fmt.Sprintf("%s.%s is not valid JSON", name, key))
		}
		ext[key] = value
	}
	return ext, true, nil
}

// logIDParam reads a log_id param, such as a history bound (§2.1): a decimal
// string of a non-negative number below 2^53, as ParseLogID reads it.
func (f Fields) logIDParam(name string) (LogID, bool, error) {
	raw, ok := f[name]
	if !ok {
		return 0, false, nil
	}
	var value string
	if isNull(raw) || json.Unmarshal(raw, &value) != nil {
		return 0, true, InvalidParams(name + " must be a decimal string")
	}
	switch id, err := parseLogID(value); err {
	case nil:
		return id, true, nil
	case errLogIDRange:
		return 0, true, InvalidParams(name + " must be below 2^53")
	}
	return 0, true, InvalidParams(name + " must be a non-negative decimal string")
}

// messageIDParam reads a message_id param (§2.2): a log_id that is not 0.
func (f Fields) messageIDParam(name string) (LogID, bool, error) {
	raw, ok := f[name]
	if !ok {
		return 0, false, nil
	}
	id, err := messageID(name, raw)
	return id, true, err
}

// messageID reads the message_id raw, the value of the param name.
func messageID(name string, raw jsontext.Value) (LogID, error) {
	var value string
	if isNull(raw) || json.Unmarshal(raw, &value) != nil {
		return 0, InvalidParams(name + " must be a positive decimal string")
	}
	switch id, err := parseLogID(value); {
	case err == nil && id > 0:
		return id, nil
	case err == errLogIDRange:
		return 0, InvalidParams(name + " must be below 2^53")
	}
	return 0, InvalidParams(name + " must be a positive decimal string")
}

// countParam reads a positive integer param, such as a limit.
func (f Fields) countParam(name string) (int, bool, error) {
	raw, ok := f[name]
	if !ok {
		return 0, false, nil
	}
	var value int
	if json.Unmarshal(raw, &value) != nil || value <= 0 {
		return 0, true, InvalidParams(name + " must be a positive integer")
	}
	return value, true, nil
}

// messageRefParam reads a reference to a message (§3.5.4), {"message_id": ...},
// and returns its message_id, as MessageID reads it.
func (f Fields) messageRefParam(name string) (LogID, bool, error) {
	raw, present := f[name]
	if !present {
		return 0, false, nil
	}
	var ref Fields
	if isNull(raw) || json.Unmarshal(raw, &ref) != nil || ref == nil {
		return 0, true, InvalidParams(name + ".message_id must be a positive decimal string")
	}
	raw, present = ref["message_id"]
	if !present {
		return 0, true, InvalidParams(name + ".message_id must be a positive decimal string")
	}
	id, err := messageID(name+".message_id", raw)
	return id, true, err
}
