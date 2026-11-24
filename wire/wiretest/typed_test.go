package wiretest_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/apron-chat/apron-server-go/wire"
	"github.com/apron-chat/apron-server-go/wire/wiretest"
)

// typedChat is chat with profiles, by ApplyMe and SendMe, and commands
// whose notices come with a message to everyone between them.
type typedChat struct{ *chat }

func newTypedChat() *typedChat {
	c := &typedChat{&chat{}}
	c.server = wire.NewServer(c.chat, c.config(wire.Config{Profiles: c, Commands: c}))
	return c
}

func (c *typedChat) Me(_ context.Context, conn *wire.Conn, p wire.MeParams, reply wire.Reply[wire.CurrentUser]) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	old := *conn.User()
	return c.server.SendMe(reply, old, wire.ApplyMe(old, p), nil)
}

// Command sends a notice, broadcasts the line, sends another notice, and
// fails a line that says so.
func (c *typedChat) Command(_ context.Context, conn *wire.Conn, p wire.CommandParams, reply wire.Reply[wire.CommandResult]) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	conn.Notify(wire.Notice(p.RoomID, "one"))
	frame, err := wire.Encode(wire.Message{From: *conn.User(), Body: &wire.Body{Text: p.Body.Text}})
	if err != nil {
		return err
	}
	c.server.Broadcast(frame, nil)
	conn.Notify(wire.Notice(p.RoomID, "two"))
	if p.Body.Text == "/fail" {
		return wire.InvalidParams("failed")
	}
	return reply.Send(wire.CommandResult{})
}

// The typed requests and expectations decode what a Client receives.
func TestTyped(t *testing.T) {
	ctx := timeout(t)
	c := newTypedChat()
	ada, bob := wiretest.Connect(c.server), wiretest.Connect(c.server)
	defer ada.Close()
	defer bob.Close()
	for _, client := range []*wiretest.Client{ada, bob} {
		if _, err := client.Auth(ctx, map[string]any{"scheme": "guest"}); err != nil {
			t.Fatal(err)
		}
		if welcome, err := client.ExpectMessage(ctx, nil); err != nil || welcome.Body.Text != "welcome" {
			t.Fatalf("welcome: %+v %v", welcome, err)
		}
	}

	sent, err := ada.Post(ctx, wire.MessageParams{Body: &wire.Body{Text: "hi"}})
	if err != nil || sent.MessageID == 0 {
		t.Fatalf("Post: %+v %v", sent, err)
	}
	for _, client := range []*wiretest.Client{ada, bob} {
		if m, err := client.ExpectMessage(ctx, func(m wire.Message) bool { return m.MessageID == sent.MessageID }); err != nil || m.Body.Text != "hi" {
			t.Fatalf("ExpectMessage: %+v %v", m, err)
		}
	}
	if _, err := ada.Post(ctx, wire.MessageParams{}); wire.CodeOf(err) != wire.CodeInvalidParams {
		t.Fatalf("Post without a body: %v", err)
	}
	history, err := wiretest.Call[wire.HistoryResult](ctx, ada, "history", wire.HistoryParams{Limit: 10})
	if err != nil || len(history.Messages) != 1 || history.Messages[0].MessageID != sent.MessageID {
		t.Fatalf("Call[HistoryResult]: %+v %v", history, err)
	}
	if _, err := wiretest.Call[wire.HistoryResult](ctx, ada, "nope", nil); wire.CodeOf(err) != wire.CodeUnsupported {
		t.Fatalf("Call of an unknown method: %v", err)
	}

	// Me returns the complete object; the others are sent the change, and
	// the user's own connection nothing besides the result.
	name := "Ada"
	you, err := ada.Me(ctx, wire.MeParams{Name: &name, Ext: wire.Ext{"tz": []byte(`"UTC"`)}})
	if err != nil || you.Name != "Ada" || string(you.Ext["tz"]) != `"UTC"` || len(you.Cleared) != 0 {
		t.Fatalf("Me: %+v %v", you, err)
	}
	update, err := wiretest.Expect(ctx, bob, func(u wire.UserUpdate) bool { return u.New != nil && u.New.UserID == you.UserID })
	if err != nil || update.New.Name != "Ada" {
		t.Fatalf("Expect[UserUpdate]: %+v %v", update, err)
	}
	empty := ""
	if you, err := ada.Me(ctx, wire.MeParams{Name: &empty, Ext: wire.Ext{"tz": []byte(`""`)}}); err != nil || you.Name != "" || you.Ext != nil || !slices.Equal(you.Cleared, []string{"name"}) {
		t.Fatalf("Me clearing: %+v %v", you, err)
	}
	if update, err := wiretest.Expect[wire.UserUpdate](ctx, bob, nil); err != nil || !slices.Equal(update.New.Cleared, []string{"name"}) || string(update.New.Ext["tz"]) != `""` {
		t.Fatalf("Expect[UserUpdate] clearing: %+v %v", update, err)
	}
	if err := ada.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if pending := ada.Pending(); slices.ContainsFunc(pending, wiretest.Method("user")) {
		t.Fatalf("Me's own connection was sent %v besides the result", pending)
	}

	// Command takes the notices ahead of its result, and leaves the rest.
	notices, err := ada.Command(ctx, "", "/go")
	if err != nil || !slices.Equal(notices, []string{"one", "two"}) {
		t.Fatalf("Command: %q %v", notices, err)
	}
	if m, err := ada.ExpectMessage(ctx, nil); err != nil || m.Body.Text != "/go" {
		t.Fatalf("the command's effect: %+v %v", m, err)
	}
	notices, err = ada.Command(ctx, "", "/fail")
	if wire.CodeOf(err) != wire.CodeInvalidParams || !slices.Equal(notices, []string{"one", "two"}) {
		t.Fatalf("a failed Command: %q %v", notices, err)
	}
	if err := ada.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if pending := ada.Pending(); len(pending) != 1 || !strings.Contains(string(pending[0].Raw), "/fail") {
		t.Fatalf("after a failed Command: %v", pending)
	}
}
