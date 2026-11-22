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

	"github.com/apron-chat/apron-server-go/internal/wirecore"
)

// Errors of a Local connection.
var (
	// ErrClosed is a call on a Local connection that has ended.
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
	// before anything else happens on it.
	Data any
	// OnFrame, if set, gets each notification and connection error sent to
	// the connection, in order, on a goroutine of the connection's own; the
	// replies to Call go to Call. Without it, they are dropped.
	OnFrame func(Frame)
	// Queue bounds the frames waiting to be written to the connection, and
	// those waiting for OnFrame; 1024 by default. An OnFrame that lets more
	// pile up ends the connection with ErrFellBehind.
	Queue int
}

// Local is an in-process connection to a Server: a client in the same
// program, such as a bot, a bridge, or a test, whose requests take the same
// path as a network client's, deduplication and all. It is the backend's
// own, already signed in: the Server calls no Hello, Connecter, or Auth for
// it, but Disconnecter when it ends. It does not count toward
// MaxConnections.
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

type localReply struct {
	result jsontext.Value
	err    *Error
}

type inFlight struct{ l *Local }

// Local opens an in-process connection.
func (s *Server) Local(options LocalOptions) (*Local, error) {
	s.init()
	if !ValidID(options.User.UserID) {
		return nil, errors.New("wire: a Local connection needs a user")
	}
	queue := orDefault(options.Queue, 1024)
	l := &Local{s: s, prefix: rand.Text()[:12] + "-", turn: make(chan struct{}, 1), closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	l.c = s.newConn(ctx, cancel, localWriter{l}, queue, func() { l.shut(ErrFellBehind) })
	l.c.SetUser(options.User)
	l.c.SetData(options.Data)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	s.conns[l.c] = struct{}{}
	s.serving.Add(1)
	s.mu.Unlock()

	if options.OnFrame != nil {
		l.frames = make(chan Frame, queue)
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
	go func() {
		defer s.serving.Done()
		<-l.c.Done()
		l.shut(ErrClosed)
		s.mu.Lock()
		delete(s.conns, l.c)
		s.mu.Unlock()
		s.disconnected(l.c)
	}()
	// In process, there is no transport to ping.
	l.c.rt.Start(false)
	return l, nil
}

// Conn is the connection, for the backend.
func (l *Local) Conn() *Conn { return l.c }

// Done is closed when the connection ends.
func (l *Local) Done() <-chan struct{} { return l.c.Done() }

// Err returns why the connection ended, such as ErrFellBehind, or nil while
// it is open.
func (l *Local) Err() error {
	l.errMu.Lock()
	defer l.errMu.Unlock()
	return l.err
}

// Close ends the connection.
func (l *Local) Close() error {
	l.c.Close()
	return nil
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
// context; a call made from inside the handling of another on the same
// connection is ErrReentrant, and one of a method that is only ever a
// notification, which would wait for a reply forever, is ErrNotRequest.
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
	return found && m.Notification
}

// process sends a frame, a request with id or else a notification, and
// handles it once the connection's frame before it is done.
func (l *Local) process(ctx context.Context, method, id string, params any) error {
	if ctx.Value(inFlight{l}) != nil {
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
	l.s.processFrame(context.WithValue(ctx, inFlight{l}, true), l.c, payload)
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
