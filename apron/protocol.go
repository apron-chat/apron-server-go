package apron

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

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

// request is a request as the code that answers it gets it: an operation's
// (registerOp), whose result goes out raw, or an auth request's, answered
// with its typed Reply.
type request struct {
	params wire.Fields
	// send sends a result, followed by frames with nothing between them.
	send func(result any, frames []wire.Frame) bool
	// auth answers an auth request (sendAuth).
	auth wire.Reply[wire.AuthResult]
}

// sendAuth sends an auth request's result now: with You, it signs the
// connection in as You, so it is sent while s.mu is held, with the
// identity the connection then takes (attachLocked).
func (req request) sendAuth(result wire.AuthResult) {
	_ = req.auth.Send(result)
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
	return req.send(result, frames)
}

// bodyObject decodes a body package wire read, as the server keeps it:
// every field as it was sent, those wire does not know too (Body.Unknown),
// as a wire.Body encodes as it was decoded. It is nil for no body.
func bodyObject(b *wire.Body) map[string]any {
	if b == nil {
		return nil
	}
	var body map[string]any
	_ = json.Unmarshal(encodeJSON(b), &body)
	return body
}

func invalidParams(format string, args ...any) *Error {
	return &Error{Code: CodeInvalidParams, Message: fmt.Sprintf(format, args...)}
}

// paramError is the error of wire's param accessors, or of a
// wire.Decoder, which is an *Error whenever it is not nil, as an *Error:
// nil stays a nil *Error.
func paramError(err error) *Error {
	if err == nil {
		return nil
	}
	if e, ok := errors.AsType[*Error](err); ok {
		return e
	}
	return internalError()
}

// Whether a param read with parseString or a wire.Decoder may be left out.
const (
	required = wire.Required
	optional = wire.Optional
)

// parseString reads one string param, for a request that reads only it;
// one that reads more reads them with a wire.Decoder.
func parseString(params wire.Fields, name string, p wire.Presence) (string, *Error) {
	d := params.Decoder()
	value := d.String(name, p)
	return value, paramError(d.Err())
}

// validID reports whether id can be a room_id or user_id (§2.4): one that is
// not empty and has no whitespace or control characters.
func validID(id string) bool { return wire.ValidID(id) }

// extObject is an ext object (§4.12) keyed by extension name, each value
// kept as the JSON it arrived as, so a merge or a store keeps it byte for
// byte, numbers beyond 2^53 included.
type extObject = wire.Ext

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
func mergeExt(kept, write extObject) extObject { return wire.MergeExt(kept, write) }

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
