package wiretest

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
	"time"

	"github.com/coder/websocket"

	"github.com/apron-chat/apron-server-go/wire"
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
	Error  *wire.Error
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
	hello  wire.ServerInfo
	nextID atomic.Uint64
	// served, for a Client of Connect, is closed once ServeTransport
	// returns, when the Server has forgotten the connection.
	served chan struct{}

	mu      sync.Mutex
	frames  []Frame
	changed chan struct{}
	err     error
}

// clientConn is the client's end of a transport.
type clientConn interface {
	read(ctx context.Context) ([]byte, error)
	write(ctx context.Context, frame []byte) error
	close() error
}

// defaultReadLimit is wire's own default of Server.ReadLimit.
const defaultReadLimit = 256 << 10

// Connect connects to s in process, over a LineTransport on a net.Pipe,
// which s serves (Server.ServeTransport) as it does a WebSocket: OnConnect,
// the server frame, auth, and all, but for an HTTP request, which the
// connection has none of.
func Connect(s *wire.Server) *Client {
	server, client := net.Pipe()
	limit := s.ReadLimit
	if limit <= 0 {
		limit = defaultReadLimit
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		s.ServeTransport(context.Background(), wire.LineTransport(server, int(limit)), nil, nil)
	}()
	return start(&lineConn{conn: client, lines: bufio.NewReader(client)}, served)
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
	c := &Client{conn: conn, served: served, changed: make(chan struct{})}
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
			c.err = fmt.Errorf("%w: %v", wire.ErrClosed, err)
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
		Error  *wire.Error    `json:"error"`
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
func (c *Client) Hello() wire.ServerInfo { return c.hello }

// Auth signs in (§3.2) with params, such as {"scheme": "guest"}, and
// returns the result, or the error reply as an *wire.Error.
func (c *Client) Auth(ctx context.Context, params any) (wire.AuthResult, error) {
	raw, err := c.Call(ctx, "auth", params)
	if err != nil {
		return wire.AuthResult{}, err
	}
	var result wire.AuthResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return wire.AuthResult{}, fmt.Errorf("wiretest: auth result %s: %w", raw, err)
	}
	return result, nil
}

// Call sends a request with an id of the Client's own, and returns its
// result, or its error reply as an *wire.Error. The frames that arrive
// before the reply are kept for Next and Expect.
func (c *Client) Call(ctx context.Context, method string, params any) (jsontext.Value, error) {
	id := "wiretest-" + strconv.FormatUint(c.nextID.Add(1), 10)
	frame := map[string]any{"method": method, "id": id}
	if params != nil {
		frame["params"] = params
	}
	payload, err := wire.Marshal(frame)
	if err != nil {
		return nil, err
	}
	if err := c.Send(ctx, payload); err != nil {
		return nil, err
	}
	reply, err := c.take(ctx, func(f Frame) bool { return f.ID == id && f.Method == "" })
	if err != nil {
		return nil, err
	}
	if reply.Error != nil {
		return nil, reply.Error
	}
	return reply.Result, nil
}

// Notify sends a notification, such as activity, which gets no reply.
func (c *Client) Notify(ctx context.Context, method string, params any) error {
	frame := map[string]any{"method": method}
	if params != nil {
		frame["params"] = params
	}
	payload, err := wire.Marshal(frame)
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

// Pending returns the frames that arrived and that the test has not taken,
// in order, and leaves them.
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
