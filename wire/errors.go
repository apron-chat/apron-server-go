package wire

import (
	"errors"
	"fmt"
)

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

// RetryAfter is a CodeRetryAfter error that asks the client to wait seconds.
func RetryAfter(message string, seconds int) *Error {
	return &Error{Code: CodeRetryAfter, Message: message, Data: map[string]any{"retry_after": seconds}}
}

// TooLarge is a CodeTooLarge error.
func TooLarge(message string) *Error { return &Error{Code: CodeTooLarge, Message: message} }

// internalError is what a client is told about an error that is not an
// *Error: nothing, as its text may be about the server's internals.
func internalError() *Error {
	return &Error{Code: CodeInternalError, Message: "Internal server error"}
}

// asError returns err as an *Error, or nil and false when it is not one.
func asError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) && e != nil {
		return e, true
	}
	return nil, false
}
