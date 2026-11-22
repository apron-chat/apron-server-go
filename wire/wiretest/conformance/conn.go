package conformance

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"fmt"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/apron-chat/apron-server-go/wire"
)

// The times a check waits. The Python suite waits two seconds for a frame
// that should come; a check here waits longer, for a busy machine under
// -race, which makes only a failure slower. It waits as long as the Python
// suite for a frame that should not come.
const (
	waitFor = 5 * time.Second
	quiet   = 500 * time.Millisecond
)

// obj is a JSON object, as decoded: numbers are float64.
type obj = map[string]any

// transport is the client's end of a connection.
type transport interface {
	// read returns the next frame, waiting for one.
	read() ([]byte, error)
	write(ctx context.Context, frame []byte) error
	// unthrottle lets a slow connection read at full speed again.
	unthrottle()
	close()
}

// open opens a connection to the server of the run, waits for its server
// frame, and signs in unless signIn is false, as name. A connection that
// reads slowly is one that, once paused, backs up what the server writes
// after little traffic: in process, any connection does, as a pipe holds
// nothing; over a WebSocket, its socket's receive buffer is small until
// it resumes.
func (c *checker) open(signIn bool, name string, slow bool) *conn {
	c.t.Helper()
	t, err := c.r.dial(c.ctx, slow)
	if err != nil {
		c.fail("cannot connect: %v", err)
	}
	conn := newConn(c.r, t)
	c.conns = append(c.conns, conn)
	_, server := conn.wait(method("server"), 0, waitFor)
	c.must(server != nil, "no server frame on connect")
	c.r.mu.Lock()
	if c.r.server == nil {
		c.r.server, _ = server["params"].(obj)
		if c.r.server == nil {
			c.r.server = obj{}
		}
	}
	c.r.mu.Unlock()
	if signIn {
		result := c.call(conn, "auth", c.auth(name))
		conn.you, _ = result["you"].(obj)
	}
	return conn
}

// auth returns the params of a sign-in as name, or skips the check.
func (c *checker) auth(name string) obj {
	if c.r.config.Auth != nil {
		return c.r.config.Auth(name)
	}
	if offers("guest")(c.r.info()) {
		return obj{"scheme": "guest", "name": name}
	}
	c.skip("the server offers no guest scheme; set Config.Auth")
	return nil
}

// dial connects to the server of the run.
func (r *run) dial(ctx context.Context, slow bool) (transport, error) {
	if r.config.Server != nil {
		return connect(r.config.Server), nil
	}
	ctx, cancel := context.WithTimeout(ctx, waitFor)
	defer cancel()
	var options *websocket.DialOptions
	var throttled net.Conn
	if slow {
		options = &websocket.DialOptions{HTTPClient: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
				if tcp, ok := conn.(*net.TCPConn); ok {
					_ = tcp.SetReadBuffer(8192)
					throttled = tcp
				}
				return conn, err
			},
		}}}
	}
	ws, _, err := websocket.Dial(ctx, r.config.URL, options)
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(-1)
	return &wsTransport{ws: ws, throttled: throttled}, nil
}

// connect connects to s in process, over a LineTransport on a net.Pipe.
func connect(s *wire.Server) transport {
	server, client := net.Pipe()
	limit := s.ReadLimit
	if limit <= 0 {
		limit = 256 << 10 // wire's default
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		s.ServeTransport(context.Background(), wire.LineTransport(server, int(limit)), nil, nil)
	}()
	return &pipeTransport{conn: client, lines: bufio.NewReader(client), served: served}
}

// pipeTransport is the client's end of a LineTransport.
type pipeTransport struct {
	conn   net.Conn
	lines  *bufio.Reader
	served chan struct{}
	mu     sync.Mutex
}

func (p *pipeTransport) read() ([]byte, error) {
	for {
		line, err := p.lines.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		if line = line[:len(line)-1]; len(line) > 0 {
			return line, nil
		}
	}
}

func (p *pipeTransport) write(ctx context.Context, frame []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if deadline, ok := ctx.Deadline(); ok {
		_ = p.conn.SetWriteDeadline(deadline)
		defer p.conn.SetWriteDeadline(time.Time{})
	}
	_, err := p.conn.Write(append(slices.Clip(frame), '\n'))
	return err
}

func (p *pipeTransport) unthrottle() {}

// close closes the connection, and waits until the server has forgotten
// it, so that the next check starts without it.
func (p *pipeTransport) close() {
	_ = p.conn.Close()
	select {
	case <-p.served:
	case <-time.After(waitFor):
	}
}

// wsTransport is the client's end of a WebSocket; throttled, for a slow
// connection, is its TCP connection.
type wsTransport struct {
	ws        *websocket.Conn
	throttled net.Conn
}

func (w *wsTransport) unthrottle() {
	if tcp, ok := w.throttled.(*net.TCPConn); ok {
		_ = tcp.SetReadBuffer(4 << 20)
	}
}

func (w *wsTransport) read() ([]byte, error) {
	_, payload, err := w.ws.Read(context.Background())
	return payload, err
}

func (w *wsTransport) write(ctx context.Context, frame []byte) error {
	return w.ws.Write(ctx, websocket.MessageText, frame)
}

func (w *wsTransport) close() { _ = w.ws.CloseNow() }

// conn is one connection: every frame from the server in the order they
// arrived, which a check reads by index, as conform.py does, and the
// requests sent.
type conn struct {
	r   *run
	t   transport
	you obj

	mu      sync.Mutex
	frames  []obj // nil for a frame that is not a JSON object
	closed  bool
	changed chan struct{}
	// paused, while reading is paused, is closed when it resumes.
	paused   chan struct{}
	methods  map[string]string
	asked    map[string]int
	answered map[string]int
	// shown is the shown status and seen of each user, as the
	// connection last received them; only the reader uses it.
	shown map[string]shown
	read  chan struct{}
}

type shown struct {
	status any
	seen   any
}

func newConn(r *run, t transport) *conn {
	c := &conn{
		r: r, t: t, changed: make(chan struct{}),
		methods: map[string]string{}, asked: map[string]int{}, answered: map[string]int{},
		shown: map[string]shown{}, read: make(chan struct{}),
	}
	go c.readAll()
	return c
}

func (c *conn) readAll() {
	defer close(c.read)
	for {
		payload, err := c.t.read()
		if err != nil {
			break
		}
		c.mu.Lock()
		paused := c.paused
		c.mu.Unlock()
		if paused != nil {
			<-paused
		}
		var frame obj
		if json.Unmarshal(payload, &frame) != nil {
			frame = nil
		}
		c.mu.Lock()
		c.inspect(frame, payload)
		c.frames = append(c.frames, frame)
		close(c.changed)
		c.changed = make(chan struct{})
		c.mu.Unlock()
	}
	c.mu.Lock()
	c.closed = true
	close(c.changed)
	c.changed = make(chan struct{})
	c.mu.Unlock()
}

// pause stops reading after the frame being read, so that what the
// server sends backs up; resume reads on.
func (c *conn) pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused == nil {
		c.paused = make(chan struct{})
	}
}

func (c *conn) resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused != nil {
		c.t.unthrottle()
		close(c.paused)
		c.paused = nil
	}
}

func (c *conn) close() {
	c.resume()
	c.t.close()
}

// isClosed reports whether the connection ended.
func (c *conn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// count is the number of frames that have arrived: the index of the next.
func (c *conn) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.frames)
}

// wait returns the index of the first frame from start that matches, and
// the frame, waiting until timeout or the connection ends; -1 and nil when
// none does.
func (c *conn) wait(match func(obj) bool, start int, timeout time.Duration) (int, obj) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		c.mu.Lock()
		for i := start; i < len(c.frames); i++ {
			if c.frames[i] != nil && match(c.frames[i]) {
				f := c.frames[i]
				c.mu.Unlock()
				return i, f
			}
		}
		closed, changed := c.closed, c.changed
		c.mu.Unlock()
		if closed {
			return -1, nil
		}
		select {
		case <-changed:
		case <-timer.C:
			return -1, nil
		}
	}
}

// since returns the frames from start that match.
func (c *conn) since(start int, match func(obj) bool) []obj {
	c.mu.Lock()
	defer c.mu.Unlock()
	var found []obj
	for _, f := range c.frames[min(start, len(c.frames)):] {
		if f != nil && match(f) {
			found = append(found, f)
		}
	}
	return found
}

// send sends a frame, and counts a request's id.
func (c *conn) send(frame obj) error {
	payload, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if id, ok := frame["id"].(string); ok {
		c.mu.Lock()
		c.methods[id], _ = frame["method"].(string)
		c.asked[id]++
		c.mu.Unlock()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*waitFor)
	defer cancel()
	return c.t.write(ctx, payload)
}

// reply waits for the reply to the request of id, from start.
func (c *conn) reply(id string, start int, timeout time.Duration) (int, obj) {
	return c.wait(func(f obj) bool {
		_, isNotification := f["method"]
		return f["id"] == id && !isNotification
	}, start, timeout)
}

// send sends a frame, and fails the check if the connection is closed.
func (c *checker) send(conn *conn, frame obj) {
	c.t.Helper()
	if err := conn.send(frame); err != nil {
		c.fail("the server closed the connection before %v", frame["method"])
	}
}

// request sends a request, with a new id when id is "", and returns the
// index of its reply and the reply; it fails the check without one.
func (c *checker) request(conn *conn, method string, params any, id string) (int, obj) {
	c.t.Helper()
	i, reply, err := c.tryRequest(conn, method, params, id)
	if err != "" {
		c.fail("%s", err)
	}
	return i, reply
}

// tryRequest is request, which returns why it got no reply in place of
// failing.
func (c *checker) tryRequest(conn *conn, method string, params any, id string) (int, obj, string) {
	if id == "" {
		id = c.r.id()
	}
	frame := obj{"method": method, "id": id}
	if params != nil {
		frame["params"] = params
	}
	start := conn.count()
	if err := conn.send(frame); err != nil {
		return -1, nil, fmt.Sprintf("the server closed the connection before %s", method)
	}
	i, reply := conn.reply(id, start, waitFor)
	if reply == nil {
		why := "no reply to " + method
		if conn.isClosed() {
			why += " (connection closed)"
		}
		return -1, nil, why
	}
	return i, reply, ""
}

// replyTo sends a request, and returns its reply.
func (c *checker) replyTo(conn *conn, method string, params any) obj {
	c.t.Helper()
	_, reply := c.request(conn, method, params, "")
	return reply
}

// call sends a request, fails the check on an error reply, and returns
// the result.
func (c *checker) call(conn *conn, method string, params any) obj {
	c.t.Helper()
	_, reply := c.request(conn, method, params, "")
	result, ok := reply["result"].(obj)
	c.must(ok, "%s: %s", method, describe(reply))
	return result
}

// notify sends a notification.
func (c *checker) notify(conn *conn, method string, params any) {
	c.t.Helper()
	frame := obj{"method": method}
	if params != nil {
		frame["params"] = params
	}
	c.send(conn, frame)
}

// sync waits for a round trip, then a little longer, so that earlier
// replies, and what an asynchronous backend sends late, have arrived.
func (c *checker) sync(conn *conn) {
	c.t.Helper()
	c.request(conn, "me", obj{}, "")
	time.Sleep(quiet)
}
