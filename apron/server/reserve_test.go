package server_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/aprontest"
	"github.com/apron-chat/apron-server-go/apron/server"
)

// placed is a request whose handler queued a notice before and after it
// placed its reply (Reserve, Later, or neither), and left the reply to the
// test.
type placed struct {
	conn  *server.Conn
	reply server.Reply[any]
}

// placing serves ext:place, whose handler sends the notice "before",
// places its reply as how says, sends "after", and hands the reply to the
// test; with hold, it first holds the connection's frames until hold is
// done (Conn.HoldUntil).
func placing(t *testing.T, config server.Config, how string, hold server.Pending) (*aprontest.Client, chan placed) {
	t.Helper()
	handed := make(chan placed, 4)
	s := wired(&backend{}, config)
	s.Handle("ext:place", server.Handler{Serve: func(_ context.Context, c *server.Conn, _ *server.Request, reply server.Reply[any]) error {
		if hold != nil {
			c.HoldUntil(hold)
		}
		c.Notify(apron.Notice("", "before"))
		var err error
		switch how {
		case "reserve":
			err = reply.Reserve()
		case "later":
			err = reply.Later()
		}
		if err != nil {
			return err
		}
		c.Notify(apron.Notice("", "after"))
		handed <- placed{c, reply}
		return nil
	}})
	c := aprontest.Connect(t, s)
	signIn(t, c)
	return c, handed
}

// briefly is a context for a wait that is to time out.
func briefly(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	t.Cleanup(cancel)
	return ctx
}

func take(t *testing.T, handed chan placed) placed {
	t.Helper()
	select {
	case p := <-handed:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not run")
		return placed{}
	}
}

// order is what a client is sent: notices by their text, and replies by
// their id.
func order(t *testing.T, c *aprontest.Client, n int) []string {
	t.Helper()
	var got []string
	for range n {
		f := next(t, c)
		if f.Method == "message" {
			var m apron.Message
			_ = f.Decode(&m)
			got = append(got, m.Body.Text)
		} else {
			got = append(got, "reply "+f.ID)
		}
	}
	return got
}

func expectOrder(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

// A reply sent after Reserve goes where Reserve placed it, after the
// frames queued before, and before those queued after, however late it is
// sent; one answered Later goes where it is sent.
func TestReserveOrder(t *testing.T) {
	c, handed := placing(t, server.Config{}, "reserve", nil)
	send(t, c, `{"method":"ext:place","id":"r"}`)
	p := take(t, handed)
	// Nothing after the reply's place is written before the reply.
	expectOrder(t, order(t, c, 1), "before")
	if err := c.Sync(briefly(t)); err == nil {
		t.Fatal("a frame after the reserved place was written before the reply")
	}
	if err := p.reply.Send(map[string]int{"n": 1}); err != nil {
		t.Fatal(err)
	}
	expectOrder(t, order(t, c, 2), "reply r", "after")
	if err := p.reply.Send(map[string]int{"n": 2}); !errors.Is(err, server.ErrReplied) {
		t.Fatalf("a second reply: %v", err)
	}

	c, handed = placing(t, server.Config{}, "later", nil)
	send(t, c, `{"method":"ext:place","id":"l"}`)
	p = take(t, handed)
	expectOrder(t, order(t, c, 2), "before", "after")
	if err := p.reply.Send(map[string]int{"n": 1}); err != nil {
		t.Fatal(err)
	}
	expectOrder(t, order(t, c, 1), "reply l")
}

// A reply that fails is sent in its place as one that succeeds is, and
// so is the error of a handler that returns one after Reserve.
func TestReserveFail(t *testing.T) {
	c, handed := placing(t, server.Config{}, "reserve", nil)
	send(t, c, `{"method":"ext:place","id":"r"}`)
	p := take(t, handed)
	if err := p.reply.Fail(apron.Denied("no")); err != nil {
		t.Fatal(err)
	}
	got := order(t, c, 3)
	expectOrder(t, got, "before", "reply r", "after")
}

// pendingDone is a Pending the test decides.
type pendingDone struct {
	done chan struct{}
	err  error
}

func (p *pendingDone) Done() <-chan struct{} { return p.done }
func (p *pendingDone) Err() error            { return p.err }

// A hold placed before the reply's place holds the reply too, and the
// frames after it, until it is done.
func TestReserveAfterHold(t *testing.T) {
	hold := &pendingDone{done: make(chan struct{})}
	c, handed := placing(t, server.Config{}, "reserve", hold)
	send(t, c, `{"method":"ext:place","id":"r"}`)
	p := take(t, handed)
	if err := p.reply.Send(map[string]int{"n": 1}); err != nil {
		t.Fatal(err)
	}
	if err := c.Sync(briefly(t)); err == nil {
		t.Fatal("a frame was written before the hold was done")
	}
	close(hold.done)
	expectOrder(t, order(t, c, 3), "before", "reply r", "after")
}

// A place whose reply never comes ends the connection once the writer
// has waited for it as long as a write may take; the request fails, and
// is not kept, so that a retry runs again.
func TestReserveNeverSent(t *testing.T) {
	c, handed := placing(t, server.Config{WriteTimeout: 100 * time.Millisecond}, "reserve", nil)
	send(t, c, `{"method":"ext:place","id":"r"}`)
	p := take(t, handed)
	expectOrder(t, order(t, c, 1), "before")
	select {
	case <-p.conn.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the connection waited for a reply that never came")
	}
	if err := p.reply.Send(map[string]int{"n": 1}); !errors.Is(err, apron.ErrClosed) {
		t.Fatalf("a reply after the connection ended: %v", err)
	}
}

// A duplicate of a request whose reply is reserved waits for it, without
// holding up its connection, and gets its outcome.
func TestReserveDuplicate(t *testing.T) {
	c, handed := placing(t, server.Config{}, "reserve", nil)
	send(t, c, `{"method":"ext:place","id":"r"}`)
	p := take(t, handed)
	send(t, c, `{"method":"ext:place","id":"r"}`)
	// The connection goes on to its next frame, whose reply is queued
	// behind the reserved one.
	send(t, c, `{"method":"nonesuch","id":"n"}`)
	if err := c.Sync(briefly(t)); err == nil {
		t.Fatal("a frame after the reserved place was written before the reply")
	}
	if err := p.reply.Send(map[string]int{"n": 1}); err != nil {
		t.Fatal(err)
	}
	// Sync's own request came before the reply was sent, and the
	// duplicate's outcome after it.
	got := order(t, c, 6)
	expectOrder(t, got, "before", "reply r", "after", "reply n", "reply aprontest-2", "reply r")
}

// Reserve is O(1) and never blocks, from any goroutine, as many times as
// it is called; auth, a barrier, is never reserved.
func TestReserveAuth(t *testing.T) {
	r := aprontest.NewRequest("auth", map[string]any{"scheme": "guest"})
	reply, _ := aprontest.NewReply[apron.AuthResult](r)
	if err := reply.Reserve(); err == nil {
		t.Fatal("auth reserved a place")
	}
	r = aprontest.NewRequest("ext:x", nil)
	other, rec := aprontest.NewReply[any](r)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if err := other.Reserve(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if err := other.Send(map[string]int{"n": 1}); err != nil || !rec.Sent() {
		t.Fatalf("send after Reserve, on no connection: %v", err)
	}
}
