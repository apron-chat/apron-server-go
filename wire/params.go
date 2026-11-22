package wire

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"strconv"
)

// Fields are the fields of a JSON object, such as a request's params, each
// kept raw. Its accessors read one field each with the protocol's rules
// for it (§1.1): a field of the wrong type, or a null where a value is
// needed, is invalid_params. Each reports whether the object has the field
// at all, as the protocol tells a field sent empty from one left out
// (§3.3.1). The typed params of the Backend's optional interfaces are read
// with them, and a Method reads its own the same way, with the Request's
// accessors, which read its params.
type Fields map[string]jsontext.Value

// Fields returns the request's params, by name.
func (r *Request) Fields() Fields {
	if r.keys == nil {
		_ = json.Unmarshal(r.Params, &r.keys)
		if r.keys == nil {
			r.keys = Fields{}
		}
	}
	return r.keys
}

// Missing is the error of a request that lacks a required param.
func Missing(name string) *Error { return InvalidParams("Missing " + name) }

func isNull(raw jsontext.Value) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

// Has reports whether the request has a param, null included.
func (r *Request) Has(name string) bool {
	_, ok := r.Fields()[name]
	return ok
}

// String reads a string param.
func (f Fields) String(name string) (value string, present bool, _ *Error) {
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
func (f Fields) IDParam(name string) (value string, present bool, _ *Error) {
	value, present, err := f.String(name)
	if err == nil && present && !ValidID(value) {
		err = InvalidParams(name + " must be an ID: not empty, without whitespace or control characters")
	}
	return value, present, err
}

// Bool reads a boolean param.
func (f Fields) Bool(name string) (value, present bool, _ *Error) {
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
func (f Fields) Object(name string) (value Fields, present bool, _ *Error) {
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
func (f Fields) Ext(name string) (Ext, bool, *Error) {
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
// string of a non-negative number.
func (f Fields) LogID(name string) (int64, bool, *Error) {
	raw, ok := f[name]
	if !ok {
		return 0, false, nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return 0, true, InvalidParams(name + " must be a decimal string")
	}
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil || number < 0 {
		return 0, true, InvalidParams(name + " must be a non-negative decimal string")
	}
	return number, true, nil
}

// Count reads a positive integer param, such as a limit.
func (f Fields) Count(name string) (int, bool, *Error) {
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
// and returns its message_id, a positive decimal string.
func (f Fields) MessageRef(name string) (string, bool, *Error) {
	raw, present := f[name]
	if !present {
		return "", false, nil
	}
	var ref struct {
		MessageID *string `json:"message_id"`
	}
	if isNull(raw) || json.Unmarshal(raw, &ref) != nil || ref.MessageID == nil || !ValidMessageID(*ref.MessageID) {
		return "", true, InvalidParams(name + ".message_id must be a positive decimal string")
	}
	return *ref.MessageID, true, nil
}

// ValidMessageID reports whether id may be a message_id or log_id: a
// positive decimal string (§2.1).
func ValidMessageID(id string) bool {
	if len(id) == 0 || id[0] < '1' || id[0] > '9' {
		return false
	}
	for _, ch := range id {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// Ext is an ext object (§4.13), keyed by extension name, each value kept
// as the JSON it arrived as, so that a merge or a store keeps it byte for
// byte.
type Ext = map[string]jsontext.Value

// MergeExt merges a write's ext into a kept one, one level deep (§4.13):
// each key the write carries replaces the kept value, a key whose value is
// empty ("", [], {}) is removed, and keys the write leaves out stay. null
// is an ordinary value. The result is a new object, or nil when no key is
// left; kept is not modified.
func MergeExt(kept, write Ext) Ext {
	merged := make(Ext, len(kept)+len(write))
	maps.Copy(merged, kept)
	for key, value := range write {
		if emptyJSON(value) {
			delete(merged, key)
		} else {
			merged[key] = value
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// emptyJSON reports whether a compact JSON value is an empty value, which
// clears what it is merged into (§3.3.1).
func emptyJSON(value jsontext.Value) bool {
	switch string(value) {
	case `""`, `[]`, `{}`:
		return true
	}
	return false
}

// The Request's accessors read its params (Fields).

func (r *Request) String(name string) (string, bool, *Error)     { return r.Fields().String(name) }
func (r *Request) IDParam(name string) (string, bool, *Error)    { return r.Fields().IDParam(name) }
func (r *Request) Bool(name string) (bool, bool, *Error)         { return r.Fields().Bool(name) }
func (r *Request) Object(name string) (Fields, bool, *Error)     { return r.Fields().Object(name) }
func (r *Request) Ext(name string) (Ext, bool, *Error)           { return r.Fields().Ext(name) }
func (r *Request) LogID(name string) (int64, bool, *Error)       { return r.Fields().LogID(name) }
func (r *Request) Count(name string) (int, bool, *Error)         { return r.Fields().Count(name) }
func (r *Request) MessageRef(name string) (string, bool, *Error) { return r.Fields().MessageRef(name) }
