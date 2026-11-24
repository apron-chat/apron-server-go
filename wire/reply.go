package wire

import (
	"errors"
	"fmt"
)

// ErrReplied is a second reply to a request, which has its reply already.
var ErrReplied = errors.New("wire: the request has its reply")

// OK is the result of a request that answers nothing but success, {}:
// room_join, room_leave, status, and reactions.
type OK struct{}

// Reply answers the request a typed handler handles, such as a
// RoomHandler's room_list. A handler calls Send exactly once, or returns an
// error, which is the error reply. A handler of a Reply[OK] that returns
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

// Later marks the request as answered after the handler returns, by Send
// or Fail from another goroutine, such as one that waits for a slow
// upstream: the handler returns what Later returns, nil, and the
// connection goes on to its next frame, rather than wait for this one's
// reply. A handler calls Later before it hands the Reply to the goroutine
// that answers, which then answers exactly once.
//
// The handler's ctx is not canceled when the handler returns: it is the
// connection's, canceled when the connection ends (for a Local
// connection, also when the Call's ctx is), and the work of a reply
// answered later uses it. A panic in that work is the backend's: the
// goroutine is the backend's own, and wire does not recover it.
//
// The reply then comes when the backend sends it, after the replies to
// later requests, maybe: replies may come in any order (§1). The
// notifications a result depends on still come before it as they do for
// any reply, if the backend queues them first; see the package
// documentation. A duplicate of the request (§1.2) waits for the reply,
// without holding up its own connection either, and gets its outcome. When
// the connection ends before the reply, the request fails, so that its
// duplicates are not left waiting, and is not kept for deduplication;
// Send and Fail then return ErrClosed.
//
// Later returns nil, and does nothing, for a request that has its reply.
// Auth is a barrier, answered before the connection's next frame
// (§3.2.2), and its Later returns an error, which, returned, answers the
// sign-in error/internal_error. A handler that returns another error after
// Later, before the reply is sent, is answered with it, as any handler is.
func (p Reply[T]) Later() error {
	if p.r == nil {
		return errors.New("wire: Later on a zero Reply")
	}
	return p.r.answerLater()
}

// Fail sends the request's error reply now: err when it is an *Error, and
// else error/internal_error, which is logged, as an error a handler
// returns is. It is how a request answered later (Later) fails, and
// returns nil once the error is queued, ErrReplied when the request has
// its reply already, and ErrClosed when the connection is closed.
func (p Reply[T]) Fail(err error) error {
	if p.r == nil {
		return errors.New("wire: Fail on a zero Reply")
	}
	return p.r.failWith(err)
}

// Request is the request the reply answers: its params as they arrived,
// such as those of an auth scheme that AuthParams has no field for.
func (p Reply[T]) Request() *Request { return p.r }

// initReply is a Reply of any T that a test (package wiretest) makes for a
// request: replyFor, for T it cannot name.
type initReply interface{ init(r *Request) }

func (p *Reply[T]) init(r *Request) { *p = replyFor[T](r, nil) }

// send queues the reply to r: the value it renders, followed by then. apply
// runs as it is queued. sent is the result as the handler gave it, which a
// request on no connection records.
func (r *Request) send(sent any, value func() (any, error), apply func(), then []Frame) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.replied {
		return r.repliedErrLocked()
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
	r.forgetUnkeptLocked()
	// Deferred after the unlock, so it runs first, once the reply is
	// queued.
	defer r.settleLocked()
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

// failLocked queues an error reply, with r.mu held, and reports whether it
// was queued.
func (r *Request) failLocked(e *Error) bool {
	r.replied = true
	r.outcome = dedupOutcome{err: e}
	r.forgetUnkeptLocked()
	defer r.settleLocked()
	if r.rec != nil {
		r.rec.Record(nil, nil, e, nil)
		return true
	}
	frame, _, _ := r.encodeReply(nil, e)
	return frame != nil && r.c.enqueue([]Frame{frame})
}

// forgetUnkeptLocked, with r.mu held, drops the request's deduplication
// entry once it has its outcome, when the outcome is not to be kept,
// before its reply is queued (§1.2).
func (r *Request) forgetUnkeptLocked() {
	if r.unkept != nil {
		r.unkept(r.outcome)
	}
}

// isEmpty reports whether T is OK, whose handlers may return without
// sending.
func isEmpty[T any]() bool {
	_, ok := any(*new(T)).(OK)
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
