package wire_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/wire"
)

// A handler that replies itself, as a backend holding its own lock does,
// gets its result and its frames out together, and nothing else.
func TestReplyInHandler(t *testing.T) {
	s := &wire.Server{Backend: &backend{}}
	var mu sync.Mutex
	s.Handle("snapshot", wire.Method{Handle: func(ctx context.Context, c *wire.Conn, r *wire.Request) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		after, _ := wire.EncodeRaw("cursor", map[string]any{"n": 1})
		if wire.RequestFrom(ctx) != r || !r.Reply(map[string]any{"state": 1}, after) {
			t.Error("Reply failed")
		}
		if r.Reply(map[string]any{"state": 2}) {
			t.Error("a second Reply was sent")
		}
		return map[string]any{"ignored": true}, nil
	}})
	c, _ := serve(t, s)
	c.signIn("a")
	c.send(`{"method":"snapshot","id":"s"}`)
	if f := c.read(); f["id"] != "s" || f.result()["state"] != float64(1) {
		t.Fatalf("got %v", f)
	}
	if f := c.read(); f["method"] != "cursor" {
		t.Fatalf("got %v, want the frame after the reply", f)
	}
	c.sync()
	// A duplicate gets the reply that was sent.
	c.send(`{"method":"snapshot","id":"s"}`)
	if f := c.read(); f.result()["state"] != float64(1) {
		t.Fatalf("duplicate: %v", f)
	}
}

// Notification methods run with or without an id, only once signed in, and
// are never answered.
func TestNotificationMethod(t *testing.T) {
	s := &wire.Server{Backend: &backend{}}
	got := make(chan string, 4)
	s.Handle("activity", wire.Method{Capability: "activity", Notification: true, Handle: func(_ context.Context, _ *wire.Conn, r *wire.Request) (any, error) {
		v, _ := r.Param("typing")
		got <- string(v)
		return nil, errors.New("never sent")
	}})
	c, hello := serve(t, s)
	if !strings.Contains(string(mustJSON(hello)), `"activity"`) {
		t.Fatalf("capabilities: %v", hello)
	}
	c.send(`{"method":"activity","params":{"typing":1}}`) // Before sign-in: ignored.
	c.signIn("a")
	c.send(`{"method":"activity","params":{"typing":2}}`)
	c.send(`{"method":"activity","id":"x","params":{"typing":3}}`)
	c.sync()
	if a, b := <-got, <-got; a != "2" || b != "3" || len(got) != 0 {
		t.Fatalf("handled %s %s, %d more", a, b, len(got))
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

type refreshing struct {
	backend
	version int
}

func (b *refreshing) Refresh(_ *wire.Conn, method string, result jsontext.Value) any {
	return map[string]any{"method": method, "version": b.version}
}

var _ wire.Profiler = (*refreshing)(nil)

func (b *refreshing) Me(_ context.Context, c *wire.Conn, _ wire.MeParams) (wire.CurrentUser, error) {
	b.version++
	return wire.CurrentUser{User: *c.User()}, nil
}

func TestRefresher(t *testing.T) {
	b := &refreshing{}
	c, _ := serve(t, &wire.Server{Backend: b})
	c.signIn("a")
	c.send(`{"method":"me","id":"m","params":{"name":"x"}}`)
	c.read()
	b.version = 7
	c.send(`{"method":"me","id":"m","params":{"name":"x"}}`)
	if f := c.read(); f.result()["version"] != float64(7) || f.result()["method"] != "me" {
		t.Fatalf("duplicate: %v", f)
	}
}

type anyScheme struct{ backend }

func (*anyScheme) CheckScheme(_ *wire.Conn, p wire.AuthParams) error {
	if p.Scheme == "secret" {
		return nil
	}
	return wire.Denied("no")
}

func TestSchemeChecker(t *testing.T) {
	c, _ := serve(t, &wire.Server{Backend: &anyScheme{}})
	c.send(`{"method":"auth","id":"1","params":{"scheme":"guest"}}`)
	if f := c.read(); f.errorCode() != wire.CodeDenied {
		t.Fatalf("got %v", f)
	}
	c.send(`{"method":"auth","id":"2","params":{"scheme":"secret"}}`)
	if f := c.read(); f.result()["you"] == nil {
		t.Fatalf("got %v", f)
	}
}

func TestAnnounce(t *testing.T) {
	s := &wire.Server{Backend: &backend{}}
	s.Handle("ext:thing", wire.Method{Capability: "ext:thing", Handle: func(context.Context, *wire.Conn, *wire.Request) (any, error) { return nil, nil }})
	c, _ := serve(t, s)
	c.signIn("a")
	conn := onlyConn(t, s)
	conn.Announce(wire.ServerInfo{Auth: []string{"guest"}, Title: "Revealed"})
	f := c.read()
	params, _ := f["params"].(map[string]any)
	if f["method"] != "server" || params["title"] != "Revealed" || params["apron"] != float64(wire.Version) ||
		!strings.Contains(string(mustJSON(params["capabilities"])), "ext:thing") {
		t.Fatalf("got %v", f)
	}
	if conn.Info().Title != "Revealed" {
		t.Fatalf("Info: %+v", conn.Info())
	}
	conn.SignOut()
	c.send(`{"method":"message","id":"m","params":{"body":{"text":"x"}}}`)
	if f := c.read(); f.errorCode() != wire.CodeDenied {
		t.Fatalf("after SignOut: %v", f)
	}
}

func TestLocal(t *testing.T) {
	b := &backend{}
	s := &wire.Server{Backend: b}
	var once sync.Once
	s.Handle("whoami", wire.Method{Handle: func(ctx context.Context, c *wire.Conn, r *wire.Request) (any, error) {
		c.NotifyRaw("hello", map[string]any{})
		var err error
		once.Do(func() {
			// Calling back into the same connection would wait for itself.
			_, err = c.Data().(*wire.Local).Call(ctx, "whoami", nil)
		})
		if !errors.Is(err, wire.ErrReentrant) && err != nil {
			t.Errorf("reentrant call: %v", err)
		}
		return map[string]any{"user_id": c.UserID()}, nil
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
	s := &wire.Server{Backend: &backend{}}
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

func (b *backend) disconnectedConns() []*wire.Conn {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*wire.Conn(nil), b.disconnected...)
}
