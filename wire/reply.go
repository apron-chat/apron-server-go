package wire

import (
	"errors"
	"fmt"
)

// ErrReplied is a second reply to a request, which has its reply already.
var ErrReplied = errors.New("wire: the request has its reply")

// Empty is the result of a request that answers nothing but success, {}:
// room_join, room_leave, status, and reactions.
type Empty struct{}

// Reply answers the request a typed handler handles, such as a
// RoomHandler's room_list. A handler calls Send exactly once, or returns an
// error, which is the error reply. A handler of a Reply[Empty] that returns
// nil without sending has {} sent for it; any other that does is a bug, which
// is logged and answered error/internal_error.
//
// Send runs what wire does with the method's result, such as making the
// user a me result carries the connection's, and so does it at the moment
// the result is queued: a backend that keeps its state under a lock calls
// Send while it holds it.
type Reply[T any] struct {
	r *Request
	// check returns what is sent for a result: the result, normalized, or
	// within its method's result object. An error is a result the backend
	// may not send, which is logged and answered error/internal_error.
	check func(*T) (any, error)
	// apply runs as the result is queued, under the request's lock, such
	// as the sign-in of an auth result.
	apply func(*T)
}

// Send sends the request's result now, followed by then with nothing
// between them, such as notifications that complete it, rather than once
// the handler returns. A backend that keeps its state under a lock replies
// while it holds it, so that the result reflects every notification sent
// before it on the connection, and none after (§1); like Conn.Send, it
// never blocks.
//
// Send returns ErrReplied when the request has its reply already, and
// ErrClosed when the connection is closed. A result the method may not
// have, such as an auth result whose You has no valid user_id, is logged
// and answered error/internal_error, which Send returns; then is not sent.
// A handler may return what Send returns.
func (p Reply[T]) Send(result T, then ...Frame) error {
	if p.r == nil {
		return errors.New("wire: Send on a zero Reply")
	}
	value := func() (any, error) { return result, nil }
	if p.check != nil {
		value = func() (any, error) { return p.check(&result) }
	}
	var apply func()
	if p.apply != nil && p.r.c != nil {
		apply = func() { p.apply(&result) }
	}
	return p.r.send(result, value, apply, then)
}

// Request is the request the reply answers, raw: its params as they
// arrived, and Request.Reply, which sends any result, without what Send
// does with the method's, such as the normalization of a room_list result,
// or the sign-in of an auth result. A handler that replies raw does that
// itself.
func (p Reply[T]) Request() *Request { return p.r }

// send queues the reply to r: the value it renders, followed by then. apply
// runs as it is queued. sent is the result as the handler gave it, which a
// request on no connection records.
func (r *Request) send(sent any, value func() (any, error), apply func(), then []Frame) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.replied {
		return ErrReplied
	}
	result, err := value()
	if err != nil {
		r.logger().Error("wire: a handler sent a result its method may not have", "method", r.Method, "error", err)
		e := internalError()
		r.failLocked(e)
		return e
	}
	frame, encoded, e := r.encodeReply(result, nil)
	r.replied = true
	r.outcome = dedupOutcome{encoded, e}
	if r.rec != nil {
		if e != nil {
			r.rec.Record(sent, nil, e, nil)
			return e
		}
		r.rec.Record(sent, encoded, nil, framesOf(then))
		return nil
	}
	switch {
	case frame == nil:
		r.c.Close()
		return ErrClosed
	case e != nil:
		r.c.enqueue([]Frame{frame})
		return e
	}
	if apply != nil {
		apply()
	}
	// A reply is never held back by auth (§3.2.2): what is held is what
	// comes after it.
	if !r.c.enqueue(append([]Frame{frame}, then...)) {
		return ErrClosed
	}
	return nil
}

// failLocked queues an error reply, with r.mu held.
func (r *Request) failLocked(e *Error) {
	r.replied = true
	r.outcome = dedupOutcome{err: e}
	if r.rec != nil {
		r.rec.Record(nil, nil, e, nil)
		return
	}
	if frame, _, _ := r.encodeReply(nil, e); frame != nil {
		r.c.enqueue([]Frame{frame})
	}
}

// isEmpty reports whether T is Empty, whose handlers may return without
// sending.
func isEmpty[T any]() bool {
	_, ok := any(*new(T)).(Empty)
	return ok
}

// errNoReply is a handler's bug: it returned nil without replying.
func errNoReply(method string) error {
	return fmt.Errorf("wire: the handler of %s returned nil without replying, as it must, or returning an error", method)
}

// framesOf returns frames as the byte slices they are.
func framesOf(frames []Frame) [][]byte {
	raw := make([][]byte, len(frames))
	for i, frame := range frames {
		raw[i] = frame
	}
	return raw
}
