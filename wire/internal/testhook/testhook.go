// Package testhook lets package wiretest make requests that are on no
// connection, and see what their handlers send, and read what a Server
// keeps for deduplication, without package wire exporting what only tests
// need. Package wire sets its
// functions as it is initialized; it cannot import wiretest, nor wiretest
// its internals, so the requests are passed as any.
package testhook

import "encoding/json/jsontext"

// Recorder gets what is sent to a request made by NewRequest.
type Recorder interface {
	// Record is called once, with the request's reply: sent is the value
	// the handler gave Reply.Send, raw the result object
	// as it would be written, or err the error reply, and then the frames
	// sent after it.
	Record(sent any, raw jsontext.Value, err error, then [][]byte)
}

var (
	// NewRequest returns a *wire.Request of method, whose params are
	// params, an object, on no connection: what is sent to it goes to rec.
	NewRequest func(method string, params jsontext.Value, rec Recorder) (any, error)
	// RecorderOf returns the Recorder of a *wire.Request made by
	// NewRequest, or nil.
	RecorderOf func(r any) Recorder
	// InitReply makes *reply, a *wire.Reply[T] of any T, the Reply of the
	// *wire.Request r, with what Send does for r's method and T, as the
	// Server makes a handler's.
	InitReply func(reply, r any)
	// DedupUsage reports what the *wire.Server server keeps for
	// deduplication: of how many users, how many requests, and what they
	// cost.
	DedupUsage func(server any) (users, requests, bytes int)
)
