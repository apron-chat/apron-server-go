package apron

import (
	"context"
	"errors"
)

// errLaterAuth is a Later of an auth request, which is a barrier.
var errLaterAuth = errors.New("wire: auth is a barrier (§3.2.2), answered before the frames after it, never later")

// later is the state of a request answered after its handler returns
// (Reply.Later).
type later struct {
	// stop stops the request being abandoned when its connection ends.
	stop func() bool
	// closed is set when the connection ended before the reply.
	closed bool
	// finish, once the handler has returned, finishes the request's
	// deduplication entry with its outcome, as its reply is queued.
	finish func(dedupOutcome)
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
	r.later = &later{}
	if r.entry != nil {
		r.entry.Later.Store(true)
	}
	if r.c != nil {
		r.later.stop = context.AfterFunc(r.c.ctx, r.abandon)
	}
	return nil
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
	if finish := l.finish; finish != nil {
		l.finish = nil
		finish(r.outcome)
	}
}

// finishOrDefer returns the request's outcome, for its deduplication entry
// to be finished with, once its handler has returned; for a request still
// to be answered later, done is false, and finish runs as its reply is
// queued, or its connection ends.
func (r *Request) finishOrDefer(finish func(dedupOutcome)) (outcome dedupOutcome, done bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.later != nil && !r.replied {
		r.later.finish = finish
		return dedupOutcome{}, false
	}
	return r.outcome, true
}

// repliedErrLocked is the error of a reply to a request that has its reply,
// or whose connection ended before it was answered later.
func (r *Request) repliedErrLocked() error {
	if r.later != nil && r.later.closed {
		return ErrClosed
	}
	return ErrReplied
}

// failWith sends err as the request's error reply, as a handler's error is
// (Server.protocolError).
func (r *Request) failWith(err error) error {
	var e *Error
	switch {
	case err == nil:
		r.logger().Error("wire: a request was failed with a nil error", "method", r.Method)
		e = internalError()
	case r.c != nil:
		e = r.c.server.protocolError(r.c.ctx, r.Method, err)
	default:
		var ok bool
		if e, ok = asError(err); !ok {
			r.logger().Error("wire: request failed", "method", r.Method, "error", err)
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
		return ErrClosed
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
