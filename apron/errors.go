package apron

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"time"
)

// ErrClosed is a call on a connection that has ended, on either side, or
// a reply to a request whose connection has.
var ErrClosed = errors.New("apron: the connection is closed")

// Error codes (PROTOCOL.md §1.1).
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	// CodeUnsupported is a method, capability, or scheme the server does not
	// offer.
	CodeUnsupported = -32601
	// CodeInvalidParams is a request that names something that does not
	// exist, or whose params are malformed.
	CodeInvalidParams = -32602
	CodeInternalError = -32603
	// CodeDenied is a request the sender may not make.
	CodeDenied = -32001
	// CodeRetryAfter is a request refused for now; Data carries
	// retry_after, in seconds.
	CodeRetryAfter = -32002
	// CodeTooLarge is a request over a size limit.
	CodeTooLarge = -32003
)

// Error is a protocol error reply (PROTOCOL.md §1.1). Message is shown to
// people as is, so it says what happened, or what to do, in a sentence.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	// Data is details for clients, such as retry_after, or nil.
	Data any `json:"data,omitzero"`
}

func (e *Error) Error() string { return fmt.Sprintf("apron error %d: %s", e.Code, e.Message) }

// Denied is a CodeDenied error.
func Denied(message string) *Error { return &Error{Code: CodeDenied, Message: message} }

// InvalidParams is a CodeInvalidParams error.
func InvalidParams(message string) *Error { return &Error{Code: CodeInvalidParams, Message: message} }

// Unsupported is a CodeUnsupported error.
func Unsupported(message string) *Error { return &Error{Code: CodeUnsupported, Message: message} }

// RetryAfter is a CodeRetryAfter error that asks the client to wait, in
// Data's retry_after: whole seconds, rounded up, and at least one (§1.1).
func RetryAfter(message string, wait time.Duration) *Error {
	seconds := max(1, int64((wait+time.Second-1)/time.Second))
	return &Error{Code: CodeRetryAfter, Message: message, Data: map[string]any{"retry_after": seconds}}
}

// The categories of limit that extension ext:limits adds to a retry_after
// error, as its data.ext.limits: which requests the same limit counts, and
// so which a client holds back until the delay ends.
const (
	// LimitRead counts history and room_list; a client holds back the
	// refused method.
	LimitRead = "read"
	// LimitWrite counts message, reactions, room_join, room_leave, and
	// room_set, all of which a client holds back.
	LimitWrite = "write"
	// LimitUpload counts message and command requests with a new upload
	// embed, all of which a client holds back.
	LimitUpload = "upload"
)

// RetryDelay returns how long a retry_after error (§1.1), which err is or
// wraps, asks the client to wait: its data.retry_after, in seconds, and the
// category of the limit, its data.ext.limits (ext:limits), or "" without
// one. It reports false for any other error, and for one without a
// positive retry_after.
func RetryDelay(err error) (delay time.Duration, category string, ok bool) {
	e, ok := asError(err)
	if !ok || e.Code != CodeRetryAfter || e.Data == nil {
		return 0, "", false
	}
	raw, err := marshal(e.Data)
	if err != nil {
		return 0, "", false
	}
	// Each member is read on its own, so that one of the wrong type, which a
	// client ignores, leaves the others.
	var data, ext map[string]jsontext.Value
	var seconds float64
	if json.Unmarshal(raw, &data) != nil || json.Unmarshal(data["retry_after"], &seconds) != nil || !(seconds > 0) {
		return 0, "", false
	}
	if json.Unmarshal(data["ext"], &ext) == nil {
		_ = json.Unmarshal(ext["limits"], &category)
	}
	// A day is longer than any wait a client would sit out.
	seconds = math.Min(seconds, 24*60*60)
	return time.Duration(seconds * float64(time.Second)), category, true
}

// Missing is the error of a request that lacks a required param.
func Missing(name string) *Error { return InvalidParams("Missing " + name) }

// TooLarge is a CodeTooLarge error.
func TooLarge(message string) *Error { return &Error{Code: CodeTooLarge, Message: message} }

// CodeOf returns the error code a client is told for err (§1.1): the Code
// of the *Error it is or wraps, CodeInternalError for any other error, as
// a handler's error is answered, and 0 for nil.
func CodeOf(err error) int {
	if err == nil {
		return 0
	}
	if e, ok := asError(err); ok {
		return e.Code
	}
	return CodeInternalError
}

// asError returns err as an *Error, or nil and false when it is not one.
func asError(err error) (*Error, bool) {
	if e, ok := errors.AsType[*Error](err); ok && e != nil {
		return e, true
	}
	return nil, false
}
