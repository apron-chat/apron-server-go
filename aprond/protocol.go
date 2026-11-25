package aprond

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

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
// that code written for either package refuses requests the same way. An
// extension needs nothing of package wire to use it: it refuses with the
// constructors below, or an Error with one of the Code constants.
//
//	type Error struct {
//		Code    int    // one of the Code constants
//		Message string // shown to people as is
//		Data    any    // details for clients, such as retry_after, or nil
//	}
//
// Message says what happened, or what to do, in a sentence. An extension
// returns an *Error from a command, method, or filter to refuse it with
// its code; any other error is logged, and the client told only of an
// internal error.
type Error = wire.Error

// Denied is a CodeDenied error with message: the sender may not do this.
func Denied(message string) *Error { return wire.Denied(message) }

// InvalidParams is a CodeInvalidParams error with message: the request
// names something that does not exist, or its params are malformed.
func InvalidParams(message string) *Error { return wire.InvalidParams(message) }

// Unsupported is a CodeUnsupported error with message: the server does not
// offer this.
func Unsupported(message string) *Error { return wire.Unsupported(message) }

// RetryAfter is a CodeRetryAfter error with message that asks the client to
// wait, in Data's retry_after: whole seconds, rounded up, and at least one.
func RetryAfter(message string, wait time.Duration) *Error { return wire.RetryAfter(message, wait) }

// TooLarge is a CodeTooLarge error with message: the request is over a
// size limit.
func TooLarge(message string) *Error { return wire.TooLarge(message) }

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

// paramError is the error of a wire.Decoder, which is an *Error whenever it is not nil, as an *Error:
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
func parseString(params wire.Fields, name string, p wire.Requirement) (string, *Error) {
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
