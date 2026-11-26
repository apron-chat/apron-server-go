package aprontest

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/server"
)

// Frame is a frame from the server: a reply, which has an id, a
// notification, which has a method, or an error about the connection as a
// whole, which has neither (§1.1).
type Frame struct {
	// Raw is the frame as it arrived.
	Raw jsontext.Value
	// ID is a reply's id, or "".
	ID string
	// Method and Params are a notification's.
	Method string
	Params jsontext.Value
	// Result is a reply's result, and Error an error reply's error, or
	// that of an error about the connection.
	Result jsontext.Value
	Error  *apron.Error
}

// Decode decodes a notification's params, or a reply's result, into v.
func (f Frame) Decode(v any) error {
	if f.Method != "" {
		return json.Unmarshal(f.Params, v)
	}
	return json.Unmarshal(f.Result, v)
}

// Method matches the notifications of a method, for Client.Expect.
func Method(name string) func(Frame) bool {
	return func(f Frame) bool { return f.Method == name }
}

// Client is a client of a Server, for tests: it opens a connection, reads
// the server frame, and keeps every frame after it that the test has not
// taken yet (Next, Expect, and Call take them), in the order they arrived.
// Its methods are safe to call from any goroutine; each that waits for a
// frame waits until its context is done, so a test passes one with a
// deadline.
type Client struct {
	conn   clientConn
	hello  apron.ServerInfo
	nextID atomic.Uint64
	// served, for a Client of Connect, is closed once ServeTransport
	// returns, when the Server has forgotten the connection.
	served chan struct{}

	mu      sync.Mutex
	frames  []Frame
	changed chan struct{}
	err     error
	// calls are the ids of the requests the Client sent and has not
	// taken the replies of, which CallFrames leaves for their callers.
	calls map[string]bool
}

// clientConn is the client's end of a transport.
type clientConn interface {
	read(ctx context.Context) ([]byte, error)
	write(ctx context.Context, frame []byte) error
	close() error
}

// Connect connects a test to s in process, over a LineTransport on a
// net.Pipe, which s serves (Server.ServeTransport) as it does a WebSocket:
// OnConnect, the server frame, auth, and all, but for an HTTP request,
// which the connection has none of. The connection closes as the test ends
// (t.Cleanup).
func Connect(t testing.TB, s *server.Server) *Client {
	t.Helper()
	serverEnd, client := net.Pipe()
	limit := s.ReadLimit()
	served := make(chan struct{})
	go func() {
		defer close(served)
		s.ServeTransport(context.Background(), server.LineTransport(serverEnd, int(limit)), nil, nil)
	}()
	c := start(&lineConn{conn: client, lines: bufio.NewReader(client)}, served)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// signInWait bounds SignIn's wait for the auth result.
const signInWait = 10 * time.Second

// SignIn connects to s for a test, as Connect does, and signs in (§3.2)
// with params, such as {"scheme": "guest", "name": "Ada"}: it returns the
// Client and the result's you, and fails the test unless the result signs
// the connection in within ten seconds. The frames sent after the result,
// such as a welcome notice, stay for Next and Expect.
func SignIn(t testing.TB, s *server.Server, params any) (*Client, apron.CurrentUser) {
	t.Helper()
	c := Connect(t, s)
	ctx, cancel := context.WithTimeout(t.Context(), signInWait)
	defer cancel()
	result, err := c.Auth(ctx, params)
	if err != nil || result.You == nil {
		t.Fatalf("aprontest: signing in with %v: result %+v, error %v", params, result, err)
	}
	return c, *result.You
}

// Dial connects to a Server over a WebSocket at url, such as that of an
// httptest.Server: ws://host/path.
func Dial(ctx context.Context, url string) (*Client, error) {
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(-1)
	return start(&wsConn{ws: ws}, nil), nil
}

// start reads the connection's frames, and its server frame.
func start(conn clientConn, served chan struct{}) *Client {
	c := &Client{conn: conn, served: served, changed: make(chan struct{}), calls: map[string]bool{}}
	go c.readAll()
	// The server frame comes first (§3.1).
	if f, err := c.take(context.Background(), func(Frame) bool { return true }); err == nil {
		if f.Method == "server" {
			_ = f.Decode(&c.hello)
		} else {
			c.mu.Lock()
			c.frames = slices.Insert(c.frames, 0, f)
			c.mu.Unlock()
		}
	}
	return c
}

// readAll reads frames until the connection ends.
func (c *Client) readAll() {
	for {
		payload, err := c.conn.read(context.Background())
		c.mu.Lock()
		if err != nil {
			c.err = fmt.Errorf("%w: %v", apron.ErrClosed, err)
		} else {
			c.frames = append(c.frames, parseFrame(payload))
		}
		close(c.changed)
		c.changed = make(chan struct{})
		c.mu.Unlock()
		if err != nil {
			return
		}
	}
}

func parseFrame(payload []byte) Frame {
	var envelope struct {
		ID     *string        `json:"id"`
		Method string         `json:"method"`
		Params jsontext.Value `json:"params"`
		Result jsontext.Value `json:"result"`
		Error  *apron.Error   `json:"error"`
	}
	f := Frame{Raw: jsontext.Value(payload)}
	if json.Unmarshal(payload, &envelope) == nil {
		f.Method, f.Params, f.Result, f.Error = envelope.Method, envelope.Params, envelope.Result, envelope.Error
		if envelope.ID != nil {
			f.ID = *envelope.ID
		}
	}
	return f
}

// take takes the first frame that matches, waiting for one until ctx is
// done or the connection ends.
func (c *Client) take(ctx context.Context, match func(Frame) bool) (Frame, error) {
	for {
		c.mu.Lock()
		for i, f := range c.frames {
			if match(f) {
				c.frames = slices.Delete(c.frames, i, i+1)
				c.mu.Unlock()
				return f, nil
			}
		}
		changed, err := c.changed, c.err
		c.mu.Unlock()
		if err != nil {
			return Frame{}, err
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return Frame{}, ctx.Err()
		}
	}
}

// Hello returns the server frame, the connection's first frame: the zero
// ServerInfo when the connection ended before one, or its first frame was
// another, which Next then returns.
func (c *Client) Hello() apron.ServerInfo { return c.hello }

// Auth signs in (§3.2) with params, such as {"scheme": "guest"}, and
// returns the result, or the error reply as an *apron.Error.
func (c *Client) Auth(ctx context.Context, params any) (apron.AuthResult, error) {
	raw, err := c.Call(ctx, "auth", params)
	if err != nil {
		return apron.AuthResult{}, err
	}
	var result apron.AuthResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return apron.AuthResult{}, fmt.Errorf("aprontest: auth result %s: %w", raw, err)
	}
	return result, nil
}

// Call sends a request with an id of the Client's own, and returns its
// result, or its error reply as an *apron.Error. The frames that arrive
// before the reply are kept for Next and Expect.
func (c *Client) Call(ctx context.Context, method string, params any) (jsontext.Value, error) {
	result, _, err := c.call(ctx, method, params, nil)
	return result, err
}

// CallFrames is Call, and also takes the frames that arrived before the
// reply, and returns them, in order: every frame the test had not taken
// ahead of the reply, including those that arrived before the request
// was sent, but for the replies to the Client's other requests, which
// stay for their calls. The frames after the reply stay for Next and
// Expect. A test that wants only what arrived after the request was sent
// takes the backlog first, such as with Sync and Next. When ctx is done
// or the connection ends before the reply, nothing is taken.
func (c *Client) CallFrames(ctx context.Context, method string, params any) (result jsontext.Value, before []Frame, err error) {
	return c.call(ctx, method, params, func(Frame) bool { return true })
}

// Sync waits until every frame the server queued to the connection before
// it handled a request Sync sends has arrived; those frames stay for
// Next, Expect, and Pending. A Server handles a connection's frames one
// at a time, in order, each to its end, and a request's reply is queued
// after what its handler sent, so when Sync returns, the replies to the
// requests sent before it, what their handlers sent, and whatever else
// was queued to the connection before, such as by a backend's own
// goroutine, have arrived; frames queued after may have too.
//
// The request is aprontest.sync, with an id of the Client's own, which a
// Server answers error/unsupported, or, before sign-in, error/denied: any
// reply will do, and Sync takes it. It is not a ping: a ping's pong
// carries no id, so it cannot tell one Sync's pong from another's or from
// a ping the test sent itself. A connection that is being closed, as when
// its account is signed out, answers no request, and Sync waits until ctx
// is done or the connection ends, and returns that error.
func (c *Client) Sync(ctx context.Context) error {
	id, err := c.send(ctx, "aprontest.sync", nil)
	if err != nil {
		return err
	}
	_, _, err = c.reply(ctx, id, nil)
	return err
}

// call sends a request and takes its reply, and the frames ahead of it that
// take matches (reply).
func (c *Client) call(ctx context.Context, method string, params any, take func(Frame) bool) (jsontext.Value, []Frame, error) {
	id, err := c.send(ctx, method, params)
	if err != nil {
		return nil, nil, err
	}
	reply, frames, err := c.reply(ctx, id, take)
	if err != nil {
		return nil, nil, err
	}
	if reply.Error != nil {
		return nil, frames, reply.Error
	}
	return reply.Result, frames, nil
}

// send sends a request of method with an id of the Client's own, which it
// returns.
func (c *Client) send(ctx context.Context, method string, params any) (string, error) {
	id := "aprontest-" + strconv.FormatUint(c.nextID.Add(1), 10)
	frame := map[string]any{"method": method, "id": id}
	if params != nil {
		frame["params"] = params
	}
	payload, err := apron.Marshal(frame)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.calls[id] = true
	c.mu.Unlock()
	if err := c.Send(ctx, payload); err != nil {
		c.mu.Lock()
		delete(c.calls, id)
		c.mu.Unlock()
		return "", err
	}
	return id, nil
}

// reply takes the reply to the request of id, waiting for it until ctx is
// done or the connection ends, and the frames ahead of it that take
// matches, when take is not nil, but for the replies to the Client's other
// requests. The request is forgotten either way.
func (c *Client) reply(ctx context.Context, id string, take func(Frame) bool) (Frame, []Frame, error) {
	defer func() {
		c.mu.Lock()
		delete(c.calls, id)
		c.mu.Unlock()
	}()
	for {
		c.mu.Lock()
		if i := slices.IndexFunc(c.frames, func(f Frame) bool { return f.ID == id && f.Method == "" }); i >= 0 {
			reply := c.frames[i]
			var ahead []Frame
			if take != nil {
				kept := c.frames[:0:0]
				for _, f := range c.frames[:i] {
					if f.Method == "" && c.calls[f.ID] || !take(f) {
						kept = append(kept, f)
					} else {
						ahead = append(ahead, f)
					}
				}
				c.frames = append(kept, c.frames[i+1:]...)
			} else {
				c.frames = slices.Delete(c.frames, i, i+1)
			}
			c.mu.Unlock()
			return reply, ahead, nil
		}
		changed, err := c.changed, c.err
		c.mu.Unlock()
		if err != nil {
			return Frame{}, nil, err
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return Frame{}, nil, ctx.Err()
		}
	}
}

// Notify sends a notification, such as activity, which gets no reply.
func (c *Client) Notify(ctx context.Context, method string, params any) error {
	frame := map[string]any{"method": method}
	if params != nil {
		frame["params"] = params
	}
	payload, err := apron.Marshal(frame)
	if err != nil {
		return err
	}
	return c.Send(ctx, payload)
}

// Send sends a frame as it is, such as one that is not valid.
func (c *Client) Send(ctx context.Context, frame []byte) error {
	return c.conn.write(ctx, frame)
}

// Next takes the next frame the test has not taken, waiting for one.
func (c *Client) Next(ctx context.Context) (Frame, error) {
	return c.take(ctx, func(Frame) bool { return true })
}

// Expect takes the first frame the test has not taken that matches, such
// as Method("message"), waiting for one; the frames before it stay, in
// order, for the next Next or Expect.
func (c *Client) Expect(ctx context.Context, match func(Frame) bool) (Frame, error) {
	return c.take(ctx, match)
}

// Pending returns the frames that have arrived and that the test has not
// taken, in order, and leaves them. It does not wait: frames arrive at any
// time, so it holds those that arrived before it was called, and no more.
// After Call returns, that is every frame that arrived before the reply,
// and maybe some that arrived after it (CallFrames returns exactly those
// before); after Sync returns, every frame the server queued to the
// connection before it handled Sync's request.
func (c *Client) Pending() []Frame {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.frames)
}

// Close closes the connection. For a Client of Connect, it returns once
// the Server has forgotten the connection: OnDisconnect has returned.
func (c *Client) Close() error {
	err := c.conn.close()
	if c.served != nil {
		<-c.served
	}
	return err
}

// lineConn is the client's end of a LineTransport.
type lineConn struct {
	conn  net.Conn
	lines *bufio.Reader
	mu    sync.Mutex
}

func (l *lineConn) read(context.Context) ([]byte, error) {
	for {
		line, err := l.lines.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		if line = line[:len(line)-1]; len(line) > 0 {
			return line, nil
		}
	}
}

func (l *lineConn) write(ctx context.Context, frame []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if deadline, ok := ctx.Deadline(); ok {
		_ = l.conn.SetWriteDeadline(deadline)
		defer l.conn.SetWriteDeadline(time.Time{})
	}
	_, err := l.conn.Write(append(slices.Clip(frame), '\n'))
	return err
}

func (l *lineConn) close() error { return l.conn.Close() }

// wsConn is the client's end of a WebSocket.
type wsConn struct{ ws *websocket.Conn }

func (w *wsConn) read(ctx context.Context) ([]byte, error) {
	_, payload, err := w.ws.Read(ctx)
	return payload, err
}

func (w *wsConn) write(ctx context.Context, frame []byte) error {
	return w.ws.Write(ctx, websocket.MessageText, frame)
}

func (w *wsConn) close() error { return w.ws.CloseNow() }
