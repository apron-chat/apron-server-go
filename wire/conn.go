package wire

import (
	"context"
	"net"
	"net/http"
	"sync"

	"github.com/apron-chat/apron-server-go/internal/wirecore"
)

// Conn is one client connection. Its methods are safe for concurrent use.
type Conn struct {
	server  *Server
	rt      *wirecore.Conn
	info    ServerInfo
	remote  net.Addr
	request *http.Request
	ctx     context.Context
	cancel  context.CancelFunc

	mu   sync.Mutex
	user *User
	// holding is set while auth runs: notifications queued meanwhile wait
	// in held until its result is queued (§3.2.2).
	holding bool
	held    [][]Frame
	data    any
}

// Fence is a condition the frames queued after it wait for, such as a
// change being stored: Done is closed once it is decided, and Err is then
// nil when it holds.
type Fence interface {
	Done() <-chan struct{}
	Err() error
}

// User returns a copy of the connection's identity, its roles and ext
// included, or nil before it signs in.
func (c *Conn) User() *User {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.user == nil {
		return nil
	}
	u := c.user.clone()
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

// SetUser changes the connection's identity, as after a sign-in or a
// rename, to a copy of u. It sends nothing: the backend tells clients with
// a user notification (§3.3.3).
func (c *Conn) SetUser(u User) {
	u = u.clone()
	c.mu.Lock()
	c.user = &u
	c.mu.Unlock()
}

// SignOut leaves the connection without an identity: its requests are
// denied until it signs in again.
func (c *Conn) SignOut() {
	c.mu.Lock()
	c.user = nil
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

// Info returns the server frame the connection was last sent.
func (c *Conn) Info() ServerInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info
}

// Announce sends the connection a new server frame, which replaces the last
// (§3.1), such as after a sign-in reveals more of the server. Server fills
// it in as it does Backend.Hello's. Unlike Hello, it may be called while
// the backend holds its own locks.
func (c *Conn) Announce(info ServerInfo) bool {
	info = c.server.complete(info)
	frame, err := Encode(info)
	if err != nil {
		c.server.logger().Error("wire: encoding the server frame", "error", err)
		return false
	}
	c.mu.Lock()
	c.info = info
	c.mu.Unlock()
	return c.Send(frame)
}

// Queued returns how many batches wait to be written to the client.
func (c *Conn) Queued() int { return c.rt.Queued() }

// Closing reports whether the connection is closing or closed: its last
// frame is queued, and the frames it sent after are dropped.
func (c *Conn) Closing() bool { return c.rt.Closing() }

// RemoteAddr is the client's address, or nil when the transport has none.
func (c *Conn) RemoteAddr() net.Addr { return c.remote }

// HTTPRequest is the HTTP request that opened the connection, or nil, as
// for a Local connection or a transport served without one.
func (c *Conn) HTTPRequest() *http.Request { return c.request }

// Context is canceled when the connection ends.
func (c *Conn) Context() context.Context { return c.ctx }

// Done is closed when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.rt.Done() }

// Notify queues a notification, such as a Message, a UserUpdate, or a
// StatusUpdate. It reports false when the connection is closed, or the
// notification cannot be encoded. Like Send, it never blocks.
func (c *Conn) Notify(n Notification) bool {
	frame, err := Encode(n)
	if err != nil {
		c.server.logger().Error("wire: encoding a notification", "error", err)
		return false
	}
	return c.Send(frame)
}

// NotifyRaw queues a notification of any method and params, such as an
// extension's own, as Notify does.
func (c *Conn) NotifyRaw(method string, params any) bool {
	frame, err := EncodeRaw(method, params)
	if err != nil {
		c.server.logger().Error("wire: encoding a notification", "method", method, "error", err)
		return false
	}
	return c.Send(frame)
}

// Send queues an encoded frame. It never blocks, and calls nothing of the
// backend's: a client whose queue is full is too slow to keep up, and is
// disconnected. So a backend that keeps its state under a lock sends while
// it holds it, which is what keeps every connection's frames in the order
// of the changes they describe (§1); one that sends after it lets go of the
// lock lets two senders' frames cross, such as two messages whose log_ids
// arrive out of order.
func (c *Conn) Send(frame Frame) bool { return c.SendBatch(frame) }

// SendBatch queues frames to be written one after another, with no other
// frame between them, such as a result and the notifications that complete
// it. They take one place in the queue (Server.OutgoingQueue). Like Send,
// it never blocks, and is meant to be called under the backend's lock.
func (c *Conn) SendBatch(frames ...Frame) bool {
	if len(frames) == 0 {
		return true
	}
	c.mu.Lock()
	if c.holding {
		c.held = append(c.held, frames)
		c.mu.Unlock()
		return true
	}
	c.mu.Unlock()
	return c.enqueue(frames)
}

func (c *Conn) enqueue(frames []Frame) bool {
	raw := make([][]byte, len(frames))
	for i, frame := range frames {
		raw[i] = frame
	}
	return c.rt.Send(raw...)
}

// Fence holds every frame queued after it until f is done, so that a reply
// queued after a change is made durable in the background, such as a new
// account's, is sent only once it is. If f then has an error, the frames
// after it are never sent: the connection is sent error/internal_error and
// closed. A fence placed while auth runs holds its result too. Fence
// reports false when the connection is closed or fell behind.
func (c *Conn) Fence(f Fence) bool { return c.rt.Fence(f) }

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
	for _, frames := range held {
		if !c.enqueue(frames) {
			break
		}
	}
	c.mu.Unlock()
}

// Close ends the connection at once.
func (c *Conn) Close() { c.rt.Stop() }

// CloseWithError sends an error about the connection as a whole, after the
// frames already queued, then closes it (§1.1).
func (c *Conn) CloseWithError(e *Error) {
	payload, err := marshal(map[string]any{"error": e})
	if err != nil {
		return
	}
	c.rt.SendLast(payload)
}

func (c *Conn) pong() { c.rt.Pong() }
