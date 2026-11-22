package wire_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/apron-chat/apron-server-go/wire"
)

// backend is a test Backend: guests sign in as guest_<n>, with a notice
// sent while Auth runs, and messages are counted.
type backend struct {
	clock    wire.Clock
	guests   atomic.Int64
	messages atomic.Int64
	// authDelay makes Auth wait, so that frames pipelined behind it are
	// read meanwhile.
	authDelay time.Duration

	mu           sync.Mutex
	disconnected []*wire.Conn
}

func (*backend) Hello(*wire.Conn) wire.ServerInfo {
	return wire.ServerInfo{Auth: []string{"guest"}, Agent: "wire-test/1"}
}

func (b *backend) Auth(_ context.Context, c *wire.Conn, p wire.AuthParams) (wire.AuthResult, error) {
	if p.Name == "refuse" {
		return wire.AuthResult{}, wire.Denied("No")
	}
	id := fmt.Sprintf("guest_%d", b.guests.Add(1))
	// Queued before the result, sent after it (§3.2.2).
	c.SendMessage(wire.Notice("", "welcome "+id))
	time.Sleep(b.authDelay)
	return wire.AuthResult{You: &wire.User{UserID: id, Name: p.Name}}, nil
}

func (b *backend) Message(_ context.Context, c *wire.Conn, p wire.MessageParams) (wire.MessageResult, error) {
	b.messages.Add(1)
	return wire.MessageResult{MessageID: b.clock.Next()}, nil
}

func (b *backend) Disconnected(c *wire.Conn) {
	b.mu.Lock()
	b.disconnected = append(b.disconnected, c)
	b.mu.Unlock()
}

// client is the other end of a LineTransport over net.Pipe.
type client struct {
	t     *testing.T
	conn  net.Conn
	lines *bufio.Scanner
	done  chan struct{}
}

type frame map[string]any

func (f frame) errorCode() int {
	e, _ := f["error"].(map[string]any)
	code, _ := e["code"].(float64)
	return int(code)
}

func (f frame) result() map[string]any {
	r, _ := f["result"].(map[string]any)
	return r
}

// serve serves one connection of s over net.Pipe, and returns the client
// end, having read the server frame.
func serve(t *testing.T, s *wire.Server) (*client, frame) {
	t.Helper()
	server, conn := net.Pipe()
	c := &client{t: t, conn: conn, lines: bufio.NewScanner(conn), done: make(chan struct{})}
	c.lines.Buffer(nil, 1<<20)
	go func() {
		defer close(c.done)
		s.ServeTransport(context.Background(), wire.LineTransport(server, 1<<16), nil, nil)
	}()
	t.Cleanup(func() {
		_ = conn.Close()
		select {
		case <-c.done:
		case <-time.After(5 * time.Second):
			t.Error("ServeTransport did not return")
		}
	})
	hello := c.read()
	if hello["method"] != "server" {
		t.Fatalf("first frame = %v, want the server frame", hello)
	}
	return c, hello
}

func (c *client) send(raw string) {
	c.t.Helper()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c.conn, raw+"\n"); err != nil {
		c.t.Fatalf("send %s: %v", raw, err)
	}
}

func (c *client) read() frame {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if !c.lines.Scan() {
		c.t.Fatalf("read: %v", c.lines.Err())
	}
	var f frame
	if err := json.Unmarshal(c.lines.Bytes(), &f); err != nil {
		c.t.Fatalf("read %q: %v", c.lines.Bytes(), err)
	}
	return f
}

// readEOF expects the server to close the connection.
func (c *client) readEOF() {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if c.lines.Scan() {
		c.t.Fatalf("read %s, want the connection closed", c.lines.Bytes())
	}
	var timeout net.Error
	if errors.As(c.lines.Err(), &timeout) && timeout.Timeout() {
		c.t.Fatal("the connection stayed open")
	}
}

// sync sends a ping with an id, which is processed in order with the frames
// before it (unlike the exact liveness ping), and expects its pong next.
func (c *client) sync() {
	c.t.Helper()
	c.send(`{"method":"ping","id":"sync"}`)
	if f := c.read(); f["method"] != "pong" || f["id"] != nil {
		c.t.Fatalf("got %v, want the pong", f)
	}
}

func (c *client) signIn(id string) string {
	c.t.Helper()
	c.send(`{"method":"auth","id":"` + id + `","params":{"scheme":"guest","name":"Ada"}}`)
	r := c.read()
	you, _ := r.result()["you"].(map[string]any)
	if r["id"] != id || you == nil {
		c.t.Fatalf("auth reply = %v", r)
	}
	if n := c.read(); n["method"] != "message" {
		c.t.Fatalf("after the auth result got %v, want the welcome notice", n)
	}
	return you["user_id"].(string)
}

func onlyConn(t *testing.T, s *wire.Server) *wire.Conn {
	t.Helper()
	conns := s.Conns()
	if len(conns) != 1 {
		t.Fatalf("%d connections, want 1", len(conns))
	}
	return conns[0]
}

func TestServerFrame(t *testing.T) {
	s := &wire.Server{Backend: &backend{}, PingInterval: 20 * time.Second}
	_, hello := serve(t, s)
	params, _ := hello["params"].(map[string]any)
	if params["apron"] != float64(wire.Version) || params["ping"] != float64(20) || params["agent"] != "wire-test/1" {
		t.Fatalf("server frame = %v", hello)
	}
}

func TestFramingErrors(t *testing.T) {
	c, _ := serve(t, &wire.Server{Backend: &backend{}})
	for _, tc := range []struct {
		send string
		code int
		id   any
	}{
		{`not json`, wire.CodeParseError, nil},
		{`[1,2]`, wire.CodeInvalidRequest, nil},
		{`{"id":"x"}`, wire.CodeInvalidRequest, "x"},
		{`{"method":"message","id":5}`, wire.CodeInvalidRequest, nil},
		{`{"method":"message","id":null}`, wire.CodeInvalidRequest, nil},
		{`{"method":"message","id":"p","params":[1]}`, wire.CodeInvalidParams, "p"},
		{`{"method":"message","id":"q","params":null}`, wire.CodeInvalidParams, "q"},
	} {
		c.send(tc.send)
		f := c.read()
		if f.errorCode() != tc.code || f["id"] != tc.id {
			t.Errorf("%s: got %v, want code %d and id %v", tc.send, f, tc.code, tc.id)
		}
	}
	// Not answered: a notification with bad params, a request method without
	// an id, an unknown notification, and activity with an id.
	c.send(`{"method":"message","params":[1]}`)
	c.send(`{"method":"message","params":{"body":{"text":"x"}}}`)
	c.send(`{"method":"nope"}`)
	c.send(`{"method":"activity","id":"a","params":{}}`)
	c.sync()
	// The exact liveness ping is answered too.
	c.send(`{"method":"ping"}`)
	if f := c.read(); f["method"] != "pong" {
		t.Fatalf("got %v, want pong", f)
	}
}

func TestDeniedBeforeAuth(t *testing.T) {
	b := &backend{}
	c, _ := serve(t, &wire.Server{Backend: b})
	c.send(`{"method":"message","id":"m","params":{"body":{"text":"hi"}}}`)
	if f := c.read(); f.errorCode() != wire.CodeDenied || f["id"] != "m" {
		t.Fatalf("got %v, want denied", f)
	}
	c.send(`{"method":"nope","id":"n"}`)
	if f := c.read(); f.errorCode() != wire.CodeDenied {
		t.Fatalf("got %v, want denied before the method is looked up", f)
	}
	// A failed sign-in leaves the connection signed out.
	c.send(`{"method":"auth","id":"a","params":{"scheme":"guest","name":"refuse"}}`)
	if f := c.read(); f.errorCode() != wire.CodeDenied || f["id"] != "a" {
		t.Fatalf("got %v, want the auth refused", f)
	}
	c.send(`{"method":"message","id":"m2","params":{"body":{"text":"hi"}}}`)
	if f := c.read(); f.errorCode() != wire.CodeDenied {
		t.Fatalf("got %v, want denied", f)
	}
	if b.messages.Load() != 0 {
		t.Fatal("a message reached the backend before sign-in")
	}
}

func TestAuthSchemes(t *testing.T) {
	c, _ := serve(t, &wire.Server{Backend: &backend{}})
	c.send(`{"method":"auth","id":"a","params":{}}`)
	if f := c.read(); f.errorCode() != wire.CodeInvalidParams {
		t.Fatalf("got %v, want invalid_params for no scheme", f)
	}
	c.send(`{"method":"auth","id":"b","params":{"scheme":"token","token":"x"}}`)
	if f := c.read(); f.errorCode() != wire.CodeUnsupported {
		t.Fatalf("got %v, want unsupported for a known scheme not offered", f)
	}
	c.send(`{"method":"auth","id":"c","params":{"scheme":"magic"}}`)
	if f := c.read(); f.errorCode() != wire.CodeUnsupported {
		t.Fatalf("got %v, want unsupported for an unknown scheme", f)
	}
}

// TestAuthBarrier pipelines a request behind auth: it is processed after
// the sign-in, and the notification Auth queued comes after its result.
func TestAuthBarrier(t *testing.T) {
	b := &backend{authDelay: 50 * time.Millisecond}
	s := &wire.Server{Backend: b}
	c, _ := serve(t, s)
	c.send(`{"method":"auth","id":"a","params":{"scheme":"guest","name":"Ada"}}`)
	c.send(`{"method":"message","id":"m","params":{"body":{"text":"hi"}}}`)
	// A frame broadcast while Auth runs is held too.
	time.Sleep(10 * time.Millisecond)
	conn := onlyConn(t, s)
	frame, err := wire.Encode("status", map[string]any{"x": 1})
	if err != nil {
		t.Fatal(err)
	}
	conn.Send(frame)

	if f := c.read(); f["id"] != "a" || f.result()["you"] == nil {
		t.Fatalf("first reply = %v, want the auth result", f)
	}
	if f := c.read(); f["method"] != "message" {
		t.Fatalf("got %v, want the held welcome notice", f)
	}
	if f := c.read(); f["method"] != "status" {
		t.Fatalf("got %v, want the held status", f)
	}
	if f := c.read(); f["id"] != "m" || f.result()["message_id"] == nil {
		t.Fatalf("got %v, want the message result", f)
	}
	if conn.UserID() != "guest_1" || conn.User().Name != "Ada" {
		t.Fatalf("conn user = %v", conn.User())
	}
}

func TestUnsupportedMethod(t *testing.T) {
	c, _ := serve(t, &wire.Server{Backend: &backend{}})
	c.signIn("a")
	c.send(`{"method":"nope","id":"u"}`)
	if f := c.read(); f.errorCode() != wire.CodeUnsupported || f["id"] != "u" {
		t.Fatalf("got %v, want unsupported", f)
	}
	// me and command without a Profiler or Commander are unsupported too.
	c.send(`{"method":"me","id":"me","params":{"name":"B"}}`)
	if f := c.read(); f.errorCode() != wire.CodeUnsupported {
		t.Fatalf("got %v, want unsupported", f)
	}
	c.send(`{"method":"command","id":"c","params":{"body":{"text":"/help"}}}`)
	if f := c.read(); f.errorCode() != wire.CodeUnsupported {
		t.Fatalf("got %v, want unsupported", f)
	}
	long := strings.Repeat("x", 300)
	c.send(`{"method":"message","id":"` + long + `","params":{"body":{"text":"hi"}}}`)
	if f := c.read(); f.errorCode() != wire.CodeTooLarge {
		t.Fatalf("got %v, want too_large for a long id", f)
	}
}

func TestHandle(t *testing.T) {
	s := &wire.Server{Backend: &backend{}}
	s.Handle("echo", wire.Method{Capability: "ext:echo", Handle: func(_ context.Context, _ *wire.Conn, r *wire.Request) (any, error) {
		return r.Params, nil
	}})
	s.Handle("fail", wire.Method{ReadOnly: true, Handle: func(context.Context, *wire.Conn, *wire.Request) (any, error) {
		return nil, errors.New("secret detail")
	}})
	c, hello := serve(t, s)
	capabilities, _ := hello["params"].(map[string]any)["capabilities"].([]any)
	if len(capabilities) != 1 || capabilities[0] != "ext:echo" {
		t.Fatalf("capabilities = %v", capabilities)
	}
	c.signIn("a")
	c.send(`{"method":"echo","id":"e","params":{"b":1,"a":2}}`)
	if f := c.read(); f.result()["a"] != float64(2) {
		t.Fatalf("got %v", f)
	}
	c.send(`{"method":"fail","id":"f"}`)
	f := c.read()
	if f.errorCode() != wire.CodeInternalError || strings.Contains(fmt.Sprint(f), "secret") {
		t.Fatalf("got %v, want an internal error that says nothing", f)
	}
}

func TestDedup(t *testing.T) {
	b := &backend{}
	c, _ := serve(t, &wire.Server{Backend: b})
	c.signIn("a")
	c.send(`{"method":"message","id":"d","params":{"room_id":"r","body":{"text":"hi"}}}`)
	first := c.read()
	// The same params in another key order are the same request (§1.2).
	c.send(`{"id":"d","params":{"body":{"text":"hi"},"room_id":"r"},"method":"message"}`)
	second := c.read()
	if first.result()["message_id"] == nil || fmt.Sprint(first) != fmt.Sprint(second) {
		t.Fatalf("a retry got %v, the original %v", second, first)
	}
	if n := b.messages.Load(); n != 1 {
		t.Fatalf("the backend saw %d messages, want 1", n)
	}
	c.send(`{"method":"message","id":"d","params":{"body":{"text":"other"}}}`)
	if f := c.read(); f.errorCode() != wire.CodeInvalidParams || f["id"] != "d" {
		t.Fatalf("got %v, want invalid_params for a reused id", f)
	}
	// A failed request is not kept: a retry runs again.
	c.send(`{"method":"message","id":"bad","params":{"room_id":"has space","body":{"text":"x"}}}`)
	if f := c.read(); f.errorCode() != wire.CodeInvalidParams {
		t.Fatalf("got %v", f)
	}
	c.send(`{"method":"message","id":"bad","params":{"body":{"text":"x"}}}`)
	if f := c.read(); f.result()["message_id"] == nil {
		t.Fatalf("got %v, want the retry to run", f)
	}
}

func TestDedupPerUser(t *testing.T) {
	b := &backend{}
	s := &wire.Server{Backend: b}
	c1, _ := serve(t, s)
	c1.signIn("a")
	c2, _ := serve(t, s)
	c2.signIn("a")
	// Different users: the same id is a different request.
	c1.send(`{"method":"message","id":"x","params":{"body":{"text":"one"}}}`)
	c2.send(`{"method":"message","id":"x","params":{"body":{"text":"two"}}}`)
	if c1.read().result()["message_id"] == nil || c2.read().result()["message_id"] == nil {
		t.Fatal("a request failed")
	}
	if n := b.messages.Load(); n != 2 {
		t.Fatalf("the backend saw %d messages, want 2", n)
	}
}

func TestDedupOff(t *testing.T) {
	b := &backend{}
	c, _ := serve(t, &wire.Server{Backend: b, DedupSize: -1})
	c.signIn("a")
	for range 2 {
		c.send(`{"method":"message","id":"d","params":{"body":{"text":"hi"}}}`)
		c.read()
	}
	if n := b.messages.Load(); n != 2 {
		t.Fatalf("the backend saw %d messages, want 2", n)
	}
}

func TestMessageChecks(t *testing.T) {
	b := &backend{}
	c, _ := serve(t, &wire.Server{Backend: b})
	c.signIn("a")
	c.send(`{"method":"message","id":"e","params":{"body":{"text":""}}}`)
	if f := c.read(); f["error"] != nil || len(f.result()) != 0 {
		t.Fatalf("an empty message got %v, want {}", f)
	}
	if b.messages.Load() != 0 {
		t.Fatal("an empty message reached the backend")
	}
	for i, tc := range []struct {
		params string
		code   int
	}{
		{`{"message_id":"1","body":{"text":"edit"}}`, wire.CodeUnsupported},
		{`{"message_id":"1","deleted":true}`, wire.CodeUnsupported},
		{`{}`, wire.CodeInvalidParams},
		{`{"body":{"text":"x","format":"html"}}`, wire.CodeInvalidParams},
		{`{"room_id":"a b","body":{"text":"x"}}`, wire.CodeInvalidParams},
		{`{"reply_to":{},"body":{"text":"x"}}`, wire.CodeInvalidParams},
		{`{"body":"x"}`, wire.CodeInvalidParams},
	} {
		c.send(`{"method":"message","id":"` + fmt.Sprint(i) + `","params":` + tc.params + `}`)
		if f := c.read(); f.errorCode() != tc.code {
			t.Errorf("%s: got %v, want code %d", tc.params, f, tc.code)
		}
	}
	if b.messages.Load() != 0 {
		t.Fatal("a refused message reached the backend")
	}
}

type editor struct {
	backend
	edits atomic.Int64
}

func (e *editor) Edit(context.Context, *wire.Conn, wire.MessageParams) (wire.MessageResult, error) {
	e.edits.Add(1)
	return wire.MessageResult{}, nil
}

func TestEditor(t *testing.T) {
	b := &editor{}
	c, hello := serve(t, &wire.Server{Backend: b})
	if fmt.Sprint(hello["params"].(map[string]any)["capabilities"]) != "[edit]" {
		t.Fatalf("server frame = %v, want capability edit", hello)
	}
	c.signIn("a")
	c.send(`{"method":"message","id":"e","params":{"message_id":"1","deleted":true}}`)
	if f := c.read(); f["error"] != nil {
		t.Fatalf("got %v", f)
	}
	if b.edits.Load() != 1 || b.messages.Load() != 0 {
		t.Fatalf("%d edits, %d messages", b.edits.Load(), b.messages.Load())
	}
}

func TestSlowClientDisconnected(t *testing.T) {
	b := &backend{}
	s := &wire.Server{Backend: b, OutgoingQueue: 2}
	c, _ := serve(t, s)
	c.signIn("a")
	conn := onlyConn(t, s)
	// The client stops reading: one frame is being written, two wait, and
	// the next finds the queue full.
	frame, _ := wire.Encode("status", map[string]any{})
	sent := 0
	for range 10 {
		if !conn.Send(frame) {
			break
		}
		sent++
	}
	if sent >= 10 {
		t.Fatal("every frame was queued")
	}
	select {
	case <-conn.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the slow connection stayed open")
	}
	if conn.Context().Err() == nil {
		t.Fatal("the connection's context was not canceled")
	}
	if conn.Send(frame) {
		t.Fatal("Send on a closed connection reported true")
	}
}

func TestCloseWithError(t *testing.T) {
	b := &backend{}
	s := &wire.Server{Backend: b}
	c, _ := serve(t, s)
	c.signIn("a")
	conn := onlyConn(t, s)
	conn.Notify("status", map[string]any{"n": 1})
	conn.CloseWithError(wire.RetryAfter("Going away", 7))
	if conn.Notify("status", map[string]any{"n": 2}) {
		t.Fatal("a frame was queued after CloseWithError")
	}
	if f := c.read(); f["method"] != "status" {
		t.Fatalf("got %v, want the frame queued first", f)
	}
	f := c.read()
	data, _ := f["error"].(map[string]any)["data"].(map[string]any)
	if f.errorCode() != wire.CodeRetryAfter || f["id"] != nil || data["retry_after"] != float64(7) {
		t.Fatalf("got %v, want the connection's error", f)
	}
	c.readEOF()
	<-conn.Done()
	// Disconnected runs as ServeTransport returns.
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		t.Fatal("ServeTransport did not return")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.disconnected) != 1 || b.disconnected[0] != conn {
		t.Fatalf("Disconnected saw %v", b.disconnected)
	}
}

func TestLineTransport(t *testing.T) {
	server, conn := net.Pipe()
	defer conn.Close()
	transport := wire.LineTransport(server, 16)
	go func() { _, _ = io.WriteString(conn, "\n\n{\"a\":1}\n") }()
	frame, err := transport.Read(context.Background())
	if err != nil || string(frame) != `{"a":1}` {
		t.Fatalf("Read = %q, %v; want blank lines skipped", frame, err)
	}
	go func() { _ = transport.Write(context.Background(), []byte(`{"b":2}`)) }()
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || line != "{\"b\":2}\n" {
		t.Fatalf("Write sent %q, %v", line, err)
	}
	if transport.Ping(context.Background()) != nil {
		t.Fatal("Ping failed")
	}
	go func() { _, _ = io.WriteString(conn, strings.Repeat("x", 32)+"\n") }()
	if _, err := transport.Read(context.Background()); err == nil {
		t.Fatal("a frame over maxFrame was read")
	}
	_ = transport.Close()
	if _, err := conn.Write([]byte("x")); err == nil {
		t.Fatal("Close left the stream open")
	}
}

// TestFrameTooLarge closes a connection whose client sends more than the
// transport's limit.
func TestFrameTooLarge(t *testing.T) {
	c, _ := serve(t, &wire.Server{Backend: &backend{}})
	// The write fails once the server stops reading and closes.
	go func() {
		_, _ = io.WriteString(c.conn, `{"method":"message","id":"x","params":{"body":{"text":"`+strings.Repeat("x", 1<<17)+`"}}}`+"\n")
	}()
	c.readEOF()
}

func TestShutdown(t *testing.T) {
	s := &wire.Server{Backend: &backend{}}
	c, _ := serve(t, s)
	c.signIn("a")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	c.readEOF()
	if len(s.Conns()) != 0 {
		t.Fatal("connections left after Shutdown")
	}
	// A connection after Shutdown is closed at once.
	server, conn := net.Pipe()
	defer conn.Close()
	s.ServeTransport(context.Background(), wire.LineTransport(server, 1024), nil, nil)
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("a connection after Shutdown was served")
	}
}

func TestBroadcast(t *testing.T) {
	s := &wire.Server{Backend: &backend{}}
	c1, _ := serve(t, s)
	c1.signIn("a")
	c2, _ := serve(t, s) // not signed in: not broadcast to
	frame, _ := wire.Encode("status", map[string]any{"x": 1})
	s.Broadcast(frame, nil)
	if f := c1.read(); f["method"] != "status" {
		t.Fatalf("got %v", f)
	}
	c2.sync()
}

func TestWebSocket(t *testing.T) {
	s := &wire.Server{Backend: &backend{}, AllowAnyOrigin: true}
	server := httptest.NewServer(s)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	_, hello, err := ws.Read(ctx)
	if err != nil || !strings.Contains(string(hello), `"method":"server"`) {
		t.Fatalf("server frame = %s, %v", hello, err)
	}
	if err := ws.Write(ctx, websocket.MessageText, []byte(`{"method":"ping"}`)); err != nil {
		t.Fatal(err)
	}
	if _, pong, err := ws.Read(ctx); err != nil || string(pong) != `{"method":"pong"}` {
		t.Fatalf("pong = %s, %v", pong, err)
	}
	// A binary frame closes the connection.
	if err := ws.Write(ctx, websocket.MessageBinary, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ws.Read(ctx); websocket.CloseStatus(err) != websocket.StatusUnsupportedData {
		t.Fatalf("after a binary frame: %v", err)
	}
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

// fence is a test Fence.
type fence struct {
	done chan struct{}
	err  error
}

func (f *fence) Done() <-chan struct{} { return f.done }
func (f *fence) Err() error            { return f.err }

func TestSendBatchAndFence(t *testing.T) {
	s := &wire.Server{Backend: &backend{}, OutgoingQueue: 2}
	c, _ := serve(t, s)
	c.signIn("a")
	conn := onlyConn(t, s)
	f := &fence{done: make(chan struct{})}
	if !conn.Fence(f) {
		t.Fatal("Fence failed")
	}
	// Three frames in one place in the queue, behind the fence.
	a, _ := wire.Encode("a", map[string]any{})
	b, _ := wire.Encode("b", map[string]any{})
	if !conn.SendBatch(a, b, a) || !conn.SendBatch() {
		t.Fatal("SendBatch failed")
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if c.lines.Scan() {
		t.Fatalf("read %s before the fence was released", c.lines.Bytes())
	}
	// A net.Pipe read that timed out leaves the stream usable once the
	// deadline is reset; the scanner is done, so it is replaced.
	c.lines = bufio.NewScanner(c.conn)
	close(f.done)
	for _, want := range []string{"a", "b", "a"} {
		if got := c.read(); got["method"] != want {
			t.Fatalf("got %v, want %s", got, want)
		}
	}

	failed := &fence{done: make(chan struct{}), err: errors.New("not stored")}
	conn.Fence(failed)
	conn.Send(a)
	close(failed.done)
	if got := c.read(); got.errorCode() != wire.CodeInternalError || got["id"] != nil {
		t.Fatalf("got %v, want internal_error in place of the frames behind a failed fence", got)
	}
	c.readEOF()
}

// TestFenceDuringAuth holds the auth result behind a fence placed while
// Auth runs, and the notifications Auth sent after it.
func TestFenceDuringAuth(t *testing.T) {
	f := &fence{done: make(chan struct{})}
	b := &fencingBackend{fence: f}
	c, _ := serve(t, &wire.Server{Backend: b})
	c.send(`{"method":"auth","id":"a","params":{"scheme":"guest","name":"Ada"}}`)
	_ = c.conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if c.lines.Scan() {
		t.Fatalf("read %s before the fence was released", c.lines.Bytes())
	}
	c.lines = bufio.NewScanner(c.conn)
	close(f.done)
	if r := c.read(); r["id"] != "a" {
		t.Fatalf("got %v, want the auth result", r)
	}
	if n := c.read(); n["method"] != "message" {
		t.Fatalf("got %v, want the welcome notice", n)
	}
}

type fencingBackend struct {
	backend
	fence *fence
}

func (b *fencingBackend) Auth(ctx context.Context, c *wire.Conn, p wire.AuthParams) (wire.AuthResult, error) {
	result, err := b.backend.Auth(ctx, c, p)
	c.Fence(b.fence)
	return result, err
}

func TestMaxConnections(t *testing.T) {
	s := &wire.Server{Backend: &backend{}, MaxConnections: 1}
	c1, _ := serve(t, s)
	c2, _ := serve(t, s)
	f := c2.read()
	if f.errorCode() != wire.CodeRetryAfter || f["id"] != nil {
		t.Fatalf("got %v, want retry_after", f)
	}
	c2.readEOF()
	c1.sync()
}
