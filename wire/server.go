package wire

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/apron-chat/apron-server-go/wire/internal/wirecore"
)

// Server serves the protocol for a Backend, and the handlers of its fields.
// It is configured by setting its fields, in any order, and registering
// methods (Handle, HandleNotification), before it serves its first
// connection or opens its first Local one: then it takes its
// configuration, and its fields must not change after.
type Server struct {
	// Backend says who the server is, and who each connection is.
	Backend Backend

	// The handlers of the protocol's requests: each that is set adds its
	// requests, with their params checked, and its capability in the server
	// frame (§3.1). Messages and Profiles are the core's, which every
	// server needs, and the Server warns as it starts serving without
	// either; the others are capabilities. Messages and Edits share the
	// message request.
	Messages  MessageHandler  // message (§3.5.1), core
	Profiles  ProfileHandler  // me (§3.3.2), core
	Edits     EditHandler     // message with message_id, capability edit (§4.4)
	Commands  CommandHandler  // command, capability command (§4.1)
	Rooms     RoomHandler     // room_*, capability rooms (§4.2)
	History   HistoryHandler  // history, capability history (§4.3)
	Status    StatusHandler   // status, capability status (§4.5)
	Reactions ReactionHandler // reactions, capability reactions (§4.7)
	Activity  ActivityHandler // activity, capability activity (§4.6)

	// OnConnect, if set, is told of a new connection before its server
	// frame is made, with ctx the connection's context, such as to keep
	// the backend's own state for it (Conn.SetData). A Local connection
	// (Conn.Local) is signed in already, as the user its caller asked for,
	// and has its LocalOptions.Data; OnConnect may make it another user
	// (Conn.SetUser), such as one with a user_id of the backend's own,
	// which the Server tells the connection of (Server.Local). An error
	// closes the connection: a network client is sent an *Error first,
	// after the server frame, and Server.Local returns it. OnDisconnect
	// follows, even then.
	OnConnect func(ctx context.Context, c *Conn) error
	// OnDisconnect, if set, is told once when a connection ends, signed in
	// or not, network or Local: every connection the Server told OnConnect
	// of, or would have had it been set, even one OnConnect refused, so
	// that it undoes what OnConnect did, all of it or part, in one place.
	// The only connections it is not told of are those refused before
	// that, at capacity (MaxConnections) or as the Server shuts down, of
	// which the backend heard nothing but Hello. For a Local connection,
	// it has returned when Server.Local returns OnConnect's error.
	OnDisconnect func(c *Conn)
	// RefreshDuplicate, if set, brings the result of a request up to date
	// for a duplicate of it (§1.2), which gets the original's outcome as
	// the state is now, such as the user's current profile for a repeated
	// me. It returns result as it was, or the result as the method's
	// handler sends it: for me a CurrentUser, which the Server sends as
	// me's result, {"you": ...}, as Reply.Send does; any other value for me
	// is a bug, which the Server logs, and the duplicate gets the original.
	// Without it, a duplicate gets the original result as it was.
	RefreshDuplicate func(ctx context.Context, c *Conn, method string, result jsontext.Value) any
	// CheckScheme, if set, decides which auth schemes a connection may use,
	// such as one the server frame does not list for everyone. Without it,
	// the schemes are those of the frame's Auth and Signup: any other
	// scheme is unsupported, even one PROTOCOL.md does not define (§3.2).
	CheckScheme func(ctx context.Context, c *Conn, p AuthParams) error
	// CheckOrigin decides whether a WebSocket may open, by the request that
	// opens it, such as by its Origin header, as OriginPatterns does; nil
	// accepts only the server's own origin, and requests without one.
	CheckOrigin func(r *http.Request) bool

	// ReadLimit bounds a frame from a client, in bytes; 256 KiB by default
	// (§1).
	ReadLimit int64
	// OutgoingQueue bounds the batches waiting to be written to one
	// connection; a client that lets more pile up is disconnected. 128 by
	// default.
	OutgoingQueue int
	// WriteTimeout bounds writing one frame; PingInterval and PingTimeout
	// pace the transport's pings, and the liveness ping the server frame
	// asks of clients (§1).
	WriteTimeout time.Duration
	PingInterval time.Duration
	PingTimeout  time.Duration
	// MaxConnections bounds the open network connections, not Local ones;
	// 0 is unlimited. A connection over it is refused before the Backend
	// hears of it, but for its server frame (Hello, without OnConnect or
	// OnDisconnect): it is sent that, then error retry_after (§1.1), and
	// closed.
	MaxConnections int
	// DedupSize is how many request ids are kept for deduplication (§1.2)
	// for each user, of the latest DedupUsers users; 1024 and 4096 by
	// default. A negative DedupSize keeps none.
	DedupSize  int
	DedupUsers int
	// Logger logs what goes wrong; slog.Default() when nil.
	Logger *slog.Logger
	// ClientAddr returns the address of the client that opened a WebSocket,
	// such as one a trusted reverse proxy names in a header, or nil for none.
	// By default it is the request's RemoteAddr: behind a proxy, every client
	// then has the proxy's address, which a backend that bans by address
	// would ban together.
	ClientAddr func(r *http.Request) net.Addr
	// ShutdownError, if set, is sent to each open connection on Shutdown,
	// after the frames already queued, such as error/retry_after to have
	// clients come back shortly; without it, connections close at once.
	ShutdownError *Error
	// FenceFailed is sent in place of the frames behind a fence that failed
	// (Conn.Fence), before the connection closes; error/internal_error by
	// default.
	FenceFailed *Error

	once       sync.Once
	freezeOnce sync.Once
	checkOnce  sync.Once
	mu         sync.Mutex
	// registered are the methods registered with Handle and
	// HandleNotification, and methods, once the Server took its
	// configuration, every method it serves.
	registered map[string]method
	methods    map[string]method
	config     config
	conns      map[*Conn]struct{}
	network    int
	closed     bool
	dedup      *dedup
	serving    sync.WaitGroup
	shutdown   chan struct{}
}

// config is what the Server took of its hooks as it started serving.
type config struct {
	onConnect        func(ctx context.Context, c *Conn) error
	onDisconnect     func(c *Conn)
	refreshDuplicate func(ctx context.Context, c *Conn, method string, result jsontext.Value) any
	checkScheme      func(ctx context.Context, c *Conn, p AuthParams) error
	checkOrigin      func(r *http.Request) bool
}

// reservedMethods are handled by the Server itself.
var reservedMethods = map[string]string{
	"auth": "auth is the Backend's",
	"ping": "ping is answered by the Server",
}

// Handle registers a request method, such as an extension's own, which
// takes the place of a handler field's for the same name: Handle("message",
// h) handles every message request, edits included, so the server frame
// lists capability edit only when h.Capability says so. It panics for a
// name the Server handles itself (auth, ping), a notification (activity;
// see HandleNotification), a name registered already, or a Handler without
// Serve. A method registered once the Server serves is served from then on,
// and its capability is in the server frames sent after.
func (s *Server) Handle(name string, h Handler) {
	if h.Serve == nil {
		panic("wire: Handle " + strconv.Quote(name) + " without Serve")
	}
	if wirecore.NotificationOnly[name] {
		panic("wire: Handle " + strconv.Quote(name) + ": it is a notification; use HandleNotification")
	}
	s.register(name, method{capability: h.Capability, noDedup: h.NoDedup, serve: h.Serve})
}

// HandleNotification registers a notification method a client sends, which
// takes the place of a handler field's for the same name, as Handle does a
// request method. It panics for a name the Server handles itself (auth,
// ping), a name registered already, or a NotificationHandler without Serve.
// Local.Call refuses a notification method with ErrNotRequest.
func (s *Server) HandleNotification(name string, h NotificationHandler) {
	if h.Serve == nil {
		panic("wire: HandleNotification " + strconv.Quote(name) + " without Serve")
	}
	s.register(name, method{capability: h.Capability, notify: h.Serve})
}

func (s *Server) register(name string, m method) {
	if why, reserved := reservedMethods[name]; reserved {
		panic("wire: cannot register " + name + ": " + why)
	}
	if name == "" {
		panic("wire: cannot register a method without a name")
	}
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, twice := s.registered[name]; twice {
		panic("wire: method " + strconv.Quote(name) + " registered twice")
	}
	s.registered[name] = m
	if s.methods != nil {
		s.methods[name] = m
	}
}

// init makes what the Server needs to hold connections and registrations,
// which takes nothing of its configuration.
func (s *Server) init() {
	s.once.Do(func() {
		s.registered = make(map[string]method)
		s.conns = make(map[*Conn]struct{})
		s.shutdown = make(chan struct{})
	})
}

// freeze takes the Server's configuration, once, as it starts serving: the
// methods of its handler fields, with those registered in their place, and
// its hooks.
func (s *Server) freeze() {
	s.init()
	s.freezeOnce.Do(func() {
		builtins := s.builtins()
		perUser := s.DedupSize
		if perUser == 0 {
			perUser = 1024
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		for name, m := range s.registered {
			builtins[name] = m
		}
		s.methods = builtins
		s.config = config{
			onConnect:        s.OnConnect,
			onDisconnect:     s.OnDisconnect,
			refreshDuplicate: s.RefreshDuplicate,
			checkScheme:      s.CheckScheme,
			checkOrigin:      s.CheckOrigin,
		}
		s.dedup = newDedup(perUser, orDefault(s.DedupUsers, 4096))
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

func (s *Server) fenceFailed() []byte {
	e := s.FenceFailed
	if e == nil {
		e = internalError()
	}
	frame, _ := marshal(map[string]any{"error": e})
	return frame
}

// ServeHTTP accepts a WebSocket and serves it until it closes.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.freeze()
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
		return
	}
	options := &websocket.AcceptOptions{}
	if check := s.config.checkOrigin; check != nil {
		if !check(r) {
			http.Error(w, fmt.Sprintf("request Origin %q is not authorized for Host %q", r.Header.Get("Origin"), r.Host), http.StatusForbidden)
			return
		}
		options.InsecureSkipVerify = true
	}
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
	s.freeze()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = t.Close()
		return
	}
	s.serving.Add(1)
	s.mu.Unlock()
	defer s.serving.Done()

	s.checkOnce.Do(func() {
		s.logger().Info("wire: serving", "capabilities", s.capabilities())
		if _, ok := s.method("message"); !ok {
			s.logger().Warn("wire: the server takes no messages, which every server should (§3.6): set Server.Messages, or Handle message")
		} else if _, ok := s.method("me"); !ok {
			s.logger().Warn("wire: the server answers no me, which clients count on to change a user's profile (§3.3.2): set Server.Profiles, or Handle me")
		}
	})
	ctx, cancel := context.WithCancel(ctx)
	c := s.newConn(ctx, cancel, t, s.outgoingQueue(), nil)
	c.remote, c.request = remote, r
	c.rt.Start(true)

	// Capacity is checked first, so that a connection refused for it costs
	// the backend nothing but its server frame, which every connection is
	// sent first (§3.1).
	s.mu.Lock()
	switch {
	case s.closed:
		s.mu.Unlock()
		c.Close()
		return
	case s.MaxConnections > 0 && s.network >= s.MaxConnections:
		s.mu.Unlock()
		c.info = s.complete(s.Backend.Hello(ctx, c))
		if frame, err := Encode(c.info); err == nil {
			c.rt.Send(frame)
		}
		// An error about the connection as a whole omits id (§1.1).
		c.CloseWithError(RetryAfter("Server at capacity; try again shortly", 30))
		s.waitClosed(c)
		return
	}
	s.network++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.network--
		s.mu.Unlock()
	}()

	if connect := s.config.onConnect; connect != nil {
		if err := connect(ctx, c); err != nil {
			if e, ok := asError(err); ok {
				c.info = s.complete(s.Backend.Hello(ctx, c))
				if frame, err := Encode(c.info); err == nil {
					c.rt.Send(frame)
				}
				c.CloseWithError(e)
				s.waitClosed(c)
			} else {
				c.Close()
			}
			s.disconnected(c)
			return
		}
	}
	c.info = s.complete(s.Backend.Hello(ctx, c))
	frame, err := Encode(c.info)
	if err != nil {
		s.logger().Error("wire: encoding the server frame", "error", err)
		c.Close()
		s.disconnected(c)
		return
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		c.Close()
		s.disconnected(c)
		return
	}
	s.conns[c] = struct{}{}
	s.mu.Unlock()
	defer func() {
		c.Close()
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		s.disconnected(c)
	}()

	// The server frame goes first, before any frame is read (§3.1).
	c.rt.Send(frame)
	// Frames are processed in order, each to completion before the next,
	// which makes auth a barrier (§3.2.2), by a worker of the connection's
	// own; the reader goes on reading meanwhile, which keeps the
	// transport's pings answered while the backend works. Frames read
	// before the client closed the connection are still processed, so that
	// a request sent just before, such as a sign-out's, takes effect.
	c.rt.Serve(func() ([]byte, error) { return t.Read(ctx) }, func(payload []byte) { s.processFrame(ctx, c, payload) }, cancel)
}

func (s *Server) newConn(ctx context.Context, cancel context.CancelFunc, w wirecore.Writer, queue int, fellBehind func()) *Conn {
	c := &Conn{server: s, ctx: ctx, cancel: cancel}
	c.rt = wirecore.NewConn(w, wirecore.Options{
		Queue:        queue,
		WriteTimeout: s.writeTimeout(),
		PingInterval: s.pingInterval(),
		PingTimeout:  s.pingTimeout(),
		FenceFailed:  s.fenceFailed(),
		OnStop:       cancel,
		OnFellBehind: func() {
			if fellBehind != nil {
				fellBehind()
				return
			}
			s.logger().Warn("wire: closing a connection that fell behind", "remote", c.remote)
		},
	})
	return c
}

// waitClosed waits for a connection that was sent its last frame to close.
func (s *Server) waitClosed(c *Conn) {
	select {
	case <-c.Done():
	case <-s.shutdown:
		c.Close()
	}
}

func (s *Server) disconnected(c *Conn) {
	if disconnect := s.config.onDisconnect; disconnect != nil {
		disconnect(c)
	}
}

// complete fills in what Server owns of a server frame: the protocol
// version, the ping interval, and the capabilities of the methods it
// handles.
func (s *Server) complete(info ServerInfo) ServerInfo {
	info.Apron = Version
	info.Ping = max(1, int(s.pingInterval()/time.Second))
	info.Capabilities = slices.Clone(info.Capabilities)
	for _, capability := range s.capabilities() {
		if !slices.Contains(info.Capabilities, capability) {
			info.Capabilities = append(info.Capabilities, capability)
		}
	}
	if !slices.Contains(info.Capabilities, "status") {
		info.Status = nil // Only servers with capability status send it (§4.5.1).
	}
	return info
}

// capabilities are those of the methods the Server handles, by the methods'
// names.
func (s *Server) capabilities() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var capabilities []string
	for _, name := range slices.Sorted(maps.Keys(s.methods)) {
		if capability := s.methods[name].capability; capability != "" && !slices.Contains(capabilities, capability) {
			capabilities = append(capabilities, capability)
		}
	}
	return capabilities
}

// Conns returns the open connections, Local ones included, such as to
// broadcast to them.
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
// returns true, or every one when to is nil. Like Conn.Send, it never
// blocks, and is meant to be called while the backend holds the lock of
// the change the frame describes, so that every connection gets such
// frames in the order of the changes. to is called in the caller's
// goroutine, so it may read what that lock guards.
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
	conns := make([]*Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		if s.ShutdownError != nil {
			c.CloseWithError(s.ShutdownError)
		} else {
			c.Close()
		}
	}
	done := make(chan struct{})
	go func() {
		s.serving.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		for _, c := range conns {
			c.Close()
		}
		return ctx.Err()
	}
}
