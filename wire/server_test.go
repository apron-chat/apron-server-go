package wire_test

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/apron-chat/apron-server-go/wire"
	"github.com/apron-chat/apron-server-go/wire/wiretest"
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

func (*backend) Hello(context.Context, *wire.Conn) wire.ServerInfo {
	return wire.ServerInfo{Auth: []string{"guest"}, Agent: "wire-test/1"}
}

func (b *backend) Auth(_ context.Context, c *wire.Conn, p wire.AuthParams, reply wire.Reply[wire.AuthResult]) error {
	you, err := b.guest(c, p)
	if err != nil {
		return err
	}
	return reply.Send(wire.AuthResult{You: you})
}

// guest is who Auth signs a connection in as.
func (b *backend) guest(c *wire.Conn, p wire.AuthParams) (*wire.CurrentUser, error) {
	if p.Name == "refuse" {
		return nil, wire.Denied("No")
	}
	id := fmt.Sprintf("guest_%d", b.guests.Add(1))
	// Queued before the result, sent after it (§3.2.2).
	c.Notify(wire.Notice("", "welcome "+id))
	time.Sleep(b.authDelay)
	return &wire.CurrentUser{User: wire.User{UserID: id, Name: p.Name}}, nil
}

func (b *backend) Message(_ context.Context, c *wire.Conn, p wire.MessageParams, reply wire.Reply[wire.MessageResult]) error {
	if p.Body.Empty() {
		return wire.InvalidParams("A new message needs text or embeds")
	}
	b.messages.Add(1)
	return reply.Send(wire.MessageResult{MessageID: b.clock.Next()})
}

func (b *backend) Disconnected(c *wire.Conn) {
	b.mu.Lock()
	b.disconnected = append(b.disconnected, c)
	b.mu.Unlock()
}

func (b *backend) disconnectedConns() []*wire.Conn {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*wire.Conn(nil), b.disconnected...)
}

// wired makes a Server for b with config, and the handlers and hooks b
// has methods for that config does not set, as each test backend is one
// value for all of them.
func wired(b wire.Backend, config wire.Config) *wire.Server {
	if h, ok := b.(wire.MessageHandler); ok {
		config.Messages = h
	}
	if h, ok := b.(wire.EditHandler); ok {
		config.Edits = h
	}
	if h, ok := b.(wire.CommandHandler); ok {
		config.Commands = h
	}
	if h, ok := b.(wire.ProfileHandler); ok {
		config.Profiles = h
	}
	if h, ok := b.(wire.RoomHandler); ok {
		config.Rooms = h
	}
	if h, ok := b.(interface{ Disconnected(*wire.Conn) }); ok && config.OnDisconnect == nil {
		config.OnDisconnect = h.Disconnected
	}
	return wire.NewServer(b, config)
}

// wait is a context for a test's waits on the server.
func wait(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// send sends a frame as it is.
func send(t *testing.T, c *wiretest.Client, raw string) {
	t.Helper()
	if err := c.Send(wait(t), []byte(raw)); err != nil {
		t.Fatalf("send %s: %v", raw, err)
	}
}

// next takes the next frame.
func next(t *testing.T, c *wiretest.Client) wiretest.Frame {
	t.Helper()
	f, err := c.Next(wait(t))
	if err != nil {
		t.Fatalf("next frame: %v", err)
	}
	return f
}

// code is the code of an error reply, or of an error about the connection,
// or 0.
func code(f wiretest.Frame) int {
	if f.Error == nil {
		return 0
	}
	return f.Error.Code
}

// fields are a reply's result, or a notification's params, decoded.
func fields(f wiretest.Frame) map[string]any {
	var m map[string]any
	_ = f.Decode(&m)
	return m
}

// params are params as they are sent, raw.
func params(raw string) jsontext.Value { return jsontext.Value(raw) }

// expectClosed expects the server to close the connection, after the
// frames not taken yet.
func expectClosed(t *testing.T, c *wiretest.Client) {
	t.Helper()
	f, err := c.Next(wait(t))
	switch {
	case err == nil:
		t.Fatalf("got %s, want the connection closed", f.Raw)
	case !errors.Is(err, wire.ErrClosed):
		t.Fatalf("the connection stayed open: %v", err)
	}
}

// quiet expects no frame to arrive for a moment.
func quiet(t *testing.T, c *wiretest.Client) {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	if pending := c.Pending(); len(pending) != 0 {
		t.Fatalf("got %s, want nothing yet", pending[0].Raw)
	}
}

// signIn signs c in as a guest, and takes the welcome notice Auth sends
// after the result.
func signIn(t *testing.T, c *wiretest.Client) string {
	t.Helper()
	result, err := c.Auth(wait(t), map[string]any{"scheme": "guest", "name": "Ada"})
	if err != nil || result.You == nil {
		t.Fatalf("auth: %+v %v", result, err)
	}
	if f := next(t, c); f.Method != "message" {
		t.Fatalf("after the auth result got %s, want the welcome notice", f.Raw)
	}
	return result.You.UserID
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
	s := wired(&backend{}, wire.Config{PingInterval: 20 * time.Second})
	if hello := wiretest.ConnectT(t, s).Hello(); hello.Apron != wire.Version || hello.Ping != 20 || hello.Agent != "wire-test/1" {
		t.Fatalf("server frame = %+v", hello)
	}
}

func TestFramingErrors(t *testing.T) {
	c := wiretest.ConnectT(t, wired(&backend{}, wire.Config{}))
	for _, tc := range []struct {
		send string
		code int
		id   string
	}{
		{`not json`, wire.CodeParseError, ""},
		{`[1,2]`, wire.CodeInvalidRequest, ""},
		{`{"id":"x"}`, wire.CodeInvalidRequest, "x"},
		{`{"method":"message","id":5}`, wire.CodeInvalidRequest, ""},
		{`{"method":"message","id":null}`, wire.CodeInvalidRequest, ""},
		{`{"method":"message","id":"p","params":[1]}`, wire.CodeInvalidParams, "p"},
		{`{"method":"message","id":"q","params":null}`, wire.CodeInvalidParams, "q"},
	} {
		send(t, c, tc.send)
		if f := next(t, c); code(f) != tc.code || f.ID != tc.id || (tc.id == "" && strings.Contains(string(f.Raw), `"id"`)) {
			t.Errorf("%s: got %s, want code %d and id %q", tc.send, f.Raw, tc.code, tc.id)
		}
	}
	// Not answered: a notification with bad params, a request method without
	// an id, an unknown notification, and activity with an id.
	send(t, c, `{"method":"message","params":[1]}`)
	send(t, c, `{"method":"message","params":{"body":{"text":"x"}}}`)
	send(t, c, `{"method":"nope"}`)
	send(t, c, `{"method":"activity","id":"a","params":{}}`)
	if err := c.Sync(wait(t)); err != nil {
		t.Fatal(err)
	}
	if pending := c.Pending(); len(pending) != 0 {
		t.Fatalf("got %s, want nothing", pending[0].Raw)
	}
	// The exact liveness ping is answered too.
	send(t, c, `{"method":"ping"}`)
	if f := next(t, c); f.Method != "pong" {
		t.Fatalf("got %s, want pong", f.Raw)
	}
}

func TestDeniedBeforeAuth(t *testing.T) {
	b := &backend{}
	c := wiretest.ConnectT(t, wired(b, wire.Config{}))
	message := params(`{"body":{"text":"hi"}}`)
	if _, err := c.Call(wait(t), "message", message); wire.CodeOf(err) != wire.CodeDenied {
		t.Fatalf("got %v, want denied", err)
	}
	if _, err := c.Call(wait(t), "nope", nil); wire.CodeOf(err) != wire.CodeDenied {
		t.Fatalf("got %v, want denied before the method is looked up", err)
	}
	// A failed sign-in leaves the connection signed out.
	if _, err := c.Auth(wait(t), map[string]any{"scheme": "guest", "name": "refuse"}); wire.CodeOf(err) != wire.CodeDenied {
		t.Fatalf("got %v, want the auth refused", err)
	}
	if _, err := c.Call(wait(t), "message", message); wire.CodeOf(err) != wire.CodeDenied {
		t.Fatalf("got %v, want denied", err)
	}
	if b.messages.Load() != 0 {
		t.Fatal("a message reached the backend before sign-in")
	}
}

func TestAuthSchemes(t *testing.T) {
	c := wiretest.ConnectT(t, wired(&backend{}, wire.Config{}))
	for _, tc := range []struct {
		params string
		code   int
	}{
		{`{}`, wire.CodeInvalidParams},                           // No scheme.
		{`{"scheme":"token","token":"x"}`, wire.CodeUnsupported}, // A known scheme not offered.
		{`{"scheme":"magic"}`, wire.CodeUnsupported},             // An unknown scheme.
	} {
		if _, err := c.Auth(wait(t), params(tc.params)); wire.CodeOf(err) != tc.code {
			t.Errorf("auth %s: %v, want code %d", tc.params, err, tc.code)
		}
	}
}

// TestAuthBarrier pipelines a request behind auth: it is processed after
// the sign-in, and the notification Auth queued comes after its result.
func TestAuthBarrier(t *testing.T) {
	b := &backend{authDelay: 50 * time.Millisecond}
	s := wired(b, wire.Config{})
	c := wiretest.ConnectT(t, s)
	send(t, c, `{"method":"auth","id":"a","params":{"scheme":"guest","name":"Ada"}}`)
	send(t, c, `{"method":"message","id":"m","params":{"body":{"text":"hi"}}}`)
	// A frame broadcast while Auth runs is held too.
	time.Sleep(10 * time.Millisecond)
	conn := onlyConn(t, s)
	frame, err := wire.EncodeRaw("status", map[string]any{"x": 1})
	if err != nil {
		t.Fatal(err)
	}
	conn.Send(frame)

	if f := next(t, c); f.ID != "a" || fields(f)["you"] == nil {
		t.Fatalf("first reply = %s, want the auth result", f.Raw)
	}
	if f := next(t, c); f.Method != "message" {
		t.Fatalf("got %s, want the held welcome notice", f.Raw)
	}
	if f := next(t, c); f.Method != "status" {
		t.Fatalf("got %s, want the held status", f.Raw)
	}
	if f := next(t, c); f.ID != "m" || fields(f)["message_id"] == nil {
		t.Fatalf("got %s, want the message result", f.Raw)
	}
	if conn.UserID() != "guest_1" || conn.User().Name != "Ada" {
		t.Fatalf("conn user = %v", conn.User())
	}
}

func TestUnsupportedMethod(t *testing.T) {
	c := wiretest.ConnectT(t, wired(&backend{}, wire.Config{}))
	signIn(t, c)
	// me and command without Profiles or Commands are unsupported too.
	for method, p := range map[string]string{"nope": `{}`, "me": `{"name":"B"}`, "command": `{"body":{"text":"/help"}}`} {
		if _, err := c.Call(wait(t), method, params(p)); wire.CodeOf(err) != wire.CodeUnsupported {
			t.Errorf("%s: %v, want unsupported", method, err)
		}
	}
	long := strings.Repeat("x", 300)
	send(t, c, `{"method":"message","id":"`+long+`","params":{"body":{"text":"hi"}}}`)
	if f := next(t, c); code(f) != wire.CodeTooLarge {
		t.Fatalf("got %s, want too_large for a long id", f.Raw)
	}
}

func TestHandle(t *testing.T) {
	s := wired(&backend{}, wire.Config{})
	s.Handle("echo", wire.Handler{Capability: "ext:echo", Serve: func(_ context.Context, _ *wire.Conn, r *wire.Request) error {
		r.Reply(r.Params)
		return nil
	}})
	s.Handle("fail", wire.Handler{NoDedup: true, Serve: func(context.Context, *wire.Conn, *wire.Request) error {
		return errors.New("secret detail")
	}})
	c := wiretest.ConnectT(t, s)
	if capabilities := c.Hello().Capabilities; !slices.Equal(capabilities, []string{"ext:echo"}) {
		t.Fatalf("capabilities = %v", capabilities)
	}
	signIn(t, c)
	if result, err := c.Call(wait(t), "echo", params(`{"b":1,"a":2}`)); err != nil || string(result) != `{"b":1,"a":2}` {
		t.Fatalf("echo: %s %v", result, err)
	}
	if _, err := c.Call(wait(t), "fail", nil); wire.CodeOf(err) != wire.CodeInternalError || strings.Contains(err.Error(), "secret") {
		t.Fatalf("got %v, want an internal error that says nothing", err)
	}
}

func TestDedup(t *testing.T) {
	b := &backend{}
	c := wiretest.ConnectT(t, wired(b, wire.Config{}))
	signIn(t, c)
	send(t, c, `{"method":"message","id":"d","params":{"room_id":"r","body":{"text":"hi"}}}`)
	first := next(t, c)
	// The same params in another key order are the same request (§1.2).
	send(t, c, `{"id":"d","params":{"body":{"text":"hi"},"room_id":"r"},"method":"message"}`)
	second := next(t, c)
	if fields(first)["message_id"] == nil || string(first.Raw) != string(second.Raw) {
		t.Fatalf("a retry got %s, the original %s", second.Raw, first.Raw)
	}
	if n := b.messages.Load(); n != 1 {
		t.Fatalf("the backend saw %d messages, want 1", n)
	}
	send(t, c, `{"method":"message","id":"d","params":{"body":{"text":"other"}}}`)
	if f := next(t, c); code(f) != wire.CodeInvalidParams || f.ID != "d" {
		t.Fatalf("got %s, want invalid_params for a reused id", f.Raw)
	}
	// A failed request is not kept: a retry runs again.
	send(t, c, `{"method":"message","id":"bad","params":{"room_id":"has space","body":{"text":"x"}}}`)
	if f := next(t, c); code(f) != wire.CodeInvalidParams {
		t.Fatalf("got %s", f.Raw)
	}
	send(t, c, `{"method":"message","id":"bad","params":{"body":{"text":"x"}}}`)
	if f := next(t, c); fields(f)["message_id"] == nil {
		t.Fatalf("got %s, want the retry to run", f.Raw)
	}
}

// returning is a backend whose calls, its commands and the test's "read"
// requests, come in pairs, an original and its retry: each fails or
// replies, and the original then does not return until the test ends.
type returning struct {
	oneUser
	calls     atomic.Int64
	returning chan struct{}
}

func (b *returning) Command(_ context.Context, _ *wire.Conn, _ wire.CommandParams, reply wire.Reply[wire.CommandResult]) error {
	n := b.calls.Add(1)
	_ = reply.Fail(wire.Denied(fmt.Sprintf("call %d", n)))
	b.stall(n)
	return nil
}

// stall holds up call n, after its reply, if it is an original.
func (b *returning) stall(n int64) {
	if n%2 == 1 {
		<-b.returning
	}
}

// A request whose outcome is not kept, a read or a failure, is forgotten
// before its reply is sent: a retry sent once the reply arrives, here on
// another connection of the same user, runs again, even while the
// original's handler is still returning, rather than wait for it and get
// its outcome.
func TestDedupForgetsBeforeTheReply(t *testing.T) {
	b := &returning{returning: make(chan struct{})}
	s := wired(b, wire.Config{})
	s.Handle("read", wire.Handler{NoDedup: true, Serve: func(_ context.Context, _ *wire.Conn, r *wire.Request) error {
		n := b.calls.Add(1)
		r.Reply(map[string]any{"call": n})
		b.stall(n)
		return nil
	}})
	// Each original holds up its connection, so each pair of requests
	// has its own connections, which end after the originals return.
	conns := []*wiretest.Client{wiretest.ConnectT(t, s), wiretest.ConnectT(t, s), wiretest.ConnectT(t, s), wiretest.ConnectT(t, s)}
	t.Cleanup(func() { close(b.returning) })
	for _, c := range conns {
		signIn(t, c)
	}
	for i, request := range []struct{ raw, first, retry string }{
		{`{"method":"read","id":"r","params":{}}`, `{"id":"r","result":{"call":1}}`, `{"id":"r","result":{"call":2}}`},
		{`{"method":"command","id":"f","params":{"body":{"text":"/fail"}}}`,
			`{"error":{"code":-32001,"message":"call 3"},"id":"f"}`, `{"error":{"code":-32001,"message":"call 4"},"id":"f"}`},
	} {
		c1, c2 := conns[2*i], conns[2*i+1]
		send(t, c1, request.raw)
		if f := next(t, c1); string(f.Raw) != request.first {
			t.Fatalf("got %s, want %s", f.Raw, request.first)
		}
		send(t, c2, request.raw)
		if f := next(t, c2); string(f.Raw) != request.retry {
			t.Fatalf("a retry after the reply got %s, want %s", f.Raw, request.retry)
		}
	}
}

func TestDedupPerUser(t *testing.T) {
	b := &backend{}
	s := wired(b, wire.Config{})
	c1, c2 := wiretest.ConnectT(t, s), wiretest.ConnectT(t, s)
	signIn(t, c1)
	signIn(t, c2)
	// Different users: the same id is a different request.
	send(t, c1, `{"method":"message","id":"x","params":{"body":{"text":"one"}}}`)
	send(t, c2, `{"method":"message","id":"x","params":{"body":{"text":"two"}}}`)
	if fields(next(t, c1))["message_id"] == nil || fields(next(t, c2))["message_id"] == nil {
		t.Fatal("a request failed")
	}
	if n := b.messages.Load(); n != 2 {
		t.Fatalf("the backend saw %d messages, want 2", n)
	}
}

func TestDedupOff(t *testing.T) {
	b := &backend{}
	c := wiretest.ConnectT(t, wired(b, wire.Config{DedupSize: -1}))
	signIn(t, c)
	for range 2 {
		send(t, c, `{"method":"message","id":"d","params":{"body":{"text":"hi"}}}`)
		next(t, c)
	}
	if n := b.messages.Load(); n != 2 {
		t.Fatalf("the backend saw %d messages, want 2", n)
	}
}

func TestMessageChecks(t *testing.T) {
	b := &backend{}
	c := wiretest.ConnectT(t, wired(b, wire.Config{}))
	signIn(t, c)
	if result, err := c.Call(wait(t), "message", params(`{"body":{"text":""}}`)); wire.CodeOf(err) != wire.CodeInvalidParams {
		t.Fatalf("an empty message got %s %v, want invalid_params", result, err)
	}
	if b.messages.Load() != 0 {
		t.Fatal("an empty message was saved")
	}
	for _, tc := range []struct {
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
		if _, err := c.Call(wait(t), "message", params(tc.params)); wire.CodeOf(err) != tc.code {
			t.Errorf("%s: got %v, want code %d", tc.params, err, tc.code)
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

func (e *editor) Edit(_ context.Context, _ *wire.Conn, _ wire.EditParams, reply wire.Reply[wire.MessageResult]) error {
	e.edits.Add(1)
	return reply.Send(wire.MessageResult{})
}

func TestEditor(t *testing.T) {
	b := &editor{}
	c := wiretest.ConnectT(t, wired(b, wire.Config{}))
	if capabilities := c.Hello().Capabilities; !slices.Equal(capabilities, []string{"edit"}) {
		t.Fatalf("capabilities = %v, want edit", capabilities)
	}
	signIn(t, c)
	if _, err := c.Call(wait(t), "message", params(`{"message_id":"1","deleted":true}`)); err != nil {
		t.Fatal(err)
	}
	if b.edits.Load() != 1 || b.messages.Load() != 0 {
		t.Fatalf("%d edits, %d messages", b.edits.Load(), b.messages.Load())
	}
}

func TestSlowClientDisconnected(t *testing.T) {
	b := &backend{}
	s := wired(b, wire.Config{OutgoingQueue: 2})
	// A client that reads nothing: the server frame is being written, and
	// the frames after it wait.
	server, client := net.Pipe()
	defer client.Close()
	go s.ServeTransport(context.Background(), wire.LineTransport(server, 1<<16), nil, nil)
	deadline := time.Now().Add(5 * time.Second)
	for len(s.Conns()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the connection did not open")
		}
		time.Sleep(time.Millisecond)
	}
	conn := onlyConn(t, s)
	frame, _ := wire.EncodeRaw("status", map[string]any{})
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
	s := wired(b, wire.Config{})
	c := wiretest.ConnectT(t, s)
	signIn(t, c)
	conn := onlyConn(t, s)
	conn.NotifyRaw("status", map[string]any{"n": 1})
	conn.CloseWithError(wire.RetryAfter("Going away", 7*time.Second))
	if conn.NotifyRaw("status", map[string]any{"n": 2}) {
		t.Fatal("a frame was queued after CloseWithError")
	}
	if f := next(t, c); f.Method != "status" {
		t.Fatalf("got %s, want the frame queued first", f.Raw)
	}
	f := next(t, c)
	data, _ := f.Error.Data.(map[string]any)
	if code(f) != wire.CodeRetryAfter || f.ID != "" || data["retry_after"] != float64(7) {
		t.Fatalf("got %s, want the connection's error", f.Raw)
	}
	expectClosed(t, c)
	// OnDisconnect has run once Close returns, as ServeTransport has.
	_ = c.Close()
	if d := b.disconnectedConns(); len(d) != 1 || d[0] != conn {
		t.Fatalf("Disconnected saw %v", d)
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
	c := wiretest.ConnectT(t, wired(&backend{}, wire.Config{ReadLimit: 1 << 16}))
	// The write fails once the server stops reading and closes.
	go func() {
		_ = c.Send(context.Background(), []byte(`{"method":"message","id":"x","params":{"body":{"text":"`+strings.Repeat("x", 1<<17)+`"}}}`))
	}()
	expectClosed(t, c)
}

func TestShutdown(t *testing.T) {
	s := wired(&backend{}, wire.Config{})
	c := wiretest.ConnectT(t, s)
	signIn(t, c)
	if err := s.Shutdown(wait(t)); err != nil {
		t.Fatal(err)
	}
	expectClosed(t, c)
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
	s := wired(&backend{}, wire.Config{})
	c1, c2 := wiretest.ConnectT(t, s), wiretest.ConnectT(t, s) // c2 is not signed in: not broadcast to.
	signIn(t, c1)
	frame, _ := wire.EncodeRaw("status", map[string]any{"x": 1})
	s.Broadcast(frame, nil)
	if f := next(t, c1); f.Method != "status" {
		t.Fatalf("got %s", f.Raw)
	}
	if err := c2.Sync(wait(t)); err != nil {
		t.Fatal(err)
	}
	if pending := c2.Pending(); len(pending) != 0 {
		t.Fatalf("a connection that is not signed in got %s", pending[0].Raw)
	}
}

func TestWebSocket(t *testing.T) {
	s := wired(&backend{}, wire.Config{CheckOrigin: wire.AnyOrigin})
	server := httptest.NewServer(s)
	defer server.Close()
	ctx := wait(t)
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	_, hello, err := ws.Read(ctx)
	if err != nil || !strings.Contains(string(hello), `"method":"server"`) {
		t.Fatalf("server frame = %s, %v", hello, err)
	}
	if r := onlyConn(t, s).HTTPRequest(); r == nil || r.URL.Path != "/" {
		t.Fatalf("HTTPRequest = %v", r)
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
	s := wired(&backend{}, wire.Config{OutgoingQueue: 2})
	c := wiretest.ConnectT(t, s)
	signIn(t, c)
	conn := onlyConn(t, s)
	f := &fence{done: make(chan struct{})}
	if !conn.Fence(f) {
		t.Fatal("Fence failed")
	}
	// Three frames in one place in the queue, behind the fence.
	a, _ := wire.EncodeRaw("a", map[string]any{})
	b, _ := wire.EncodeRaw("b", map[string]any{})
	if !conn.SendBatch(a, b, a) || !conn.SendBatch() {
		t.Fatal("SendBatch failed")
	}
	quiet(t, c)
	close(f.done)
	for _, want := range []string{"a", "b", "a"} {
		if got := next(t, c); got.Method != want {
			t.Fatalf("got %s, want %s", got.Raw, want)
		}
	}

	failed := &fence{done: make(chan struct{}), err: errors.New("not stored")}
	conn.Fence(failed)
	conn.Send(a)
	close(failed.done)
	if got := next(t, c); code(got) != wire.CodeInternalError || got.ID != "" {
		t.Fatalf("got %s, want internal_error in place of the frames behind a failed fence", got.Raw)
	}
	expectClosed(t, c)
}

// TestFenceDuringAuth holds the auth result behind a fence placed while
// Auth runs, and the notifications Auth sent after it.
func TestFenceDuringAuth(t *testing.T) {
	f := &fence{done: make(chan struct{})}
	b := &fencingBackend{fence: f}
	c := wiretest.ConnectT(t, wired(b, wire.Config{}))
	send(t, c, `{"method":"auth","id":"a","params":{"scheme":"guest","name":"Ada"}}`)
	quiet(t, c)
	close(f.done)
	if r := next(t, c); r.ID != "a" {
		t.Fatalf("got %s, want the auth result", r.Raw)
	}
	if n := next(t, c); n.Method != "message" {
		t.Fatalf("got %s, want the welcome notice", n.Raw)
	}
}

type fencingBackend struct {
	backend
	fence *fence
}

func (b *fencingBackend) Auth(_ context.Context, c *wire.Conn, p wire.AuthParams, reply wire.Reply[wire.AuthResult]) error {
	you, err := b.guest(c, p)
	if err != nil {
		return err
	}
	c.Fence(b.fence)
	return reply.Send(wire.AuthResult{You: you})
}

func TestMaxConnections(t *testing.T) {
	s := wired(&backend{}, wire.Config{MaxConnections: 1})
	c1, c2 := wiretest.ConnectT(t, s), wiretest.ConnectT(t, s)
	if f := next(t, c2); code(f) != wire.CodeRetryAfter || f.ID != "" {
		t.Fatalf("got %s, want retry_after", f.Raw)
	}
	expectClosed(t, c2)
	if err := c1.Sync(wait(t)); err != nil {
		t.Fatal(err)
	}
}
