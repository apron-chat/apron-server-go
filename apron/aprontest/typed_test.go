package aprontest_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/aprontest"
	"github.com/apron-chat/apron-server-go/apron/server"
)

// typedChat is chat with profiles, by ApplyMe and PublishUser, and commands
// whose notices come with a message to everyone between them.
type typedChat struct{ *chat }

func newTypedChat() *typedChat {
	c := &typedChat{&chat{}}
	c.server = server.New(c.chat, c.config(server.Config{Profiles: c, Commands: c}))
	return c
}

func (c *typedChat) Me(_ context.Context, conn *server.Conn, p apron.MeParams, reply server.Reply[apron.CurrentUser]) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	old, _ := conn.Identity()
	cur := server.ApplyMe(old, p)
	c.server.PublishUser(server.UserChange{Old: old, New: cur, Except: conn}, nil)
	return reply.Send(cur.Complete(old))
}

// Command sends a notice, broadcasts the line, sends another notice, and
// fails a line that says so.
func (c *typedChat) Command(_ context.Context, conn *server.Conn, p apron.CommandParams, reply server.Reply[apron.CommandResult]) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	conn.Notify(apron.Notice(p.RoomID, "one"))
	frame, err := apron.Encode(apron.Message{From: identity(conn), Body: &apron.Body{Text: p.Body.Text}})
	if err != nil {
		return err
	}
	c.server.Broadcast(frame, nil)
	conn.Notify(apron.Notice(p.RoomID, "two"))
	if p.Body.Text == "/fail" {
		return apron.InvalidParams("failed")
	}
	return reply.Send(apron.CommandResult{})
}

// The typed requests and expectations decode what a Client receives.
func TestTyped(t *testing.T) {
	ctx := timeout(t)
	c := newTypedChat()
	ada, bob := aprontest.Connect(t, c.server), aprontest.Connect(t, c.server)
	defer ada.Close()
	defer bob.Close()
	for _, client := range []*aprontest.Client{ada, bob} {
		if _, err := client.Auth(ctx, map[string]any{"scheme": "guest"}); err != nil {
			t.Fatal(err)
		}
		if welcome, err := aprontest.Expect[apron.Message](ctx, client, nil); err != nil || welcome.Body.Text != "welcome" {
			t.Fatalf("welcome: %+v %v", welcome, err)
		}
	}

	sent, err := ada.Post(ctx, apron.MessageParams{Body: &apron.Body{Text: "hi"}})
	if err != nil || sent.MessageID == 0 {
		t.Fatalf("Post: %+v %v", sent, err)
	}
	for _, client := range []*aprontest.Client{ada, bob} {
		if m, err := aprontest.Expect(ctx, client, func(m apron.Message) bool { return m.MessageID == sent.MessageID }); err != nil || m.Body.Text != "hi" {
			t.Fatalf("Expect: %+v %v", m, err)
		}
	}
	if _, err := ada.Post(ctx, apron.MessageParams{}); apron.CodeOf(err) != apron.CodeInvalidParams {
		t.Fatalf("Post without a body: %v", err)
	}
	history, err := aprontest.Call[apron.HistoryResult](ctx, ada, "history", apron.HistoryParams{Limit: 10})
	if err != nil || len(history.Messages) != 1 || history.Messages[0].MessageID != sent.MessageID {
		t.Fatalf("Call[HistoryResult]: %+v %v", history, err)
	}
	if _, err := aprontest.Call[apron.HistoryResult](ctx, ada, "nope", nil); apron.CodeOf(err) != apron.CodeUnsupported {
		t.Fatalf("Call of an unknown method: %v", err)
	}

	// Me returns the complete object; the others are sent the change, and
	// the user's own connection nothing besides the result.
	name := "Ada"
	you, err := ada.Me(ctx, apron.MeParams{Name: &name, Ext: apron.Ext{"tz": []byte(`"UTC"`)}})
	if err != nil || you.Name != "Ada" || string(you.Ext["tz"]) != `"UTC"` || len(you.Cleared) != 0 {
		t.Fatalf("Me: %+v %v", you, err)
	}
	update, err := aprontest.Expect(ctx, bob, func(u apron.UserUpdate) bool { return u.New != nil && u.New.UserID == you.UserID })
	if err != nil || update.New.Name != "Ada" {
		t.Fatalf("Expect[UserUpdate]: %+v %v", update, err)
	}
	empty := ""
	if you, err := ada.Me(ctx, apron.MeParams{Name: &empty, Ext: apron.Ext{"tz": []byte(`""`)}}); err != nil || you.Name != "" || you.Ext != nil || !slices.Equal(you.Cleared, []string{"name"}) {
		t.Fatalf("Me clearing: %+v %v", you, err)
	}
	if update, err := aprontest.Expect[apron.UserUpdate](ctx, bob, nil); err != nil || !slices.Equal(update.New.Cleared, []string{"name"}) || string(update.New.Ext["tz"]) != `""` {
		t.Fatalf("Expect[UserUpdate] clearing: %+v %v", update, err)
	}
	if err := ada.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if pending := ada.Pending(); slices.ContainsFunc(pending, aprontest.Method("user")) {
		t.Fatalf("Me's own connection was sent %v besides the result", pending)
	}

	// Command takes the notices ahead of its result, and leaves the rest.
	notices, err := ada.Command(ctx, "", "/go")
	if err != nil || !slices.Equal(notices, []string{"one", "two"}) {
		t.Fatalf("Command: %q %v", notices, err)
	}
	if m, err := aprontest.Expect[apron.Message](ctx, ada, nil); err != nil || m.Body.Text != "/go" {
		t.Fatalf("the command's effect: %+v %v", m, err)
	}
	notices, err = ada.Command(ctx, "", "/fail")
	if apron.CodeOf(err) != apron.CodeInvalidParams || !slices.Equal(notices, []string{"one", "two"}) {
		t.Fatalf("a failed Command: %q %v", notices, err)
	}
	if err := ada.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if pending := ada.Pending(); len(pending) != 1 || !strings.Contains(string(pending[0].Raw), "/fail") {
		t.Fatalf("after a failed Command: %v", pending)
	}
}

// identity is a connection's identity, its user once signed in.
func identity(c *server.Conn) apron.User {
	u, _ := c.Identity()
	return u
}
