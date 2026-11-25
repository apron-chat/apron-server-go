package bot_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/aprontest"
	"github.com/apron-chat/apron-server-go/apron/bot"
	"github.com/apron-chat/apron-server-go/apron/server/commands"
)

// chat is a test backend: guests, one room whose messages everyone gets,
// history, a /ping command, and no room requests.
type chat struct {
	apron.ReadOnlyRooms
	server   *apron.Server
	clock    apron.Clock
	commands commands.Router

	// rename, if set, is who OnConnect makes a Local connection.
	rename func(apron.User) apron.User

	mu        sync.Mutex
	guests    int
	records   []apron.Record
	connected []string
}

func newChat() *chat {
	c := &chat{}
	c.commands.MustAdd(commands.Command{Name: "ping", Run: func(_ context.Context, call *commands.Call) error {
		call.Notice("pong " + call.Conn.UserID())
		return call.Done()
	}})
	c.server = apron.NewServer(c, apron.Config{
		Messages: c, History: c, Commands: &c.commands, Rooms: c,
		OnConnect: func(_ context.Context, conn *apron.Conn) error {
			c.mu.Lock()
			defer c.mu.Unlock()
			if data, ok := conn.Data().(string); ok {
				c.connected = append(c.connected, data)
			}
			if c.rename != nil && conn.Local() {
				conn.SetUser(c.rename(*conn.User()))
			}
			return nil
		},
	})
	return c
}

func (*chat) Hello(context.Context, *apron.Conn) apron.ServerInfo {
	return apron.ServerInfo{Auth: []string{"guest"}}
}

func (c *chat) Auth(_ context.Context, _ *apron.Conn, _ apron.AuthParams, reply apron.Reply[apron.AuthResult]) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.guests++
	return reply.Send(apron.AuthResult{You: &apron.CurrentUser{User: apron.User{UserID: fmt.Sprintf("guest_%d", c.guests)}}})
}

func (c *chat) Message(_ context.Context, conn *apron.Conn, p apron.MessageParams, reply apron.Reply[apron.MessageResult]) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.clock.Next()
	m := apron.Message{MessageID: id, LogID: id, RoomID: "general", From: *conn.User(), Body: p.Body, ReplyTo: p.ReplyTo}
	frame, err := apron.Encode(m)
	if err != nil {
		return err
	}
	c.records = append(c.records, apron.Record{LogID: id, Message: &m})
	c.server.Broadcast(frame, nil)
	return reply.Send(apron.MessageResult{MessageID: id})
}

func (c *chat) History(_ context.Context, _ *apron.Conn, p apron.HistoryParams, reply apron.Reply[apron.HistoryResult]) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	log := apron.Log{Records: c.records, Latest: c.clock.Last()}
	if len(c.records) > 0 {
		log.Lower = c.records[0].LogID
	}
	return reply.Send(apron.Page(log, p, 50))
}

func (c *chat) RoomList(context.Context, *apron.Conn, apron.RoomListParams, apron.Reply[apron.RoomListResult]) error {
	return apron.Unsupported("No rooms")
}

func timeout(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// A bot answers a mention with a reply, and only a mention: not its own
// messages, not others'.
func TestMentions(t *testing.T) {
	c := newChat()
	other := make(chan apron.Message, 8)
	echo, err := bot.Start(c.server, bot.Options{
		User: apron.User{UserID: "echo", Name: "Echo"},
		Data: func() any { return "echo's" },
		OnMessage: func(ctx context.Context, b *bot.Bot, m apron.Message) {
			if !b.Mentioned(m) {
				other <- m
				return
			}
			if _, err := b.Reply(ctx, m, "You said: "+m.Body.Text); err != nil {
				t.Error(err)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { echo.Close() })
	ctx := timeout(t)
	client, _ := aprontest.SignIn(t, c.server, map[string]any{"scheme": "guest"})
	if _, err := client.Post(ctx, apron.MessageParams{Body: &apron.Body{Text: "hi all"}}); err != nil {
		t.Fatal(err)
	}
	asked, err := client.Post(ctx, apron.MessageParams{Body: &apron.Body{Text: "@echo hello", Mentions: []string{"echo"}}})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := aprontest.Expect(ctx, client, func(m apron.Message) bool { return m.From.UserID == "echo" })
	if err != nil || reply.Body.Text != "You said: @echo hello" || reply.ReplyTo == nil || reply.ReplyTo.MessageID != asked.MessageID {
		t.Fatalf("reply %+v: %v", reply, err)
	}
	// The bot was told of the message that did not mention it, not of
	// its own reply.
	if m := <-other; m.Body.Text != "hi all" {
		t.Fatalf("other: %+v", m)
	}
	select {
	case m := <-other:
		t.Fatalf("other: %+v", m)
	case <-time.After(50 * time.Millisecond):
	}
	if c.mu.Lock(); !slices.Equal(c.connected, []string{"echo's"}) {
		t.Errorf("OnConnect saw %q", c.connected)
	}
	c.mu.Unlock()
}

// A Conn's typed requests answer with wire's results and errors.
func TestRequests(t *testing.T) {
	c := newChat()
	ctx := timeout(t)
	conn, err := bot.Dial(c.server, apron.LocalOptions{User: apron.User{UserID: "helper"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if conn.UserID() != "helper" {
		t.Errorf("UserID %q", conn.UserID())
	}
	posted, err := conn.Post(ctx, apron.MessageParams{Body: &apron.Body{Text: "one", Format: apron.Markdown}})
	if err != nil || posted.MessageID == 0 {
		t.Fatalf("Post: %+v %v", posted, err)
	}
	if _, err := conn.Command(ctx, apron.CommandParams{Body: &apron.Body{Text: "/ping"}}); err != nil {
		t.Fatalf("Command: %v", err)
	}
	if _, err := conn.Command(ctx, apron.CommandParams{Body: &apron.Body{Text: "/pong"}}); apron.CodeOf(err) != apron.CodeInvalidParams {
		t.Fatalf("an unknown command: %v", err)
	}
	if err := conn.Join(ctx, "general"); apron.CodeOf(err) != apron.CodeUnsupported {
		t.Fatalf("Join: %v", err)
	}
	after := posted.MessageID
	page, err := conn.History(ctx, apron.HistoryParams{After: &after, Limit: 5})
	if err != nil || len(page.Messages) != 1 || page.Messages[0].Body.Text != "one" || page.Messages[0].Body.Format != apron.Markdown || page.Messages[0].From.UserID != "helper" {
		t.Fatalf("History: %+v %v", page, err)
	}
	conn.Close()
	if _, err := conn.Post(ctx, apron.MessageParams{Body: &apron.Body{Text: "two"}}); !errors.Is(err, apron.ErrClosed) {
		t.Fatalf("Post after Close: %v", err)
	}
}

// A Bot opens its connection again when the server ends it, at once for
// a request, and by itself when it listens; not once it is closed.
func TestReopen(t *testing.T) {
	c := newChat()
	ctx := timeout(t)
	quiet, err := bot.Start(c.server, bot.Options{User: apron.User{UserID: "quiet"}})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := quiet.Conn()
	first.Conn().Close()
	<-first.Done()
	if _, err := quiet.Post(ctx, apron.MessageParams{Body: &apron.Body{Text: "back"}}); err != nil {
		t.Fatalf("Post after the server closed the connection: %v", err)
	}
	if second, _ := quiet.Conn(); second == first {
		t.Fatal("the connection was not opened again")
	}

	heard := make(chan string, 8)
	listener, err := bot.Start(c.server, bot.Options{
		User:      apron.User{UserID: "listener"},
		OnMessage: func(_ context.Context, _ *bot.Bot, m apron.Message) { heard <- m.Body.Text },
	})
	if err != nil {
		t.Fatal(err)
	}
	conn, _ := listener.Conn()
	conn.Conn().Close()
	<-conn.Done()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := quiet.Post(ctx, apron.MessageParams{Body: &apron.Body{Text: "anyone?"}}); err != nil {
			t.Fatal(err)
		}
		select {
		case text := <-heard:
			if text != "anyone?" {
				t.Fatalf("heard %q", text)
			}
		case <-time.After(100 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("the listener did not come back")
			}
			continue
		}
		break
	}

	listener.Close()
	quiet.Close()
	if _, err := quiet.Post(ctx, apron.MessageParams{Body: &apron.Body{Text: "gone"}}); !errors.Is(err, apron.ErrClosed) {
		t.Fatalf("Post after Close: %v", err)
	}
	if err := c.server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

// say posts text as a new guest, with mentions, and returns its message.
func say(t *testing.T, c *chat, text string, mentions ...string) (*aprontest.Client, apron.LogID) {
	t.Helper()
	ctx := timeout(t)
	client, _ := aprontest.SignIn(t, c.server, map[string]any{"scheme": "guest"})
	result, err := client.Post(ctx, apron.MessageParams{Body: &apron.Body{Text: text, Mentions: mentions}})
	if err != nil {
		t.Fatal(err)
	}
	return client, result.MessageID
}

// A bot the backend makes another user as it opens is that user: it is
// told so, User says so, its own messages are told by that user_id, and
// its mentions are of it.
func TestUserFromBackend(t *testing.T) {
	c := newChat()
	c.rename = func(u apron.User) apron.User {
		return apron.User{UserID: "b1", Name: strings.ToLower(u.Name)}
	}
	heard := make(chan apron.Message, 8)
	you := make(chan string, 8)
	echo, err := bot.Start(c.server, bot.Options{
		User: apron.User{UserID: "echo", Name: "Echo"},
		OnMessage: func(ctx context.Context, b *bot.Bot, m apron.Message) {
			heard <- m
			if b.Mentioned(m) {
				if _, err := b.Reply(ctx, m, "hi"); err != nil {
					t.Error(err)
				}
			}
		},
		OnFrame: func(_ context.Context, _ *bot.Bot, f apron.Frame) {
			var n struct {
				Method string           `json:"method"`
				Params apron.UserUpdate `json:"params"`
			}
			if json.Unmarshal(f, &n) == nil && n.Method == "user" && n.Params.You != nil {
				you <- n.Params.You.UserID + " " + n.Params.You.Name
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { echo.Close() })
	if u := echo.User(); u.UserID != "b1" || u.Name != "echo" || echo.UserID() != "b1" {
		t.Fatalf("User %+v, UserID %s", u, echo.UserID())
	}
	if got := <-you; got != "b1 echo" {
		t.Fatalf("told %q", got)
	}
	// Mentions of the user_id it asked for are not of it.
	say(t, c, "@echo", "echo")
	if m := <-heard; m.Body.Text != "@echo" {
		t.Fatalf("heard %+v", m)
	}
	client, asked := say(t, c, "@b1", "b1")
	if _, err := client.Expect(timeout(t), func(f aprontest.Frame) bool {
		var m apron.Message
		return f.Method == "message" && f.Decode(&m) == nil && m.From.UserID == "b1" && m.ReplyTo != nil && m.ReplyTo.MessageID == asked
	}); err != nil {
		t.Fatal(err)
	}
	// It heard the mention, and not its own reply.
	if m := <-heard; m.Body.Text != "@b1" {
		t.Fatalf("heard %+v", m)
	}
	select {
	case m := <-heard:
		t.Fatalf("heard %+v", m)
	case <-time.After(50 * time.Millisecond):
	}
}

// Addressed takes a message that names the bot in its text, as well as
// one that mentions it.
func TestAddressed(t *testing.T) {
	c := newChat()
	addressed := make(chan string, 8)
	echo, err := bot.Start(c.server, bot.Options{
		User: apron.User{UserID: "echo_1", Name: "Echo"},
		OnMessage: func(_ context.Context, b *bot.Bot, m apron.Message) {
			if b.Addressed(m) {
				addressed <- m.Body.Text
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { echo.Close() })
	for _, text := range []string{"hello all", "echo: hi", "thanks @ECHO!", "ping @echo_1", "mail me@echo"} {
		say(t, c, text)
	}
	say(t, c, "you there?", "echo_1")
	for _, want := range []string{"echo: hi", "thanks @ECHO!", "ping @echo_1", "you there?"} {
		if got := <-addressed; got != want {
			t.Fatalf("addressed %q, want %q", got, want)
		}
	}
}

func TestAddresses(t *testing.T) {
	for _, test := range []struct {
		text, name string
		want       bool
	}{
		{"@echo", "echo", true},
		{"hi @Echo, how are you", "echo", true},
		{"ask @echo.", "echo", true},
		{"(@echo)", "echo", true},
		{"echo: hi", "echo", true},
		{"  Echo, hi", "echo", true},
		{"@echo bot hi", "echo bot", true},
		{"@écho", "ÉCHO", true},
		{"echo hi", "echo", false},
		{"hi echo: there", "echo", false},
		{"@echoes", "echo", false},
		{"@echo-1", "echo", false},
		{"@echo_1", "echo", false},
		{"@echo.org", "echo", false},
		{"me@echo", "echo", false},
		{"a.@echo", "echo", false},
		{"@@echo", "echo", false},
		{"@ech", "echo", false},
		{"@", "echo", false},
		{"@echo", "", false},
	} {
		if got := bot.Addresses(test.text, test.name); got != test.want {
			t.Errorf("Addresses(%q, %q) = %v", test.text, test.name, got)
		}
	}
}

// A handler may make requests and wait for their results, more of them
// than its queue holds frames: results do not wait behind the frames the
// handler has yet to take.
func TestRequestsFromHandler(t *testing.T) {
	c := newChat()
	done := make(chan error, 1)
	echo, err := bot.Start(c.server, bot.Options{
		User:  apron.User{UserID: "echo"},
		Queue: 2,
		OnMessage: func(ctx context.Context, b *bot.Bot, m apron.Message) {
			for range 10 {
				if _, err := b.History(ctx, apron.HistoryParams{Limit: 5}); err != nil {
					done <- err
					return
				}
			}
			_, err := b.Reply(ctx, m, "read it")
			done <- err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { echo.Close() })
	say(t, c, "hello")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-timeout(t).Done():
		t.Fatal("the handler's requests got no results")
	}
}
