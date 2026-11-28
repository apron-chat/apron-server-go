package server_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/aprontest"
	"github.com/apron-chat/apron-server-go/apron/server"
)

// padded registers "big", whose results have size bytes of padding, and
// counts its calls.
func padded(s *server.Server, size int) *atomic.Int64 {
	calls := new(atomic.Int64)
	pad := strings.Repeat("x", size)
	s.Handle("big", server.Handler{Serve: func(_ context.Context, _ *server.Conn, _ *server.Request, reply server.Reply[any]) error {
		return reply.Send(map[string]any{"call": calls.Add(1), "pad": pad})
	}})
	return calls
}

// retry sends a big request with id, and returns the call that answered it.
func retry(t *testing.T, c *aprontest.Client, id string) int {
	t.Helper()
	send(t, c, `{"method":"big","id":"`+id+`"}`)
	f := next(t, c)
	call, ok := fields(f)["call"].(float64)
	if !ok {
		t.Fatalf("got %s, want a big result", f.Raw)
	}
	return int(call)
}

// ForgetUser frees a user's requests: a user who left for good, such as a
// guest, keeps no memory, which it otherwise does until other users' push
// it out, as it may come back to retry (§1.2).
func TestDedupForgetUser(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := wired(&backend{}, server.Config{})
		calls := padded(s, 10<<10)
		c := aprontest.Connect(t, s)
		userID := signIn(t, c)
		for range 2 {
			if retry(t, c, "k") != 1 {
				t.Fatal("a retry ran again")
			}
		}
		for range 20 {
			if _, err := c.Call(wait(t), "big", nil); err != nil {
				t.Fatal(err)
			}
		}
		// Kept across the connection's end, for a retry on the next.
		_ = c.Close()
		expectClosed(t, c)
		// A request is finished, and what it keeps counted, after its
		// reply is sent: once all is still, every one is.
		synctest.Wait()
		if users, requests, bytes := aprontest.DedupUsage(s); users != 1 || requests != 21 || bytes < 21*10<<10 {
			t.Fatalf("%d users, %d requests, %d bytes kept; want 1, 21, and their results", users, requests, bytes)
		}
		s.ForgetUser(userID)
		if users, requests, bytes := aprontest.DedupUsage(s); users != 0 || requests != 0 || bytes != 0 {
			t.Fatalf("%d users, %d requests, %d bytes kept once the user is forgotten", users, requests, bytes)
		}
		if n := calls.Load(); n != 21 {
			t.Fatalf("%d calls, want 21", n)
		}
	})
}

// What is kept is bounded in bytes, for each user and for all, the oldest
// of the least recently active users forgotten first; a result too large
// to fit is not kept.
func TestDedupBytes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const size = 10 << 10
		s := wired(&backend{}, server.Config{DedupBytes: 100 << 10, DedupTotalBytes: 250 << 10})
		calls := padded(s, size)
		conns := make([]*aprontest.Client, 4)
		for i := range conns {
			conns[i] = aprontest.Connect(t, s)
			signIn(t, conns[i])
			for j := range 30 {
				retry(t, conns[i], fmt.Sprint(j))
				users, _, bytes := aprontest.DedupUsage(s)
				if bytes > 250<<10 {
					t.Fatalf("%d bytes kept, over DedupTotalBytes", bytes)
				}
				if users == i+1 && bytes > (i+1)*100<<10 {
					t.Fatalf("%d bytes kept for %d users, over DedupBytes each", bytes, users)
				}
			}
		}
		n := int(calls.Load())
		last := conns[len(conns)-1]
		if retry(t, last, "29") != n {
			t.Fatal("the latest request of the latest user was not kept")
		}
		if retry(t, last, "0") != n+1 {
			t.Fatal("the oldest request of a user over DedupBytes was kept")
		}
		if retry(t, conns[0], "29") != n+2 {
			t.Fatal("the least recently active user's request was kept over DedupTotalBytes")
		}

		huge := wired(&backend{}, server.Config{DedupBytes: 4 << 10})
		padded(huge, 8<<10)
		c := aprontest.Connect(t, huge)
		signIn(t, c)
		if retry(t, c, "h") != 1 || retry(t, c, "h") != 2 {
			t.Fatal("a result larger than DedupBytes was kept")
		}
		synctest.Wait() // The result too large to keep is forgotten after its reply.
		if _, requests, bytes := aprontest.DedupUsage(huge); requests != 0 || bytes != 0 {
			t.Fatalf("%d requests, %d bytes kept; want none", requests, bytes)
		}
	})
}

// scoped signs every connection in as one user, in the deduplication scope
// its name asks for.
type scoped struct{ oneUser }

func (*scoped) Auth(_ context.Context, c *server.Conn, p apron.AuthParams, reply server.Reply[apron.AuthResult]) error {
	c.SetDedupScope(p.Name)
	return reply.Send(apron.AuthResult{You: apron.CurrentUser{UserID: "same"}})
}

// A request's duplicate gets its outcome only on a connection of the same
// scope: on another, such as one signed in with a restricted token, it
// runs again, and its handler decides.
func TestDedupScope(t *testing.T) {
	s := wired(&scoped{}, server.Config{})
	padded(s, 0)
	conns := map[string]*aprontest.Client{}
	for _, name := range []string{"full", "restricted", "full again"} {
		c := aprontest.Connect(t, s)
		scope := strings.TrimSuffix(name, " again")
		if _, err := c.Auth(wait(t), map[string]any{"scheme": "guest", "name": scope}); err != nil {
			t.Fatal(err)
		}
		conns[name] = c
	}
	// In this order: the first request runs, and only its duplicate from
	// another scope runs again.
	for _, step := range []struct {
		name string
		want int
	}{{"full", 1}, {"restricted", 2}, {"full again", 1}} {
		if got := retry(t, conns[step.name], "x"); got != step.want {
			t.Fatalf("%s: answered by call %d, want %d", step.name, got, step.want)
		}
	}
}

// A retry of a request, by its user and id (§1.2), gets the original's
// outcome, unless it failed; another user's request of the same id is
// another request; and DedupSize -1 keeps nothing.
func TestDedup(t *testing.T) {
	b := &backend{}
	s := wired(b, server.Config{})
	c, other := aprontest.Connect(t, s), aprontest.Connect(t, s)
	signIn(t, c)
	signIn(t, other)
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
	if f := next(t, c); code(f) != apron.CodeInvalidParams || f.ID != "d" {
		t.Fatalf("got %s, want invalid_params for a reused id", f.Raw)
	}
	// A failed request is not kept: a retry runs again.
	send(t, c, `{"method":"message","id":"bad","params":{"room_id":"has space","body":{"text":"x"}}}`)
	if f := next(t, c); code(f) != apron.CodeInvalidParams {
		t.Fatalf("got %s", f.Raw)
	}
	send(t, c, `{"method":"message","id":"bad","params":{"body":{"text":"x"}}}`)
	if f := next(t, c); fields(f)["message_id"] == nil {
		t.Fatalf("got %s, want the retry to run", f.Raw)
	}
	// Another user's request with the same id and params runs.
	send(t, other, `{"method":"message","id":"d","params":{"room_id":"r","body":{"text":"hi"}}}`)
	if f := next(t, other); fields(f)["message_id"] == nil || fields(f)["message_id"] == fields(first)["message_id"] {
		t.Fatalf("another user's request got %s, the first user's %s", f.Raw, first.Raw)
	}
	if n := b.messages.Load(); n != 3 {
		t.Fatalf("the backend saw %d messages, want 3", n)
	}

	off := &backend{}
	c = aprontest.Connect(t, wired(off, server.Config{DedupSize: -1}))
	signIn(t, c)
	for range 2 {
		send(t, c, `{"method":"message","id":"d","params":{"body":{"text":"hi"}}}`)
		next(t, c)
	}
	if n := off.messages.Load(); n != 2 {
		t.Fatalf("with DedupSize -1 the backend saw %d messages, want 2", n)
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

func (b *returning) Command(_ context.Context, _ *server.Conn, _ apron.CommandParams, reply server.Reply[apron.CommandResult]) error {
	n := b.calls.Add(1)
	_ = reply.Fail(apron.Denied(fmt.Sprintf("call %d", n)))
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
	s := wired(b, server.Config{})
	s.Handle("read", server.Handler{NoDedup: true, Serve: func(_ context.Context, _ *server.Conn, r *server.Request, reply server.Reply[any]) error {
		n := b.calls.Add(1)
		_ = reply.Send(map[string]any{"call": n})
		b.stall(n)
		return nil
	}})
	// Each original holds up its connection, so each pair of requests
	// has its own connections, which end after the originals return.
	conns := []*aprontest.Client{aprontest.Connect(t, s), aprontest.Connect(t, s), aprontest.Connect(t, s), aprontest.Connect(t, s)}
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
