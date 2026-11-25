package apron

import (
	"errors"
	"fmt"
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
