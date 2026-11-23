package bot_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/wire"
	"github.com/apron-chat/apron-server-go/wire/bot"
	"github.com/apron-chat/apron-server-go/wire/commands"
	"github.com/apron-chat/apron-server-go/wire/wiretest"
)

// chat is a test backend: guests, one room whose messages everyone gets,
// history, a /ping command, and no room requests.
type chat struct {
	wire.UnimplementedRooms
	server   *wire.Server
	clock    wire.Clock
	commands commands.Router

	mu        sync.Mutex
	guests    int
	records   []wire.Record
	connected []string
}

func newChat() *chat {
	c := &chat{}
	c.commands.MustAdd(commands.Command{Name: "ping", Run: func(_ context.Context, call *commands.Call) error {
		call.Notice("pong " + call.Conn.UserID())
		return call.Done()
	}})
	c.server = &wire.Server{
		Backend: c, Messages: c, History: c, Commands: &c.commands, Rooms: c,
		OnConnect: func(_ context.Context, conn *wire.Conn) error {
			c.mu.Lock()
			defer c.mu.Unlock()
			if data, ok := conn.Data().(string); ok {
				c.connected = append(c.connected, data)
			}
			return nil
		},
	}
	return c
}

func (*chat) Hello(context.Context, *wire.Conn) wire.ServerInfo {
	return wire.ServerInfo{Auth: []string{"guest"}}
}

func (c *chat) Auth(_ context.Context, _ *wire.Conn, _ wire.AuthParams, reply wire.Reply[wire.AuthResult]) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.guests++
	return reply.Send(wire.AuthResult{You: &wire.CurrentUser{User: wire.User{UserID: fmt.Sprintf("guest_%d", c.guests)}}})
}

func (c *chat) Message(_ context.Context, conn *wire.Conn, p wire.MessageParams, reply wire.Reply[wire.MessageResult]) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.clock.Next()
	m := wire.Message{MessageID: id, LogID: id, RoomID: "general", From: *conn.User(), Body: p.Body, ReplyTo: p.ReplyTo}
	frame, err := wire.Encode(m)
	if err != nil {
		return err
	}
	c.records = append(c.records, wire.Record{LogID: id, Message: &m})
	c.server.Broadcast(frame, nil)
	return reply.Send(wire.MessageResult{MessageID: id})
}

func (c *chat) History(_ context.Context, _ *wire.Conn, p wire.HistoryParams, reply wire.Reply[wire.HistoryResult]) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	log := wire.Log{Records: c.records, Latest: c.clock.Last()}
	if len(c.records) > 0 {
		log.Lower = c.records[0].LogID
	}
	return reply.Send(wire.Page(log, p, 50))
}

func (c *chat) RoomList(context.Context, *wire.Conn, wire.RoomListParams, wire.Reply[wire.RoomListResult]) error {
	return wire.Unsupported("No rooms")
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
	other := make(chan wire.Message, 8)
	echo, err := bot.Start(c.server, bot.Options{
		User: wire.User{UserID: "echo", Name: "Echo"},
		Data: func() any { return "echo's" },
		OnMessage: func(ctx context.Context, b *bot.Bot, m wire.Message) {
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
	client := wiretest.Connect(c.server)
	t.Cleanup(func() { client.Close() })
	if _, err := client.Auth(ctx, map[string]any{"scheme": "guest"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Call(ctx, "message", map[string]any{"body": map[string]any{"text": "hi all"}}); err != nil {
		t.Fatal(err)
	}
	raw, err := client.Call(ctx, "message", map[string]any{"body": map[string]any{"text": "@echo hello", "mentions": []string{"echo"}}})
	if err != nil {
		t.Fatal(err)
	}
	var asked wire.MessageResult
	if err := json.Unmarshal(raw, &asked); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	f, err := client.Expect(ctx, func(f wiretest.Frame) bool {
		var m wire.Message
		return f.Method == "message" && f.Decode(&m) == nil && m.From.UserID == "echo"
	})
	if err != nil {
		t.Fatal(err)
	}
	var reply wire.Message
	if err := f.Decode(&reply); err != nil || reply.Body.Text != "You said: @echo hello" || reply.ReplyTo == nil || reply.ReplyTo.MessageID != asked.MessageID {
		t.Fatalf("reply %s: %v", f.Raw, err)
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
	conn, err := bot.Dial(c.server, wire.LocalOptions{User: wire.User{UserID: "helper"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if conn.UserID() != "helper" {
		t.Errorf("UserID %q", conn.UserID())
	}
	posted, err := conn.Post(ctx, wire.MessageParams{Body: &wire.Body{Text: "one", Format: wire.Markdown}})
	if err != nil || posted.MessageID == 0 {
		t.Fatalf("Post: %+v %v", posted, err)
	}
	if _, err := conn.Command(ctx, wire.CommandParams{Body: &wire.Body{Text: "/ping"}}); err != nil {
		t.Fatalf("Command: %v", err)
	}
	if _, err := conn.Command(ctx, wire.CommandParams{Body: &wire.Body{Text: "/pong"}}); wire.CodeOf(err) != wire.CodeInvalidParams {
		t.Fatalf("an unknown command: %v", err)
	}
	if err := conn.Join(ctx, "general"); wire.CodeOf(err) != wire.CodeUnsupported {
		t.Fatalf("Join: %v", err)
	}
	after := posted.MessageID
	page, err := conn.History(ctx, wire.HistoryParams{After: &after, Limit: 5})
	if err != nil || len(page.Messages) != 1 || page.Messages[0].Body.Text != "one" || page.Messages[0].Body.Format != wire.Markdown || page.Messages[0].From.UserID != "helper" {
		t.Fatalf("History: %+v %v", page, err)
	}
	conn.Close()
	if _, err := conn.Post(ctx, wire.MessageParams{Body: &wire.Body{Text: "two"}}); !errors.Is(err, wire.ErrClosed) {
		t.Fatalf("Post after Close: %v", err)
	}
}

// A Bot opens its connection again when the server ends it, at once for
// a request, and by itself when it listens; not once it is closed.
func TestReopen(t *testing.T) {
	c := newChat()
	ctx := timeout(t)
	quiet, err := bot.Start(c.server, bot.Options{User: wire.User{UserID: "quiet"}})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := quiet.Conn()
	first.Conn().Close()
	<-first.Done()
	if _, err := quiet.Post(ctx, wire.MessageParams{Body: &wire.Body{Text: "back"}}); err != nil {
		t.Fatalf("Post after the server closed the connection: %v", err)
	}
	if second, _ := quiet.Conn(); second == first {
		t.Fatal("the connection was not opened again")
	}

	heard := make(chan string, 8)
	listener, err := bot.Start(c.server, bot.Options{
		User:      wire.User{UserID: "listener"},
		OnMessage: func(_ context.Context, _ *bot.Bot, m wire.Message) { heard <- m.Body.Text },
	})
	if err != nil {
		t.Fatal(err)
	}
	conn, _ := listener.Conn()
	conn.Conn().Close()
	<-conn.Done()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := quiet.Post(ctx, wire.MessageParams{Body: &wire.Body{Text: "anyone?"}}); err != nil {
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
	if _, err := quiet.Post(ctx, wire.MessageParams{Body: &wire.Body{Text: "gone"}}); !errors.Is(err, wire.ErrClosed) {
		t.Fatalf("Post after Close: %v", err)
	}
	if err := c.server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}
