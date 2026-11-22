package wire

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strconv"
	"sync"

	"github.com/apron-chat/apron-server-go/internal/wirecore"
)

// Handler handles one request method by name, raw: an extension's own
// method, or one that takes the place of a handler field's (Server.Handle).
type Handler struct {
	// Capability, if set, is listed in the server frame.
	Capability string
	// NoDedup results are not kept for deduplication (§1.2): a retry runs
	// the request again, as is right for one that changes nothing, such as
	// a read.
	NoDedup bool
	// Serve handles a request from a signed-in connection, and replies with
	// r.Reply, or returns an error: one that is an *Error is the error
	// reply, and any other is error/internal_error. A Serve that returns
	// nil without replying has {} sent for it.
	Serve func(ctx context.Context, c *Conn, r *Request) error
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
// method, id, and params, raw, which a Handler reads with its accessors,
// such as Request.String, and answers with Request.Reply. A typed
// handler's Reply has it too (Reply.Request), and its context carries it
// (RequestFrom).
type Request struct {
	// Method is the request's method, and ID its id, or "" for a
	// notification.
	Method string
	ID     string
	// Params are the request's params, always an object.
	Params jsontext.Value

	c            *Conn
	notification bool
	hasID        bool
	keys         Fields

	mu      sync.Mutex
	replied bool
	outcome dedupOutcome
}

type requestKey struct{}

// RequestFrom returns the request a handler's context was made for, or nil,
// for code deep in a handler's calls that has the context but not the
// handler's Request or Reply.
func RequestFrom(ctx context.Context) *Request {
	r, _ := ctx.Value(requestKey{}).(*Request)
	return r
}

// Conn is the connection the request came on.
func (r *Request) Conn() *Conn { return r.c }

// Notification reports whether the request is a notification, which gets
// no reply.
func (r *Request) Notification() bool { return r.notification }

// Param returns the raw value of a param, and whether the request has it:
// a param sent as "" or null is there.
func (r *Request) Param(name string) (jsontext.Value, bool) {
	value, ok := r.Fields()[name]
	return value, ok
}

// Decode decodes the params into v. A failure is error/invalid_params.
func (r *Request) Decode(v any) error {
	if err := json.Unmarshal(r.Params, v); err != nil {
		return InvalidParams("Invalid params for " + r.Method + ": " + err.Error())
	}
	return nil
}

// Reply sends the request's result, any value that encodes as a JSON
// object, {} for nil, now, followed by frames with nothing between them,
// as Reply.Send does, but raw: without what Send does with a typed
// method's result, such as the sign-in of an auth result, or the
// normalization of a room_list result, which a handler that replies raw
// does itself. Like Conn.Send, it never blocks. It reports false for a
// second reply, a result that is not an object (answered
// error/internal_error), or a closed connection.
// A notification gets no reply, but its frames are sent.
func (r *Request) Reply(result any, then ...Frame) bool {
	if r.notification {
		return r.c.SendBatch(then...)
	}
	return r.send(func() (any, error) { return result, nil }, nil, then) == nil
}

// Replied reports whether the request has its reply.
func (r *Request) Replied() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.replied
}

// failed reports whether the request's reply is an error.
func (r *Request) failed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.outcome.err != nil
}

// fail sends an error reply, unless the request has its reply.
func (r *Request) fail(e *Error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.replied || r.notification {
		return
	}
	r.failLocked(e)
}

// encodeReply renders the reply to r: its result, or e.
func (s *Server) encodeReply(r *Request, result any, e *Error) (frame Frame, encoded jsontext.Value, _ *Error) {
	if e == nil {
		switch v := result.(type) {
		case jsontext.Value:
			encoded = v
		case nil:
			encoded = jsontext.Value("{}")
		default:
			raw, err := marshal(v)
			if err == nil && string(raw) == "null" {
				raw = []byte("{}") // A typed nil, such as a nil map.
			}
			if err != nil || !jsontext.Value(raw).IsValid() || raw[0] != '{' {
				s.logger().Error("wire: a result is not a JSON object", "method", r.Method, "error", err)
				e = internalError()
			} else {
				encoded = raw
			}
		}
	}
	reply := map[string]any{}
	if e != nil {
		reply["error"] = e
		encoded = nil
	} else {
		reply["result"] = encoded
	}
	if r.hasID {
		reply["id"] = r.ID
	}
	payload, err := marshal(reply)
	if err != nil {
		return nil, nil, e
	}
	return payload, encoded, e
}

// protocolError turns a handler's error into the error reply.
func (s *Server) protocolError(ctx context.Context, method string, err error) *Error {
	if e, ok := asError(err); ok {
		return e
	}
	// A request cut short by its connection's end is not the backend's
	// failure.
	if ctx.Err() == nil {
		s.logger().Error("wire: request failed", "method", method, "error", err)
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
		s.logger().Error("wire: a handler panicked", "method", r.Method, "panic", v, "stack", string(debug.Stack()))
		r.fail(internalError())
	}
}

// afterReply logs the error of a handler that had already replied, which
// no client is told: at debug level for an *Error, which is a refusal that
// came too late, and as an error for any other.
func (s *Server) afterReply(ctx context.Context, method string, err error) {
	switch e, ok := asError(err); {
	case ok:
		s.logger().Debug("wire: a request was refused after its reply", "method", method, "error", e)
	case ctx.Err() != nil, errors.Is(err, ErrClosed):
		// Cut short by its connection's end.
	default:
		s.logger().Error("wire: a request failed after its reply", "method", method, "error", err)
	}
}

// processFrame handles one frame from a connection, with ctx as its
// requests' context. A panic while it does is logged, and answered
// error/internal_error when the frame is a request; the connection goes on.
func (s *Server) processFrame(ctx context.Context, c *Conn, payload []byte) {
	parsed, failed := wirecore.ParseRequest(payload)
	r := &Request{Method: parsed.Method, ID: parsed.ID, Params: parsed.Params, c: c, notification: !parsed.HasID, hasID: parsed.HasID}
	defer s.recoverFrame(r)
	if failed != nil {
		// An error about a frame that has no id omits it (§1.1).
		r.notification = false
		perr := &Error{Code: failed.Code, Message: failed.Message}
		switch {
		case wirecore.NotificationOnly[parsed.Method]:
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
			m.notify(context.WithValue(ctx, requestKey{}, r), c, r)
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
	userID := c.UserID()
	switch {
	case userID == "":
		r.fail(Denied("Sign in before sending requests"))
		return
	case len(r.ID) > wirecore.MaxRequestIDBytes:
		// Request ids are any string (§2.4), but each is kept for
		// deduplication, so their size is bounded.
		r.fail(TooLarge(fmt.Sprintf("A request id is at most %d bytes", wirecore.MaxRequestIDBytes)))
		return
	case !found:
		r.fail(Unsupported("Unsupported method " + strconv.Quote(r.Method)))
		return
	}

	// Deduplicate by user_id and id (§1.2): a duplicate waits for the
	// original, even on another connection, and gets its outcome, brought up
	// to date.
	fingerprint := wirecore.Fingerprint(r.Method, r.Params)
	entry, prior := s.dedup.claim(userID, r.ID, fingerprint)
	if prior != nil {
		if prior.Fingerprint != fingerprint {
			r.fail(InvalidParams(fmt.Sprintf("Request id %q was already used for another operation", r.ID)))
			return
		}
		// A duplicate whose connection ends first, or whose Local call's
		// context does, is sent nothing.
		select {
		case <-prior.Done():
		case <-ctx.Done():
			return
		}
		outcome := prior.Value
		if outcome.err != nil {
			r.fail(outcome.err)
			return
		}
		var result any = outcome.result
		if refresh := s.config.refreshDuplicate; refresh != nil {
			result = refresh(ctx, c, r.Method, outcome.result)
		}
		r.Reply(result)
		return
	}

	s.serve(ctx, c, r, m)
	if entry != nil {
		r.mu.Lock()
		outcome := r.outcome
		r.mu.Unlock()
		// Failed requests are not kept, and neither are reads: a retry
		// runs again. Concurrent duplicates still share the outcome.
		s.dedup.finish(userID, r.ID, entry, outcome, outcome.err != nil || m.noDedup)
	}
}

// serve runs a request's handler, and sends its reply: its result, or its
// error. A panic in either is the request's error/internal_error, so that
// the request has its outcome for deduplication all the same.
func (s *Server) serve(ctx context.Context, c *Conn, r *Request, m method) {
	defer s.recoverFrame(r)
	s.finish(ctx, r, m.strict, m.serve(context.WithValue(ctx, requestKey{}, r), c, r))
}

// finish answers a request whose handler returned err: with err, unless it
// replied, and else with {}, or, when strict, error/internal_error, as a
// handler that returns nil without replying is a bug.
func (s *Server) finish(ctx context.Context, r *Request, strict bool, err error) {
	switch replied := r.Replied(); {
	case err != nil && replied:
		s.afterReply(ctx, r.Method, err)
	case err != nil:
		r.fail(s.protocolError(ctx, r.Method, err))
	case replied:
	case strict:
		s.logger().Error(errNoReply(r.Method).Error(), "method", r.Method)
		r.fail(internalError())
	default:
		r.Reply(nil)
	}
}

// method returns the handler of a method: one registered with Handle or
// HandleNotification, or one of the Server's handler fields.
func (s *Server) method(name string) (method, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.methods[name]
	return m, ok
}

// typed makes a request handler of a typed one: its params parsed by parse,
// and its Reply finished by reply, such as with the method's checks.
func typed[P, T any](parse func(*Request) (P, error), handle func(context.Context, *Conn, P, Reply[T]) error, reply func(c *Conn, p P, reply *Reply[T])) method {
	return method{strict: !isEmpty[T](), serve: func(ctx context.Context, c *Conn, r *Request) error {
		p, err := parse(r)
		if err != nil {
			return err
		}
		rp := Reply[T]{r: r}
		if reply != nil {
			reply(c, p, &rp)
		}
		return handle(ctx, c, p, rp)
	}}
}

// with returns m with a capability, and noDedup.
func (m method) with(capability string, noDedup bool) method {
	m.capability, m.noDedup = capability, noDedup
	return m
}

// builtins are the methods of the Server's handler fields.
func (s *Server) builtins() map[string]method {
	methods := map[string]method{}
	switch messages, edits := s.Messages, s.Edits; {
	case edits != nil:
		methods["message"] = method{capability: "edit", strict: true, serve: s.message(messages, edits)}
	case messages != nil:
		methods["message"] = method{strict: true, serve: s.message(messages, nil)}
	}
	if p := s.Profiles; p != nil {
		methods["me"] = typed(parseMe, p.Me, func(c *Conn, _ MeParams, reply *Reply[CurrentUser]) {
			reply.check = func(you *CurrentUser) (any, error) { return meResult{You: *you}, nil }
			reply.apply = func(you *CurrentUser) { c.SetUser(you.User) }
		})
	}
	if cmd := s.Commands; cmd != nil {
		methods["command"] = typed(parseCommand, cmd.Command, nil).with("command", false)
	}
	if rooms := s.Rooms; rooms != nil {
		methods["room_list"] = typed(parseRoomList, rooms.RoomList, func(_ *Conn, p RoomListParams, reply *Reply[RoomListResult]) {
			reply.check = func(result *RoomListResult) (any, error) { return normalizeRoomList(p, *result), nil }
		}).with("rooms", true)
		methods["room_join"] = typed(parseRoomJoin, rooms.RoomJoin, nil).with("rooms", false)
		methods["room_leave"] = typed(parseRoomLeave, rooms.RoomLeave, nil).with("rooms", false)
		methods["room_set"] = typed(parseRoomSet, rooms.RoomSet, nil).with("rooms", false)
	}
	if h := s.History; h != nil {
		methods["history"] = typed(parseHistory, h.History, nil).with("history", true)
	}
	if st := s.Status; st != nil {
		methods["status"] = typed(parseStatus, st.Status, nil).with("status", false)
	}
	if reactions := s.Reactions; reactions != nil {
		methods["reactions"] = typed(parseReact, reactions.React, nil).with("reactions", false)
	}
	if a := s.Activity; a != nil {
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

// messageFields are the params of a message request or a command (§3.5),
// before they are told apart.
type messageFields struct {
	messageID LogID
	roomID    string
	deleted   bool
	body      *Body
	replyTo   *Ref
	ext       Ext
}

// parseMessage reads the params of a message or command (§3.5): what each
// field is, as the protocol defines it. Limits, such as on the count of
// mentions, are the backend's.
func parseMessage(r *Request) (p messageFields, err error) {
	if p.roomID, _, err = r.IDParam("room_id"); err != nil {
		return p, err
	}
	if p.messageID, _, err = r.MessageID("message_id"); err != nil {
		return p, err
	}
	if replyTo, present, err := r.MessageRef("reply_to"); err != nil {
		return p, err
	} else if present {
		p.replyTo = &Ref{MessageID: replyTo}
	}
	if p.deleted, _, err = r.Bool("deleted"); err != nil {
		return p, err
	}
	if p.ext, _, err = r.Ext("ext"); err != nil {
		return p, err
	}
	if p.deleted {
		// A deletion leaves no body (§4.4): what it sends is ignored.
		return p, nil
	}
	fields, present, err := r.Object("body")
	if err != nil || !present {
		return p, err
	}
	raw, _ := r.Param("body")
	p.body, err = parseBody(raw, fields)
	return p, err
}

// parseBody reads a message body (§3.5), raw and by its fields: each field
// this package knows is checked, and the others are kept in Unknown.
func parseBody(raw jsontext.Value, fields Fields) (*Body, error) {
	body := &Body{}
	if raw, ok := fields["text"]; ok && (isNull(raw) || json.Unmarshal(raw, &body.Text) != nil) {
		return nil, InvalidParams("body.text must be a string")
	}
	if raw, ok := fields["format"]; ok {
		if isNull(raw) || json.Unmarshal(raw, &body.Format) != nil || (body.Format != Plain && body.Format != Markdown) {
			return nil, InvalidParams("body.format must be plain or markdown")
		}
	}
	if raw, ok := fields["mentions"]; ok {
		if isNull(raw) || json.Unmarshal(raw, &body.Mentions) != nil || slices.ContainsFunc(body.Mentions, func(id string) bool { return !ValidID(id) }) {
			return nil, InvalidParams("body.mentions must be an array of user_id strings")
		}
	}
	if raw, ok := fields["embeds"]; ok {
		if isNull(raw) || json.Unmarshal(raw, &body.Embeds) != nil {
			return nil, InvalidParams("body.embeds must be an array")
		}
		for i, embed := range body.Embeds {
			var kind struct {
				Kind *string `json:"kind"`
			}
			if json.Unmarshal(embed, &kind) != nil || isNull(embed) || embed[0] != '{' {
				return nil, InvalidParams(fmt.Sprintf("body.embeds[%d] must be an object", i))
			}
			if kind.Kind == nil || *kind.Kind == "" {
				return nil, InvalidParams(fmt.Sprintf("body.embeds[%d].kind must be a non-empty string", i))
			}
		}
	}
	// Every field it knows is well formed, so the body decodes, and keeps
	// the fields it does not know as they arrived.
	if err := json.Unmarshal(raw, body); err != nil {
		return nil, InvalidParams("body must be an object")
	}
	return body, nil
}

// message applies the core rules of a message request (§3.5.1, §3.6), and
// passes it on to messages, a new message, or edits, an existing one.
func (s *Server) message(messages MessageHandler, edits EditHandler) func(context.Context, *Conn, *Request) error {
	return func(ctx context.Context, c *Conn, r *Request) error {
		m, err := parseMessage(r)
		if err != nil {
			return err
		}
		reply := Reply[MessageResult]{r: r}
		if m.messageID != 0 || m.deleted {
			switch {
			case edits == nil:
				return Unsupported("This server does not edit messages")
			case m.messageID == 0:
				// Only a save of an existing message deletes it (§4.4).
				return InvalidParams("Cannot create a deleted message")
			}
			return edits.Edit(ctx, c, EditParams{MessageID: m.messageID, RoomID: m.roomID, Deleted: m.deleted, Body: m.body, ReplyTo: m.replyTo, Ext: m.ext}, reply)
		}
		if m.body == nil {
			return Missing("body")
		}
		if messages == nil {
			return Unsupported("This server takes no new messages")
		}
		return messages.Message(ctx, c, MessageParams{RoomID: m.roomID, Body: m.body, ReplyTo: m.replyTo, Ext: m.ext}, reply)
	}
}

// parseCommand reads a command's params (§4.1): a message's, but for
// message_id and deleted, and with a body.
func parseCommand(r *Request) (CommandParams, error) {
	for _, name := range []string{"message_id", "deleted"} {
		if r.Has(name) {
			return CommandParams{}, InvalidParams("A command has no " + name + "; send it as a message instead")
		}
	}
	m, err := parseMessage(r)
	if err != nil {
		return CommandParams{}, err
	}
	if m.body == nil {
		return CommandParams{}, InvalidParams("A command needs a body")
	}
	return CommandParams{RoomID: m.roomID, Body: m.body, ReplyTo: m.replyTo, Ext: m.ext}, nil
}

// definedSchemes are the auth schemes PROTOCOL.md defines.
var definedSchemes = []string{"guest", "token", "webauthn", "email", "external"}

// checkScheme is the default of Server.CheckScheme: the schemes of the
// server frame.
func checkScheme(_ context.Context, c *Conn, p AuthParams) error {
	info := c.Info()
	if slices.Contains(info.Auth, p.Scheme) || slices.Contains(info.Signup, p.Scheme) {
		return nil
	}
	if slices.Contains(definedSchemes, p.Scheme) {
		return Unsupported("This server does not offer sign-in with " + p.Scheme)
	}
	// Even a scheme PROTOCOL.md does not define is unsupported (§3.2).
	return Unsupported("Unknown sign-in scheme " + strconv.Quote(p.Scheme))
}

// authenticate handles auth, which is a barrier (§3.2.2): the frames after
// it wait for it, and it runs on every connection, also with an id used
// before (§1.2).
func (s *Server) authenticate(ctx context.Context, c *Conn, r *Request) {
	if c.Closing() {
		return
	}
	var p AuthParams
	if err := json.Unmarshal(r.Params, &p); err != nil || p.Scheme == "" {
		r.fail(InvalidParams("auth needs a scheme"))
		return
	}
	p.Raw = r.Params
	check := checkScheme
	if s.config.checkScheme != nil {
		check = s.config.checkScheme
	}
	if err := check(ctx, c, p); err != nil {
		r.fail(s.protocolError(ctx, "auth", err))
		return
	}
	// Whatever the backend sends while the sign-in runs comes after its
	// result (§3.2.2), and, when it fails, is not sent: it was about a
	// sign-in that did not happen.
	c.hold()
	defer func() { c.release(r.failed()) }()
	// A panic is answered before the frames held meanwhile are let go, as
	// an error is.
	defer s.recoverFrame(r)
	reply := Reply[AuthResult]{r: r, check: s.checkAuth, apply: func(result *AuthResult) {
		if result.You != nil {
			c.SetUser(*result.You)
		}
	}}
	err := s.Backend.Auth(context.WithValue(ctx, requestKey{}, r), c, p, reply)
	s.finish(ctx, r, true, err)
}

// checkAuth checks an auth result: one that signs in has a valid user_id,
// and Extra names no field of its own.
func (s *Server) checkAuth(result *AuthResult) (any, error) {
	if result.You != nil && !ValidID(result.You.UserID) {
		return nil, fmt.Errorf("auth assigned an invalid user_id %q", result.You.UserID)
	}
	for _, name := range []string{"you", "token"} {
		if _, ok := result.Extra[name]; ok {
			return nil, fmt.Errorf("an auth result's Extra names its own field %s", name)
		}
	}
	return *result, nil
}

// meResult is the result of me (§3.3.2).
type meResult struct {
	You CurrentUser `json:"you"`
}
