package apron

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"

	"github.com/apron-chat/apron-server-go/wire"
)

// Error codes (PROTOCOL.md §1.1). Extensions return them in an *Error to
// refuse a request, a command, or a message. They are package wire's.
const (
	CodeParseError     = wire.CodeParseError
	CodeInvalidRequest = wire.CodeInvalidRequest
	// CodeUnsupported is a method or scheme the server does not offer.
	CodeUnsupported = wire.CodeUnsupported
	// CodeInvalidParams is a request that names something that does not
	// exist, or whose params are malformed.
	CodeInvalidParams = wire.CodeInvalidParams
	CodeInternalError = wire.CodeInternalError
	// CodeDenied is a request the sender may not make.
	CodeDenied = wire.CodeDenied
	// CodeRetryAfter is a request refused for now; Data carries
	// retry_after, in seconds (see RetryAfter).
	CodeRetryAfter = wire.CodeRetryAfter
	// CodeTooLarge is a request over a size limit.
	CodeTooLarge = wire.CodeTooLarge
)

// Error is a protocol error reply (PROTOCOL.md §1.1): package wire's, so
// that code written for either package refuses requests the same way.
// Message is shown to people, so it says what to do in a sentence.
type Error = wire.Error

// Denied is a CodeDenied error with message.
func Denied(message string) *Error { return wire.Denied(message) }

// InvalidParams is a CodeInvalidParams error with message.
func InvalidParams(message string) *Error { return wire.InvalidParams(message) }

type request struct {
	method string
	params map[string]jsontext.Value
	id     string
	hasID  bool
	// w is the request as package wire handles it, which a reply goes
	// through.
	w *wire.Request
}

// newRequest is the request of r, with its params by name.
func newRequest(r *wire.Request) request {
	req := request{method: r.Method, id: r.ID, hasID: !r.Notification(), w: r}
	_ = json.Unmarshal(r.Params, &req.params)
	if req.params == nil {
		req.params = map[string]jsontext.Value{}
	}
	return req
}

// reply sends the request's result now, followed by frames, with nothing
// between them: handlers whose results describe state reply while they
// hold s.mu, so that a result reflects every notification sent before it
// on the connection, and none after (§1).
func (req request) reply(result any, then ...any) bool {
	frames := make([]wire.Frame, 0, len(then))
	for _, value := range then {
		payload, rendered := value.(jsontext.Value)
		if !rendered {
			payload = encodeJSON(value)
		}
		if payload != nil {
			frames = append(frames, wire.Frame(payload))
		}
	}
	return req.w.Reply(result, frames...)
}

func invalidParams(format string, args ...any) *Error {
	return &Error{Code: CodeInvalidParams, Message: fmt.Sprintf(format, args...)}
}

func parseString(params map[string]jsontext.Value, name string, required bool) (string, *Error) {
	raw, ok := params[name]
	if !ok {
		if required {
			return "", invalidParams("Missing %s", name)
		}
		return "", nil
	}
	var value string
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
		return "", invalidParams("%s must be a string", name)
	}
	return value, nil
}

// validID reports whether id can be a room_id or user_id (§2.4): one that is
// not empty and has no whitespace or control characters.
func validID(id string) bool { return wire.ValidID(id) }

func parseBool(params map[string]jsontext.Value, name string, required bool) (bool, *Error) {
	raw, ok := params[name]
	if !ok {
		if required {
			return false, invalidParams("Missing %s", name)
		}
		return false, nil
	}
	var value bool
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
		return false, invalidParams("%s must be a boolean", name)
	}
	return value, nil
}

func parseObject(params map[string]jsontext.Value, name string, required bool) (map[string]any, *Error) {
	raw, ok := params[name]
	if !ok {
		if required {
			return nil, invalidParams("Missing %s", name)
		}
		return nil, nil
	}
	var value map[string]any
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return nil, invalidParams("%s must be an object", name)
	}
	return value, nil
}

// extObject is an ext object (§4.12) keyed by extension name, each value
// kept as the JSON it arrived as, so a merge or a store keeps it byte for
// byte, numbers beyond 2^53 included.
type extObject = map[string]jsontext.Value

// parseExt reads an optional ext object of a write (§4.12). present reports
// whether the request carried it.
func parseExt(params map[string]jsontext.Value, name string) (ext extObject, present bool, err *Error) {
	raw, ok := params[name]
	if !ok {
		return nil, false, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &ext) != nil || ext == nil {
		return nil, true, invalidParams("%s must be an object", name)
	}
	for key, value := range ext {
		value = slices.Clone(value)
		if value.Compact() != nil {
			return nil, true, invalidParams("%s.%s is not valid JSON", name, key)
		}
		ext[key] = value
	}
	return ext, true, nil
}

// emptyJSON reports whether a compact JSON value is an empty value ("", [],
// or {}), which clears what it is merged into (§3.3).
func emptyJSON(value jsontext.Value) bool {
	switch string(value) {
	case `""`, `[]`, `{}`:
		return true
	}
	return false
}

// mergeExt merges a write's ext into the kept one, one level deep (§4.12):
// each key the write carries replaces the kept value, a key whose value is
// empty is removed, and keys the write leaves out stay. null is an ordinary
// value, and an empty write changes nothing. The result is a new object, or
// nil when no key is left; kept is not modified.
func mergeExt(kept, write extObject) extObject {
	merged := make(extObject, len(kept)+len(write))
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

// clearedExt lists, as their empty values, the keys of a write's ext that
// cleared a key that was kept, for a notification that carries the change
// (§4.12).
func clearedExt(kept, write extObject) extObject {
	var cleared extObject
	for key, value := range write {
		if _, had := kept[key]; had && emptyJSON(value) {
			if cleared == nil {
				cleared = make(extObject)
			}
			cleared[key] = value
		}
	}
	return cleared
}

// extOf returns the ext of an object decoded with decodeObject, or nil.
func extOf(value any) extObject {
	ext, _ := value.(extObject)
	return ext
}

// decodeObject decodes a JSON object, keeping the values of a top-level
// ext as raw JSON so that re-encoding the object keeps them exactly.
func decodeObject(raw []byte) map[string]any {
	var fields map[string]jsontext.Value
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil
	}
	value := make(map[string]any, len(fields))
	for key, field := range fields {
		if key == "ext" {
			var ext extObject
			if json.Unmarshal(field, &ext) == nil && ext != nil {
				value[key] = ext
				continue
			}
		}
		var decoded any
		if json.Unmarshal(field, &decoded) == nil {
			value[key] = decoded
		}
	}
	return value
}

// encodedSize is the size of a value encoded as JSON.
func encodedSize(value any) int {
	return len(encodeJSON(value))
}

func cloneValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		copyValue := make(map[string]any, len(value))
		for key, child := range value {
			copyValue[key] = cloneValue(child)
		}
		return copyValue
	case []any:
		copyValue := make([]any, len(value))
		for i, child := range value {
			copyValue[i] = cloneValue(child)
		}
		return copyValue
	default:
		return value
	}
}

func cloneObject(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	return cloneValue(value).(map[string]any)
}
