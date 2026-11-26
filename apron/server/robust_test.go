package server_test

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
	"testing/synctest"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/aprontest"
	"github.com/apron-chat/apron-server-go/apron/server"
)

// The Decoder's errors are plain errors, nil when there is none, and
// *Errors underneath when there is.
func TestParamErrors(t *testing.T) {
	read := func(f server.Fields, name string) error {
		d := f.Decoder()
		d.String(name, server.Optional)
		return d.Err()
	}
	if err := read(server.Fields{}, "x"); err != nil {
		t.Fatalf("an absent field: %v", err)
	}
	err := read(server.Fields{"x": []byte("1")}, "x")
	if apron.CodeOf(err) != apron.CodeInvalidParams {
		t.Fatalf("a number for a string: %v", err)
	}
	if _, ok := errors.AsType[*apron.Error](err); !ok {
		t.Fatalf("%T is not an *Error", err)
	}
	for err, want := range map[error]int{
		nil:                                     0,
		errors.New("x"):                         apron.CodeInternalError,
		apron.Denied("no"):                      apron.CodeDenied,
		fmt.Errorf("w: %w", apron.TooLarge("")): apron.CodeTooLarge,
	} {
		if got := apron.CodeOf(err); got != want {
			t.Errorf("CodeOf(%v) = %d, want %d", err, got, want)
		}
	}
}

// panicky is a backend whose Auth panics for the name "panic".
type panicky struct{ backend }

func (b *panicky) Auth(ctx context.Context, c *server.Conn, p apron.AuthParams, reply server.Reply[apron.AuthResult]) error {
	if p.Name == "panic" {
		c.Notify(apron.Notice("", "held"))
		panic("auth went wrong")
	}
	return b.backend.Auth(ctx, c, p, reply)
}

// A panicking handler is answered error/internal_error, its outcome is not
// kept, and the connection goes on.
func TestHandlerPanics(t *testing.T) {
	s := wired(&panicky{}, server.Config{Logger: slog.New(slog.DiscardHandler)})
	var calls atomic.Int64
	s.Handle("boom", server.Handler{Serve: func(_ context.Context, _ *server.Conn, r *server.Request, reply server.Reply[any]) error {
		if calls.Add(1) == 1 {
			panic("first time")
		}
		return reply.Send(map[string]any{"ok": true})
	}})
	s.HandleNotification("tick", server.NotificationHandler{Serve: func(context.Context, *server.Conn, *server.Request) {
		panic("in a notification")
	}})
	c := aprontest.Connect(t, s)
	if _, err := c.Auth(wait(t), map[string]any{"scheme": "guest", "name": "panic"}); apron.CodeOf(err) != apron.CodeInternalError {
		t.Fatalf("got %v, want the auth's internal_error", err)
	}
	// The frame held during the failed auth is dropped: the next is the
	// sign-in's result.
	signIn(t, c)
	send(t, c, `{"method":"boom","id":"b"}`)
	if f := next(t, c); f.ID != "b" || code(f) != apron.CodeInternalError {
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
	synctest.Test(t, func(t *testing.T) {
		var logged bytes.Buffer
		var mu sync.Mutex
		s := wired(&backend{}, server.Config{Logger: slog.New(slog.NewTextHandler(lockedWriter{&mu, &logged}, &slog.HandlerOptions{Level: slog.LevelDebug}))})
		s.Handle("late", server.Handler{Serve: func(_ context.Context, _ *server.Conn, r *server.Request, reply server.Reply[any]) error {
			_ = reply.Send(nil)
			return apron.Denied("too late")
		}})
		c := aprontest.Connect(t, s)
		signIn(t, c)
		if _, err := c.Call(wait(t), "late", nil); err != nil {
			t.Fatal(err)
		}
		// The client has the reply before the handler returns its error, which
		// is logged once it does.
		logs := func() string {
			mu.Lock()
			defer mu.Unlock()
			return logged.String()
		}
		synctest.Wait()
		if out := logs(); !strings.Contains(out, "level=DEBUG") || !strings.Contains(out, "too late") {
			t.Fatalf("log: %s", out)
		}
	})
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

func (*oneUser) Auth(_ context.Context, c *server.Conn, _ apron.AuthParams, reply server.Reply[apron.AuthResult]) error {
	c.Notify(apron.Notice("", "welcome"))
	return reply.Send(apron.AuthResult{You: &apron.CurrentUser{UserID: "same"}})
}

// A duplicate waiting for its original on another connection stops waiting
// when its own connection ends.
func TestDuplicateWaitEndsWithConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := &oneUser{}
		s := wired(b, server.Config{})
		started, release := make(chan struct{}), make(chan struct{})
		defer close(release)
		s.Handle("slow", server.Handler{Serve: func(context.Context, *server.Conn, *server.Request, server.Reply[any]) error {
			close(started)
			<-release
			return nil
		}})
		c1, c2 := aprontest.Connect(t, s), aprontest.Connect(t, s)
		signIn(t, c1)
		signIn(t, c2)
		send(t, c1, `{"method":"slow","id":"x"}`)
		<-started
		send(t, c2, `{"method":"slow","id":"x"}`)
		synctest.Wait() // Let it start waiting.
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
	})
}

// Local notifications are handled, and a Call of a notification-only method
// returns at once.
func TestLocalNotify(t *testing.T) {
	s := wired(&backend{}, server.Config{})
	got := make(chan string, 1)
	s.HandleNotification("activity", server.NotificationHandler{Capability: "activity", Serve: func(_ context.Context, _ *server.Conn, r *server.Request) {
		v := r.Fields()["typing"]
		got <- string(v)
	}})
	l, err := s.Local(server.LocalOptions{User: apron.User{UserID: "bot"}})
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
		if _, err := l.Call(ctx, method, nil); !errors.Is(err, server.ErrNotRequest) {
			t.Errorf("Call(%s): %v, want ErrNotRequest", method, err)
		}
	}
}

// connecting is a backend that counts the connections it is told of.
type connecting struct {
	backend
	connected atomic.Int64
}

func (b *connecting) Connected(context.Context, *server.Conn) error {
	b.connected.Add(1)
	return nil
}

// A connection over MaxConnections is refused before the backend is told of
// it, though it is sent the server frame.
func TestMaxConnectionsFirst(t *testing.T) {
	b := &connecting{}
	s := wired(b, server.Config{MaxConnections: 1, OnConnect: b.Connected})
	c1, c2 := aprontest.Connect(t, s), aprontest.Connect(t, s)
	if f := next(t, c2); code(f) != apron.CodeRetryAfter {
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
	s := wired(&backend{}, server.Config{})
	roles := []string{"op"}
	l, err := s.Local(server.LocalOptions{User: apron.User{UserID: "bot", Roles: roles, Ext: apron.Ext{}.With("x", map[string]any{"y": 1})}})
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

// Marshal encodes as frames are: keys sorted, nil as null, HTML characters
// as they are.
func TestMarshal(t *testing.T) {
	got, err := apron.Marshal(map[string]any{"b": []string(nil), "a": "<&>"})
	if err != nil || string(got) != `{"a":"<&>","b":null}` {
		t.Fatalf("Marshal = %s, %v", got, err)
	}
}
