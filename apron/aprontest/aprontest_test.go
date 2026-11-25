package aprontest_test

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/aprontest"
	"github.com/apron-chat/apron-server-go/apron/server"
)

// chat is a test backend: guests, one room whose messages everyone gets,
// history, and typing.
type chat struct {
	server *server.Server
	clock  server.Clock

	mu      sync.Mutex
	guests  int
	records []apron.Record

	typing       atomic.Int64
	disconnected atomic.Int64
}

func newChat() *chat {
	c := &chat{}
	c.server = server.New(c, c.config(server.Config{}))
	return c
}

// config is more with the chat's handlers and hooks.
func (c *chat) config(more server.Config) server.Config {
	more.Messages, more.History, more.Activity = c, c, c
	more.OnDisconnect = func(*server.Conn) { c.disconnected.Add(1) }
	return more
}

func (*chat) Hello(context.Context, *server.Conn) apron.ServerInfo {
	return apron.ServerInfo{Auth: []string{"guest"}, Title: "Test"}
}

func (c *chat) Auth(_ context.Context, _ *server.Conn, p apron.AuthParams, reply server.Reply[apron.AuthResult]) error {
	if p.Name == "refuse" {
		return apron.Denied("Not you")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.guests++
	welcome, err := apron.Encode(apron.Notice("", "welcome"))
	if err != nil {
		return err
	}
	return reply.Send(apron.AuthResult{You: &apron.CurrentUser{User: apron.User{UserID: fmt.Sprintf("guest_%d", c.guests)}}}, welcome)
}

func (c *chat) Message(_ context.Context, conn *server.Conn, p apron.MessageParams, reply server.Reply[apron.MessageResult]) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.clock.Next()
	m := apron.Message{MessageID: id, LogID: id, From: *conn.User(), Body: p.Body}
	frame, err := apron.Encode(m)
	if err != nil {
		return err
	}
	c.records = append(c.records, apron.Record{LogID: id, Message: &m})
	c.server.Broadcast(frame, nil)
	return reply.Send(apron.MessageResult{MessageID: id})
}

func (c *chat) History(_ context.Context, _ *server.Conn, p apron.HistoryParams, reply server.Reply[apron.HistoryResult]) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	log := server.Log{Records: c.records, Latest: c.clock.Last()}
	if len(c.records) > 0 {
		log.Lower = c.records[0].LogID
	}
	return reply.Send(server.Page(log, p, 50))
}

func (c *chat) Activity(_ context.Context, _ *server.Conn, p apron.ActivityParams) {
	if p.Typing != nil {
		c.typing.Add(1)
	}
}

func timeout(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// A typed handler called directly sends to a recording Reply.
func TestNewReply(t *testing.T) {
	c := newChat()
	c.records = []apron.Record{{LogID: 1, Message: &apron.Message{MessageID: 1, LogID: 1}}}
	reply, sent := aprontest.NewReply[apron.HistoryResult](nil)
	if sent.Sent() {
		t.Fatal("sent before the handler ran")
	}
	if err := c.History(context.Background(), nil, apron.HistoryParams{}, reply); err != nil {
		t.Fatal(err)
	}
	if !sent.Sent() || len(sent.Result().Messages) != 1 || !strings.Contains(string(sent.Raw()), `"messages":[`) || sent.Err() != nil {
		t.Fatalf("sent %v %+v %s %v", sent.Sent(), sent.Result(), sent.Raw(), sent.Err())
	}

	// What wire does with a method's result is done, and recorded.
	me, you := aprontest.NewReply[apron.CurrentUser](aprontest.NewRequest("me", map[string]any{"name": "Ada"}))
	if err := me.Send(apron.CurrentUser{User: apron.User{UserID: "ada", Name: "Ada"}}); err != nil {
		t.Fatal(err)
	}
	if you.Result().Name != "Ada" || string(you.Raw()) != `{"you":{"user_id":"ada","name":"Ada"}}` {
		t.Fatalf("me: %+v %s", you.Result(), you.Raw())
	}
	auth, signedIn := aprontest.NewReply[apron.AuthResult](aprontest.NewRequest("auth", map[string]any{"scheme": "guest"}))
	if err := auth.Send(apron.AuthResult{You: &apron.CurrentUser{User: apron.User{UserID: "not valid"}}}); apron.CodeOf(err) != apron.CodeInternalError || apron.CodeOf(signedIn.Err()) != apron.CodeInternalError {
		t.Fatalf("an invalid auth result: %v, recorded %v", err, signedIn.Err())
	}
}

// A Handler gets a request with params, and its reply and the frames after
// it are recorded.
func TestNewRequest(t *testing.T) {
	h := server.Handler{Serve: func(_ context.Context, _ *server.Conn, r *server.Request, reply server.Reply[any]) error {
		d := r.Decoder()
		text := d.String("text", server.Required)
		if err := d.Err(); err != nil {
			return err
		}
		notice, _ := apron.Encode(apron.Notice("", "echoed"))
		return reply.Send(map[string]any{"echo": text}, notice)
	}}
	r := aprontest.NewRequest("x_echo", map[string]any{"text": "hi"})
	reply, sent := aprontest.NewReply[any](r)
	if err := h.Serve(context.Background(), nil, r, reply); err != nil {
		t.Fatal(err)
	}
	if string(sent.Raw()) != `{"echo":"hi"}` || len(sent.Then()) != 1 {
		t.Fatalf("sent %s %q", sent.Raw(), sent.Then())
	}
	r = aprontest.NewRequest("x_echo", nil)
	reply, _ = aprontest.NewReply[any](r)
	if err := h.Serve(context.Background(), nil, r, reply); apron.CodeOf(err) != apron.CodeInvalidParams {
		t.Fatalf("without text: %v", err)
	}
}

// testClient runs the same conversation on a Client of either kind.
func testClient(t *testing.T, c *chat, client *aprontest.Client) {
	ctx := timeout(t)
	if hello := client.Hello(); hello.Title != "Test" || hello.Apron != apron.Version {
		t.Fatalf("Hello: %+v", hello)
	}
	if _, err := client.Call(ctx, "history", nil); apron.CodeOf(err) != apron.CodeDenied {
		t.Fatalf("before auth: %v", err)
	}
	if _, err := client.Auth(ctx, map[string]any{"scheme": "guest", "name": "refuse"}); apron.CodeOf(err) != apron.CodeDenied {
		t.Fatalf("a refused auth: %v", err)
	}
	result, err := client.Auth(ctx, map[string]any{"scheme": "guest"})
	if err != nil || result.You == nil || !strings.HasPrefix(result.You.UserID, "guest_") {
		t.Fatalf("auth: %+v %v", result, err)
	}
	if f, err := client.Next(ctx); err != nil || f.Method != "message" {
		t.Fatalf("after auth: %+v %v", f, err)
	}

	// The message's echo arrives before its result, and is kept.
	raw, err := client.Call(ctx, "message", map[string]any{"body": map[string]any{"text": "hi"}})
	if err != nil || !strings.Contains(string(raw), `"message_id"`) {
		t.Fatalf("message: %s %v", raw, err)
	}
	if pending := client.Pending(); len(pending) != 1 {
		t.Fatalf("pending: %v", pending)
	}
	echo, err := client.Expect(ctx, aprontest.Method("message"))
	var m apron.Message
	if err != nil || echo.Decode(&m) != nil || m.Body.Text != "hi" {
		t.Fatalf("echo: %+v %v", echo, err)
	}

	if _, err := client.Call(ctx, "nope", nil); apron.CodeOf(err) != apron.CodeUnsupported {
		t.Fatalf("an unknown method: %v", err)
	}
	if err := client.Notify(ctx, "activity", map[string]any{"typing": 5}); err != nil {
		t.Fatal(err)
	}
	// Frames are processed in order: once history answers, activity was
	// handled.
	if _, err := client.Call(ctx, "history", nil); err != nil || c.typing.Load() != 1 {
		t.Fatalf("history: %v, typing %d", err, c.typing.Load())
	}
	if err := client.Send(ctx, []byte(`{"method":"history","id":7}`)); err != nil {
		t.Fatal(err)
	}
	if f, err := client.Next(ctx); err != nil || f.Error == nil || f.Error.Code != apron.CodeInvalidRequest || f.ID != "" {
		t.Fatalf("an invalid request: %+v %v", f, err)
	}
}

func TestConnect(t *testing.T) {
	c := newChat()
	client := aprontest.Connect(t, c.server)
	testClient(t, c, client)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if n := c.disconnected.Load(); n != 1 {
		t.Fatalf("%d disconnected after Close", n)
	}
	if _, err := client.Next(timeout(t)); !errors.Is(err, apron.ErrClosed) {
		t.Fatalf("Next after Close: %v", err)
	}
}

// SignIn signs a connection in, which closes as the test ends.
func TestSignIn(t *testing.T) {
	c := newChat()
	var client *aprontest.Client
	t.Run("signed in", func(t *testing.T) {
		var you apron.CurrentUser
		client, you = aprontest.SignIn(t, c.server, map[string]any{"scheme": "guest", "name": "Ada"})
		if you.UserID == "" || client.Hello().Title == "" {
			t.Fatalf("you %+v, server frame %+v", you, client.Hello())
		}
	})
	ctx := timeout(t)
	for {
		f, err := client.Next(ctx) // The frames sent after the result, then the close.
		if errors.Is(err, apron.ErrClosed) {
			break
		}
		if err != nil || f.Method == "" {
			t.Fatalf("Next once the test ended: %+v %v", f, err)
		}
	}
}

func TestDial(t *testing.T) {
	c := newChat()
	ts := httptest.NewServer(c.server)
	defer ts.Close()
	client, err := aprontest.Dial(timeout(t), "ws"+strings.TrimPrefix(ts.URL, "http"))
	if err != nil {
		t.Fatal(err)
	}
	testClient(t, c, client)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
}

// noticeText is the text of a notice frame, or "".
func noticeText(f aprontest.Frame) string {
	var m apron.Message
	if f.Method != "message" || f.Decode(&m) != nil {
		return ""
	}
	return m.Body.Text
}

// texts are the notices' texts of frames, in order.
func texts(frames []aprontest.Frame) []string {
	var out []string
	for _, f := range frames {
		out = append(out, noticeText(f))
	}
	return out
}

// CallFrames takes exactly the frames ahead of its reply, and Sync waits
// for every frame queued before its request was handled.
func TestCallFramesAndSync(t *testing.T) {
	ctx := timeout(t)
	c := newChat()
	// test.order sends a notice before its reply and one after it.
	c.server.Handle("test.order", server.Handler{Serve: func(_ context.Context, conn *server.Conn, r *server.Request, reply server.Reply[any]) error {
		conn.Notify(apron.Notice("", "before"))
		_ = reply.Send(nil)
		conn.Notify(apron.Notice("", "after"))
		return nil
	}})
	ada, bob := aprontest.Connect(t, c.server), aprontest.Connect(t, c.server)
	defer ada.Close()
	defer bob.Close()

	// Before sign-in, Sync's request is refused, which is reply enough.
	if err := ada.Sync(ctx); err != nil {
		t.Fatalf("Sync before sign-in: %v", err)
	}
	for _, client := range []*aprontest.Client{ada, bob} {
		if _, err := client.Auth(ctx, map[string]any{"scheme": "guest"}); err != nil {
			t.Fatal(err)
		}
		if _, err := client.Expect(ctx, func(f aprontest.Frame) bool { return noticeText(f) == "welcome" }); err != nil {
			t.Fatal(err)
		}
	}

	// bob's message was queued to ada before bob's result; once ada's
	// Sync returns, it has arrived, and stays.
	if _, err := bob.Call(ctx, "message", map[string]any{"body": map[string]any{"text": "one"}}); err != nil {
		t.Fatal(err)
	}
	if err := ada.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := texts(ada.Pending()); len(got) != 1 || got[0] != "one" {
		t.Fatalf("pending after Sync: %q", got)
	}

	// The backlog, then what the request sent before its reply, in order;
	// what it sent after stays.
	result, before, err := ada.CallFrames(ctx, "test.order", nil)
	if err != nil || string(result) != "{}" {
		t.Fatalf("test.order: %s %v", result, err)
	}
	if got := texts(before); len(got) != 2 || got[0] != "one" || got[1] != "before" {
		t.Fatalf("before the reply: %q", got)
	}
	if err := ada.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := texts(ada.Pending()); len(got) != 1 || got[0] != "after" {
		t.Fatalf("after the reply: %q", got)
	}
	if _, err := ada.Next(ctx); err != nil {
		t.Fatal(err)
	}

	// An error reply returns the frames before it too.
	_, before, err = ada.CallFrames(ctx, "message", map[string]any{})
	if apron.CodeOf(err) != apron.CodeInvalidParams || len(before) != 0 {
		t.Fatalf("an invalid message: %v, before %v", err, before)
	}
}

// Concurrent calls each get their own reply: CallFrames never takes the
// reply to another of the Client's requests.
func TestCallFramesConcurrent(t *testing.T) {
	ctx := timeout(t)
	c := newChat()
	client, _ := aprontest.SignIn(t, c.server, map[string]any{"scheme": "guest"})
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			for range 10 {
				var err error
				switch i % 3 {
				case 0:
					var before []aprontest.Frame
					_, before, err = client.CallFrames(ctx, "history", nil)
					for _, f := range before {
						if f.Method == "" && strings.HasPrefix(f.ID, "wiretest-") {
							t.Errorf("CallFrames took another call's reply: %s", f.Raw)
						}
					}
				case 1:
					_, err = client.Call(ctx, "message", map[string]any{"body": map[string]any{"text": "x"}})
				default:
					err = client.Sync(ctx)
				}
				if err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
}

// slow answers each command but /fast later, once its upstream does:
// answers takes the text to answer the next one with.
type slow struct {
	asked   chan string
	answers chan string
}

func newSlow() *slow { return &slow{asked: make(chan string, 4), answers: make(chan string)} }

func (s *slow) Command(_ context.Context, _ *server.Conn, p apron.CommandParams, reply server.Reply[apron.CommandResult]) error {
	if p.Body.Text == "/fast" {
		return reply.Send(apron.CommandResult{})
	}
	if err := reply.Later(); err != nil {
		return err
	}
	s.asked <- p.Body.Text
	go func() {
		answer := <-s.answers
		_ = reply.Send(apron.CommandResult{Unknown: jsontext.Value(`{"answer":"` + answer + `"}`)})
	}()
	return nil
}

// A request answered later has no reply when its handler returns; Wait
// waits for it, and a Client's Call of it returns once it comes, without
// holding up the requests after it.
func TestLater(t *testing.T) {
	ctx := timeout(t)
	s := newSlow()
	reply, sent := aprontest.NewReply[apron.CommandResult](aprontest.NewRequest("command", nil))
	if err := s.Command(ctx, nil, apron.CommandParams{Body: &apron.Body{Text: "/slow"}}, reply); err != nil || sent.Sent() {
		t.Fatalf("command: %v, sent %v; want it answered later", err, sent.Sent())
	}
	waiting, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if err := sent.Wait(waiting); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait before the reply: %v", err)
	}
	s.answers <- "42"
	if err := sent.Wait(ctx); err != nil || !sent.Sent() || string(sent.Raw()) != `{"answer":"42"}` {
		t.Fatalf("Wait: %v, sent %v %s", err, sent.Sent(), sent.Raw())
	}

	client, _ := aprontest.SignIn(t, server.New(newChat(), server.Config{Commands: s}), map[string]any{"scheme": "guest"})
	done := make(chan string, 1)
	go func() {
		result, err := client.Call(ctx, "command", map[string]any{"body": map[string]any{"text": "/slow"}})
		if err != nil {
			t.Error(err)
		}
		done <- string(result)
	}()
	<-s.asked
	if _, err := client.Call(ctx, "command", map[string]any{"body": map[string]any{"text": "/fast"}}); err != nil {
		t.Fatalf("a request after one answered later: %v", err)
	}
	select {
	case result := <-done:
		t.Fatalf("the request answered later returned %s before its answer", result)
	default:
	}
	s.answers <- "later"
	if result := <-done; result != `{"answer":"later"}` {
		t.Fatalf("got %s", result)
	}
}
