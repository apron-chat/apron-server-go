package apron_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/aprontest"
)

// upstream answers commands later, as a gateway to a slow upstream does:
// each one but /now goes to calls, for the test to answer.
type upstream struct {
	oneUser
	calls chan pending
	// later counts the commands answered later.
	later atomic.Int64
}

// pending is a command answered later.
type pending struct {
	ctx   context.Context
	text  string
	reply apron.Reply[apron.CommandResult]
}

func newUpstream() *upstream { return &upstream{calls: make(chan pending, 8)} }

func (b *upstream) Command(ctx context.Context, _ *apron.Conn, p apron.CommandParams, reply apron.Reply[apron.CommandResult]) error {
	if p.Body.Text == "/now" {
		return reply.Send(apron.CommandResult{})
	}
	b.later.Add(1)
	if err := reply.Later(); err != nil {
		return err
	}
	b.calls <- pending{ctx, p.Body.Text, reply}
	if p.Body.Text == "/changed-mind" {
		return apron.Denied("not after all")
	}
	return nil
}

// take takes the next command answered later.
func (b *upstream) take(t *testing.T) pending {
	t.Helper()
	select {
	case c := <-b.calls:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no command reached the backend")
		return pending{}
	}
}

func result(text string) apron.CommandResult {
	return apron.CommandResult{Unknown: []byte(`{"text":"` + text + `"}`)}
}

// A request answered later does not hold up the frames after it: their
// replies come first, and its own once the backend sends it, from another
// goroutine, with the handler's ctx still live.
func TestLater(t *testing.T) {
	b := newUpstream()
	s := wired(b, apron.Config{})
	c := aprontest.Connect(t, s)
	signIn(t, c)
	send(t, c, `{"method":"command","id":"slow","params":{"body":{"text":"/slow"}}}`)
	pending := b.take(t)
	if _, err := c.Call(wait(t), "command", params(`{"body":{"text":"/now"}}`)); err != nil {
		t.Fatalf("a request after one answered later: %v", err)
	}
	if err := c.Sync(wait(t)); err != nil || len(c.Pending()) != 0 {
		t.Fatalf("sync: %v, %d frames, want no reply yet", err, len(c.Pending()))
	}
	if pending.ctx.Err() != nil {
		t.Fatal("the handler's ctx ended with the handler")
	}
	if err := pending.reply.Send(result("done")); err != nil {
		t.Fatal(err)
	}
	if f := next(t, c); f.ID != "slow" || string(f.Result) != `{"text":"done"}` {
		t.Fatalf("got %s, want the reply answered later", f.Raw)
	}
	if err := pending.reply.Send(result("again")); !errors.Is(err, apron.ErrReplied) {
		t.Fatalf("a second Send: %v, want ErrReplied", err)
	}
}

// A duplicate of a request answered later, on its connection or another,
// waits for the reply without holding up its connection, and gets its
// outcome; the backend runs the request once.
func TestLaterDuplicate(t *testing.T) {
	b := newUpstream()
	s := wired(b, apron.Config{})
	c1, c2 := aprontest.Connect(t, s), aprontest.Connect(t, s)
	signIn(t, c1)
	signIn(t, c2)
	const slow = `{"method":"command","id":"x","params":{"body":{"text":"/slow"}}}`
	send(t, c1, slow)
	pending := b.take(t)
	send(t, c1, slow)
	send(t, c2, slow)
	if _, err := c1.Call(wait(t), "command", params(`{"body":{"text":"/now"}}`)); err != nil {
		t.Fatalf("c1, behind a duplicate: %v", err)
	}
	if _, err := c2.Call(wait(t), "command", params(`{"body":{"text":"/now"}}`)); err != nil {
		t.Fatalf("c2, behind a duplicate: %v", err)
	}
	if err := pending.reply.Send(result("once")); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{string(next(t, c1).Raw), string(next(t, c1).Raw), string(next(t, c2).Raw)} {
		if f != `{"id":"x","result":{"text":"once"}}` {
			t.Errorf("got %s, want the outcome of the original", f)
		}
	}
	if n := b.later.Load(); n != 1 {
		t.Fatalf("the backend ran the command %d times, want once", n)
	}
	// Once answered, a duplicate is answered at once, in order.
	if _, before, err := c2.CallFrames(wait(t), "command", params(`{"body":{"text":"/now"}}`)); err != nil || len(before) != 0 {
		t.Fatalf("%v, %d frames before", err, len(before))
	}
	send(t, c2, slow)
	if f := next(t, c2); string(f.Raw) != `{"id":"x","result":{"text":"once"}}` {
		t.Fatalf("a duplicate after the reply: %s", f.Raw)
	}
}

// A request answered later that fails is not kept: a retry runs again.
func TestLaterFail(t *testing.T) {
	b := newUpstream()
	s := wired(b, apron.Config{Logger: slog.New(slog.DiscardHandler)})
	c := aprontest.Connect(t, s)
	signIn(t, c)
	const slow = `{"method":"command","id":"f","params":{"body":{"text":"/slow"}}}`
	send(t, c, slow)
	if err := b.take(t).reply.Fail(apron.RetryAfter("Upstream busy", time.Second)); err != nil {
		t.Fatal(err)
	}
	if f := next(t, c); f.ID != "f" || code(f) != apron.CodeRetryAfter {
		t.Fatalf("got %s, want the error", f.Raw)
	}
	send(t, c, slow)
	pending := b.take(t)
	// An error that is not an *Error is internal_error, and says nothing.
	if err := pending.reply.Fail(errors.New("secret detail")); err != nil {
		t.Fatal(err)
	}
	if f := next(t, c); code(f) != apron.CodeInternalError || strings.Contains(string(f.Raw), "secret") {
		t.Fatalf("got %s, want internal_error", f.Raw)
	}
	if err := pending.reply.Fail(apron.Denied("again")); !errors.Is(err, apron.ErrReplied) {
		t.Fatalf("a second Fail: %v, want ErrReplied", err)
	}
	// A handler that returns an error after Later is answered with it.
	if _, err := c.Call(wait(t), "command", params(`{"body":{"text":"/changed-mind"}}`)); apron.CodeOf(err) != apron.CodeDenied {
		t.Fatalf("got %v, want the handler's error", err)
	}
	if err := b.take(t).reply.Send(result("late")); !errors.Is(err, apron.ErrReplied) {
		t.Fatalf("Send after the handler's error: %v, want ErrReplied", err)
	}
}

// A request answered later whose connection ends fails: its duplicate on
// another connection is not left waiting, the outcome is not kept, and
// Send returns ErrClosed.
func TestLaterConnectionEnds(t *testing.T) {
	b := newUpstream()
	s := wired(b, apron.Config{})
	c1, c2 := aprontest.Connect(t, s), aprontest.Connect(t, s)
	signIn(t, c1)
	signIn(t, c2)
	const slow = `{"method":"command","id":"x","params":{"body":{"text":"/slow"}}}`
	send(t, c1, slow)
	pending := b.take(t)
	send(t, c2, slow)
	if err := c2.Sync(wait(t)); err != nil {
		t.Fatal(err)
	}
	_ = c1.Close()
	if f := next(t, c2); f.ID != "x" || code(f) != apron.CodeInternalError {
		t.Fatalf("the duplicate got %s, want internal_error", f.Raw)
	}
	if pending.ctx.Err() == nil {
		t.Fatal("the request's ctx outlived its connection")
	}
	if err := pending.reply.Send(result("too late")); !errors.Is(err, apron.ErrClosed) {
		t.Fatalf("Send after the connection ended: %v, want ErrClosed", err)
	}
	// Not kept: a retry runs again.
	send(t, c2, slow)
	if err := b.take(t).reply.Send(result("again")); err != nil {
		t.Fatal(err)
	}
	if f := next(t, c2); string(f.Result) != `{"text":"again"}` {
		t.Fatalf("got %s, want the retry's result", f.Raw)
	}
}

// laterAuth tries to answer auth later, which it may not.
type laterAuth struct{ backend }

func (*laterAuth) Auth(_ context.Context, _ *apron.Conn, _ apron.AuthParams, reply apron.Reply[apron.AuthResult]) error {
	return reply.Later()
}

// Auth is a barrier, and is never answered later.
func TestLaterAuth(t *testing.T) {
	c := aprontest.Connect(t, wired(&laterAuth{}, apron.Config{Logger: slog.New(slog.DiscardHandler)}))
	if _, err := c.Auth(wait(t), map[string]any{"scheme": "guest"}); apron.CodeOf(err) != apron.CodeInternalError {
		t.Fatalf("got %v, want internal_error", err)
	}
}

// A Local call waits for a reply answered later, and the next call on the
// connection does not; a Handler answers later as a typed handler does.
func TestLaterLocal(t *testing.T) {
	b := newUpstream()
	s := wired(b, apron.Config{})
	release := make(chan struct{})
	s.Handle("ext:slow", apron.Handler{Serve: func(ctx context.Context, c *apron.Conn, r *apron.Request, reply apron.Reply[any]) error {
		go func() {
			<-release
			// The work answered later may call the connection again.
			if _, err := c.Data().(*apron.Local).Call(ctx, "command", map[string]any{"body": map[string]any{"text": "/now"}}); err != nil {
				_ = reply.Fail(err)
				return
			}
			_ = reply.Send(map[string]any{"raw": true})
		}()
		return reply.Later()
	}})
	l := local(t, s)
	l.Conn().SetData(l)
	done := make(chan string, 1)
	go func() {
		result, err := call(t, l, "ext:slow", `{}`)
		if err != nil {
			result = err.Error()
		}
		done <- result
	}()
	time.Sleep(10 * time.Millisecond)
	if result, err := call(t, l, "command", `{"body":{"text":"/now"}}`); err != nil || result != `{}` {
		t.Fatalf("a call behind one answered later: %s %v", result, err)
	}
	select {
	case result := <-done:
		t.Fatalf("Call returned %s before the reply", result)
	default:
	}
	close(release)
	select {
	case result := <-done:
		if result != `{"raw":true}` {
			t.Fatalf("Call: %s", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Call did not return the reply answered later")
	}
}
