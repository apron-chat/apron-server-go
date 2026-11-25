package client_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/bot"
	"github.com/apron-chat/apron-server-go/apron/client"
	"github.com/apron-chat/apron-server-go/apron/server"
	"github.com/apron-chat/apron-server-go/apron/server/commands"
)

// *client.Conn is the network bot.Conn.
var _ bot.Conn = (*client.Conn)(nil)

// chat is a test backend: guests, and the token "ada"; one room whose
// messages everyone gets; history; profiles; and the commands /rename,
// which renames the sender as a server's own change would, and /wait,
// which answers once release is closed.
type chat struct {
	server   *server.Server
	clock    server.Clock
	commands commands.Router
	release  chan struct{}

	mu      sync.Mutex
	guests  int
	records []apron.Record
}

func newChat(config server.Config) *chat {
	c := &chat{release: make(chan struct{})}
	c.commands.MustAdd(commands.Command{Name: "rename", Run: func(_ context.Context, call *commands.Call) error {
		c.mu.Lock()
		old := *call.Conn.User()
		renamed := old
		renamed.Name = call.Args
		c.server.PublishUser(server.UserChange{Old: old, New: renamed}, nil)
		c.mu.Unlock()
		return call.Done()
	}})
	c.commands.MustAdd(commands.Command{Name: "wait", Run: func(_ context.Context, call *commands.Call) error {
		<-c.release
		return call.Done()
	}})
	config.Messages, config.Profiles, config.History, config.Commands = c, c, c, &c.commands
	c.server = server.New(c, config)
	return c
}

// serve serves c over HTTP for the test, and returns its address.
func (c *chat) serve(t *testing.T) string {
	ts := httptest.NewServer(c.server)
	t.Cleanup(ts.Close)
	return "ws" + strings.TrimPrefix(ts.URL, "http")
}

// serveInMemory serves handler over HTTP on connections in memory, until
// the test ends, and returns its address and the option that dials it. A
// test in a synctest bubble serves so, as a real network's waits are not
// the bubble's.
func serveInMemory(t *testing.T, handler http.Handler) (string, client.Option) {
	p := &pipes{conns: make(chan net.Conn), closed: make(chan struct{})}
	srv := &http.Server{Handler: handler}
	go srv.Serve(p)
	transport := &http.Transport{DialContext: p.dial}
	t.Cleanup(func() {
		_ = srv.Close()
		transport.CloseIdleConnections()
	})
	return "ws://chat.test/ws", client.HTTPClient(&http.Client{Transport: transport})
}

// pipes is a net.Listener whose connections are the server ends of
// net.Pipes that dial opens.
type pipes struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func (p *pipes) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	server, client := net.Pipe()
	select {
	case p.conns <- server:
		return client, nil
	case <-p.closed:
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *pipes) Accept() (net.Conn, error) {
	select {
	case conn := <-p.conns:
		return conn, nil
	case <-p.closed:
		return nil, net.ErrClosed
	}
}

func (p *pipes) Close() error {
	p.once.Do(func() { close(p.closed) })
	return nil
}

func (*pipes) Addr() net.Addr { return pipeAddr{} }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "chat.test" }

func (*chat) Hello(context.Context, *server.Conn) apron.ServerInfo {
	return apron.ServerInfo{Auth: []string{"guest", "token"}, Title: "Test"}
}

func (c *chat) Auth(_ context.Context, _ *server.Conn, p apron.AuthParams, reply server.Reply[apron.AuthResult]) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	you := apron.User{UserID: "ada", Name: "Ada"}
	switch {
	case p.Scheme == "guest":
		c.guests++
		you = apron.User{UserID: fmt.Sprintf("guest_%d", c.guests), Name: p.Name}
	case p.Token != "ada":
		return apron.Denied("Unknown token")
	}
	return reply.Send(apron.AuthResult{You: &apron.CurrentUser{User: you}})
}

func (c *chat) Message(_ context.Context, conn *server.Conn, p apron.MessageParams, reply server.Reply[apron.MessageResult]) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.clock.Next()
	m := apron.Message{MessageID: id, LogID: id, RoomID: "general", From: *conn.User(), Body: p.Body, ReplyTo: p.ReplyTo}
	frame, err := apron.Encode(m)
	if err != nil {
		return err
	}
	c.records = append(c.records, apron.Record{LogID: id, Message: &m})
	c.server.Broadcast(frame, nil)
	return reply.Send(apron.MessageResult{MessageID: id})
}

func (c *chat) History(_ context.Context, _ *server.Conn, p apron.HistoryParams, reply server.Reply[apron.HistoryResult]) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	log := server.Log{Records: c.records, Latest: c.clock.Last()}
	if len(c.records) > 0 {
		log.Lower = c.records[0].LogID
	}
	return reply.Send(server.Page(log, p, 50))
}

func (c *chat) Me(_ context.Context, conn *server.Conn, p apron.MeParams, reply server.Reply[apron.CurrentUser]) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	old := *conn.User()
	cur := server.ApplyMe(old, p)
	c.server.PublishUser(server.UserChange{Old: old, New: cur, Except: conn}, nil)
	return reply.Send(cur.Complete(old))
}

func timeout(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// frames collects what OnFrame gets.
type frames chan apron.Frame

func (f frames) option() client.Option {
	return client.OnFrame(func(frame apron.Frame) { f <- frame })
}

// next returns the next notification of T's method that match is true
// of, skipping the others.
func next[T apron.Notification](t *testing.T, f frames, match func(T) bool) T {
	t.Helper()
	var zero T
	deadline := time.After(5 * time.Second)
	for {
		select {
		case frame := <-f:
			var head struct {
				Method string         `json:"method"`
				Params jsontext.Value `json:"params"`
			}
			var n T
			if json.Unmarshal(frame, &head) == nil && head.Method == zero.Method() && json.Unmarshal(head.Params, &n) == nil && (match == nil || match(n)) {
				return n
			}
		case <-deadline:
			t.Fatalf("no %s frame", zero.Method())
			return zero
		}
	}
}

func dial(t *testing.T, url string, auth apron.AuthParams, opts ...client.Option) *client.Conn {
	t.Helper()
	conn, err := client.Dial(timeout(t), url, auth, opts...)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func text(s string) apron.MessageParams {
	return apron.MessageParams{Body: &apron.Body{Text: s}}
}

func TestRequests(t *testing.T) {
	ctx := timeout(t)
	c := newChat(server.Config{})
	url := c.serve(t)
	got := make(frames, 64)
	ada := dial(t, url, apron.AuthParams{Scheme: "token", Token: "ada"}, got.option())
	if hello := ada.Hello(); hello.Apron != apron.Version || hello.Title != "Test" {
		t.Fatalf("hello %+v", hello)
	}
	if u := ada.User(); u.UserID != "ada" || u.Name != "Ada" {
		t.Fatalf("user %+v", u)
	}
	// OnFrame gets the server frame first.
	if info := next[apron.ServerInfo](t, got, nil); info.Title != "Test" {
		t.Fatalf("server frame %+v", info)
	}

	sent, err := ada.Post(ctx, text("hi"))
	if err != nil || sent.MessageID == 0 {
		t.Fatalf("post: %+v %v", sent, err)
	}
	if m := next[apron.Message](t, got, nil); m.MessageID != sent.MessageID || m.Body.Text != "hi" {
		t.Fatalf("message %+v", m)
	}
	page, err := ada.History(ctx, apron.HistoryParams{})
	if err != nil || len(page.Messages) != 1 || page.Messages[0].MessageID != sent.MessageID {
		t.Fatalf("history: %+v %v", page, err)
	}

	// Error replies are *apron.Error.
	if _, err := ada.Post(ctx, apron.MessageParams{}); apron.CodeOf(err) != apron.CodeInvalidParams {
		t.Fatalf("post without a body: %v", err)
	}
	_, err = ada.Call(ctx, "nonsense", nil)
	if e, ok := errors.AsType[*apron.Error](err); !ok || e.Code != apron.CodeUnsupported {
		t.Fatalf("unknown method: %v", err)
	}
	if _, err := ada.Command(ctx, apron.CommandParams{Body: &apron.Body{Text: "/nonsense"}}); err == nil {
		t.Fatal("unknown command: no error")
	}

	// Another connection gets the message too, through its own OnFrame.
	others := make(frames, 64)
	guest := dial(t, url, apron.AuthParams{Scheme: "guest", Name: "Bob"}, others.option())
	if u := guest.User(); u.UserID != "guest_1" || u.Name != "Bob" {
		t.Fatalf("guest %+v", u)
	}
	if _, err := ada.Post(ctx, text("bob?")); err != nil {
		t.Fatal(err)
	}
	if m := next[apron.Message](t, others, nil); m.Body.Text != "bob?" || m.From.UserID != "ada" {
		t.Fatalf("guest's message %+v", m)
	}
}

// User follows the me results, and the user notifications about the
// connection's own user.
func TestUser(t *testing.T) {
	ctx := timeout(t)
	c := newChat(server.Config{})
	url := c.serve(t)
	got := make(frames, 64)
	ada := dial(t, url, apron.AuthParams{Scheme: "token", Token: "ada"}, got.option())

	avatar := "https://example.com/ada.png"
	you, err := ada.Me(ctx, apron.MeParams{Avatar: &avatar})
	if err != nil || you.Avatar != avatar || ada.User().Avatar != avatar || ada.User().Name != "Ada" {
		t.Fatalf("me: %+v %v; user %+v", you, err, ada.User())
	}
	// A change the server makes is a patch, which merges.
	if _, err := ada.Command(ctx, apron.CommandParams{Body: &apron.Body{Text: "/rename Ada L"}}); err != nil {
		t.Fatal(err)
	}
	update := next(t, got, func(u apron.UserUpdate) bool { return u.You != nil })
	if update.You.Avatar != "" {
		t.Fatalf("patch %+v carries the avatar", update.You)
	}
	if u := ada.User(); u.Name != "Ada L" || u.Avatar != avatar {
		t.Fatalf("user after the rename %+v", u)
	}
	empty := ""
	if _, err := ada.Me(ctx, apron.MeParams{Avatar: &empty}); err != nil {
		t.Fatal(err)
	}
	if u := ada.User(); u.Name != "Ada L" || u.Avatar != "" {
		t.Fatalf("user after clearing the avatar %+v", u)
	}
}

func TestDialErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := timeout(t)
		c := newChat(server.Config{MaxConnections: 1})
		url, memory := serveInMemory(t, c.server)

		// An auth error reply is returned as it is. Once all is still, the
		// server has let go of the connection, which leaves room for the next.
		_, err := client.Dial(ctx, url, apron.AuthParams{Scheme: "token", Token: "eve"}, memory)
		if apron.CodeOf(err) != apron.CodeDenied || errors.Is(err, apron.ErrClosed) {
			t.Fatalf("bad token: %v", err)
		}
		synctest.Wait()
		if _, err := client.Dial(ctx, url, apron.AuthParams{Scheme: "passkey"}, memory); apron.CodeOf(err) != apron.CodeUnsupported {
			t.Fatalf("unknown scheme: %v", err)
		}
		synctest.Wait()

		// A server at capacity says so, and closes the connection: not for
		// good.
		dial(t, url, apron.AuthParams{Scheme: "guest"}, memory)
		_, err = client.Dial(ctx, url, apron.AuthParams{Scheme: "guest"}, memory)
		if apron.CodeOf(err) != apron.CodeRetryAfter || errors.Is(err, apron.ErrClosed) {
			t.Fatalf("at capacity: %v", err)
		}

		if _, err := client.Dial(ctx, "ws://127.0.0.1:1/ws", apron.AuthParams{Scheme: "guest"}); err == nil || errors.Is(err, apron.ErrClosed) {
			t.Fatalf("nothing listening: %v", err)
		}
	})
}

func TestHeader(t *testing.T) {
	c := newChat(server.Config{CheckOrigin: func(r *http.Request) bool {
		return r.Header.Get("Origin") == "https://chat.example"
	}})
	url := c.serve(t)
	if _, err := client.Dial(timeout(t), url, apron.AuthParams{Scheme: "guest"}); err == nil {
		t.Fatal("dial without the origin")
	}
	dial(t, url, apron.AuthParams{Scheme: "guest"}, client.Header(http.Header{"Origin": {"https://chat.example"}}))
}

func TestClose(t *testing.T) {
	ctx := timeout(t)
	c := newChat(server.Config{})
	conn := dial(t, c.serve(t), apron.AuthParams{Scheme: "guest"})
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	<-conn.Done()
	if err := conn.Err(); err != apron.ErrClosed {
		t.Fatalf("err %v", err)
	}
	if _, err := conn.Post(ctx, text("hi")); !errors.Is(err, apron.ErrClosed) {
		t.Fatalf("post after close: %v", err)
	}
	if err := conn.Notify(ctx, "activity", nil); !errors.Is(err, apron.ErrClosed) {
		t.Fatalf("notify after close: %v", err)
	}
	if err := conn.Close(); !errors.Is(err, apron.ErrClosed) {
		t.Fatalf("second close: %v", err)
	}
	if conn.User().UserID == "" {
		t.Fatal("no user once closed")
	}
}

// A server that ends the connection fails the calls waiting on it.
func TestServerEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := timeout(t)
		c := newChat(server.Config{})
		url, memory := serveInMemory(t, c.server)
		conn := dial(t, url, apron.AuthParams{Scheme: "guest"}, memory)
		failed := make(chan error, 1)
		go func() {
			_, err := conn.Command(ctx, apron.CommandParams{Body: &apron.Body{Text: "/wait"}})
			failed <- err
		}()
		synctest.Wait() // The command waits.
		shutdown, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		go c.server.Shutdown(shutdown)
		select {
		case <-conn.Done():
		case <-ctx.Done():
			t.Fatal("the connection did not end")
		}
		if err := <-failed; !errors.Is(err, apron.ErrClosed) {
			t.Fatalf("waiting call: %v", err)
		}
		if err := conn.Err(); !errors.Is(err, apron.ErrClosed) {
			t.Fatalf("err %v", err)
		}
		close(c.release)
	})
}

// A call whose context is done returns, and leaves the connection open.
func TestCallContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newChat(server.Config{})
		url, memory := serveInMemory(t, c.server)
		conn := dial(t, url, apron.AuthParams{Scheme: "guest"}, memory)
		short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if _, err := conn.Command(short, apron.CommandParams{Body: &apron.Body{Text: "/wait"}}); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("call: %v", err)
		}
		close(c.release)
		if _, err := conn.Post(timeout(t), text("still here")); err != nil {
			t.Fatal(err)
		}
	})
}

// OnFrame may make calls and wait for them: their replies do not wait
// behind the frames it has yet to take.
func TestCallFromOnFrame(t *testing.T) {
	ctx := timeout(t)
	c := newChat(server.Config{})
	url := c.serve(t)
	var echo *client.Conn
	opened := make(chan struct{})
	echoed := make(chan error, 8)
	echo = dial(t, url, apron.AuthParams{Scheme: "guest", Name: "echo"}, client.OnFrame(func(frame apron.Frame) {
		<-opened
		var m struct {
			Params apron.Message `json:"params"`
		}
		if json.Unmarshal(frame, &m) != nil || m.Params.Body == nil || m.Params.From.UserID == echo.User().UserID {
			return
		}
		_, err := echo.Post(ctx, text("echo: "+m.Params.Body.Text))
		echoed <- err
	}))
	close(opened)
	ada := dial(t, url, apron.AuthParams{Scheme: "token", Token: "ada"})
	for range 3 {
		if _, err := ada.Post(ctx, text("hi")); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		if err := <-echoed; err != nil {
			t.Fatal(err)
		}
	}
}

// An OnFrame that lets too many frames pile up ends the connection.
func TestFellBehind(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := timeout(t)
		c := newChat(server.Config{})
		stuck := make(chan struct{})
		defer close(stuck)
		conn := dial(t, c.serve(t), apron.AuthParams{Scheme: "guest"}, client.OnFrame(func(apron.Frame) { <-stuck }))
		for i := 0; ; i++ {
			if _, err := conn.Post(ctx, text("x")); err != nil {
				if !errors.Is(err, apron.ErrClosed) || i < 1000 {
					t.Fatalf("post %d: %v", i, err)
				}
				break
			}
		}
		if err := conn.Err(); !errors.Is(err, client.ErrFellBehind) || !errors.Is(err, apron.ErrClosed) {
			t.Fatalf("err %v", err)
		}
	})
}

// A Conn pings as the server frame asks, and the connection stays open.
func TestPing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newChat(server.Config{PingInterval: time.Second})
		url, memory := serveInMemory(t, c.server)
		conn := dial(t, url, apron.AuthParams{Scheme: "guest"}, memory)
		if conn.Hello().Ping != 1 {
			t.Fatalf("ping %d", conn.Hello().Ping)
		}
		select {
		case <-conn.Done():
			t.Fatalf("ended: %v", conn.Err())
		case <-time.After(10 * time.Second):
		}
	})
}

// A server that stops answering pings is gone, once a ping has gone a
// whole interval unanswered.
func TestUnanswered(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pinged := make(chan struct{}, 8)
		url, memory := serveInMemory(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer ws.CloseNow()
			ctx := r.Context()
			ws.Write(ctx, websocket.MessageText, []byte(`{"method":"server","params":{"apron":10,"auth":["guest"],"ping":1}}`))
			for {
				_, frame, err := ws.Read(ctx)
				if err != nil {
					return
				}
				var request struct {
					ID     string `json:"id"`
					Method string `json:"method"`
				}
				json.Unmarshal(frame, &request)
				switch request.Method {
				case "auth":
					ws.Write(ctx, websocket.MessageText, []byte(`{"id":"`+request.ID+`","result":{"you":{"user_id":"x"}}}`))
				case "ping":
					pinged <- struct{}{}
				}
			}
		}))
		conn := dial(t, url, apron.AuthParams{Scheme: "guest"}, memory)
		dialed := time.Now()
		select {
		case <-conn.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("the connection did not end")
		}
		if took := time.Since(dialed); took != 2*time.Second {
			t.Errorf("ended after %v, want 2s: a ping at 1s, unanswered at 2s", took)
		}
		if err := conn.Err(); !errors.Is(err, client.ErrUnanswered) {
			t.Fatalf("err %v", err)
		}
		if len(pinged) == 0 {
			t.Fatal("no ping")
		}
	})
}

// A bot runs over the network on a Dialer of Dial.
func TestBot(t *testing.T) {
	ctx := timeout(t)
	c := newChat(server.Config{})
	url := c.serve(t)
	echo, err := bot.Start(func(ctx context.Context, onFrame func(apron.Frame)) (bot.Conn, error) {
		return client.Dial(ctx, url, apron.AuthParams{Scheme: "guest", Name: "echo"}, client.OnFrame(onFrame))
	}, bot.Options{
		OnMessage: func(ctx context.Context, b *bot.Bot, m apron.Message) {
			if b.Addressed(m) {
				b.Reply(ctx, m, "You said: "+m.Body.Text)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	if echo.User().Name != "echo" {
		t.Fatalf("bot user %+v", echo.User())
	}
	got := make(frames, 64)
	ada := dial(t, url, apron.AuthParams{Scheme: "token", Token: "ada"}, got.option())
	sent, err := ada.Post(ctx, text("echo: hi"))
	if err != nil {
		t.Fatal(err)
	}
	reply := next(t, got, func(m apron.Message) bool { return m.From.UserID == echo.UserID() })
	if reply.Body.Text != "You said: echo: hi" || reply.ReplyTo == nil || reply.ReplyTo.MessageID != sent.MessageID {
		t.Fatalf("reply %+v", reply)
	}
}
