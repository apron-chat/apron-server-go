package server_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

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
}

// What is kept is bounded in bytes, for each user and for all, the oldest
// of the least recently active users forgotten first; a result too large
// to fit is not kept.
func TestDedupBytes(t *testing.T) {
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
	if _, requests, bytes := aprontest.DedupUsage(huge); requests != 0 || bytes != 0 {
		t.Fatalf("%d requests, %d bytes kept; want none", requests, bytes)
	}
}

// scoped signs every connection in as one user, in the deduplication scope
// its name asks for.
type scoped struct{ oneUser }

func (*scoped) Auth(_ context.Context, c *server.Conn, p apron.AuthParams, reply server.Reply[apron.AuthResult]) error {
	c.SetDedupScope(p.Name)
	return reply.Send(apron.AuthResult{You: &apron.CurrentUser{UserID: "same"}})
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
