package server_test

import (
	"context"
	"errors"
	"fmt"
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

func TestLocal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := &backend{}
		s := wired(b, server.Config{})
		var once sync.Once
		s.Handle("whoami", server.Handler{Serve: func(ctx context.Context, c *server.Conn, r *server.Request, reply server.Reply[any]) error {
			c.NotifyRaw("hello", map[string]any{})
			var err error
			once.Do(func() {
				// Calling back into the same connection would wait for itself.
				_, err = c.Data().(*server.Local).Call(ctx, "whoami", nil)
			})
			if !errors.Is(err, server.ErrReentrant) && err != nil {
				t.Errorf("reentrant call: %v", err)
			}
			return reply.Send(map[string]any{"user_id": c.UserID()})
		}})
		frames := make(chan string, 8)
		l, err := s.Local(server.LocalOptions{User: apron.User{UserID: "bot"}, OnFrame: func(f apron.Frame) { frames <- string(f) }})
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
		_, err = l.Call(context.Background(), "nope", nil)
		if e, ok := errors.AsType[*apron.Error](err); !ok || e.Code != apron.CodeUnsupported {
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
		if _, err := l.Call(context.Background(), "whoami", nil); !errors.Is(err, apron.ErrClosed) {
			t.Fatalf("after Close: %v", err)
		}
		synctest.Wait()
		if len(s.Conns()) != 0 || len(b.disconnectedConns()) != 1 {
			t.Fatal("the Local connection was not forgotten")
		}
		if err := s.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestLocalFellBehind(t *testing.T) {
	s := wired(&backend{}, server.Config{})
	block := make(chan struct{})
	defer close(block)
	l, err := s.Local(server.LocalOptions{User: apron.User{UserID: "bot"}, Queue: 2, OnFrame: func(apron.Frame) { <-block }})
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
	if !errors.Is(l.Err(), server.ErrFellBehind) {
		t.Fatalf("Err: %v", l.Err())
	}
}

// The frames queued for OnFrame before a Local connection ends, an error
// about the connection last, still reach it.
func TestLocalFramesBeforeEnd(t *testing.T) {
	s := wired(&backend{}, server.Config{})
	release := make(chan struct{})
	frames := make(chan string, 64)
	l, err := s.Local(server.LocalOptions{User: apron.User{UserID: "bot"}, OnFrame: func(f apron.Frame) {
		<-release
		frames <- string(f)
	}})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		l.Conn().NotifyRaw("tick", map[string]any{"n": i})
	}
	l.Conn().CloseWithError(apron.RetryAfter("Going away", 7*time.Second))
	<-l.Done()
	close(release)
	var last string
	for n := 0; n < 22; n++ { // the server frame, 20 ticks, the error
		select {
		case last = <-frames:
		case <-wait(t).Done():
			t.Fatalf("OnFrame got %d frames, want 22", n)
		}
	}
	if !strings.Contains(last, `"retry_after"`) {
		t.Fatalf("last frame %s, want the error", last)
	}
}

// A Local connection opens as a network one does: OnConnect, signed in
// already, then Hello, whose server frame goes before anything sent to it
// meanwhile. An error of OnConnect is Local's, and OnDisconnect follows it
// before Local returns.
func TestLocalOpens(t *testing.T) {
	refuse := errors.New("refused")
	var disconnects atomic.Int64
	var config server.Config
	config.OnConnect = func(_ context.Context, c *server.Conn) error {
		if c.HTTPRequest() != nil || c.Data() == nil || !c.Local() {
			t.Errorf("OnConnect: request %v, data %v, local %v", c.HTTPRequest(), c.Data(), c.Local())
		}
		if c.UserID() == "nobody" {
			return refuse
		}
		c.NotifyRaw("welcome", map[string]any{"to": c.UserID()})
		return nil
	}
	config.OnDisconnect = func(*server.Conn) { disconnects.Add(1) }
	s := wired(&backend{}, config)
	frames := make(chan string, 8)
	l, err := s.Local(server.LocalOptions{User: apron.User{UserID: "bot"}, Data: 1, OnFrame: func(f apron.Frame) { frames <- string(f) }})
	if err != nil {
		t.Fatal(err)
	}
	if f := <-frames; !strings.HasPrefix(f, `{"method":"server"`) {
		t.Fatalf("first frame %s, want the server frame", f)
	}
	if f := <-frames; f != `{"method":"welcome","params":{"to":"bot"}}` {
		t.Fatalf("second frame %s, want the welcome", f)
	}
	if info := l.Conn().ServerInfo(); info.Agent != "server-test/1" {
		t.Fatalf("Info: %+v", info)
	}
	if _, err := s.Local(server.LocalOptions{User: apron.User{UserID: "nobody"}, Data: 1}); err != refuse {
		t.Fatalf("a refused Local: %v", err)
	}
	if n := disconnects.Load(); n != 1 {
		t.Fatalf("%d disconnects once a Local was refused, want its own", n)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	<-l.Done()
	if err := l.Close(); !errors.Is(err, apron.ErrClosed) {
		t.Fatalf("a second Close: %v", err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := disconnects.Load(); n != 2 {
		t.Fatalf("%d disconnects, want the refused connection's and the one that opened", n)
	}
	if _, err := s.Local(server.LocalOptions{User: apron.User{UserID: "bot"}}); !errors.Is(err, apron.ErrClosed) {
		t.Fatalf("after Shutdown: %v", err)
	}
}

// Conn.Local tells a Local connection from one of any other transport,
// though neither has an HTTP request.
func TestConnLocal(t *testing.T) {
	local := make(chan bool, 2)
	var config server.Config
	config.OnConnect = func(_ context.Context, c *server.Conn) error {
		if c.HTTPRequest() != nil {
			t.Errorf("OnConnect: request %v", c.HTTPRequest())
		}
		local <- c.Local()
		return nil
	}
	s := wired(&backend{}, config)
	client := aprontest.Connect(t, s)
	if err := client.Sync(wait(t)); err != nil {
		t.Fatal(err)
	}
	if <-local {
		t.Error("a ServeTransport connection is Local")
	}
	l, err := s.Local(server.LocalOptions{User: apron.User{UserID: "bot"}})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if !<-local || !l.Conn().Local() {
		t.Error("a Local connection is not Local")
	}
}

// OnConnect may make a Local connection another user than its caller
// asked for, which the connection is told of as a user notification right
// after its server frame, before what OnConnect sent it; it may not leave
// it signed out.
func TestLocalUserFromOnConnect(t *testing.T) {
	var config server.Config
	var signedOut atomic.Int64
	config.OnDisconnect = func(c *server.Conn) {
		if userOf(c) == nil {
			signedOut.Add(1)
		}
	}
	config.OnConnect = func(_ context.Context, c *server.Conn) error {
		u := userOf(c)
		switch u.UserID {
		case "echo":
			// A user_id of the backend's own.
			c.SetUser(apron.User{UserID: "u7", Name: "echo", Status: "online"})
		case "same":
			// The name normalized.
			u.Name = "Same"
			c.SetUser(*u)
		case "gone":
			c.SignOut()
		}
		c.Notify(apron.Notice("", "welcome"))
		return nil
	}
	s := wired(&backend{}, config)
	for _, tc := range []struct {
		asked apron.User
		you   string // the user notification, or "" for none
		is    string
	}{
		{apron.User{UserID: "echo", Name: "Echo!"}, `{"method":"user","params":{"you":{"user_id":"u7","name":"echo","status":"online"}}}`, "u7"},
		{apron.User{UserID: "same", Name: "same", Roles: []string{"bot"}}, `{"method":"user","params":{"you":{"user_id":"same","name":"Same"}}}`, "same"},
		{apron.User{UserID: "kept", Name: "Kept"}, "", "kept"},
	} {
		frames := make(chan string, 8)
		l, err := s.Local(server.LocalOptions{User: tc.asked, OnFrame: func(f apron.Frame) { frames <- string(f) }})
		if err != nil {
			t.Fatal(err)
		}
		if f := <-frames; !strings.HasPrefix(f, `{"method":"server"`) {
			t.Fatalf("%s: first frame %s", tc.asked.UserID, f)
		}
		if tc.you != "" {
			if f := <-frames; f != tc.you {
				t.Fatalf("%s: second frame %s, want %s", tc.asked.UserID, f, tc.you)
			}
		}
		if f := <-frames; !strings.Contains(f, "welcome") {
			t.Fatalf("%s: then %s, want the welcome", tc.asked.UserID, f)
		}
		if got := l.Conn().UserID(); got != tc.is {
			t.Fatalf("%s: the connection is %s, want %s", tc.asked.UserID, got, tc.is)
		}
		l.Close()
	}
	if _, err := s.Local(server.LocalOptions{User: apron.User{UserID: "gone"}}); err == nil {
		t.Fatal("a Local connection OnConnect signed out opened")
	}
	if n := signedOut.Load(); n != 1 {
		t.Fatalf("%d disconnects of the connection OnConnect signed out, want 1", n)
	}
}

// A Call that stops waiting, as its context ends, leaves its reply, when
// it comes, to no other Call: each gets its own, whatever the calls before
// it abandoned, and those answered later once released get theirs.
func TestLocalAbandonedCalls(t *testing.T) {
	release := make(chan struct{})
	s := server.New(helloBackend{}, server.Config{})
	s.Handle("ext:echo", server.Handler{Serve: func(_ context.Context, _ *server.Conn, r *server.Request, reply server.Reply[any]) error {
		n := r.Decoder().Count("n", server.Required)
		if n%2 == 0 {
			return reply.Send(map[string]int{"n": n})
		}
		// Odd ones are answered later, once the test lets them.
		if err := reply.Later(); err != nil {
			return err
		}
		go func() {
			<-release
			_ = reply.Send(map[string]int{"n": n})
		}()
		return nil
	}})
	l, err := s.Local(server.LocalOptions{User: apron.User{UserID: "ada"}})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for n := 1; n <= 40; n++ {
		ctx := context.Background()
		var cancel context.CancelFunc = func() {}
		abandoned := n%2 == 1 && n < 20
		if abandoned {
			ctx, cancel = context.WithTimeout(ctx, time.Millisecond)
		}
		result, err := l.Call(ctx, "ext:echo", map[string]int{"n": n})
		cancel()
		switch {
		case abandoned && !errors.Is(err, context.DeadlineExceeded):
			t.Fatalf("call %d, answered later: %s, %v", n, result, err)
		case !abandoned && (err != nil || string(result) != fmt.Sprintf(`{"n":%d}`, n)):
			t.Fatalf("call %d: %s, %v", n, result, err)
		}
		if n == 20 {
			// The abandoned calls' replies come now, to nobody.
			close(release)
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// Local notifications are handled, and a Call of a notification-only method
// returns at once.
func TestLocalNotify(t *testing.T) {
	s := wired(&backend{}, server.Config{})
	got := make(chan string, 1)
	s.HandleNotification("activity", server.NotificationHandler{Capability: "activity", Serve: func(_ context.Context, _ *server.Conn, r *server.Request) {
		v, _ := r.Fields().Get("typing")
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
