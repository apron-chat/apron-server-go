package wire_test

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/wire"
)

// A handler that replies itself, as a backend holding its own lock does,
// gets its result and its frames out together, and nothing else.
func TestReplyInHandler(t *testing.T) {
	s := wired(&wire.Server{Backend: &backend{}})
	var mu sync.Mutex
	s.Handle("snapshot", wire.Handler{Serve: func(ctx context.Context, c *wire.Conn, r *wire.Request) error {
		mu.Lock()
		defer mu.Unlock()
		after, _ := wire.EncodeRaw("cursor", map[string]any{"n": 1})
		if !r.Reply(map[string]any{"state": 1}, after) {
			t.Error("Reply failed")
		}
		if r.Reply(map[string]any{"state": 2}) {
			t.Error("a second Reply was sent")
		}
		return nil
	}})
	c := connect(t, s)
	signIn(t, c)
	send(t, c, `{"method":"snapshot","id":"s"}`)
	if f := next(t, c); f.ID != "s" || fields(f)["state"] != float64(1) {
		t.Fatalf("got %s", f.Raw)
	}
	if f := next(t, c); f.Method != "cursor" {
		t.Fatalf("got %s, want the frame after the reply", f.Raw)
	}
	if err := c.Sync(wait(t)); err != nil || len(c.Pending()) != 0 {
		t.Fatalf("sync: %v, %d frames more", err, len(c.Pending()))
	}
	// A duplicate gets the reply that was sent.
	send(t, c, `{"method":"snapshot","id":"s"}`)
	if f := next(t, c); fields(f)["state"] != float64(1) {
		t.Fatalf("duplicate: %s", f.Raw)
	}
}

// Notification methods run with or without an id, only once signed in, and
// are never answered.
func TestNotificationMethod(t *testing.T) {
	s := wired(&wire.Server{Backend: &backend{}})
	got := make(chan string, 4)
	s.HandleNotification("activity", wire.NotificationHandler{Capability: "activity", Serve: func(_ context.Context, _ *wire.Conn, r *wire.Request) {
		v, _ := r.Param("typing")
		got <- string(v)
	}})
	c := connect(t, s)
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

type refreshing struct {
	backend
	version int
}

func (b *refreshing) Refresh(_ context.Context, _ *wire.Conn, method string, result jsontext.Value) any {
	return map[string]any{"method": method, "version": b.version}
}

func (b *refreshing) Me(_ context.Context, c *wire.Conn, _ wire.MeParams, reply wire.Reply[wire.CurrentUser]) error {
	b.version++
	return reply.Send(wire.CurrentUser{User: *c.User()})
}

func TestRefreshDuplicate(t *testing.T) {
	b := &refreshing{}
	c := connect(t, wired(&wire.Server{Backend: b, RefreshDuplicate: b.Refresh}))
	signIn(t, c)
	send(t, c, `{"method":"me","id":"m","params":{"name":"x"}}`)
	next(t, c)
	b.version = 7
	send(t, c, `{"method":"me","id":"m","params":{"name":"x"}}`)
	if f := fields(next(t, c)); f["version"] != float64(7) || f["method"] != "me" {
		t.Fatalf("duplicate: %v", f)
	}
}

type anyScheme struct{ backend }

func (*anyScheme) CheckScheme(_ context.Context, _ *wire.Conn, p wire.AuthParams) error {
	if p.Scheme == "secret" {
		return nil
	}
	return wire.Denied("no")
}

func TestCheckScheme(t *testing.T) {
	b := &anyScheme{}
	c := connect(t, wired(&wire.Server{Backend: b, CheckScheme: b.CheckScheme}))
	if _, err := c.Auth(wait(t), map[string]any{"scheme": "guest"}); wire.CodeOf(err) != wire.CodeDenied {
		t.Fatalf("got %v", err)
	}
	if result, err := c.Auth(wait(t), map[string]any{"scheme": "secret"}); err != nil || result.You == nil {
		t.Fatalf("got %+v %v", result, err)
	}
}

func TestAnnounce(t *testing.T) {
	s := wired(&wire.Server{Backend: &backend{}})
	s.Handle("ext:thing", wire.Handler{Capability: "ext:thing", Serve: func(context.Context, *wire.Conn, *wire.Request) error { return nil }})
	c := connect(t, s)
	signIn(t, c)
	conn := onlyConn(t, s)
	conn.Announce(wire.ServerInfo{Auth: []string{"guest"}, Title: "Revealed"})
	f := next(t, c)
	var info wire.ServerInfo
	if err := f.Decode(&info); err != nil || f.Method != "server" || info.Title != "Revealed" || info.Apron != wire.Version ||
		!slices.Contains(info.Capabilities, "ext:thing") {
		t.Fatalf("got %s", f.Raw)
	}
	if conn.Info().Title != "Revealed" {
		t.Fatalf("Info: %+v", conn.Info())
	}
	conn.SignOut()
	if _, err := c.Call(wait(t), "message", params(`{"body":{"text":"x"}}`)); wire.CodeOf(err) != wire.CodeDenied {
		t.Fatalf("after SignOut: %v", err)
	}
}

func TestLocal(t *testing.T) {
	b := &backend{}
	s := wired(&wire.Server{Backend: b})
	var once sync.Once
	s.Handle("whoami", wire.Handler{Serve: func(ctx context.Context, c *wire.Conn, r *wire.Request) error {
		c.NotifyRaw("hello", map[string]any{})
		var err error
		once.Do(func() {
			// Calling back into the same connection would wait for itself.
			_, err = c.Data().(*wire.Local).Call(ctx, "whoami", nil)
		})
		if !errors.Is(err, wire.ErrReentrant) && err != nil {
			t.Errorf("reentrant call: %v", err)
		}
		r.Reply(map[string]any{"user_id": c.UserID()})
		return nil
	}})
	frames := make(chan string, 8)
	l, err := s.Local(wire.LocalOptions{User: wire.User{UserID: "bot"}, OnFrame: func(f wire.Frame) { frames <- string(f) }})
	if err != nil {
		t.Fatal(err)
	}
	l.Conn().SetData(l)
	result, err := l.Call(context.Background(), "whoami", nil)
	if err != nil || !strings.Contains(string(result), `"bot"`) {
		t.Fatalf("Call: %s %v", result, err)
	}
	if f := <-frames; !strings.HasPrefix(f, `{"method":"server"`) {
		t.Fatalf("first frame: %s, want the server frame", f)
	}
	if f := <-frames; !strings.Contains(f, `"hello"`) {
		t.Fatalf("OnFrame: %s", f)
	}
	var e *wire.Error
	if _, err := l.Call(context.Background(), "nope", nil); !errors.As(err, &e) || e.Code != wire.CodeUnsupported {
		t.Fatalf("unknown method: %v", err)
	}
	if _, err := l.Call(context.Background(), "message", map[string]any{"body": map[string]any{"text": "hi"}}); err != nil || b.messages.Load() != 1 {
		t.Fatalf("message: %v", err)
	}
	if len(s.Conns()) != 1 {
		t.Fatalf("%d connections", len(s.Conns()))
	}
	l.Close()
	select {
	case <-l.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Local did not close")
	}
	if _, err := l.Call(context.Background(), "whoami", nil); !errors.Is(err, wire.ErrClosed) {
		t.Fatalf("after Close: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(s.Conns()) != 0 || len(b.disconnectedConns()) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the Local connection was not forgotten")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLocalFellBehind(t *testing.T) {
	s := wired(&wire.Server{Backend: &backend{}})
	block := make(chan struct{})
	defer close(block)
	l, err := s.Local(wire.LocalOptions{User: wire.User{UserID: "bot"}, Queue: 2, OnFrame: func(wire.Frame) { <-block }})
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		l.Conn().NotifyRaw("x", map[string]any{})
	}
	select {
	case <-l.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a Local whose OnFrame fell behind stayed open")
	}
	if !errors.Is(l.Err(), wire.ErrFellBehind) {
		t.Fatalf("Err: %v", l.Err())
	}
}

// A Local connection opens as a network one does: OnConnect, signed in
// already, then Hello, whose server frame goes before anything sent to it
// meanwhile. An error of OnConnect is Local's, and no OnDisconnect follows.
func TestLocalOpens(t *testing.T) {
	refuse := errors.New("refused")
	var disconnects atomic.Int64
	s := wired(&wire.Server{Backend: &backend{}})
	s.OnConnect = func(_ context.Context, c *wire.Conn) error {
		if c.HTTPRequest() != nil || c.Data() == nil {
			t.Errorf("OnConnect: request %v, data %v", c.HTTPRequest(), c.Data())
		}
		if c.UserID() == "nobody" {
			return refuse
		}
		c.NotifyRaw("welcome", map[string]any{"to": c.UserID()})
		return nil
	}
	s.OnDisconnect = func(*wire.Conn) { disconnects.Add(1) }
	frames := make(chan string, 8)
	l, err := s.Local(wire.LocalOptions{User: wire.User{UserID: "bot"}, Data: 1, OnFrame: func(f wire.Frame) { frames <- string(f) }})
	if err != nil {
		t.Fatal(err)
	}
	if f := <-frames; !strings.HasPrefix(f, `{"method":"server"`) {
		t.Fatalf("first frame %s, want the server frame", f)
	}
	if f := <-frames; f != `{"method":"welcome","params":{"to":"bot"}}` {
		t.Fatalf("second frame %s, want the welcome", f)
	}
	if info := l.Conn().Info(); info.Agent != "wire-test/1" {
		t.Fatalf("Info: %+v", info)
	}
	if _, err := s.Local(wire.LocalOptions{User: wire.User{UserID: "nobody"}, Data: 1}); err != refuse {
		t.Fatalf("a refused Local: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	<-l.Done()
	if err := l.Close(); !errors.Is(err, wire.ErrClosed) {
		t.Fatalf("a second Close: %v", err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := disconnects.Load(); n != 1 {
		t.Fatalf("%d disconnects, want the one of the connection that opened", n)
	}
	if _, err := s.Local(wire.LocalOptions{User: wire.User{UserID: "bot"}}); !errors.Is(err, wire.ErrClosed) {
		t.Fatalf("after Shutdown: %v", err)
	}
}
