package wire

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
)

// Fields are the fields of a JSON object, such as a request's params, each
// kept raw. Its accessors read one field each with the protocol's rules
// for it (§1.1): a field of the wrong type, or a null where a value is
// needed, is invalid_params. Each reports whether the object has the field
// at all, as the protocol tells a field sent empty from one left out
// (§3.3.1), and returns its error as an error that is an *Error, nil when
// the field is absent or well formed. The typed params of the Server's
// handlers are read with them (ParseMessage, ...), and a Handler reads its
// own the same way, with the Request's accessors, which read its params,
// or a Decoder, which reads several and keeps the first error.
type Fields map[string]jsontext.Value

// Fields returns the request's params, by name, as they were decoded once
// when the frame was read. They are shared: a handler reads them, and does
// not change them.
func (r *Request) Fields() Fields { return r.fields }

// Missing is the error of a request that lacks a required param.
func Missing(name string) *Error { return InvalidParams("Missing " + name) }

func isNull(raw jsontext.Value) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

// Has reports whether the request has a param, null included.
func (r *Request) Has(name string) bool {
	_, ok := r.Fields()[name]
	return ok
}

// String reads a string param.
func (f Fields) String(name string) (value string, present bool, _ error) {
	raw, ok := f[name]
	if !ok {
		return "", false, nil
	}
	if isNull(raw) || json.Unmarshal(raw, &value) != nil {
		return "", true, InvalidParams(name + " must be a string")
	}
	return value, true, nil
}

// IDParam reads a room_id or user_id param, which is not empty and has no
// whitespace or control characters (§2.4).
func (f Fields) IDParam(name string) (value string, present bool, _ error) {
	value, present, err := f.String(name)
	if err == nil && present && !ValidID(value) {
		err = InvalidParams(name + " must be an ID: not empty, without whitespace or control characters")
	}
	return value, present, err
}

// Bool reads a boolean param.
func (f Fields) Bool(name string) (value, present bool, _ error) {
	raw, ok := f[name]
	if !ok {
		return false, false, nil
	}
	if isNull(raw) || json.Unmarshal(raw, &value) != nil {
		return false, true, InvalidParams(name + " must be a boolean")
	}
	return value, true, nil
}

// Object reads an object param, with its values raw.
func (f Fields) Object(name string) (value Fields, present bool, _ error) {
	raw, ok := f[name]
	if !ok {
		return nil, false, nil
	}
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return nil, true, InvalidParams(name + " must be an object")
	}
	return value, true, nil
}

// Ext reads an ext param (§4.13): an object whose values are any JSON,
// kept as they arrived, compacted.
func (f Fields) Ext(name string) (Ext, bool, error) {
	object, present, err := f.Object(name)
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

// LogID reads a log_id param, such as a history bound (§2.1): a decimal
// string of a non-negative number, as ParseLogID reads it.
func (f Fields) LogID(name string) (LogID, bool, error) {
	raw, ok := f[name]
	if !ok {
		return 0, false, nil
	}
	var value string
	if isNull(raw) || json.Unmarshal(raw, &value) != nil {
		return 0, true, InvalidParams(name + " must be a decimal string")
	}
	id, err := ParseLogID(value)
	if err != nil {
		return 0, true, InvalidParams(name + " must be a non-negative decimal string")
	}
	return id, true, nil
}

// MessageID reads a message_id param (§2.2): a log_id that is not 0.
func (f Fields) MessageID(name string) (LogID, bool, error) {
	raw, ok := f[name]
	if !ok {
		return 0, false, nil
	}
	var value string
	if isNull(raw) || json.Unmarshal(raw, &value) != nil {
		return 0, true, InvalidParams(name + " must be a positive decimal string")
	}
	if id, err := ParseLogID(value); err == nil && id > 0 {
		return id, true, nil
	}
	return 0, true, InvalidParams(name + " must be a positive decimal string")
}

// Count reads a positive integer param, such as a limit.
func (f Fields) Count(name string) (int, bool, error) {
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

// MessageRef reads a reference to a message (§3.5.4), {"message_id": ...},
// and returns its message_id, as MessageID reads it.
func (f Fields) MessageRef(name string) (LogID, bool, error) {
	raw, present := f[name]
	if !present {
		return 0, false, nil
	}
	var ref Fields
	if isNull(raw) || json.Unmarshal(raw, &ref) != nil || ref == nil {
		return 0, true, InvalidParams(name + ".message_id must be a positive decimal string")
	}
	id, present, err := ref.MessageID("message_id")
	if err != nil || !present {
		return 0, true, InvalidParams(name + ".message_id must be a positive decimal string")
	}
	return id, true, nil
}

// The Request's accessors read its params (Fields).

func (r *Request) String(name string) (string, bool, error)    { return r.Fields().String(name) }
func (r *Request) IDParam(name string) (string, bool, error)   { return r.Fields().IDParam(name) }
func (r *Request) Bool(name string) (bool, bool, error)        { return r.Fields().Bool(name) }
func (r *Request) Object(name string) (Fields, bool, error)    { return r.Fields().Object(name) }
func (r *Request) Ext(name string) (Ext, bool, error)          { return r.Fields().Ext(name) }
func (r *Request) LogID(name string) (LogID, bool, error)      { return r.Fields().LogID(name) }
func (r *Request) MessageID(name string) (LogID, bool, error)  { return r.Fields().MessageID(name) }
func (r *Request) Count(name string) (int, bool, error)        { return r.Fields().Count(name) }
func (r *Request) MessageRef(name string) (LogID, bool, error) { return r.Fields().MessageRef(name) }
