package server_test

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
	"testing/synctest"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/aprontest"
	"github.com/apron-chat/apron-server-go/apron/server"
	"github.com/coder/websocket"
)

// backend is a test Backend: guests sign in as guest_<n>, with a notice
// sent while Auth runs, and messages are counted.
type backend struct {
	clock    server.Clock
	guests   atomic.Int64
	messages atomic.Int64
	// authDelay makes Auth wait, so that frames pipelined behind it are
	// read meanwhile.
	authDelay time.Duration

	mu           sync.Mutex
	disconnected []*server.Conn
}

func (*backend) Hello(context.Context, *server.Conn) apron.ServerInfo {
	return apron.ServerInfo{Auth: []string{"guest"}, Agent: "server-test/1"}
}

func (b *backend) Auth(_ context.Context, c *server.Conn, p apron.AuthParams, reply server.Reply[apron.AuthResult]) error {
	you, err := b.guest(c, p)
	if err != nil {
		return err
	}
	return reply.Send(apron.AuthResult{You: *you})
}

// guest is who Auth signs a connection in as.
func (b *backend) guest(c *server.Conn, p apron.AuthParams) (*apron.CurrentUser, error) {
	if p.Name == "refuse" {
		return nil, apron.Denied("No")
	}
	id := fmt.Sprintf("guest_%d", b.guests.Add(1))
	// Queued before the result, sent after it (§3.2.2).
	c.Notify(apron.Notice("", "welcome "+id))
	time.Sleep(b.authDelay)
	return &apron.CurrentUser{UserID: id, Name: p.Name}, nil
}

func (b *backend) Message(_ context.Context, c *server.Conn, p apron.MessageParams, reply server.Reply[apron.MessageResult]) error {
	if p.Body.Empty() {
		return apron.InvalidParams("A new message needs text or embeds")
	}
	b.messages.Add(1)
	return reply.Send(apron.MessageResult{MessageID: b.clock.Next()})
}

func (b *backend) Disconnected(c *server.Conn) {
	b.mu.Lock()
	b.disconnected = append(b.disconnected, c)
	b.mu.Unlock()
}

func (b *backend) disconnectedConns() []*server.Conn {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.disconnected)
}

// wired makes a Server for b with config, and the handlers and hooks b
// has methods for that config does not set, as each test backend is one
// value for all of them.
func wired(b server.Backend, config server.Config) *server.Server {
	if h, ok := b.(server.MessageHandler); ok {
		config.Messages = h
	}
	if h, ok := b.(server.EditHandler); ok {
		config.Edits = h
	}
	if h, ok := b.(server.CommandHandler); ok {
		config.Commands = h
	}
	if h, ok := b.(server.ProfileHandler); ok {
		config.Profiles = h
	}
	if h, ok := b.(server.RoomHandler); ok {
		config.Rooms = h
	}
	if h, ok := b.(interface{ Disconnected(*server.Conn) }); ok && config.OnDisconnect == nil {
		config.OnDisconnect = h.Disconnected
	}
	return server.New(b, config)
}

// wait is a context for a test's waits on the server.
func wait(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// send sends a frame as it is.
func send(t *testing.T, c *aprontest.Client, raw string) {
	t.Helper()
	if err := c.Send(wait(t), []byte(raw)); err != nil {
		t.Fatalf("send %s: %v", raw, err)
	}
}

// next takes the next frame.
func next(t *testing.T, c *aprontest.Client) aprontest.Frame {
	t.Helper()
	f, err := c.Next(wait(t))
	if err != nil {
		t.Fatalf("next frame: %v", err)
	}
	return f
}

// code is the code of an error reply, or of an error about the connection,
// or 0.
func code(f aprontest.Frame) int {
	if f.Error == nil {
		return 0
	}
	return f.Error.Code
}

// fields are a reply's result, or a notification's params, decoded.
func fields(f aprontest.Frame) map[string]any {
	var m map[string]any
	_ = f.Decode(&m)
	return m
}

// params are params as they are sent, raw.
func params(raw string) jsontext.Value { return jsontext.Value(raw) }

// expectClosed expects the server to close the connection, after the
// frames not taken yet.
func expectClosed(t *testing.T, c *aprontest.Client) {
	t.Helper()
	f, err := c.Next(wait(t))
	switch {
	case err == nil:
		t.Fatalf("got %s, want the connection closed", f.Raw)
	case !errors.Is(err, apron.ErrClosed):
		t.Fatalf("the connection stayed open: %v", err)
	}
}

// quiet expects no frame to arrive for a moment.
func quiet(t *testing.T, c *aprontest.Client) {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	if pending := c.Pending(); len(pending) != 0 {
		t.Fatalf("got %s, want nothing yet", pending[0].Raw)
	}
}

// signIn signs c in as a guest, and takes the welcome notice Auth sends
// after the result.
func signIn(t *testing.T, c *aprontest.Client) string {
	t.Helper()
	result, err := c.Auth(wait(t), map[string]any{"scheme": "guest", "name": "Ada"})
	if err != nil || result.You.UserID == "" {
		t.Fatalf("auth: %+v %v", result, err)
	}
	if f := next(t, c); f.Method != "message" {
		t.Fatalf("after the auth result got %s, want the welcome notice", f.Raw)
	}
	return result.You.UserID
}

func onlyConn(t *testing.T, s *server.Server) *server.Conn {
	t.Helper()
	conns := s.Conns()
	if len(conns) != 1 {
		t.Fatalf("%d connections, want 1", len(conns))
	}
	return conns[0]
}

func TestServerFrame(t *testing.T) {
	s := wired(&backend{}, server.Config{PingInterval: 20 * time.Second})
	if hello := aprontest.Connect(t, s).Hello(); hello.Apron != apron.Version || hello.Ping != 20 || hello.Agent != "server-test/1" {
		t.Fatalf("server frame = %+v", hello)
	}
}

func TestFramingErrors(t *testing.T) {
	c := aprontest.Connect(t, wired(&backend{}, server.Config{}))
	for _, tc := range []struct {
		send string
		code int
		id   string
	}{
		{`not json`, apron.CodeParseError, ""},
		{`[1,2]`, apron.CodeInvalidRequest, ""},
		{`{"id":"x"}`, apron.CodeInvalidRequest, "x"},
		{`{"method":"message","id":5}`, apron.CodeInvalidRequest, ""},
		{`{"method":"message","id":null}`, apron.CodeInvalidRequest, ""},
		{`{"method":"message","id":"p","params":[1]}`, apron.CodeInvalidParams, "p"},
		{`{"method":"message","id":"q","params":null}`, apron.CodeInvalidParams, "q"},
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
	c := aprontest.Connect(t, wired(b, server.Config{}))
	message := params(`{"body":{"text":"hi"}}`)
	if _, err := c.Call(wait(t), "message", message); apron.CodeOf(err) != apron.CodeDenied {
		t.Fatalf("got %v, want denied", err)
	}
	if _, err := c.Call(wait(t), "nope", nil); apron.CodeOf(err) != apron.CodeDenied {
		t.Fatalf("got %v, want denied before the method is looked up", err)
	}
	// A failed sign-in leaves the connection signed out.
	if _, err := c.Auth(wait(t), map[string]any{"scheme": "guest", "name": "refuse"}); apron.CodeOf(err) != apron.CodeDenied {
		t.Fatalf("got %v, want the auth refused", err)
	}
	if _, err := c.Call(wait(t), "message", message); apron.CodeOf(err) != apron.CodeDenied {
		t.Fatalf("got %v, want denied", err)
	}
	if b.messages.Load() != 0 {
		t.Fatal("a message reached the backend before sign-in")
	}
}

func TestAuthSchemes(t *testing.T) {
	c := aprontest.Connect(t, wired(&backend{}, server.Config{}))
	for _, tc := range []struct {
		params string
		code   int
	}{
		{`{}`, apron.CodeInvalidParams},                           // No scheme.
		{`{"scheme":"token","token":"x"}`, apron.CodeUnsupported}, // A known scheme not offered.
		{`{"scheme":"magic"}`, apron.CodeUnsupported},             // An unknown scheme.
	} {
		if _, err := c.Auth(wait(t), params(tc.params)); apron.CodeOf(err) != tc.code {
			t.Errorf("auth %s: %v, want code %d", tc.params, err, tc.code)
		}
	}
}

// TestAuthBarrier pipelines a request behind auth: it is processed after
// the sign-in, and the notification Auth queued comes after its result.
func TestAuthBarrier(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := &backend{authDelay: 50 * time.Millisecond}
		s := wired(b, server.Config{})
		c := aprontest.Connect(t, s)
		send(t, c, `{"method":"auth","id":"a","params":{"scheme":"guest","name":"Ada"}}`)
		send(t, c, `{"method":"message","id":"m","params":{"body":{"text":"hi"}}}`)
		// A frame broadcast while Auth runs is held too: once all is still,
		// Auth is in its delay.
		synctest.Wait()
		conn := onlyConn(t, s)
		frame, err := server.EncodeRaw("status", map[string]any{"x": 1})
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
		if conn.UserID() != "guest_1" || userOf(conn).Name != "Ada" {
			t.Fatalf("conn user = %v", userOf(conn))
		}
	})
}

// refusing refuses a sign-in as "slow", once the test lets it.
type refusing struct {
	backend
	started, proceed chan struct{}
}

func (b *refusing) Auth(ctx context.Context, c *server.Conn, p apron.AuthParams, reply server.Reply[apron.AuthResult]) error {
	if p.Name != "slow" {
		return b.backend.Auth(ctx, c, p, reply)
	}
	b.started <- struct{}{}
	<-b.proceed
	return apron.Denied("No")
}

// The frames sent to a connection while an auth runs wait for its result
// (§3.2.2). When it fails, a connection that was signed in, and stays so,
// gets them all the same, as they are its user's, such as a broadcast;
// one that was not gets none, as they were about a sign-in that did not
// happen.
func TestFailedAuthHeldFrames(t *testing.T) {
	for _, signedIn := range []bool{true, false} {
		t.Run(fmt.Sprintf("signed in %v", signedIn), func(t *testing.T) {
			b := &refusing{started: make(chan struct{}), proceed: make(chan struct{})}
			s := wired(b, server.Config{})
			c := aprontest.Connect(t, s)
			var userID string
			if signedIn {
				userID = signIn(t, c)
			}
			send(t, c, `{"method":"auth","id":"a","params":{"scheme":"guest","name":"slow"}}`)
			<-b.started
			frame, err := server.EncodeRaw("status", map[string]any{"x": 1})
			if err != nil {
				t.Fatal(err)
			}
			onlyConn(t, s).Send(frame)
			quiet(t, c)
			close(b.proceed)
			if f := next(t, c); f.ID != "a" || code(f) != apron.CodeDenied {
				t.Fatalf("got %s, want the auth's error", f.Raw)
			}
			if !signedIn {
				quiet(t, c)
				return
			}
			if f := next(t, c); f.Method != "status" {
				t.Fatalf("got %s, want the frame held while the auth ran", f.Raw)
			}
			if got := onlyConn(t, s).UserID(); got != userID {
				t.Fatalf("signed in as %q, want %q still", got, userID)
			}
		})
	}
}

func TestUnsupportedMethod(t *testing.T) {
	c := aprontest.Connect(t, wired(&backend{}, server.Config{}))
	signIn(t, c)
	// me and command without Profiles or Commands are unsupported too.
	for method, p := range map[string]string{"nope": `{}`, "me": `{"name":"B"}`, "command": `{"body":{"text":"/help"}}`} {
		if _, err := c.Call(wait(t), method, params(p)); apron.CodeOf(err) != apron.CodeUnsupported {
			t.Errorf("%s: %v, want unsupported", method, err)
		}
	}
	long := strings.Repeat("x", 300)
	send(t, c, `{"method":"message","id":"`+long+`","params":{"body":{"text":"hi"}}}`)
	if f := next(t, c); code(f) != apron.CodeTooLarge {
		t.Fatalf("got %s, want too_large for a long id", f.Raw)
	}
}

func TestHandle(t *testing.T) {
	s := wired(&backend{}, server.Config{})
	s.Handle("echo", server.Handler{Capability: "ext:echo", Serve: func(_ context.Context, _ *server.Conn, r *server.Request, reply server.Reply[any]) error {
		return reply.Send(r.Fields())
	}})
	s.Handle("fail", server.Handler{NoDedup: true, Serve: func(context.Context, *server.Conn, *server.Request, server.Reply[any]) error {
		return errors.New("secret detail")
	}})
	c := aprontest.Connect(t, s)
	if capabilities := c.Hello().Capabilities; !slices.Equal(capabilities, []string{"ext:echo"}) {
		t.Fatalf("capabilities = %v", capabilities)
	}
	signIn(t, c)
	if result, err := c.Call(wait(t), "echo", params(`{"b":1,"a":2}`)); err != nil || string(result) != `{"a":2,"b":1}` {
		t.Fatalf("echo: %s %v", result, err)
	}
	if _, err := c.Call(wait(t), "fail", nil); apron.CodeOf(err) != apron.CodeInternalError || strings.Contains(err.Error(), "secret") {
		t.Fatalf("got %v, want an internal error that says nothing", err)
	}
}

func TestMessageChecks(t *testing.T) {
	b := &backend{}
	c := aprontest.Connect(t, wired(b, server.Config{}))
	signIn(t, c)
	if result, err := c.Call(wait(t), "message", params(`{"body":{"text":""}}`)); apron.CodeOf(err) != apron.CodeInvalidParams {
		t.Fatalf("an empty message got %s %v, want invalid_params", result, err)
	}
	if b.messages.Load() != 0 {
		t.Fatal("an empty message was saved")
	}
	for _, tc := range []struct {
		params string
		code   int
	}{
		{`{"message_id":"1","body":{"text":"edit"}}`, apron.CodeUnsupported},
		{`{"message_id":"1","deleted":true}`, apron.CodeUnsupported},
		{`{}`, apron.CodeInvalidParams},
		{`{"body":{"text":"x","format":"html"}}`, apron.CodeInvalidParams},
		{`{"room_id":"a b","body":{"text":"x"}}`, apron.CodeInvalidParams},
		{`{"reply_to":{},"body":{"text":"x"}}`, apron.CodeInvalidParams},
		{`{"body":"x"}`, apron.CodeInvalidParams},
	} {
		if _, err := c.Call(wait(t), "message", params(tc.params)); apron.CodeOf(err) != tc.code {
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

func (e *editor) Edit(_ context.Context, _ *server.Conn, _ apron.EditParams, reply server.Reply[apron.MessageResult]) error {
	e.edits.Add(1)
	return reply.Send(apron.MessageResult{})
}

func TestEditor(t *testing.T) {
	b := &editor{}
	c := aprontest.Connect(t, wired(b, server.Config{}))
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
	synctest.Test(t, func(t *testing.T) {
		b := &backend{}
		s := wired(b, server.Config{OutgoingQueue: 2})
		// A client that reads nothing: the server frame is being written, and
		// the frames after it wait.
		serverEnd, client := net.Pipe()
		defer client.Close()
		go s.ServeTransport(context.Background(), server.LineTransport(serverEnd, 1<<16), nil, nil)
		synctest.Wait()
		conn := onlyConn(t, s)
		frame, _ := server.EncodeRaw("status", map[string]any{})
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
	})
}

func TestCloseWithError(t *testing.T) {
	b := &backend{}
	s := wired(b, server.Config{})
	c := aprontest.Connect(t, s)
	signIn(t, c)
	conn := onlyConn(t, s)
	conn.NotifyRaw("status", map[string]any{"n": 1})
	conn.CloseWithError(apron.RetryAfter("Going away", 7*time.Second))
	if conn.NotifyRaw("status", map[string]any{"n": 2}) {
		t.Fatal("a frame was queued after CloseWithError")
	}
	if f := next(t, c); f.Method != "status" {
		t.Fatalf("got %s, want the frame queued first", f.Raw)
	}
	f := next(t, c)
	data, _ := f.Error.Data.(map[string]any)
	if code(f) != apron.CodeRetryAfter || f.ID != "" || data["retry_after"] != float64(7) {
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
	serverEnd, conn := net.Pipe()
	defer conn.Close()
	transport := server.LineTransport(serverEnd, 16)
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
	c := aprontest.Connect(t, wired(&backend{}, server.Config{ReadLimit: 1 << 16}))
	// The write fails once the server stops reading and closes.
	go func() {
		_ = c.Send(context.Background(), []byte(`{"method":"message","id":"x","params":{"body":{"text":"`+strings.Repeat("x", 1<<17)+`"}}}`))
	}()
	expectClosed(t, c)
}

func TestShutdown(t *testing.T) {
	s := wired(&backend{}, server.Config{})
	c := aprontest.Connect(t, s)
	signIn(t, c)
	if err := s.Shutdown(wait(t)); err != nil {
		t.Fatal(err)
	}
	expectClosed(t, c)
	if len(s.Conns()) != 0 {
		t.Fatal("connections left after Shutdown")
	}
	// A connection after Shutdown is closed at once.
	serverEnd, conn := net.Pipe()
	defer conn.Close()
	s.ServeTransport(context.Background(), server.LineTransport(serverEnd, 1024), nil, nil)
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("a connection after Shutdown was served")
	}
}

func TestBroadcast(t *testing.T) {
	s := wired(&backend{}, server.Config{})
	c1, c2 := aprontest.Connect(t, s), aprontest.Connect(t, s) // c2 is not signed in: not broadcast to.
	signIn(t, c1)
	frame, _ := server.EncodeRaw("status", map[string]any{"x": 1})
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
	s := wired(&backend{}, server.Config{CheckOrigin: server.AnyOrigin})
	ts := httptest.NewServer(s)
	defer ts.Close()
	ctx := wait(t)
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http"), nil)
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

// fence is a test Pending.
type fence struct {
	done chan struct{}
	err  error
}

func (f *fence) Done() <-chan struct{} { return f.done }
func (f *fence) Err() error            { return f.err }

func TestSendFramesAndFence(t *testing.T) {
	s := wired(&backend{}, server.Config{OutgoingQueue: 2})
	c := aprontest.Connect(t, s)
	signIn(t, c)
	conn := onlyConn(t, s)
	f := &fence{done: make(chan struct{})}
	if !conn.HoldUntil(f) {
		t.Fatal("HoldUntil failed")
	}
	// Three frames in one place in the queue, behind the fence.
	a, _ := server.EncodeRaw("a", map[string]any{})
	b, _ := server.EncodeRaw("b", map[string]any{})
	if !conn.Send(a, b, a) || !conn.Send() {
		t.Fatal("Send failed")
	}
	quiet(t, c)
	close(f.done)
	for _, want := range []string{"a", "b", "a"} {
		if got := next(t, c); got.Method != want {
			t.Fatalf("got %s, want %s", got.Raw, want)
		}
	}

	failed := &fence{done: make(chan struct{}), err: errors.New("not stored")}
	conn.HoldUntil(failed)
	conn.Send(a)
	close(failed.done)
	if got := next(t, c); code(got) != apron.CodeInternalError || got.ID != "" {
		t.Fatalf("got %s, want internal_error in place of the frames behind a failed fence", got.Raw)
	}
	expectClosed(t, c)
}

// TestFenceDuringAuth holds the auth result behind a fence placed while
// Auth runs, and the notifications Auth sent after it.
func TestFenceDuringAuth(t *testing.T) {
	f := &fence{done: make(chan struct{})}
	b := &fencingBackend{fence: f}
	c := aprontest.Connect(t, wired(b, server.Config{}))
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

func (b *fencingBackend) Auth(_ context.Context, c *server.Conn, p apron.AuthParams, reply server.Reply[apron.AuthResult]) error {
	you, err := b.guest(c, p)
	if err != nil {
		return err
	}
	c.HoldUntil(b.fence)
	return reply.Send(apron.AuthResult{You: *you})
}

// Notification methods run with or without an id, only once signed in, and
// are never answered.
func TestNotificationMethod(t *testing.T) {
	s := wired(&backend{}, server.Config{})
	got := make(chan string, 4)
	s.HandleNotification("activity", server.NotificationHandler{Capability: "activity", Serve: func(_ context.Context, _ *server.Conn, r *server.Request) {
		v, _ := r.Fields().Get("typing")
		got <- string(v)
	}})
	c := aprontest.Connect(t, s)
	if capabilities := c.Hello().Capabilities; !slices.Contains(capabilities, "activity") {
		t.Fatalf("capabilities: %v", capabilities)
	}
	send(t, c, `{"method":"activity","params":{"typing":1}}`) // Before sign-in: ignored.
	signIn(t, c)
	send(t, c, `{"method":"activity","params":{"typing":2}}`)
	send(t, c, `{"method":"activity","id":"x","params":{"typing":3}}`)
	if err := c.Sync(wait(t)); err != nil {
		t.Fatal(err)
	}
	if a, b := <-got, <-got; a != "2" || b != "3" || len(got) != 0 {
		t.Fatalf("handled %s %s, %d more", a, b, len(got))
	}
}

type anyScheme struct{ backend }

func (*anyScheme) CheckScheme(_ context.Context, _ *server.Conn, p apron.AuthParams) error {
	if p.Scheme == "secret" {
		return nil
	}
	return apron.Denied("no")
}

func TestCheckScheme(t *testing.T) {
	b := &anyScheme{}
	c := aprontest.Connect(t, wired(b, server.Config{CheckScheme: b.CheckScheme}))
	if _, err := c.Auth(wait(t), map[string]any{"scheme": "guest"}); apron.CodeOf(err) != apron.CodeDenied {
		t.Fatalf("got %v", err)
	}
	if result, err := c.Auth(wait(t), map[string]any{"scheme": "secret"}); err != nil || result.You.UserID == "" {
		t.Fatalf("got %+v %v", result, err)
	}
}

func TestAnnounce(t *testing.T) {
	s := wired(&backend{}, server.Config{})
	s.Handle("ext:thing", server.Handler{Capability: "ext:thing", Serve: func(context.Context, *server.Conn, *server.Request, server.Reply[any]) error { return nil }})
	c := aprontest.Connect(t, s)
	signIn(t, c)
	conn := onlyConn(t, s)
	conn.Announce(apron.ServerInfo{Auth: []string{"guest"}, Title: "Revealed"})
	f := next(t, c)
	var info apron.ServerInfo
	if err := f.Decode(&info); err != nil || f.Method != "server" || info.Title != "Revealed" || info.Apron != apron.Version ||
		!slices.Contains(info.Capabilities, "ext:thing") {
		t.Fatalf("got %s", f.Raw)
	}
	if conn.ServerInfo().Title != "Revealed" {
		t.Fatalf("Info: %+v", conn.ServerInfo())
	}
	conn.SignOut()
	if _, err := c.Call(wait(t), "message", params(`{"body":{"text":"x"}}`)); apron.CodeOf(err) != apron.CodeDenied {
		t.Fatalf("after SignOut: %v", err)
	}
}

// A server frame announced while a network connection opens, once
// OnConnect has told the backend of it, goes after the one it opens with,
// and is the connection's ServerInfo.
func TestAnnounceWhileOpening(t *testing.T) {
	conns := make(chan *server.Conn, 1)
	s := wired(&backend{}, server.Config{OnConnect: func(_ context.Context, c *server.Conn) error {
		c.Announce(apron.ServerInfo{Title: "Revealed"})
		conns <- c
		return nil
	}})
	c := aprontest.Connect(t, s)
	if title := c.Hello().Title; title == "Revealed" {
		t.Fatalf("the first server frame is the announced one")
	}
	f := next(t, c)
	var info apron.ServerInfo
	if err := f.Decode(&info); err != nil || f.Method != "server" || info.Title != "Revealed" {
		t.Fatalf("got %s, want the announced server frame", f.Raw)
	}
	if conn := <-conns; conn.ServerInfo().Title != "Revealed" {
		t.Fatalf("ServerInfo: %+v", conn.ServerInfo())
	}
}

// OnDisconnect follows a network connection OnConnect refused, with an
// *Error or any other error, so that the backend undoes what OnConnect did
// in one place.
func TestDisconnectAfterRefusal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, refusal := range []error{apron.Denied("No"), errors.New("no")} {
			b := &backend{}
			var config server.Config
			config.OnConnect = func(_ context.Context, c *server.Conn) error {
				c.SetData("half done")
				return refusal
			}
			s := wired(b, config)
			client := aprontest.Connect(t, s)
			for {
				// The server frame and the error, for an *Error.
				if _, err := client.Next(wait(t)); err != nil {
					if !errors.Is(err, apron.ErrClosed) {
						t.Fatalf("refused with %v: %v", refusal, err)
					}
					break
				}
			}
			synctest.Wait()
			if d := b.disconnectedConns(); len(d) != 1 || d[0].Data() != "half done" {
				t.Fatalf("refused with %v: disconnected %v", refusal, d)
			}
		}
	})
}
