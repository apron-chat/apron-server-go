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

func (b *panicky) Auth(ctx context.Context, c *wire.Conn, p wire.AuthParams) (wire.AuthResult, error) {
	if p.Name == "panic" {
		c.SendMessage(wire.Notice("", "held"))
		panic("auth went wrong")
	}
	return b.backend.Auth(ctx, c, p)
}

// A panicking handler is answered error/internal_error, its outcome is not
// kept, and the connection goes on.
func TestHandlerPanics(t *testing.T) {
	s := &wire.Server{Backend: &panicky{}, Logger: slog.New(slog.DiscardHandler)}
	var calls atomic.Int64
	s.Handle("boom", wire.Method{Handle: func(context.Context, *wire.Conn, *wire.Request) (any, error) {
		if calls.Add(1) == 1 {
			panic("first time")
		}
		return map[string]any{"ok": true}, nil
	}})
	s.Handle("tick", wire.Method{Notification: true, Handle: func(context.Context, *wire.Conn, *wire.Request) (any, error) {
		panic("in a notification")
	}})
	c, _ := serve(t, s)
	c.send(`{"method":"auth","id":"p","params":{"scheme":"guest","name":"panic"}}`)
	if f := c.read(); f["id"] != "p" || f.errorCode() != wire.CodeInternalError {
		t.Fatalf("got %v, want the auth's internal_error", f)
	}
	if f := c.read(); f["method"] != "message" {
		t.Fatalf("got %v, want the frame held during auth", f)
	}
	c.signIn("a")
	c.send(`{"method":"boom","id":"b"}`)
	if f := c.read(); f["id"] != "b" || f.errorCode() != wire.CodeInternalError {
		t.Fatalf("got %v, want internal_error", f)
	}
	c.send(`{"method":"tick"}`)
	c.sync()
	// The failure is not kept: a retry runs again.
	c.send(`{"method":"boom","id":"b"}`)
	if f := c.read(); f["id"] != "b" || f.result()["ok"] != true {
		t.Fatalf("retry: got %v", f)
	}
}

// An error returned after the reply is logged, at debug level for an *Error.
func TestErrorAfterReply(t *testing.T) {
	var logged bytes.Buffer
	var mu sync.Mutex
	s := &wire.Server{Backend: &backend{}, Logger: slog.New(slog.NewTextHandler(lockedWriter{&mu, &logged}, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	s.Handle("late", wire.Method{Handle: func(_ context.Context, _ *wire.Conn, r *wire.Request) (any, error) {
		r.Reply(nil)
		return nil, wire.Denied("too late")
	}})
	c, _ := serve(t, s)
	c.signIn("a")
	c.send(`{"method":"late","id":"l"}`)
	if f := c.read(); f["id"] != "l" || f["error"] != nil {
		t.Fatalf("got %v", f)
	}
	c.sync()
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

// Local notifications are handled, and a Call of a notification-only method
// returns at once.
func TestLocalNotify(t *testing.T) {
	s := &wire.Server{Backend: &backend{}}
	got := make(chan string, 1)
	s.Handle("activity", wire.Method{Capability: "activity", Notification: true, Handle: func(_ context.Context, _ *wire.Conn, r *wire.Request) (any, error) {
		v, _ := r.Param("typing")
		got <- string(v)
		return nil, nil
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
