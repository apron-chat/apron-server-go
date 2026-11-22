package wire

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"runtime/debug"
	"slices"
	"strconv"
	"sync"

	"github.com/apron-chat/apron-server-go/internal/wirecore"
)

// Method handles one request method, or one notification a client sends,
// such as activity.
type Method struct {
	// Capability, if set, is listed in the server frame.
	Capability string
	// ReadOnly results are not kept for deduplication (§1.2): a retry runs
	// the request again.
	ReadOnly bool
	// Notification methods are handled with or without an id, and never
	// answered (§1.1): Handle's result and error are dropped.
	Notification bool
	// Handle handles a request from a signed-in connection. Its result is
	// the reply, {} when nil, unless it replied itself (Request.Reply); an
	// error that is an *Error is the error reply, and any other is
	// error/internal_error. The request is also RequestFrom(ctx).
	Handle func(ctx context.Context, c *Conn, r *Request) (any, error)
}

// Request is the request a method or a Backend method handles, which its
// context carries (RequestFrom).
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

// RequestFrom returns the request a context was made for, or nil.
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

// Reply sends the request's result now, followed by frames with nothing
// between them, rather than once its handler returns. A backend that keeps
// its state under a lock replies while it holds it, so that the result
// reflects every notification sent before it on the connection, and none
// after (§1). It reports false for a second reply, or a closed connection.
// A notification gets no reply, but its frames are sent.
func (r *Request) Reply(result any, then ...Frame) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.notification {
		return r.c.SendBatch(then...)
	}
	if r.replied {
		return false
	}
	frame, encoded, e := r.c.server.encodeReply(r, result, nil)
	r.replied = true
	r.outcome = dedupOutcome{encoded, e}
	if frame == nil {
		r.c.Close()
		return false
	}
	// A reply is never held back by auth (§3.2.2): what is held is what
	// comes after it.
	return r.c.enqueue(append([]Frame{frame}, then...))
}

// Replied reports whether the request has its reply.
func (r *Request) Replied() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.replied
}

// fail sends an error reply, unless the request has its reply.
func (r *Request) fail(e *Error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.replied || r.notification {
		return
	}
	r.replied = true
	r.outcome = dedupOutcome{err: e}
	if frame, _, _ := r.c.server.encodeReply(r, nil, e); frame != nil {
		r.c.enqueue([]Frame{frame})
	}
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
	case ctx.Err() != nil:
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
	if found && m.Notification {
		// The id of a notification method may be ignored, and the frame
		// handled as a notification (§1.1); before sign-in, it is ignored
		// (§3.2.2).
		r.notification = true
		if c.UserID() != "" && !c.Closing() {
			_, _ = m.Handle(context.WithValue(ctx, requestKey{}, r), c, r)
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
		if refresher, ok := s.Backend.(Refresher); ok {
			result = refresher.Refresh(c, r.Method, outcome.result)
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
		s.dedup.finish(userID, r.ID, entry, outcome, outcome.err != nil || m.ReadOnly)
	}
}

// serve runs a request's handler, and sends its reply: its result, or its
// error. A panic in either is the request's error/internal_error, so that
// the request has its outcome for deduplication all the same.
func (s *Server) serve(ctx context.Context, c *Conn, r *Request, m Method) {
	defer s.recoverFrame(r)
	result, err := m.Handle(context.WithValue(ctx, requestKey{}, r), c, r)
	switch {
	case err != nil && r.Replied():
		s.afterReply(ctx, r.Method, err)
	case err != nil:
		r.fail(s.protocolError(ctx, r.Method, err))
	default:
		r.Reply(result)
	}
}

// method returns the handler of a method: one registered with Handle, or
// one of the Backend's optional interfaces.
func (s *Server) method(name string) (Method, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.methods[name]
	return m, ok
}

// builtins are the methods of the Backend's optional interfaces.
func (s *Server) builtins() map[string]Method {
	methods := map[string]Method{}
	_, messenger := s.Backend.(Messenger)
	_, editor := s.Backend.(Editor)
	switch {
	case editor:
		methods["message"] = Method{Capability: "edit", Handle: s.message}
	case messenger:
		methods["message"] = Method{Handle: s.message}
	}
	if p, ok := s.Backend.(Profiler); ok {
		methods["me"] = Method{Handle: func(ctx context.Context, c *Conn, r *Request) (any, error) {
			params, perr := parseMe(r)
			if perr != nil {
				return nil, perr
			}
			you, err := p.Me(ctx, c, params)
			if err != nil || r.Replied() {
				return nil, err
			}
			c.SetUser(you)
			return map[string]any{"you": you}, nil
		}}
	}
	if cmd, ok := s.Backend.(Commander); ok {
		methods["command"] = Method{Capability: "command", Handle: func(ctx context.Context, c *Conn, r *Request) (any, error) {
			for _, name := range []string{"message_id", "deleted"} {
				if r.Has(name) {
					return nil, InvalidParams("A command has no " + name + "; send it as a message instead")
				}
			}
			p, err := parseMessage(r)
			if err != nil {
				return nil, err
			}
			if p.Body == nil {
				return nil, InvalidParams("A command needs a body")
			}
			return cmd.Command(ctx, c, p)
		}}
	}
	if rooms, ok := s.Backend.(Rooms); ok {
		for _, name := range []string{"room_list", "room_join", "room_leave", "room_set"} {
			methods[name] = Method{Capability: "rooms", ReadOnly: name == "room_list", Handle: roomHandler(rooms, name)}
		}
	}
	if h, ok := s.Backend.(Historian); ok {
		methods["history"] = Method{Capability: "history", ReadOnly: true, Handle: historyHandler(h)}
	}
	if st, ok := s.Backend.(Statuser); ok {
		methods["status"] = Method{Capability: "status", Handle: statusHandler(st)}
	}
	if reactor, ok := s.Backend.(Reactor); ok {
		methods["reactions"] = Method{Capability: "reactions", Handle: reactionsHandler(reactor)}
	}
	if a, ok := s.Backend.(Activity); ok {
		methods["activity"] = Method{Capability: "activity", Notification: true, Handle: activityHandler(a)}
	}
	return methods
}

// parseMessage reads the params of a message or command (§3.5): what each
// field is, as the protocol defines it. Limits, such as on the count of
// mentions, are the backend's.
func parseMessage(r *Request) (p MessageParams, err error) {
	if p.RoomID, _, err = r.IDParam("room_id"); err != nil {
		return p, err
	}
	if id, present, err := r.String("message_id"); err != nil {
		return p, err
	} else if present {
		if !ValidMessageID(id) {
			return p, InvalidParams("message_id must be a positive decimal string")
		}
		p.MessageID = id
	}
	if replyTo, present, err := r.MessageRef("reply_to"); err != nil {
		return p, err
	} else if present {
		p.ReplyTo = &Ref{MessageID: replyTo}
	}
	if p.Deleted, _, err = r.Bool("deleted"); err != nil {
		return p, err
	}
	if p.Ext, _, err = r.Ext("ext"); err != nil {
		return p, err
	}
	if p.Deleted {
		// A deletion leaves no body (§4.4): what it sends is ignored.
		return p, nil
	}
	fields, present, err := r.Object("body")
	if err != nil || !present {
		return p, err
	}
	p.BodyRaw, _ = r.Param("body")
	body, err := parseBody(fields)
	p.Body = body
	return p, err
}

// parseBody reads a message body's fields (§3.5).
func parseBody(fields Fields) (*Body, error) {
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
	return body, nil
}

// message applies the core rules of a message request (§3.5.1, §3.6), and
// passes it on to the backend.
func (s *Server) message(ctx context.Context, c *Conn, r *Request) (any, error) {
	p, err := parseMessage(r)
	if err != nil {
		return nil, err
	}
	if p.MessageID != "" || p.Deleted {
		editor, ok := s.Backend.(Editor)
		if !ok {
			return nil, Unsupported("This server does not edit messages")
		}
		return editor.Edit(ctx, c, p)
	}
	if p.Body == nil {
		return nil, Missing("body")
	}
	messenger, ok := s.Backend.(Messenger)
	if !ok {
		return nil, Unsupported("This server takes no new messages")
	}
	return messenger.Message(ctx, c, p)
}

// definedSchemes are the auth schemes PROTOCOL.md defines.
var definedSchemes = []string{"guest", "token", "webauthn", "email", "external"}

// checkScheme is the default SchemeChecker: the schemes of the server frame.
func checkScheme(c *Conn, p AuthParams) error {
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
	if checker, ok := s.Backend.(SchemeChecker); ok {
		check = checker.CheckScheme
	}
	if err := check(c, p); err != nil {
		r.fail(s.protocolError(ctx, "auth", err))
		return
	}
	// Whatever the backend sends while the sign-in runs comes after its
	// result (§3.2.2).
	c.hold()
	defer c.release()
	// A panic is answered before the frames held meanwhile are sent, as an
	// error is.
	defer s.recoverFrame(r)
	result, err := s.Backend.Auth(context.WithValue(ctx, requestKey{}, r), c, p)
	switch {
	case r.Replied():
		if err != nil {
			s.afterReply(ctx, "auth", err)
		}
		return
	case err != nil:
		r.fail(s.protocolError(ctx, "auth", err))
		return
	case result.You != nil && !ValidID(result.You.UserID):
		s.logger().Error("wire: auth assigned an invalid user_id", "user_id", result.You.UserID)
		r.fail(internalError())
		return
	}
	fields := make(map[string]any, len(result.Extra)+2)
	for k, v := range result.Extra {
		fields[k] = v
	}
	if result.You != nil {
		fields["you"] = result.You
		c.SetUser(*result.You)
	}
	if result.Token != "" {
		fields["token"] = result.Token
	}
	r.Reply(fields)
}
