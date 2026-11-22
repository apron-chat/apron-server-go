package wire

import (
	"context"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apron-chat/apron-server-go/internal/wirecore"
)

// Conn is one client connection. Its methods are safe for concurrent use.
type Conn struct {
	server  *Server
	t       Transport
	info    ServerInfo
	remote  net.Addr
	request *http.Request
	ctx     context.Context
	cancel  context.CancelFunc

	out     chan outbound
	done    chan struct{}
	stop    sync.Once
	closing atomic.Bool

	mu   sync.Mutex
	user *User
	// holding is set while auth runs: notifications queued meanwhile wait
	// in held until its result is queued (§3.2.2).
	holding bool
	held    []Frame
	data    any

	pinged    atomic.Bool
	lastFrame atomic.Int64
}

type outbound struct {
	frame Frame
	// last closes the connection once the frame is written.
	last bool
}

// User returns a copy of the connection's identity, or nil before it signs
// in.
func (c *Conn) User() *User {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.user == nil {
		return nil
	}
	u := *c.user
	return &u
}

// UserID returns the connection's user_id, or "" before it signs in.
func (c *Conn) UserID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.user == nil {
		return ""
	}
	return c.user.UserID
}

// SetUser changes the connection's identity, as after a rename. It sends
// nothing: the backend tells clients with a user notification (§3.3.3).
func (c *Conn) SetUser(u User) {
	c.mu.Lock()
	c.user = &u
	c.mu.Unlock()
}

// Data returns the value the backend keeps with the connection.
func (c *Conn) Data() any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data
}

// SetData keeps a value with the connection, such as the backend's own
// object for its user.
func (c *Conn) SetData(v any) {
	c.mu.Lock()
	c.data = v
	c.mu.Unlock()
}

// Info returns the server frame the connection was sent.
func (c *Conn) Info() ServerInfo { return c.info }

// RemoteAddr is the client's address, or nil when the transport has none.
func (c *Conn) RemoteAddr() net.Addr { return c.remote }

// Request is the HTTP request that opened the connection, or nil.
func (c *Conn) Request() *http.Request { return c.request }

// Context is canceled when the connection ends.
func (c *Conn) Context() context.Context { return c.ctx }

// Done is closed when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Notify queues a notification. It reports false when the connection is
// closed, or the params cannot be encoded.
func (c *Conn) Notify(method string, params any) bool {
	frame, err := Encode(method, params)
	if err != nil {
		c.server.logger().Error("wire: encoding a notification", "method", method, "error", err)
		return false
	}
	return c.Send(frame)
}

// SendMessage queues a message notification (§3.5).
func (c *Conn) SendMessage(m Message) bool { return c.Notify("message", m) }

// Send queues an encoded frame. A client whose queue is full is too slow to
// keep up, and is disconnected.
func (c *Conn) Send(frame Frame) bool {
	c.mu.Lock()
	if c.holding {
		c.held = append(c.held, frame)
		c.mu.Unlock()
		return true
	}
	c.mu.Unlock()
	return c.enqueue(frame)
}

func (c *Conn) enqueue(frame Frame) bool {
	if c.closing.Load() {
		return false
	}
	select {
	case <-c.done:
		return false
	case c.out <- outbound{frame: frame}:
		return true
	default:
		c.server.logger().Warn("wire: closing a connection that fell behind", "remote", c.remote)
		c.Close()
		return false
	}
}

// hold queues notifications aside until release.
func (c *Conn) hold() {
	c.mu.Lock()
	c.holding = true
	c.mu.Unlock()
}

// release queues the notifications held since hold, after what is already
// queued.
func (c *Conn) release() {
	c.mu.Lock()
	held := c.held
	c.held, c.holding = nil, false
	// Queued under mu, so that a Send that finds holding cleared comes after.
	for _, frame := range held {
		if !c.enqueue(frame) {
			break
		}
	}
	c.mu.Unlock()
}

// Close ends the connection at once.
func (c *Conn) Close() {
	c.stop.Do(func() {
		close(c.done)
		c.cancel()
		_ = c.t.Close()
	})
}

// CloseWithError sends an error about the connection as a whole, after the
// frames already queued, then closes it (§1.1).
func (c *Conn) CloseWithError(e *Error) {
	payload, err := marshal(map[string]any{"error": e})
	if err != nil || !c.closing.CompareAndSwap(false, true) {
		return
	}
	select {
	case c.out <- outbound{frame: payload, last: true}:
	default:
		c.Close()
	}
}

func (c *Conn) writeLoop() {
	timeout := c.server.writeTimeout()
	for {
		select {
		case <-c.done:
			return
		case o := <-c.out:
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			err := c.t.Write(ctx, o.frame)
			cancel()
			if err != nil || o.last {
				c.Close()
				return
			}
		}
	}
}

// pingLoop checks the transport, and closes a connection whose client sent
// liveness pings (§1) and then fell silent for three intervals.
func (c *Conn) pingLoop() {
	interval, timeout := c.server.pingInterval(), c.server.pingTimeout()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			if c.pinged.Load() && time.Since(time.Unix(0, c.lastFrame.Load())) > 3*interval+timeout {
				c.Close()
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			err := c.t.Ping(ctx)
			cancel()
			if err != nil {
				c.Close()
				return
			}
		}
	}
}

func (c *Conn) pong() {
	c.pinged.Store(true)
	c.enqueue(wirecore.PongFrame)
}
