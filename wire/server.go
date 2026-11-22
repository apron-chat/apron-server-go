package wire

import (
	"context"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/apron-chat/apron-server-go/internal/wirecore"
)

// Server serves the protocol for a Backend. Set its fields before it serves
// its first connection, or registers its first method; they must not change
// after.
type Server struct {
	Backend Backend

	// OriginPatterns are the origins, other than the server's own, that may
	// open a WebSocket, as in github.com/coder/websocket's AcceptOptions.
	OriginPatterns []string
	// AllowAnyOrigin accepts a WebSocket from any origin, and AllowOrigin,
	// if set, from the origins it accepts, such as native apps'.
	AllowAnyOrigin bool
	AllowOrigin    func(origin string) bool
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
	// hears of it, but for its server frame (Hello, without Connecter or
	// Disconnecter): it is sent that, then error retry_after (§1.1), and
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

	once      sync.Once
	checkOnce sync.Once
	mu        sync.Mutex
	methods   map[string]Method
	conns     map[*Conn]struct{}
	network   int
	closed    bool
	dedup     *dedup
	serving   sync.WaitGroup
	shutdown  chan struct{}
}

// Handle registers a method, which takes the place of the Backend's for the
// same name, if any.
func (s *Server) Handle(name string, m Method) {
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.methods[name] = m
}

func (s *Server) init() {
	s.once.Do(func() {
		s.methods = s.builtins()
		s.conns = make(map[*Conn]struct{})
		s.shutdown = make(chan struct{})
		perUser := s.DedupSize
		if perUser == 0 {
			perUser = 1024
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
	s.init()
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
		return
	}
	options := &websocket.AcceptOptions{OriginPatterns: s.OriginPatterns}
	if s.AllowAnyOrigin || s.AllowOrigin != nil && s.AllowOrigin(r.Header.Get("Origin")) {
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

	s.checkOnce.Do(func() {
		if _, ok := s.method("message"); !ok {
			s.logger().Warn("wire: the backend takes no messages, which every server should (§3.6): implement Messenger, or Handle message")
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
		c.info = s.complete(s.Backend.Hello(c))
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

	if connecter, ok := s.Backend.(Connecter); ok {
		if err := connecter.Connected(c); err != nil {
			if e, ok := asError(err); ok {
				c.info = s.complete(s.Backend.Hello(c))
				if frame, err := Encode(c.info); err == nil {
					c.rt.Send(frame)
				}
				c.CloseWithError(e)
				s.waitClosed(c)
			} else {
				c.Close()
			}
			return
		}
	}
	c.info = s.complete(s.Backend.Hello(c))
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
	if d, ok := s.Backend.(Disconnecter); ok {
		d.Disconnected(c)
	}
}

// complete fills in what Server owns of a server frame: the protocol
// version, the ping interval, and the capabilities of the methods it
// handles.
func (s *Server) complete(info ServerInfo) ServerInfo {
	info.Apron = Version
	info.Ping = max(1, int(s.pingInterval()/time.Second))
	info.Capabilities = slices.Clone(info.Capabilities)
	s.mu.Lock()
	for _, name := range slices.Sorted(maps.Keys(s.methods)) {
		if capability := s.methods[name].Capability; capability != "" && !slices.Contains(info.Capabilities, capability) {
			info.Capabilities = append(info.Capabilities, capability)
		}
	}
	s.mu.Unlock()
	if !slices.Contains(info.Capabilities, "status") {
		info.Status = nil // Only servers with capability status send it (§4.5.1).
	}
	return info
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
