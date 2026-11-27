package server

import (
	"context"
	"errors"

	"github.com/apron-chat/apron-server-go/apron"
)

// errLaterAuth is a Later of an auth request, which is a barrier.
var errLaterAuth = errors.New("apron: auth is a barrier (§3.2.2), answered before the frames after it, never later")

// later is the state of a request answered after its handler returns
// (Reply.Later).
type later struct {
	// stop stops the request being abandoned when its connection ends.
	stop func() bool
	// closed is set when the connection ended before the reply.
	closed bool
	// finish is set once the handler has returned, for the request's
	// deduplication entry to be finished with its outcome as its reply is
	// queued.
	finish bool
}

// state reports whether the request has its reply, and whether it is to
// be answered later.
func (r *Request) state() (replied, later bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.replied, r.later != nil
}

// answerLater marks the request as answered after its handler returns. A
// request that has its reply, a notification, or one marked already, is
// left as it is; auth is never answered later.
func (r *Request) answerLater() error {
	if r.Method == "auth" {
		return errLaterAuth
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.replied || r.notification || r.later != nil {
		return nil
	}
	r.answerLaterLocked()
	return nil
}

// answerLaterLocked marks the request, with r.mu held, as answered after
// its handler returns.
func (r *Request) answerLaterLocked() {
	r.later = &later{}
	if r.entry != nil {
		r.entry.Later.Store(true)
	}
	if r.c != nil {
		r.later.stop = context.AfterFunc(r.c.ctx, r.abandon)
	}
}

// reserve fixes the place of the request's reply among its connection's
// frames (Reply.Reserve), and marks it answered later, unless it has its
// reply, or is a notification; a place reserved already stays.
func (r *Request) reserve() error {
	if r.Method == "auth" {
		return errLaterAuth
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.replied || r.notification || r.slot != nil {
		return nil
	}
	if r.later == nil {
		r.answerLaterLocked()
	}
	if r.c != nil {
		// Nil when the connection is closing: the reply then is not
		// queued, as it would not be without a place.
		r.slot = r.c.rt.Reserve()
		r.reserved = true
	}
	return nil
}

// queue queues the request's reply, with r.mu held: in its reserved place,
// if it has one, or else after what is queued.
func (r *Request) queue(frames ...[]byte) bool {
	switch {
	case r.slot != nil:
		// A slot of a connection that stopped is written by no one.
		r.slot.Fill(frames...)
		return !r.c.rt.Stopped()
	case r.reserved:
		return false
	case len(frames) == 1:
		return r.c.rt.SendFrame(frames[0])
	}
	return r.c.rt.Send(frames...)
}

// abandon ends a request answered later whose connection ended before its
// reply: it fails, so that its duplicates are not left waiting, and is not
// kept (§1.2); nothing is sent.
func (r *Request) abandon() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.replied {
		return
	}
	r.later.closed = true
	r.replied = true
	r.outcome = dedupOutcome{err: internalError()}
	r.settleLocked()
}

// settleLocked runs, with r.mu held, once the request has its reply: a
// request answered later is no longer abandoned when its connection ends,
// and its deduplication entry is finished.
func (r *Request) settleLocked() {
	l := r.later
	if l == nil {
		return
	}
	if l.stop != nil {
		l.stop()
	}
	if l.finish {
		l.finish = false
		r.finishDedup(r.outcome)
	}
}

// finishOrDefer returns the request's outcome, for its deduplication entry
// to be finished with, once its handler has returned; for a request still
// to be answered later, done is false, and the entry is finished as its
// reply is queued, or its connection ends.
func (r *Request) finishOrDefer() (outcome dedupOutcome, done bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.later != nil && !r.replied {
		r.later.finish = true
		return dedupOutcome{}, false
	}
	return r.outcome, true
}

// repliedErrLocked is the error of a reply to a request that has its reply,
// or whose connection ended before it was answered later.
func (r *Request) repliedErrLocked() error {
	if r.later != nil && r.later.closed {
		return apron.ErrClosed
	}
	return ErrReplied
}

// failWith sends err as the request's error reply, as a handler's error is
// (Server.protocolError).
func (r *Request) failWith(err error) error {
	var e *apron.Error
	switch {
	case err == nil:
		r.logger().Error("apron: a request was failed with a nil error", "method", r.Method)
		e = internalError()
	case r.c != nil:
		e = r.c.server.protocolError(r.c.ctx, r.Method, err)
	default:
		var ok bool
		if e, ok = asError(err); !ok {
			r.logger().Error("apron: request failed", "method", r.Method, "error", err)
			e = internalError()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.replied:
		return r.repliedErrLocked()
	case r.notification:
		return nil
	case !r.failLocked(e):
		return apron.ErrClosed
	}
	return nil
}

// duplicateLater answers a duplicate of a request answered later, once the
// original has its reply, on a goroutine of its own, so that the duplicate
// does not hold up its connection either.
func (s *Server) duplicateLater(c *Conn, r *Request, prior *dedupEntry) {
	_ = r.answerLater()
	go func() {
		defer s.recoverFrame(r)
		select {
		case <-prior.Done():
		case <-c.ctx.Done():
			// The duplicate is abandoned with its connection.
			return
		}
		s.duplicate(c.ctx, c, r, prior.Value)
	}()
}
