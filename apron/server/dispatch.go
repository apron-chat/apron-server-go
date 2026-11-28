package server

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"strconv"
	"sync"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/internal/core"
	"github.com/apron-chat/apron-server-go/apron/internal/testhook"
)

// MaxRequestIDBytes bounds a request id: ids are any string (§2.4), but
// each is kept for deduplication (§1.2), so a longer one is
// error/too_large.
const MaxRequestIDBytes = core.MaxRequestIDBytes

// Handler handles one request method by name, raw: an extension's own
// method, or one that takes the place of a Config handler's (Server.Handle).
type Handler struct {
	// Capability, if set, is listed in the server frame.
	Capability string
	// NoDedup results are not kept for deduplication (§1.2): a retry runs
	// the request again, as is right for one that changes nothing, such as
	// a read.
	NoDedup bool
	// Serve handles a request from a signed-in connection, r, whose params
	// it reads with r.Decoder, and answers with reply, as a typed handler
	// does: reply.Send sends any result that encodes as a JSON object, or
	// Serve returns an error, one that is an *Error the error reply, and
	// any other error/internal_error. A Serve that returns nil without
	// sending has {} sent for it, unless it answers later (Reply.Later).
	Serve func(ctx context.Context, c *Conn, r *Request, reply Reply[any]) error
}

// NotificationHandler handles a notification method a client sends, such
// as activity, by name (Server.HandleNotification). It is handled with or
// without an id, and never answered (§1.1), and before sign-in, ignored
// (§3.2.2).
type NotificationHandler struct {
	// Capability, if set, is listed in the server frame.
	Capability string
	// Serve handles a notification from a signed-in connection.
	Serve func(ctx context.Context, c *Conn, r *Request)
}

// method is how the Server handles a method: a request, or else a
// notification.
type method struct {
	capability string
	noDedup    bool
	// serve handles a request; strict is set when one that returns nil
	// without replying is a bug, rather than {}.
	serve  func(ctx context.Context, c *Conn, r *Request) error
	strict bool
	// notify, when set, makes the method a notification.
	notify func(ctx context.Context, c *Conn, r *Request)
}

// Request is a request, or a notification, as a handler gets it: its
// method, id, and params, which a Handler reads with its Decoder. A typed
// handler's Reply has it too (Reply.Request). Its params are decoded once,
// as the frame is read, and may be read from any goroutine.
type Request struct {
	// Method is the request's method, and ID its id, or "" for a
	// notification.
	Method string
	ID     string

	// params are the request's params, always an object.
	params       jsontext.Value
	c            *Conn
	notification bool
	hasID        bool
	fields       Fields
	// rec, for a request on no connection, made by package aprontest, gets
	// its reply.
	rec testhook.Recorder
	// entry is the request's deduplication entry (§1.2), or nil, kept
	// for userID by key; noDedup is set when its outcome is not to be
	// kept even when it succeeds (Handler.NoDedup).
	entry   *dedupEntry
	userID  string
	key     dedupKey
	noDedup bool

	mu      sync.Mutex
	replied bool
	// slot, once reserved is set, is the reply's place in its connection's
	// queue (Reply.Reserve), nil when the connection was closing.
	slot     *core.Slot
	reserved bool
	outcome  dedupOutcome
	// later is set once the request is to be answered after its handler
	// returns (Reply.Later).
	later *later
}

// Conn is the connection the request came on.
func (r *Request) Conn() *Conn { return r.c }

// reply sends the request's result, raw, any value that encodes as a JSON
// object, {} for nil, and reports whether it was queued: a duplicate's
// outcome, or the {} of a handler that returned without sending.
func (r *Request) reply(result any) bool {
	return r.send(result, func() (any, error) { return result, nil }, nil, nil) == nil
}

// failed reports whether the request's reply is an error.
func (r *Request) failed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.outcome.err != nil
}

// fail sends an error reply, unless the request has its reply.
func (r *Request) fail(e *apron.Error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.replied || r.notification {
		return
	}
	r.failLocked(e)
}

// encodeReply renders the reply to r: its result, or e.
func (r *Request) encodeReply(result any, e *apron.Error) (frame apron.Frame, encoded jsontext.Value, _ *apron.Error) {
	// canonical is set when encoded is as Marshal writes it, which
	// encoding it again within the reply leaves as it is.
	canonical := false
	if e == nil {
		switch v := result.(type) {
		case jsontext.Value:
			encoded = v
		case nil:
			encoded, canonical = jsontext.Value("{}"), true
		default:
			raw, err := core.Marshal(v)
			if err == nil && string(raw) == "null" {
				raw = []byte("{}") // A typed nil, such as a nil map.
			}
			// What Marshal writes is valid JSON.
			if err != nil || raw[0] != '{' {
				r.logger().Error("apron: a result is not a JSON object", "method", r.Method, "error", err)
				e = internalError()
			} else {
				encoded, canonical = raw, true
			}
		}
	}
	if e == nil && canonical {
		// The reply is written around the result, rather than the result
		// encoded again in it, which would copy it as it is: a page of
		// history is tens of kilobytes.
		if payload, ok := r.resultFrame(encoded); ok {
			return payload, encoded, nil
		}
	}
	var reply core.ReplyFrame
	if e != nil {
		reply.Error = e
		encoded = nil
	} else {
		reply.Result = encoded
	}
	if r.hasID {
		reply.ID = &r.ID
	}
	payload, err := core.Marshal(&reply)
	if err != nil {
		return nil, nil, e
	}
	return payload, encoded, e
}

// resultFrame writes the reply of a result as Marshal writes it, as
// encoding a ReplyFrame of it does: {"id":...,"result":...}, the members in
// the order of their names. It reports false for an id Marshal refuses,
// which is not valid UTF-8.
func (r *Request) resultFrame(result jsontext.Value) (apron.Frame, bool) {
	frame := make([]byte, 0, len(`{"id":,"result":}`)+len(r.ID)+2+len(result))
	frame = append(frame, '{')
	if r.hasID {
		var err error
		frame, err = jsontext.AppendQuote(append(frame, `"id":`...), r.ID)
		if err != nil {
			return nil, false
		}
		frame = append(frame, ',')
	}
	frame = append(append(append(frame, `"result":`...), result...), '}')
	return frame, true
}

// logger is the logger of the request's Server.
func (r *Request) logger() *slog.Logger {
	if r.c == nil {
		return slog.Default()
	}
	return r.c.server.logger()
}

// protocolError turns a handler's error into the error reply.
func (s *Server) protocolError(ctx context.Context, method string, err error) *apron.Error {
	if e, ok := asError(err); ok {
		return e
	}
	// A request cut short by its connection's end is not the backend's
	// failure.
	if ctx.Err() == nil {
		s.logger().Error("apron: request failed", "method", method, "error", err)
	}
	return internalError()
}

// recoverFrame recovers a panic in handling r, and is deferred, as recover
// works only there: the panic is logged, and r, if it is a request without
// its reply, is answered error/internal_error. A handler's panic is the
// server's failure, not the client's, so, as in net/http, the connection
// goes on.
func (s *Server) recoverFrame(r *Request) {
	if v := recover(); v != nil {
		s.logger().Error("apron: a handler panicked", "method", r.Method, "panic", v, "stack", string(debug.Stack()))
		r.fail(internalError())
	}
}

// afterReply logs the error of a handler that had already replied, which
// no client is told: at debug level for an *Error, which is a refusal that
// came too late, and as an error for any other.
func (s *Server) afterReply(ctx context.Context, method string, err error) {
	switch e, ok := asError(err); {
	case ok:
		s.logger().Debug("apron: a request was refused after its reply", "method", method, "error", e)
	case ctx.Err() != nil, errors.Is(err, apron.ErrClosed):
		// Cut short by its connection's end.
	default:
		s.logger().Error("apron: a request failed after its reply", "method", method, "error", err)
	}
}

// processFrame handles one frame from a connection, with ctx as its
// requests' context. A panic while it does is logged, and answered
// error/internal_error when the frame is a request; the connection goes on.
func (s *Server) processFrame(ctx context.Context, c *Conn, payload []byte) {
	parsed, failed := core.ParseRequest(payload)
	fields := Fields{parsed.Fields}
	r := &Request{Method: parsed.Method, ID: parsed.ID, params: parsed.Params, fields: fields, c: c, notification: !parsed.HasID, hasID: parsed.HasID}
	defer s.recoverFrame(r)
	if failed != nil {
		// An error about a frame that has no id omits it (§1.1).
		r.notification = false
		perr := &apron.Error{Code: failed.Code, Message: failed.Message}
		switch {
		case core.NotificationOnly[parsed.Method]:
			// A notification is never answered, and a ping with other
			// spacing or keys is still a ping (§1.1).
			if parsed.Method == "ping" {
				c.pong()
			}
		case parsed.HasID, !failed.InvalidParams():
			r.fail(perr)
		}
		return
	}
	if r.Method == "ping" {
		c.pong()
		return
	}
	if r.Method == "auth" {
		// A request method sent without an id is ignored (§1.1).
		if !r.notification {
			s.authenticate(ctx, c, r)
		}
		return
	}
	m, found := s.method(r.Method)
	if found && m.notify != nil {
		// The id of a notification method may be ignored, and the frame
		// handled as a notification (§1.1); before sign-in, it is ignored
		// (§3.2.2).
		r.notification = true
		if c.UserID() != "" && !c.Closing() {
			m.notify(ctx, c, r)
		}
		return
	}
	// Unknown notifications, and request methods sent without an id, are
	// ignored (§1.1).
	if r.notification {
		return
	}
	// A connection being closed, as when its account is signed out, does
	// nothing more.
	if c.Closing() {
		return
	}
	userID, scope := c.dedupScope()
	switch {
	case userID == "":
		r.fail(apron.Denied("Sign in before sending requests"))
		return
	case len(r.ID) > MaxRequestIDBytes:
		// Request ids are any string (§2.4), but each is kept for
		// deduplication, so their size is bounded.
		r.fail(apron.TooLarge(fmt.Sprintf("A request id is at most %d bytes", MaxRequestIDBytes)))
		return
	case !found:
		r.fail(apron.Unsupported("Unsupported method " + strconv.Quote(r.Method)))
		return
	}

	// Deduplicate by user_id and id (§1.2): a duplicate waits for the
	// original, even on another connection, and gets its outcome, brought up
	// to date. Only a connection of the same deduplication scope has it
	// (Conn.SetDedupScope), as no handler runs for it.
	key := dedupKey{scope: scope, id: r.ID}
	entry, prior := s.dedup.claim(userID, key, r.Method, r.params)
	if prior != nil {
		if !prior.Operation.Same(r.Method, r.params) {
			r.fail(apron.InvalidParams(fmt.Sprintf("Request id %q was already used for another operation", r.ID)))
			return
		}
		select {
		case <-prior.Done():
		default:
			if prior.Later.Load() {
				// The original is answered later, and so is its
				// duplicate, which waits for it without holding up its
				// connection.
				s.duplicateLater(c, r, prior)
				return
			}
		}
		// A duplicate whose connection ends first, or whose Local call's
		// context does, is sent nothing.
		select {
		case <-prior.Done():
		case <-ctx.Done():
			return
		}
		s.duplicate(ctx, c, r, prior.Value)
		return
	}

	// Failed requests are not kept, and neither are reads: a retry runs
	// again. Concurrent duplicates still share the outcome. Such a request
	// is forgotten before its reply is queued, as the client may retry as
	// soon as it has the reply, while the handler may still be returning.
	r.entry, r.userID, r.key, r.noDedup = entry, userID, key, m.noDedup
	s.serve(ctx, c, r, m)
	if entry != nil {
		// A request answered later finishes as its reply is queued.
		if outcome, done := r.finishOrDefer(); done {
			r.finishDedup(outcome)
		}
	}
}

// kept reports whether a request's outcome is kept for deduplication: it
// succeeded, and its method's outcomes are kept.
func (r *Request) kept(outcome dedupOutcome) bool { return outcome.err == nil && !r.noDedup }

// finishDedup finishes the request's deduplication entry with its
// outcome, which its duplicates then get.
func (r *Request) finishDedup(outcome dedupOutcome) {
	r.c.server.dedup.finish(r.userID, r.key, r.entry, outcome, !r.kept(outcome))
}

// duplicate answers a duplicate with its original's outcome (§1.2), brought
// up to date.
func (s *Server) duplicate(ctx context.Context, c *Conn, r *Request, outcome dedupOutcome) {
	if outcome.err != nil {
		r.fail(outcome.err)
		return
	}
	var result any = outcome.result
	if refresh := s.config.RefreshDuplicate; refresh != nil {
		result = s.refreshed(r.Method, outcome.result, refresh(ctx, c, r.Method, outcome.result))
	}
	r.reply(result)
}

// refreshed returns the result RefreshDuplicate brought up to date, as it
// is sent: a me result's CurrentUser in the {"you": ...} of me's result.
func (s *Server) refreshed(method string, original jsontext.Value, result any) any {
	if method != "me" {
		return result
	}
	switch you := result.(type) {
	case apron.CurrentUser:
		return meResult{You: you}
	case *apron.CurrentUser:
		if you != nil {
			return meResult{You: *you}
		}
	case jsontext.Value:
		return you
	}
	s.logger().Error("apron: RefreshDuplicate brought a me result up to date as something other than a CurrentUser", "type", fmt.Sprintf("%T", result))
	return original
}

// serve runs a request's handler, and sends its reply: its result, or its
// error. A panic in either is the request's error/internal_error, so that
// the request has its outcome for deduplication all the same.
func (s *Server) serve(ctx context.Context, c *Conn, r *Request, m method) {
	defer s.recoverFrame(r)
	s.finish(ctx, r, m.strict, m.serve(ctx, c, r))
}

// finish answers a request whose handler returned err: with err, unless it
// replied, and else, unless it is answered later, with {}, or, when strict,
// error/internal_error, as a handler that returns nil without replying is a
// bug.
func (s *Server) finish(ctx context.Context, r *Request, strict bool, err error) {
	switch replied, later := r.state(); {
	case err != nil && replied:
		s.afterReply(ctx, r.Method, err)
	case err != nil:
		r.fail(s.protocolError(ctx, r.Method, err))
	case replied, later:
	case strict:
		s.logger().Error(errNoReply(r.Method).Error(), "method", r.Method)
		r.fail(internalError())
	default:
		r.reply(nil)
	}
}

// method returns the handler of a method: one registered with Handle or
// HandleNotification, or one of the Server's Config handlers.
func (s *Server) method(name string) (method, bool) {
	m, ok := s.methods.Load().methods[name]
	return m, ok
}

// typed makes a request handler of a typed one: its params parsed by parse,
// and its Reply doing what Send does for the method (replyFor).
func typed[P, T any](parse func(*Request) (P, error), handle func(context.Context, *Conn, P, Reply[T]) error) method {
	return method{strict: !isEmpty[T](), serve: func(ctx context.Context, c *Conn, r *Request) error {
		p, err := parse(r)
		if err != nil {
			return err
		}
		return handle(ctx, c, p, replyFor[T](r, p))
	}}
}

// with returns m with a capability, and noDedup.
func (m method) with(capability string, noDedup bool) method {
	m.capability, m.noDedup = capability, noDedup
	return m
}

// builtins are the methods of the Server's Config handlers.
func (s *Server) builtins() map[string]method {
	methods := map[string]method{}
	switch messages, edits := s.config.Messages, s.config.Edits; {
	case edits != nil:
		methods["message"] = method{capability: "edit", strict: true, serve: s.message(messages, edits)}
	case messages != nil:
		methods["message"] = method{strict: true, serve: s.message(messages, nil)}
	}
	if p := s.config.Profiles; p != nil {
		methods["me"] = typed(parseMe, p.Me)
	}
	if cmd := s.config.Commands; cmd != nil {
		methods["command"] = typed(parseCommand, cmd.Command).with("command", false)
	}
	if rooms := s.config.Rooms; rooms != nil {
		methods["room_list"] = typed(parseRoomList, rooms.RoomList).with("rooms", true)
		methods["room_join"] = typed(parseRoomJoin, rooms.RoomJoin).with("rooms", false)
		methods["room_leave"] = typed(parseRoomLeave, rooms.RoomLeave).with("rooms", false)
		methods["room_set"] = typed(parseRoomSet, rooms.RoomSet).with("rooms", false)
	}
	if h := s.config.History; h != nil {
		methods["history"] = typed(parseHistory, h.History).with("history", true)
	}
	if st := s.config.Status; st != nil {
		methods["status"] = typed(parseStatus, st.Status).with("status", false)
	}
	if reactions := s.config.Reactions; reactions != nil {
		methods["reactions"] = typed(parseReact, reactions.React).with("reactions", false)
	}
	if a := s.config.Activity; a != nil {
		methods["activity"] = method{capability: "activity", notify: func(ctx context.Context, c *Conn, r *Request) {
			// Params that are not valid are ignored, as a notification is
			// never answered (§1.1).
			if p, err := parseActivity(r); err == nil {
				a.Activity(ctx, c, p)
			}
		}}
	}
	return methods
}

// message applies the core rules of a message request (§3.5.1, §3.6), and
// passes it on to messages, a new message, or edits, an existing one.
func (s *Server) message(messages MessageHandler, edits EditHandler) func(context.Context, *Conn, *Request) error {
	return func(ctx context.Context, c *Conn, r *Request) error {
		m, err := parseMessageFields(r)
		if err != nil {
			return err
		}
		reply := replyFor[apron.MessageResult](r, nil)
		if m.edit() {
			switch {
			case edits == nil:
				return apron.Unsupported("This server does not edit messages")
			case m.messageID == 0:
				// Only a save of an existing message deletes it (§4.4).
				return apron.InvalidParams("Cannot create a deleted message")
			}
			return edits.Edit(ctx, c, m.editParams(), reply)
		}
		if m.body == nil {
			return apron.Missing("body")
		}
		if messages == nil {
			return apron.Unsupported("This server takes no new messages")
		}
		return messages.Message(ctx, c, apron.MessageParams{RoomID: m.roomID, Body: *m.body, ReplyTo: m.reply(), Ext: m.ext}, reply)
	}
}

// definedSchemes are the auth schemes PROTOCOL.md defines.
var definedSchemes = []string{"guest", "token", "webauthn", "email", "external"}

// checkScheme is the default of Config.CheckScheme: the schemes of the
// server frame.
func checkScheme(_ context.Context, c *Conn, p apron.AuthParams) error {
	info := c.ServerInfo()
	if slices.Contains(info.Auth, p.Scheme) || slices.Contains(info.Signup, p.Scheme) {
		return nil
	}
	if slices.Contains(definedSchemes, p.Scheme) {
		return apron.Unsupported("This server does not offer sign-in with " + p.Scheme)
	}
	// Even a scheme PROTOCOL.md does not define is unsupported (§3.2).
	return apron.Unsupported("Unknown sign-in scheme " + strconv.Quote(p.Scheme))
}

// authenticate handles auth, which is a barrier (§3.2.2): the frames after
// it wait for it, and it runs on every connection, also with an id used
// before (§1.2).
func (s *Server) authenticate(ctx context.Context, c *Conn, r *Request) {
	if c.Closing() {
		return
	}
	p, err := parseAuth(r)
	if err != nil {
		r.fail(s.protocolError(ctx, "auth", err))
		return
	}
	check := checkScheme
	if s.config.CheckScheme != nil {
		check = s.config.CheckScheme
	}
	if err := check(ctx, c, p); err != nil {
		r.fail(s.protocolError(ctx, "auth", err))
		return
	}
	// Whatever the backend sends while the sign-in runs comes after its
	// result (§3.2.2). When it fails on a connection not signed in, it is
	// not sent: it was about a sign-in that did not happen. On one signed
	// in, which a failure leaves as it was (§3.2.2), it is sent all the
	// same, as it is what the connection's user would have been sent
	// without the auth, such as the messages broadcast meanwhile.
	before := c.UserID()
	c.hold()
	defer func() { c.release(r.failed() && (before == "" || c.UserID() != before)) }()
	// A panic is answered before the frames held meanwhile are let go, as
	// an error is.
	defer s.recoverFrame(r)
	err = s.backend.Auth(ctx, c, p, replyFor[apron.AuthResult](r, p))
	s.finish(ctx, r, true, err)
}
