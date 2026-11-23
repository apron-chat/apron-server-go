// Package wiretest tests a backend of package wire, in two ways.
//
// A handler is called directly with a Request on no connection
// (NewRequest), and a Reply that records what it sends (NewReply):
//
//	reply, sent := wiretest.NewReply[wire.HistoryResult](wiretest.NewRequest("history", nil))
//	err := backend.History(ctx, conn, wire.HistoryParams{Limit: 10}, reply)
//	if err != nil || !sent.Sent() || len(sent.Result().Messages) != 10 {
//		t.Fatalf("history: %v %+v", err, sent.Result())
//	}
//
// A whole Server is tested as a client sees it, with a Client: in process
// (Connect), or over a WebSocket (Dial), with the server frame, auth, and
// every frame after, in order. A Client keeps the frames a test has not
// taken yet, so that one can expect a notification that arrived before the
// result of the request it waited for:
//
//	c := wiretest.Connect(server)
//	defer c.Close()
//	if _, err := c.Auth(ctx, map[string]any{"scheme": "guest"}); err != nil {
//		t.Fatal(err)
//	}
//	result, err := c.Call(ctx, "message", map[string]any{"body": map[string]any{"text": "hi"}})
//	...
//	echo, err := c.Expect(ctx, wiretest.Method("message"))
//
// A handler that answers later (wire.Reply.Later) returns before its
// reply is sent, which a goroutine of the backend's own sends: called
// directly, its Recorded's Sent is false when it returns, and Wait waits
// for the reply:
//
//	reply, sent := wiretest.NewReply[wire.CommandResult](wiretest.NewRequest("command", nil))
//	if err := backend.Command(ctx, conn, p, reply); err != nil || sent.Sent() {
//		t.Fatalf("command: %v, sent %v; want it answered later", err, sent.Sent())
//	}
//	upstream.answer()
//	if err := sent.Wait(ctx); err != nil || sent.Err() != nil {
//		t.Fatalf("command: %v %v", err, sent.Err())
//	}
//
// An error the handler returns after Later is its own return value, as
// any is. Through a Client, a request's Call returns once its reply comes,
// so a test makes the Call of one answered later from a goroutine, and
// meanwhile sees the requests sent after it answered first, as replies may
// come in any order (§1):
//
//	done := make(chan error, 1)
//	go func() { _, err := c.Call(ctx, "command", slow); done <- err }()
//	upstream.waitFor(slow)
//	if _, err := c.Call(ctx, "command", fast); err != nil { // Not held up.
//		t.Fatal(err)
//	}
//	upstream.answer()
//	if err := <-done; err != nil {
//		t.Fatal(err)
//	}
//
// Frames arrive whenever the server sends them, so what a test asserts
// of their order it reads in order: CallFrames returns the frames that
// arrived before a request's reply, and Sync waits until every frame the
// server queued before it handled Sync's request has arrived, after which
// Pending holds them.
//
// Package conformance, below this one, checks a whole Server against the
// core of PROTOCOL.md (conformance.Run), as the protocol's own conformance
// suite does, so that every backend runs those checks as tests.
package wiretest

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"sync"

	"github.com/apron-chat/apron-server-go/wire"
	"github.com/apron-chat/apron-server-go/wire/internal/testhook"
)

// NewRequest returns a request of method on no connection, with params,
// any value that encodes as a JSON object, {} for nil, as a handler gets
// it: for a Handler (Server.Handle), for a parser (wire.ParseMessage,
// ...), and for NewReply. What a handler sends to it is recorded, and read
// with NewReply's Recorded. Its Conn is nil. It panics on params that are
// not an object.
func NewRequest(method string, params any) *wire.Request {
	raw := jsontext.Value("{}")
	if params != nil {
		encoded, err := wire.Marshal(params)
		if err != nil {
			panic("wiretest: " + err.Error())
		}
		raw = encoded
	}
	r, err := testhook.NewRequest(method, raw, &recording{done: make(chan struct{})})
	if err != nil {
		panic("wiretest: " + err.Error())
	}
	return r.(*wire.Request)
}

// NewReply returns the Reply of r, a request made by NewRequest, or of a
// request of no method when r is nil, for a typed handler of T, and what is
// sent to r. Send does what the Server does with the result of r's method
// (wire.NewReply), such as the {"you": ...} around a me result; Recorded
// has both. NewReply panics on a request NewRequest did not make.
//
// A Handler's test reads what it sent with any T, such as
// jsontext.Value.
func NewReply[T any](r *wire.Request) (wire.Reply[T], *Recorded[T]) {
	if r == nil {
		r = NewRequest("", nil)
	}
	rec, ok := testhook.RecorderOf(r).(*recording)
	if !ok {
		panic("wiretest: NewReply needs a request made by NewRequest")
	}
	return wire.NewReply[T](r), &Recorded[T]{rec}
}

// Recorded is what a handler sent to a request made by NewRequest: its
// result, or the error wire answered with in its place, and the frames
// sent after it. An error the handler returns is its own return value; a
// Server would send that as the reply.
type Recorded[T any] struct{ rec *recording }

// recording is what was sent to one request.
type recording struct {
	mu    sync.Mutex
	sent  bool
	value any
	raw   jsontext.Value
	err   error
	then  []wire.Frame
	// done is closed once the request has its reply.
	done chan struct{}
}

func (r *recording) Record(sent any, raw jsontext.Value, err error, then [][]byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.sent {
		close(r.done)
	}
	r.sent, r.value, r.raw, r.err = true, sent, raw, err
	r.then = nil
	for _, frame := range then {
		r.then = append(r.then, slices.Clone(wire.Frame(frame)))
	}
}

// Sent reports whether the request has its reply: false for one answered
// later (wire.Reply.Later) until it is sent, which Wait waits for.
func (r *Recorded[T]) Sent() bool {
	r.rec.mu.Lock()
	defer r.rec.mu.Unlock()
	return r.rec.sent
}

// Wait waits until the request has its reply, such as one a handler sends
// after it returns (wire.Reply.Later), or until ctx is done, and returns
// ctx's error then.
func (r *Recorded[T]) Wait(ctx context.Context) error {
	select {
	case <-r.rec.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Result returns the result the handler sent: the value it gave Send, or
// Request.Reply, when it is a T, and else the result as encoded (Raw),
// decoded into a T. It is the zero T until the request has its reply, or
// when the reply is an error.
func (r *Recorded[T]) Result() T {
	r.rec.mu.Lock()
	defer r.rec.mu.Unlock()
	var result T
	if value, ok := r.rec.value.(T); ok {
		return value
	}
	if r.rec.raw != nil {
		_ = json.Unmarshal(r.rec.raw, &result)
	}
	return result
}

// Raw returns the result object as the client would get it, after what
// wire does with the method's result, or nil.
func (r *Recorded[T]) Raw() jsontext.Value {
	r.rec.mu.Lock()
	defer r.rec.mu.Unlock()
	return r.rec.raw
}

// Err returns the error wire answered with in place of the result, an
// *wire.Error, such as error/internal_error for a result its method may
// not have (an auth result whose user_id is not valid), or nil.
func (r *Recorded[T]) Err() error {
	r.rec.mu.Lock()
	defer r.rec.mu.Unlock()
	return r.rec.err
}

// Then returns the frames sent right after the result, in order.
func (r *Recorded[T]) Then() []wire.Frame {
	r.rec.mu.Lock()
	defer r.rec.mu.Unlock()
	return slices.Clone(r.rec.then)
}
