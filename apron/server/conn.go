package server

import (
	"context"
	"net"
	"net/http"
	"sync"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/internal/core"
)

// Conn is one client connection. Its methods are safe for concurrent use.
type Conn struct {
	server  *Server
	rt      *core.Conn
	remote  net.Addr
	request *http.Request
	local   bool
	ctx     context.Context
	cancel  context.CancelFunc

	mu   sync.Mutex
	user *apron.User
	// info is the last server frame the connection was sent, or is about
	// to be; announced is set once Announce has replaced it, so that the
	// frame a connection opens with, made before, does not (opened).
	info      apron.ServerInfo
	announced bool
	// scope is the connection's deduplication scope (SetDedupScope).
	scope string
	// holding is set while the connection opens, and while auth runs:
	// notifications queued meanwhile wait in held until its server frame,
	// or auth's result, is queued (§3.1, §3.2.2).
	holding bool
	held    [][]apron.Frame
	data    any
}

// Pending is an outcome not decided yet, such as a change being stored,
// which HoldUntil holds a connection's frames for: Done is closed once it
// is decided, and Err is then nil when it succeeded.
type Pending interface {
	Done() <-chan struct{}
	Err() error
}

// User returns a copy of the connection's identity, its roles and ext
// included, or nil before it signs in.
func (c *Conn) User() *apron.User {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.user == nil {
		return nil
	}
	u := cloneUser(*c.user)
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
// a user notification (§3.3.3), but for a Local connection's identity
// changed in OnConnect, which the Server tells it of (Server.Local).
func (c *Conn) SetUser(u apron.User) {
	u = cloneUser(u)
	c.mu.Lock()
	c.user = &u
	c.mu.Unlock()
}

// SignOut leaves the connection without an identity: its requests are
// denied until it signs in again. It clears its deduplication scope too
// (SetDedupScope).
func (c *Conn) SignOut() {
	c.mu.Lock()
	c.user, c.scope = nil, ""
	c.mu.Unlock()
}

// SetDedupScope sets the connection's deduplication scope, "" until it is
// set, and again after SignOut; SetUser leaves it as it is. A duplicate of
// a request (§1.2) gets the original's outcome without its handler
// running, so the requests of one user's connections are each other's
// duplicates only within one scope: a backend whose connections of one
// user may do different things, such as one signed in with a restricted
// token, which its handlers refuse what another connection of the user
// may do, gives each kind of connection its own scope as it signs it in.
// A retry from another scope then runs again, its handler's checks and
// all, rather than get what a connection allowed more did.
func (c *Conn) SetDedupScope(scope string) {
	c.mu.Lock()
	c.scope = scope
	c.mu.Unlock()
}

// dedupScope returns the connection's user_id, "" before it signs in, and
// its deduplication scope.
func (c *Conn) dedupScope() (userID, scope string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.user == nil {
		return "", c.scope
	}
	return c.user.UserID, c.scope
}

// Data returns the value the backend keeps with the connection, which it
// reads with a type assertion, such as c.Data().(*session).
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

// ServerInfo returns the server frame the connection was last sent.
func (c *Conn) ServerInfo() apron.ServerInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info
}

// Announce sends the connection a new server frame, which replaces the last
// (§3.1), such as after a sign-in reveals more of the server. Server fills
// it in as it does Backend.Hello's. Unlike Hello, it may be called while
// the backend holds its own locks.
func (c *Conn) Announce(info apron.ServerInfo) bool {
	info = c.server.complete(info)
	frame, err := apron.Encode(info)
	if err != nil {
		c.server.logger().Error("apron: encoding the server frame", "error", err)
		return false
	}
	c.mu.Lock()
	c.info, c.announced = info, true
	c.mu.Unlock()
	return c.Send(frame)
}

// opened records info as the server frame the connection opens with,
// unless Announce replaced it while the connection was held, opening: that
// one's frame is queued after (release), so it is the last.
func (c *Conn) opened(info apron.ServerInfo) {
	c.mu.Lock()
	if !c.announced {
		c.info = info
	}
	c.mu.Unlock()
}

// Closing reports whether the connection is closing or closed: its last
// frame is queued, and the frames it sent after are dropped.
func (c *Conn) Closing() bool { return c.rt.Closing() }

// RemoteAddr is the client's address, or nil when the transport has none.
func (c *Conn) RemoteAddr() net.Addr { return c.remote }

// HTTPRequest is the HTTP request that opened the connection, or nil, as
// for a Local connection or a transport served without one.
func (c *Conn) HTTPRequest() *http.Request { return c.request }

// Local reports whether the connection is in process (Server.Local): one
// the program opened itself, such as a bot's, signed in from the start as
// its LocalOptions.User, where any other signs in with auth. A Local
// connection has no HTTPRequest or RemoteAddr, but nor may one served with
// ServeTransport, such as a test's, so Local is what tells them apart, as
// in OnConnect.
func (c *Conn) Local() bool { return c.local }

// Context is canceled when the connection ends.
func (c *Conn) Context() context.Context { return c.ctx }

// Done is closed when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.rt.Done() }

// Notify queues a notification to this connection, such as a Message, a
// UserUpdate, or a StatusUpdate: Encode, then Send. It reports false when
// the connection is closed, or the notification cannot be encoded, which
// is logged. Like Send, it never blocks. To send one notification to many
// connections, Encode it once, and Broadcast or Send the Frame.
func (c *Conn) Notify(n apron.Notification) bool {
	frame, err := apron.Encode(n)
	if err != nil {
		c.server.logger().Error("apron: encoding a notification", "error", err)
		return false
	}
	return c.Send(frame)
}

// Send queues encoded frames, such as one Encode made once for many
// connections, to be written one after another, with no other frame
// between them, such as a room's state and the cursors that complete it;
// they take one place in the queue (Config.OutgoingQueue). It reports
// false when the connection is closed.
//
// Send never blocks, and calls nothing of the backend's: a client whose
// queue is full is too slow to keep up, and is disconnected. So a backend
// that keeps its state under a lock sends while it holds it, which is what
// keeps every connection's frames in the order of the changes they
// describe (§1); one that sends after it lets go of the lock lets two
// senders' frames cross, such as two messages whose log_ids arrive out of
// order.
func (c *Conn) Send(frames ...apron.Frame) bool {
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

func (c *Conn) enqueue(frames []apron.Frame) bool {
	return c.rt.Send(framesOf(frames)...)
}

// HoldUntil holds every frame queued after it until p is done, so that a
// reply queued after a change is made durable in the background, such as a
// new account's, is sent only once it is. If p then has an error, the
// frames after it are never sent: the connection is sent Config.HoldFailed
// and closed. A hold placed while auth runs holds its result too. HoldUntil
// reports false when the connection is closed or fell behind.
func (c *Conn) HoldUntil(p Pending) bool { return c.rt.Fence(p) }

// hold queues notifications aside until release.
func (c *Conn) hold() {
	c.mu.Lock()
	c.holding = true
	c.mu.Unlock()
}

// release queues the notifications held since hold, after what is already
// queued, or, with discard, drops them.
func (c *Conn) release(discard bool) {
	c.mu.Lock()
	held := c.held
	c.held, c.holding = nil, false
	// Queued under mu, so that a Send that finds holding cleared comes after.
	for _, frames := range held {
		if discard || !c.enqueue(frames) {
			break
		}
	}
	c.mu.Unlock()
}

// Close ends the connection at once.
func (c *Conn) Close() { c.rt.Stop() }

// CloseWithError sends an error about the connection as a whole, after the
// frames already queued, then closes it (§1.1).
func (c *Conn) CloseWithError(e *apron.Error) {
	payload, err := core.Marshal(map[string]any{"error": e})
	if err != nil {
		return
	}
	c.rt.SendLast(payload)
}

func (c *Conn) pong() { c.rt.Pong() }
