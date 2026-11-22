package wire

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/apron-chat/apron-server-go/internal/wirecore"
)

// Backend answers for a Server. Its three methods are the core of the
// protocol (§3.6); optional interfaces add the rest ([Profiler],
// [Commander], [Editor], [Disconnecter]), as do methods registered with
// [Server.Handle].
//
// The Server calls a Backend from one goroutine per connection, for one
// frame at a time, so calls for different connections are concurrent.
type Backend interface {
	// Hello returns the server frame for a new connection (§3.1). Server
	// sets Apron and Ping, and adds the capabilities of the optional
	// interfaces the Backend implements.
	Hello(c *Conn) ServerInfo
	// Auth signs a connection in (§3.2). The scheme is one of the frame's
	// Auth or Signup. A result with You set signs the connection in as You;
	// notifications the backend sends to the connection before Auth returns
	// are sent after its result (§3.2.2).
	Auth(ctx context.Context, c *Conn, p AuthParams) (AuthResult, error)
	// Message saves a new message from a signed-in connection (§3.5.1). The
	// body is present, with text or embeds, its format is "", plain, or
	// markdown, and room_id is empty (the default room) or a valid ID. It
	// returns the new message_id; the backend broadcasts the snapshot,
	// including to c, before it returns.
	Message(ctx context.Context, c *Conn, p MessageParams) (MessageResult, error)
}

// Disconnecter is told when a connection ends, signed in or not.
type Disconnecter interface {
	Disconnected(c *Conn)
}

// Profiler handles me (§3.3.2): it changes the user's profile and returns
// the complete current user object, which becomes the connection's.
type Profiler interface {
	Me(ctx context.Context, c *Conn, p MeParams) (User, error)
}

// Commander handles command requests, and adds capability command (§4.1).
// The body is present; p has no message_id and is not deleted. The result is
// nil or extra result fields, such as embeds.
type Commander interface {
	Command(ctx context.Context, c *Conn, p MessageParams) (map[string]any, error)
}

// Editor handles a message request with message_id, an edit or a deletion,
// and adds capability edit (§4.4).
type Editor interface {
	Edit(ctx context.Context, c *Conn, p MessageParams) (MessageResult, error)
}

// HandlerFunc handles a request method registered with Server.Handle. Its
// result is encoded as the result, {} when nil; an error that is an *Error
// is the error reply, and any other is error/internal_error.
type HandlerFunc func(ctx context.Context, c *Conn, params jsontext.Value) (any, error)

// Server serves the protocol for a Backend. Set its fields before it serves
// its first connection; they must not change after.
type Server struct {
	Backend Backend

	// OriginPatterns are the origins, other than the server's own, that may
	// open a WebSocket, as in github.com/coder/websocket's AcceptOptions.
	OriginPatterns []string
	// AllowAnyOrigin accepts a WebSocket from any origin.
	AllowAnyOrigin bool
	// ReadLimit bounds a frame from a client, in bytes; 256 KiB by default
	// (§1).
	ReadLimit int64
	// OutgoingQueue bounds the frames waiting to be written to one
	// connection; a client that lets more pile up is disconnected. 128 by
	// default.
	OutgoingQueue int
	// WriteTimeout bounds writing one frame; PingInterval and PingTimeout
	// pace the transport's pings, and the liveness ping the server frame
	// asks of clients (§1).
	WriteTimeout time.Duration
	PingInterval time.Duration
	PingTimeout  time.Duration
	// MaxConnections bounds the open connections; 0 is unlimited. A
	// connection over it is sent the server frame, then error retry_after
	// (§1.1), and closed.
	MaxConnections int
	// DedupSize is how many request ids are kept for deduplication (§1.2),
	// across all users; 4096 by default, and negative keeps none.
	DedupSize int
	// Logger logs what goes wrong; slog.Default() when nil.
	Logger *slog.Logger
	// ClientAddr returns the address of the client that opened a WebSocket,
	// such as one a trusted reverse proxy names in a header, or nil for none.
	// By default it is the request's RemoteAddr: behind a proxy, every client
	// then has the proxy's address, which a backend that bans by address
	// would ban together.
	ClientAddr func(r *http.Request) net.Addr

	once     sync.Once
	mu       sync.Mutex
	methods  map[string]method
	conns    map[*Conn]struct{}
	closed   bool
	dedup    dedupCache
	serving  sync.WaitGroup
	shutdown chan struct{}
}

type method struct {
	capability string
	readOnly   bool
	handle     HandlerFunc
}

// Handle registers a request method, which a signed-in connection may call.
// A non-empty capability is added to the server frame. A readOnly method's
// results are not kept for deduplication: a retry runs it again.
func (s *Server) Handle(name, capability string, readOnly bool, h HandlerFunc) {
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.methods[name] = method{capability: capability, readOnly: readOnly, handle: h}
}

func (s *Server) init() {
	s.once.Do(func() {
		s.methods = make(map[string]method)
		s.conns = make(map[*Conn]struct{})
		s.shutdown = make(chan struct{})
		size := s.DedupSize
		if size == 0 {
			size = 4096
		}
		s.dedup = dedupCache{Size: size}
	})
}

func (s *Server) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func orDefault[T int | int64 | time.Duration](v, d T) T {
	if v <= 0 {
		return d
	}
	return v
}

func (s *Server) readLimit() int64            { return orDefault(s.ReadLimit, 256<<10) }
func (s *Server) outgoingQueue() int          { return orDefault(s.OutgoingQueue, 128) }
func (s *Server) writeTimeout() time.Duration { return orDefault(s.WriteTimeout, 10*time.Second) }
func (s *Server) pingInterval() time.Duration { return orDefault(s.PingInterval, 30*time.Second) }
func (s *Server) pingTimeout() time.Duration  { return orDefault(s.PingTimeout, 10*time.Second) }

// ServeHTTP accepts a WebSocket and serves it until it closes.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.init()
	options := &websocket.AcceptOptions{OriginPatterns: s.OriginPatterns, InsecureSkipVerify: s.AllowAnyOrigin}
	ws, err := websocket.Accept(w, r, options)
	if err != nil {
		return
	}
	ws.SetReadLimit(s.readLimit())
	var remote net.Addr
	if s.ClientAddr != nil {
		remote = s.ClientAddr(r)
	} else if addr, err := net.ResolveTCPAddr("tcp", r.RemoteAddr); err == nil {
		remote = addr
	}
	s.ServeTransport(r.Context(), wirecore.WebSocket{Conn: ws}, remote, r)
}

// ServeTransport serves a connection on any transport until it closes.
// remote and r describe the client, and may be nil.
func (s *Server) ServeTransport(ctx context.Context, t Transport, remote net.Addr, r *http.Request) {
	s.init()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = t.Close()
		return
	}
	s.serving.Add(1)
	s.mu.Unlock()
	defer s.serving.Done()

	ctx, cancel := context.WithCancel(ctx)
	c := &Conn{
		server:  s,
		remote:  remote,
		request: r,
		ctx:     ctx,
		cancel:  cancel,
	}
	c.rt = wirecore.NewConn(t, wirecore.Options{
		Queue:        s.outgoingQueue(),
		WriteTimeout: s.writeTimeout(),
		PingInterval: s.pingInterval(),
		PingTimeout:  s.pingTimeout(),
		FenceFailed:  internalErrorFrame,
		OnStop:       cancel,
		OnFellBehind: func() {
			s.logger().Warn("wire: closing a connection that fell behind", "remote", remote)
		},
	})
	c.info = s.hello(c)
	c.rt.Start(true)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		c.Close()
		return
	}
	if s.MaxConnections > 0 && len(s.conns) >= s.MaxConnections {
		s.mu.Unlock()
		// An error about the connection as a whole omits id (§1.1).
		if frame, err := Encode("server", c.info); err == nil {
			c.rt.Send(frame)
		}
		c.CloseWithError(RetryAfter("Server at capacity; try again shortly", 30))
		select {
		case <-c.Done():
		case <-s.shutdown:
			c.Close()
		}
		return
	}
	s.conns[c] = struct{}{}
	s.mu.Unlock()
	defer func() {
		c.Close()
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		if d, ok := s.Backend.(Disconnecter); ok {
			d.Disconnected(c)
		}
	}()

	// The server frame goes first, before any frame is read (§3.1).
	if frame, err := Encode("server", c.info); err == nil {
		c.rt.Send(frame)
	} else {
		s.logger().Error("wire: encoding the server frame", "error", err)
		return
	}

	// Frames are processed in order, each to completion before the next,
	// which makes auth a barrier (§3.2.2), by a worker of the connection's
	// own; the reader goes on reading meanwhile, which keeps the
	// transport's pings answered while the backend works.
	c.rt.Serve(func() ([]byte, error) { return t.Read(ctx) }, func(payload []byte) { s.processFrame(c, payload) }, nil)
}

// internalErrorFrame closes a connection whose fence failed.
var internalErrorFrame, _ = marshal(map[string]any{"error": internalError()})

func (s *Server) hello(c *Conn) ServerInfo {
	info := s.Backend.Hello(c)
	info.Apron = Version
	info.Ping = max(1, int(s.pingInterval()/time.Second))
	add := func(capability string) {
		if capability != "" && !slices.Contains(info.Capabilities, capability) {
			info.Capabilities = append(info.Capabilities, capability)
		}
	}
	if _, ok := s.Backend.(Commander); ok {
		add("command")
	}
	if _, ok := s.Backend.(Editor); ok {
		add("edit")
	}
	if _, ok := s.Backend.(Rooms); ok {
		add("rooms")
	}
	if _, ok := s.Backend.(Historian); ok {
		add("history")
	}
	if _, ok := s.Backend.(Statuser); ok {
		add("status")
	} else {
		info.Status = nil // Only servers with capability status send it (§4.5.1).
	}
	s.mu.Lock()
	for _, m := range s.methods {
		add(m.capability)
	}
	s.mu.Unlock()
	return info
}

// Conns returns the open connections, such as to broadcast to them.
func (s *Server) Conns() []*Conn {
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	conns := make([]*Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	return conns
}

// Broadcast sends a frame to every signed-in connection for which to
// returns true, or every one when to is nil.
func (s *Server) Broadcast(frame Frame, to func(*Conn) bool) {
	for _, c := range s.Conns() {
		if c.UserID() != "" && (to == nil || to(c)) {
			c.Send(frame)
		}
	}
}

// Shutdown stops accepting connections, closes the open ones, and waits for
// them to finish, or for ctx.
func (s *Server) Shutdown(ctx context.Context) error {
	s.init()
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.shutdown)
	}
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		s.serving.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// request is a parsed frame.
type request = wirecore.Request

func (s *Server) processFrame(c *Conn, payload []byte) {
	req, failed := wirecore.ParseRequest(payload)
	if failed != nil {
		perr := &Error{Code: failed.Code, Message: failed.Message}
		switch {
		case wirecore.NotificationOnly[req.Method]:
			// A notification is never answered, and a ping with other
			// spacing or keys is still a ping (§1.1).
			if req.Method == "ping" {
				c.pong()
			}
		case req.HasID:
			s.reply(c, req, nil, perr)
		case !failed.InvalidParams():
			s.reply(c, req, nil, perr)
		}
		return
	}
	if req.Method == "ping" {
		c.pong()
		return
	}
	// Unknown notifications, and request methods sent without an id, are
	// ignored (§1.1).
	if !req.HasID {
		return
	}
	if req.Method == "auth" {
		s.authenticate(c, req)
		return
	}
	userID := c.UserID()
	if userID == "" {
		s.reply(c, req, nil, Denied("Sign in before sending requests"))
		return
	}
	if len(req.ID) > wirecore.MaxRequestIDBytes {
		s.reply(c, req, nil, TooLarge(fmt.Sprintf("A request id is at most %d bytes", wirecore.MaxRequestIDBytes)))
		return
	}
	handle, readOnly := s.handler(req.Method)
	if handle == nil {
		s.reply(c, req, nil, Unsupported("Unsupported method "+strconv.Quote(req.Method)))
		return
	}

	// Deduplicate by user_id and id (§1.2): a duplicate waits for the
	// original, even on another connection, and gets its outcome.
	key := dedupKey{userID: userID, id: req.ID}
	fingerprint := wirecore.Fingerprint(req.Method, req.Params)
	s.mu.Lock()
	entry, prior := s.dedup.Claim(key, fingerprint)
	s.mu.Unlock()
	if prior != nil {
		if prior.Fingerprint != fingerprint {
			s.reply(c, req, nil, InvalidParams(fmt.Sprintf("Request id %q was already used for another operation", req.ID)))
			return
		}
		<-prior.Done()
		s.reply(c, req, prior.Value.result, prior.Value.err)
		return
	}

	result, err := handle(c.ctx, c, req.Params)
	var rerr *Error
	if err != nil {
		var ok bool
		if rerr, ok = asError(err); !ok {
			// A request cut short by its connection's end is not the
			// backend's failure.
			if c.ctx.Err() == nil {
				s.logger().Error("wire: request failed", "method", req.Method, "error", err)
			}
			rerr = internalError()
		}
	}
	encoded := s.reply(c, req, result, rerr)
	if entry != nil {
		s.mu.Lock()
		s.dedup.Finish(key, entry, dedupOutcome{encoded, rerr}, rerr != nil || readOnly)
		s.mu.Unlock()
	}
}

// reply queues the reply to req, and returns its encoded result.
func (s *Server) reply(c *Conn, req request, result any, e *Error) jsontext.Value {
	var encoded jsontext.Value
	if e == nil {
		switch r := result.(type) {
		case jsontext.Value:
			encoded = r
		case nil:
			encoded = jsontext.Value("{}")
		default:
			raw, err := marshal(r)
			if err == nil && string(raw) == "null" {
				// A typed nil, such as a nil map: nothing to say.
				raw = []byte("{}")
			}
			if err != nil || !jsontext.Value(raw).IsValid() || raw[0] != '{' {
				s.logger().Error("wire: a result is not a JSON object", "method", req.Method, "error", err)
				e = internalError()
			} else {
				encoded = raw
			}
		}
	}
	var frame map[string]any
	if e != nil {
		frame = map[string]any{"error": e}
	} else {
		frame = map[string]any{"result": encoded}
	}
	if req.HasID {
		frame["id"] = req.ID
	}
	payload, err := marshal(frame)
	if err != nil {
		c.Close()
		return nil
	}
	c.rt.Send(payload)
	return encoded
}

func (s *Server) handler(name string) (HandlerFunc, bool) {
	switch name {
	case "message":
		return s.message, false
	case "me":
		if p, ok := s.Backend.(Profiler); ok {
			return func(ctx context.Context, c *Conn, raw jsontext.Value) (any, error) {
				var params MeParams
				if err := json.Unmarshal(raw, &params); err != nil {
					return nil, InvalidParams("Invalid profile")
				}
				params.Raw = raw
				if params.Status != nil && *params.Status != StatusOnline && *params.Status != "" && !slices.Contains(c.info.Status, *params.Status) {
					return nil, InvalidParams("This server does not offer status " + strconv.Quote(*params.Status))
				}
				you, err := p.Me(ctx, c, params)
				if err != nil {
					return nil, err
				}
				c.SetUser(you)
				return map[string]any{"you": you}, nil
			}, false
		}
	case "command":
		if cmd, ok := s.Backend.(Commander); ok {
			return func(ctx context.Context, c *Conn, raw jsontext.Value) (any, error) {
				p, err := parseMessage(raw)
				if err != nil {
					return nil, err
				}
				if p.MessageID != "" || p.Deleted {
					return nil, InvalidParams("A command is never saved: it has no message_id")
				}
				if p.Body == nil {
					return nil, InvalidParams("A command needs a body")
				}
				return cmd.Command(ctx, c, p)
			}, false
		}
	case "room_list", "room_join", "room_leave", "room_set":
		if rooms, ok := s.Backend.(Rooms); ok {
			return s.roomHandler(rooms, name), name == "room_list"
		}
	case "history":
		if h, ok := s.Backend.(Historian); ok {
			return s.historyHandler(h), true
		}
	case "status":
		if st, ok := s.Backend.(Statuser); ok {
			return s.statusHandler(st), false
		}
	}
	s.mu.Lock()
	m, ok := s.methods[name]
	s.mu.Unlock()
	if !ok {
		return nil, false
	}
	return m.handle, m.readOnly
}

func parseMessage(raw jsontext.Value) (MessageParams, error) {
	var p MessageParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, InvalidParams("Invalid message: " + err.Error())
	}
	p.Raw = raw
	if p.RoomID != "" && !ValidID(p.RoomID) {
		return p, InvalidParams("Invalid room_id")
	}
	if p.Body != nil && p.Body.Format != "" && p.Body.Format != Plain && p.Body.Format != Markdown {
		return p, InvalidParams(`format is "plain" or "markdown"`)
	}
	if p.ReplyTo != nil && p.ReplyTo.MessageID == "" {
		return p, InvalidParams("reply_to needs a message_id")
	}
	return p, nil
}

// message applies the core rules of a message request (§3.5.1, §3.6), and
// passes it on to the backend.
func (s *Server) message(ctx context.Context, c *Conn, raw jsontext.Value) (any, error) {
	p, err := parseMessage(raw)
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
		return nil, InvalidParams("A new message needs a body")
	}
	if p.Body.Text == "" && len(p.Body.Embeds) == 0 {
		// An empty message is not logged or broadcast (§3.5.1).
		return nil, nil
	}
	return s.Backend.Message(ctx, c, p)
}

func (s *Server) authenticate(c *Conn, req request) {
	var p AuthParams
	if err := json.Unmarshal(req.Params, &p); err != nil || p.Scheme == "" {
		s.reply(c, req, nil, InvalidParams("auth needs a scheme"))
		return
	}
	p.Raw = req.Params
	if !slices.Contains(c.info.Auth, p.Scheme) && !slices.Contains(c.info.Signup, p.Scheme) {
		switch p.Scheme {
		case "guest", "token", "webauthn", "email", "external":
			s.reply(c, req, nil, Unsupported("This server does not offer sign-in with "+p.Scheme))
		default:
			// Even a scheme PROTOCOL.md does not define (§3.2).
			s.reply(c, req, nil, Unsupported("Unknown sign-in scheme "+strconv.Quote(p.Scheme)))
		}
		return
	}
	// Whatever the backend sends while the sign-in runs comes after its
	// result (§3.2.2).
	c.hold()
	defer c.release()
	result, err := s.Backend.Auth(c.ctx, c, p)
	if err != nil {
		e, ok := asError(err)
		if !ok {
			s.logger().Error("wire: auth failed", "scheme", p.Scheme, "error", err)
			e = internalError()
		}
		s.reply(c, req, nil, e)
		return
	}
	if result.You != nil && !ValidID(result.You.UserID) {
		s.logger().Error("wire: auth assigned an invalid user_id", "user_id", result.You.UserID)
		s.reply(c, req, nil, internalError())
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
	s.reply(c, req, fields, nil)
}
