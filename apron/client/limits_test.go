package client_test

import (
	"encoding/json/jsontext"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/client"
	"github.com/apron-chat/apron-server-go/apron/server"
)

// refusal is a retry_after error of delay seconds, with an ext:limits
// category unless it is "".
func refusal(seconds int, category string) *apron.Error {
	data := map[string]any{"retry_after": seconds}
	if category != "" {
		data["ext"] = map[string]any{"limits": category}
	}
	return &apron.Error{Code: apron.CodeRetryAfter, Message: "Posting limit reached", Data: data}
}

// limited serves a chat whose next refuse messages are refused with err,
// in a synctest bubble, and dials it with opts.
func limited(t *testing.T, refuse int, err *apron.Error, opts ...client.Option) (*chat, *client.Conn) {
	c := newChat(server.Config{})
	c.refuse, c.refusal = refuse, err
	url, memory := serveInMemory(t, c.server)
	return c, dial(t, url, apron.AuthParams{Scheme: "guest"}, append(opts, memory)...)
}

// With HoldBack, a refused request is sent again once its delay passes,
// and the requests that its write category counts wait for it too.
func TestHoldBackWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := timeout(t)
		c, conn := limited(t, 2, refusal(2, apron.LimitWrite), client.HoldBack(time.Minute))
		start := time.Now()
		var wg sync.WaitGroup
		wg.Go(func() {
			if _, err := conn.Post(ctx, text("hi")); err != nil {
				t.Errorf("post: %v", err)
			}
		})
		synctest.Wait() // refused once, and waiting
		// A join, which write counts, waits for the hold, without being sent.
		if err := conn.RoomJoin(ctx, apron.RoomJoinParams{RoomID: "general"}); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		c.mu.Lock()
		defer c.mu.Unlock()
		if len(c.posts) != 3 || c.posts[1].Sub(start) != 2*time.Second || c.posts[2].Sub(start) != 4*time.Second {
			t.Errorf("posts at %v, want 0s, 2s, 4s after %v", c.posts, start)
		}
		if len(c.joined) != 1 || c.joined[0].Sub(start) < 2*time.Second {
			t.Errorf("joined at %v, before the hold ended", c.joined)
		}
	})
}

// Without a category, the refused method alone is held back.
func TestHoldBackMethod(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := timeout(t)
		c, conn := limited(t, 1, refusal(3, ""), client.HoldBack(time.Minute))
		start := time.Now()
		var wg sync.WaitGroup
		wg.Go(func() {
			if _, err := conn.Post(ctx, text("hi")); err != nil {
				t.Errorf("post: %v", err)
			}
		})
		synctest.Wait()
		if err := conn.RoomJoin(ctx, apron.RoomJoinParams{RoomID: "general"}); err != nil {
			t.Fatal(err)
		}
		if took := time.Since(start); took != 0 {
			t.Errorf("join waited %v", took)
		}
		wg.Wait()
		if took := time.Since(start); took != 3*time.Second {
			t.Errorf("post took %v", took)
		}
		// A read category for a write is no category the request counts:
		// held back by method, too.
		c.mu.Lock()
		c.refuse, c.refusal = 1, refusal(1, apron.LimitRead)
		c.mu.Unlock()
		start = time.Now()
		if _, err := conn.Post(ctx, text("again")); err != nil || time.Since(start) != time.Second {
			t.Errorf("post: %v after %v", err, time.Since(start))
		}
	})
}

// A call waits at most HoldBack's max: one that would wait longer gets
// the error, and a request held back past it is not sent.
func TestHoldBackMax(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := timeout(t)
		c, conn := limited(t, 1, refusal(30, apron.LimitWrite), client.HoldBack(10*time.Second))
		start := time.Now()
		if _, err := conn.Post(ctx, text("hi")); apron.CodeOf(err) != apron.CodeRetryAfter || time.Since(start) != 0 {
			t.Fatalf("post: %v after %v", err, time.Since(start))
		}
		time.Sleep(5 * time.Second)
		err := conn.RoomJoin(ctx, apron.RoomJoinParams{RoomID: "general"})
		if delay, category, ok := apron.RetryDelay(err); !ok || delay != 25*time.Second || category != "" {
			t.Fatalf("held join: %v (%v, %q)", err, delay, category)
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if len(c.joined) != 0 || len(c.posts) != 1 {
			t.Fatalf("sent %d posts, %d joins", len(c.posts), len(c.joined))
		}
	})
}

// Without HoldBack, a refusal is returned as it is.
func TestNoHoldBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, conn := limited(t, 1, refusal(2, apron.LimitWrite))
		if _, err := conn.Post(timeout(t), text("hi")); apron.CodeOf(err) != apron.CodeRetryAfter {
			t.Fatalf("post: %v", err)
		}
		if _, err := conn.Post(timeout(t), text("hi")); err != nil {
			t.Fatalf("post: %v", err)
		}
	})
}

// An upload category holds back the requests with a new upload embed, and
// not the others.
func TestHoldBackUpload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := timeout(t)
		_, conn := limited(t, 1, refusal(2, apron.LimitUpload), client.HoldBack(time.Minute))
		upload := apron.MessageParams{Body: apron.Body{Text: "file", Embeds: []jsontext.Value{jsontext.Value(`{"kind":"upload","title":"a.txt"}`)}}}
		start := time.Now()
		var wg sync.WaitGroup
		wg.Go(func() {
			if _, err := conn.Post(ctx, upload); err != nil || time.Since(start) != 2*time.Second {
				t.Errorf("upload: %v after %v", err, time.Since(start))
			}
		})
		synctest.Wait()
		if _, err := conn.Post(ctx, text("plain")); err != nil || time.Since(start) != 0 {
			t.Errorf("plain: %v after %v", err, time.Since(start))
		}
		wg.Wait()
	})
}
