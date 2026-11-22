package wire

import (
	"context"
	"crypto/rand"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apron-chat/apron-server-go/wire/internal/wirecore"
)

// Errors of a Local connection.
var (
	// ErrClosed is a call on a connection that has ended, such as a
	// Local one, or a reply to a request whose connection has.
	ErrClosed = errors.New("wire: the connection is closed")
	// ErrFellBehind ends a Local connection whose OnFrame did not keep up.
	ErrFellBehind = errors.New("wire: the connection's frame handler fell behind")
	// ErrReentrant is a call made from inside the handling of another call
	// on the same Local connection, which would wait for itself.
	ErrReentrant = errors.New("wire: the connection is waiting on this call; use another connection")
	// ErrNotRequest is a Call of a method clients send only as a
	// notification, such as ping or activity, which is never answered; send
	// it with Notify.
	ErrNotRequest = errors.New("wire: the method is a notification, which gets no reply; use Notify")
)

// LocalOptions configure a Local connection.
type LocalOptions struct {
	// User is who the connection is signed in as, from the start.
	User User
	// Data is the backend's value for the connection (Conn.SetData), set
	// before OnConnect is called, such as the backend's own record of the
	// connection, which OnConnect then tells from a network one's.
	Data any
	// OnFrame, if set, gets each frame sent to the connection but the
	// replies to Call, which go to Call, in order, on a goroutine of the
	// connection's own: the server frame first, then notifications and
	// connection errors. Without it, they are dropped.
	OnFrame func(Frame)
	// Queue bounds the frames waiting to be written to the connection, and
	// those waiting for OnFrame; 1024 by default. An OnFrame that lets more
	// pile up ends the connection with ErrFellBehind.
	Queue int
}

// Local is an in-process connection to a Server: a client in the same
// program, such as a bot, a bridge, or a test, whose requests take the same
// path as a network client's, deduplication and all.
//
// It is signed in from the start, as LocalOptions.User, without Auth, and
// otherwise opens as a network connection does: the Server calls OnConnect
// for it, with Conn.HTTPRequest nil, then Hello, whose server frame is its
// first frame, and OnDisconnect when it ends. The frames the backend sends
// it while OnConnect and Hello run are sent after the server frame, so a
// backend that keeps its connections under its own lock adds a Local one,
// and sends it what a sign-in would, in OnConnect, as for a network
// connection in Auth. Server.Local calls them in the caller's goroutine
// and returns their outcome, such as OnConnect's error, so the backend
// does not open a Local connection while it holds a lock they take: that
// waits forever, and is logged once it has waited 10 seconds. It does not
// count toward MaxConnections.
type Local struct {
	s      *Server
	c      *Conn
	prefix string
	nextID atomic.Uint64
	// turn holds a token while a call is processed: a connection's frames
	// are processed one at a time.
	turn    chan struct{}
	pending sync.Map // request id → chan localReply
	frames  chan Frame

	closed    chan struct{}
	closeOnce sync.Once
	errMu     sync.Mutex
	err       error
}

// localOpenStall is how long Server.Local waits for OnConnect and Hello
// before it logs that it does.
const localOpenStall = 10 * time.Second

type localReply struct {
	result jsontext.Value
	err    *Error
}

// inFlight is the context key of a call being handled on a Local
// connection, whose value is set while its handler runs.
type inFlight struct{ l *Local }

// Local opens an in-process connection, and returns once its server frame
// is queued. An error of OnConnect is returned as it is, and ErrClosed when
// the Server is shut down.
func (s *Server) Local(options LocalOptions) (*Local, error) {
	s.freeze()
	if !ValidID(options.User.UserID) {
		return nil, errors.New("wire: a Local connection needs a user")
	}
	queue := orDefault(options.Queue, 1024)
	l := &Local{s: s, prefix: rand.Text()[:12] + "-", turn: make(chan struct{}, 1), closed: make(chan struct{})}
	if options.OnFrame != nil {
		l.frames = make(chan Frame, queue)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := s.newConn(ctx, cancel, localWriter{l}, queue, func() { l.shut(ErrFellBehind) })
	l.c = c
	c.SetUser(options.User)
	c.SetData(options.Data)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		cancel()
		return nil, ErrClosed
	}
	s.serving.Add(1)
	s.mu.Unlock()

	if options.OnFrame != nil {
		s.serving.Add(1)
		go func() {
			defer s.serving.Done()
			for {
				select {
				case frame := <-l.frames:
					options.OnFrame(frame)
				case <-l.closed:
					return
				}
			}
		}()
	}
	// In process, there is no transport to ping.
	c.rt.Start(false)
	// A caller that holds a lock OnConnect or Hello takes waits for
	// itself forever, which no runtime check tells, so the wait is logged.
	stalled := time.AfterFunc(localOpenStall, func() {
		s.logger().Warn("wire: Server.Local is still waiting for OnConnect and Hello; does its caller hold a lock they take?", "user_id", options.User.UserID, "waited", localOpenStall)
	})
	err := s.openLocal(ctx, c)
	stalled.Stop()
	if err != nil {
		c.Close()
		s.serving.Done()
		return nil, err
	}
	go func() {
		defer s.serving.Done()
		<-c.Done()
		l.shut(ErrClosed)
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		s.disconnected(c)
	}()
	return l, nil
}

// openLocal opens a Local connection as a network one opens (ServeTransport):
// OnConnect, then the server frame, which goes before anything the backend
// sends meanwhile. OnDisconnect is called for a connection that OnConnect
// accepted but that does not open.
func (s *Server) openLocal(ctx context.Context, c *Conn) error {
	c.hold()
	if connect := s.config.onConnect; connect != nil {
		if err := connect(ctx, c); err != nil {
			c.release(true)
			return err
		}
	}
	var info ServerInfo
	if s.Backend != nil {
		info = s.Backend.Hello(ctx, c)
	}
	info = s.complete(info)
	frame, err := Encode(info)
	if err != nil {
		s.logger().Error("wire: encoding the server frame", "error", err)
	}
	s.mu.Lock()
	if err == nil && s.closed {
		err = ErrClosed
	}
	if err != nil {
		s.mu.Unlock()
		c.release(true)
		s.disconnected(c)
		return err
	}
	s.conns[c] = struct{}{}
	s.mu.Unlock()
	c.mu.Lock()
	c.info = info
	c.mu.Unlock()
	c.enqueue([]Frame{frame})
	c.release(false)
	return nil
}

// Conn is the connection, for the backend.
func (l *Local) Conn() *Conn { return l.c }

// Done is closed when the connection ends, once Err has the reason.
func (l *Local) Done() <-chan struct{} { return l.closed }

// Err returns why the connection ended, such as ErrFellBehind, or nil while
// it is open.
func (l *Local) Err() error {
	l.errMu.Lock()
	defer l.errMu.Unlock()
	return l.err
}

// Close ends the connection. It returns nil, or, when the connection had
// ended already, why, as Err does: ErrClosed after an earlier Close or the
// Server's Shutdown, or ErrFellBehind. OnDisconnect may still be running
// when it returns: Done is closed as the connection ends, before it.
func (l *Local) Close() error {
	err := l.Err()
	l.c.Close()
	return err
}

func (l *Local) shut(err error) {
	l.errMu.Lock()
	if l.err == nil {
		l.err = err
	}
	l.errMu.Unlock()
	l.closeOnce.Do(func() { close(l.closed) })
}

// Call sends a request and returns its result, or its error reply as an
// *Error. The request is handled in the caller's goroutine, with ctx as its
// context, and Call waits for its reply, also one answered later
// (Reply.Later), which the next Call on the connection does not wait for. A
// call made from inside the handler of another on the same connection is
// ErrReentrant, and one of a method that is only ever a notification, which
// would wait for a reply forever, is ErrNotRequest.
func (l *Local) Call(ctx context.Context, method string, params any) (jsontext.Value, error) {
	if l.notificationOnly(method) {
		return nil, ErrNotRequest
	}
	id := l.prefix + strconv.FormatUint(l.nextID.Add(1), 10)
	reply := make(chan localReply, 1)
	l.pending.Store(id, reply)
	defer l.pending.Delete(id)
	if err := l.process(ctx, method, id, params); err != nil {
		return nil, err
	}
	select {
	case r := <-reply:
		if r.err != nil {
			return nil, r.err
		}
		return r.result, nil
	case <-l.closed:
		return nil, l.Err()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Notify sends a notification, such as activity, and returns once it is
// handled, in the caller's goroutine with ctx as its context, as Call does
// a request. Nothing answers a notification: a method that is not one, or
// params that are not valid, are ignored (§1.1).
func (l *Local) Notify(ctx context.Context, method string, params any) error {
	return l.process(ctx, method, "", params)
}

// notificationOnly reports whether a method is only ever handled as a
// notification (§1.1): ping, and the methods registered as notifications.
func (l *Local) notificationOnly(method string) bool {
	if wirecore.NotificationOnly[method] {
		return true
	}
	m, found := l.s.method(method)
	return found && m.notify != nil
}

// process sends a frame, a request with id or else a notification, and
// handles it once the connection's frame before it is done.
func (l *Local) process(ctx context.Context, method, id string, params any) error {
	if running, _ := ctx.Value(inFlight{l}).(*atomic.Bool); running != nil && running.Load() {
		return ErrReentrant
	}
	frame := map[string]any{"method": method}
	if id != "" {
		frame["id"] = id
	}
	if params != nil {
		frame["params"] = params
	}
	payload, err := marshal(frame)
	if err != nil {
		return err
	}
	select {
	case l.turn <- struct{}{}:
	case <-l.closed:
		return l.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-l.turn }()
	select {
	case <-l.closed:
		return l.Err()
	default:
	}
	// A call from the work of a reply answered later, once the handler
	// has returned, is no reentrant one.
	running := new(atomic.Bool)
	running.Store(true)
	defer running.Store(false)
	l.s.processFrame(context.WithValue(ctx, inFlight{l}, running), l.c, payload)
	return nil
}

// receive takes a frame written to the connection: a reply goes to its
// Call, and anything else to OnFrame.
func (l *Local) receive(payload []byte) error {
	var frame struct {
		ID     *string        `json:"id"`
		Method string         `json:"method"`
		Result jsontext.Value `json:"result"`
		Error  *Error         `json:"error"`
	}
	if err := json.Unmarshal(payload, &frame); err != nil {
		return err
	}
	if frame.ID != nil && frame.Method == "" {
		if waiting, ok := l.pending.LoadAndDelete(*frame.ID); ok {
			waiting.(chan localReply) <- localReply{frame.Result, frame.Error}
		}
		return nil
	}
	if l.frames == nil {
		return nil
	}
	select {
	case l.frames <- Frame(payload):
		return nil
	default:
		l.shut(ErrFellBehind)
		return ErrFellBehind
	}
}

// localWriter is a Local connection's transport.
type localWriter struct{ l *Local }

func (w localWriter) Write(_ context.Context, frame []byte) error { return w.l.receive(frame) }
func (localWriter) Ping(context.Context) error                    { return nil }
func (w localWriter) Close() error {
	w.l.shut(ErrClosed)
	return nil
}
