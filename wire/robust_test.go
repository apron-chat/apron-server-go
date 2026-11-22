package wire_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/wire"
)

// The accessors' errors are plain errors, nil when there is none, and
// *Errors underneath when there is.
func TestParamErrors(t *testing.T) {
	read := func(f wire.Fields, name string) error {
		_, _, err := f.String(name)
		return err
	}
	if err := read(wire.Fields{}, "x"); err != nil {
		t.Fatalf("an absent field: %v", err)
	}
	err := read(wire.Fields{"x": []byte("1")}, "x")
	if wire.CodeOf(err) != wire.CodeInvalidParams {
		t.Fatalf("a number for a string: %v", err)
	}
	if _, ok := errors.AsType[*wire.Error](err); !ok {
		t.Fatalf("%T is not an *Error", err)
	}
	for err, want := range map[error]int{
		nil:                                    0,
		errors.New("x"):                        wire.CodeInternalError,
		wire.Denied("no"):                      wire.CodeDenied,
		fmt.Errorf("w: %w", wire.TooLarge("")): wire.CodeTooLarge,
	} {
		if got := wire.CodeOf(err); got != want {
			t.Errorf("CodeOf(%v) = %d, want %d", err, got, want)
		}
	}
}

// panicky is a backend whose Auth panics for the name "panic".
type panicky struct{ backend }

func (b *panicky) Auth(ctx context.Context, c *wire.Conn, p wire.AuthParams, reply wire.Reply[wire.AuthResult]) error {
	if p.Name == "panic" {
		c.Notify(wire.Notice("", "held"))
		panic("auth went wrong")
	}
	return b.backend.Auth(ctx, c, p, reply)
}

// A panicking handler is answered error/internal_error, its outcome is not
// kept, and the connection goes on.
func TestHandlerPanics(t *testing.T) {
	s := wired(&wire.Server{Backend: &panicky{}, Logger: slog.New(slog.DiscardHandler)})
	var calls atomic.Int64
	s.Handle("boom", wire.Handler{Serve: func(_ context.Context, _ *wire.Conn, r *wire.Request) error {
		if calls.Add(1) == 1 {
			panic("first time")
		}
		r.Reply(map[string]any{"ok": true})
		return nil
	}})
	s.HandleNotification("tick", wire.NotificationHandler{Serve: func(context.Context, *wire.Conn, *wire.Request) {
		panic("in a notification")
	}})
	c := connect(t, s)
	if _, err := c.Auth(wait(t), map[string]any{"scheme": "guest", "name": "panic"}); wire.CodeOf(err) != wire.CodeInternalError {
		t.Fatalf("got %v, want the auth's internal_error", err)
	}
	// The frame held during the failed auth is dropped: the next is the
	// sign-in's result.
	signIn(t, c)
	send(t, c, `{"method":"boom","id":"b"}`)
	if f := next(t, c); f.ID != "b" || code(f) != wire.CodeInternalError {
		t.Fatalf("got %s, want internal_error", f.Raw)
	}
	if err := c.Notify(wait(t), "tick", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Sync(wait(t)); err != nil {
		t.Fatal(err)
	}
	// The failure is not kept: a retry runs again.
	send(t, c, `{"method":"boom","id":"b"}`)
	if f := next(t, c); f.ID != "b" || fields(f)["ok"] != true {
		t.Fatalf("retry: got %s", f.Raw)
	}
}

// An error returned after the reply is logged, at debug level for an *Error.
func TestErrorAfterReply(t *testing.T) {
	var logged bytes.Buffer
	var mu sync.Mutex
	s := wired(&wire.Server{Backend: &backend{}, Logger: slog.New(slog.NewTextHandler(lockedWriter{&mu, &logged}, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	s.Handle("late", wire.Handler{Serve: func(_ context.Context, _ *wire.Conn, r *wire.Request) error {
		r.Reply(nil)
		return wire.Denied("too late")
	}})
	c := connect(t, s)
	signIn(t, c)
	if _, err := c.Call(wait(t), "late", nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if out := logged.String(); !strings.Contains(out, "level=DEBUG") || !strings.Contains(out, "too late") {
		t.Fatalf("log: %s", out)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

// oneUser signs every connection in as the same user, so that they share
// request ids (§1.2).
type oneUser struct{ backend }

func (*oneUser) Auth(_ context.Context, c *wire.Conn, _ wire.AuthParams, reply wire.Reply[wire.AuthResult]) error {
	c.Notify(wire.Notice("", "welcome"))
	return reply.Send(wire.AuthResult{You: &wire.CurrentUser{User: wire.User{UserID: "same"}}})
}

// A duplicate waiting for its original on another connection stops waiting
// when its own connection ends.
func TestDuplicateWaitEndsWithConnection(t *testing.T) {
	b := &oneUser{}
	s := wired(&wire.Server{Backend: b})
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	s.Handle("slow", wire.Handler{Serve: func(context.Context, *wire.Conn, *wire.Request) error {
		close(started)
		<-release
		return nil
	}})
	c1, c2 := connect(t, s), connect(t, s)
	signIn(t, c1)
	signIn(t, c2)
	send(t, c1, `{"method":"slow","id":"x"}`)
	<-started
	send(t, c2, `{"method":"slow","id":"x"}`)
	time.Sleep(20 * time.Millisecond) // Let it start waiting.
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		_ = c2.Close()
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the duplicate's connection did not end while its original ran")
	}
	if n := len(b.disconnectedConns()); n != 1 {
		t.Fatalf("%d connections ended, want the duplicate's", n)
	}
}

// Local notifications are handled, and a Call of a notification-only method
// returns at once.
func TestLocalNotify(t *testing.T) {
	s := wired(&wire.Server{Backend: &backend{}})
	got := make(chan string, 1)
	s.HandleNotification("activity", wire.NotificationHandler{Capability: "activity", Serve: func(_ context.Context, _ *wire.Conn, r *wire.Request) {
		v, _ := r.Param("typing")
		got <- string(v)
	}})
	l, err := s.Local(wire.LocalOptions{User: wire.User{UserID: "bot"}})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.Notify(context.Background(), "activity", map[string]any{"typing": 4}); err != nil {
		t.Fatal(err)
	}
	if v := <-got; v != "4" {
		t.Fatalf("typing = %s", v)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, method := range []string{"ping", "activity"} {
		if _, err := l.Call(ctx, method, nil); !errors.Is(err, wire.ErrNotRequest) {
			t.Errorf("Call(%s): %v, want ErrNotRequest", method, err)
		}
	}
}

// connecting is a backend that counts the connections it is told of.
type connecting struct {
	backend
	connected atomic.Int64
}

func (b *connecting) Connected(context.Context, *wire.Conn) error {
	b.connected.Add(1)
	return nil
}

// A connection over MaxConnections is refused before the backend is told of
// it, though it is sent the server frame.
func TestMaxConnectionsFirst(t *testing.T) {
	b := &connecting{}
	s := wired(&wire.Server{Backend: b, MaxConnections: 1, OnConnect: b.Connected})
	c1, c2 := connect(t, s), connect(t, s)
	if f := next(t, c2); code(f) != wire.CodeRetryAfter {
		t.Fatalf("got %s, want retry_after", f.Raw)
	}
	expectClosed(t, c2)
	if err := c1.Sync(wait(t)); err != nil {
		t.Fatal(err)
	}
	if n := b.connected.Load(); n != 1 {
		t.Fatalf("Connected %d times, want 1", n)
	}
	if n := len(b.disconnectedConns()); n != 0 {
		t.Fatalf("Disconnected %d times for a refused connection", n)
	}
}

// Conn.User and SetUser copy the user, roles and ext included.
func TestUserCopies(t *testing.T) {
	s := wired(&wire.Server{Backend: &backend{}})
	roles := []string{"op"}
	l, err := s.Local(wire.LocalOptions{User: wire.User{UserID: "bot", Roles: roles, Ext: wire.MustExt("x", map[string]any{"y": 1})}})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	roles[0] = "changed"
	u := l.Conn().User()
	u.Roles[0] = "mutated"
	u.Ext["x"][5] = '2'
	again := l.Conn().User()
	if again.Roles[0] != "op" || string(again.Ext["x"]) != `{"y":1}` {
		t.Fatalf("the connection's user changed: %+v", again)
	}
}

// DataOf returns the connection's value as its type.
func TestDataOf(t *testing.T) {
	s := wired(&wire.Server{Backend: &backend{}})
	type session struct{ name string }
	l, err := s.Local(wire.LocalOptions{User: wire.User{UserID: "bot"}, Data: &session{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if got, ok := wire.DataOf[*session](l.Conn()); !ok || got.name != "x" {
		t.Fatalf("DataOf = %v, %v", got, ok)
	}
	if got, ok := wire.DataOf[string](l.Conn()); ok || got != "" {
		t.Fatalf("DataOf of another type = %q, %v", got, ok)
	}
}

// Marshal encodes as frames are: keys sorted, nil as null, HTML characters
// as they are.
func TestMarshal(t *testing.T) {
	got, err := wire.Marshal(map[string]any{"b": []string(nil), "a": "<&>"})
	if err != nil || string(got) != `{"a":"<&>","b":null}` {
		t.Fatalf("Marshal = %s, %v", got, err)
	}
}
