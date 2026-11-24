package main

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/wire"
	"github.com/apron-chat/apron-server-go/wire/wiretest"
)

// signIn connects a guest, and takes the welcome notice after the result.
func signIn(t *testing.T, ctx context.Context, chat *Chat, name string) (*wiretest.Client, wire.User) {
	t.Helper()
	c := wiretest.Connect(chat.Server)
	t.Cleanup(func() { _ = c.Close() })
	if hello := c.Hello(); hello.Title != "Minimal" {
		t.Fatalf("server frame: %+v", hello)
	}
	result, err := c.Auth(ctx, map[string]any{"scheme": "guest", "name": name})
	if err != nil || result.You == nil || result.You.Name != name {
		t.Fatalf("auth: %+v %v", result, err)
	}
	if welcome, err := c.ExpectMessage(ctx, nil); err != nil || welcome.From.UserID != wire.Private.UserID || !strings.HasPrefix(welcome.Body.Text, "Welcome, "+name) {
		t.Fatalf("welcome: %+v %v", welcome, err)
	}
	return c, result.You.User
}

// text is a message request's params with text.
func text(s string) wire.MessageParams {
	return wire.MessageParams{Body: &wire.Body{Text: s}}
}

func TestChat(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	chat := NewChat()
	ada, _ := signIn(t, ctx, chat, "Ada")
	bob, bobUser := signIn(t, ctx, chat, "Bob")

	// A message goes to everyone, the sender's copy before the result.
	sent, err := ada.Post(ctx, text("hi"))
	if err != nil || sent.MessageID == 0 {
		t.Fatalf("message: %+v %v", sent, err)
	}
	for _, c := range []*wiretest.Client{ada, bob} {
		if m, err := c.ExpectMessage(ctx, nil); err != nil || m.MessageID != sent.MessageID || m.Body.Text != "hi" || m.From.Name != "Ada" {
			t.Fatalf("message: %+v %v", m, err)
		}
	}
	elsewhere := text("x")
	elsewhere.RoomID = "elsewhere"
	if _, err := ada.Post(ctx, elsewhere); wire.CodeOf(err) != wire.CodeInvalidParams {
		t.Fatalf("another room: %v", err)
	}
	toNothing := text("x")
	toNothing.ReplyTo = &wire.Ref{MessageID: 999}
	if _, err := ada.Post(ctx, toNothing); wire.CodeOf(err) != wire.CodeInvalidParams {
		t.Fatalf("a reply to nothing: %v", err)
	}

	// A profile change is the result, complete, with what it clears as its
	// empty value (§3.3.2), and goes to the others as new (§3.3.3).
	empty := ""
	you, err := bob.Me(ctx, wire.MeParams{Name: &empty})
	if err != nil || you.UserID != bobUser.UserID || !slices.Equal(you.Cleared, []string{"name"}) {
		t.Fatalf("me: %+v %v", you, err)
	}
	update, err := wiretest.Expect(ctx, ada, func(u wire.UserUpdate) bool { return u.New != nil })
	if err != nil || update.New.UserID != bobUser.UserID || !slices.Equal(update.New.Cleared, []string{"name"}) {
		t.Fatalf("ada's user frame: %+v %v", update, err)
	}
	// The result told Bob's connection: it gets no user frame besides.
	if err := bob.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(bob.Pending(), wiretest.Method("user")); i >= 0 {
		t.Fatalf("bob's user frame %s besides the result", bob.Pending()[i].Raw)
	}

	if notices, err := ada.Command(ctx, "", "/help"); err != nil || len(notices) != 1 || !strings.Contains(notices[0], "/help") || !strings.Contains(notices[0], "/nick <name>") {
		t.Fatalf("/help: %q %v", notices, err)
	}
	if _, err := ada.Command(ctx, "", "/nope"); wire.CodeOf(err) != wire.CodeInvalidParams {
		t.Fatalf("an unknown command: %v", err)
	}
	if _, err := ada.Command(ctx, "", "/nick"); wire.CodeOf(err) != wire.CodeInvalidParams {
		t.Fatalf("/nick without a name: %v", err)
	}

	// /nick renames, as me does: the user update comes before the result.
	_, before, err := ada.CallFrames(ctx, "command", wire.CommandParams{Body: &wire.Body{Text: "/nick Ada L."}})
	var renamed wire.UserUpdate
	if err != nil || len(before) != 1 || before[0].Decode(&renamed) != nil || renamed.You == nil || renamed.You.Name != "Ada L." {
		t.Fatalf("/nick: %v %v", before, err)
	}
	if update, err := wiretest.Expect[wire.UserUpdate](ctx, bob, nil); err != nil || update.New == nil || update.New.Name != "Ada L." {
		t.Fatalf("bob's user frame: %+v %v", update, err)
	}
	if raw, err := ada.Call(ctx, "ext:command_list", nil); err != nil || string(raw) != `{"commands":[{"description":"list the commands you can use here","group":"all","name":"help"},{"description":"change your name","hint":"<name>","name":"nick"}]}` {
		t.Fatalf("command list: %s %v", raw, err)
	}
}

// The bot answers a mention, or its name in the text, as a reply that
// everyone gets.
func TestEcho(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	chat := NewChat()
	ada, _ := signIn(t, ctx, chat, "Ada")
	bob, _ := signIn(t, ctx, chat, "Bob")
	for _, body := range []wire.Body{
		{Text: "@echo hi", Mentions: []string{"echo"}},
		{Text: "echo: hi"},
	} {
		asked, err := ada.Post(ctx, wire.MessageParams{Body: &body})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []*wiretest.Client{ada, bob} {
			m, err := c.ExpectMessage(ctx, func(m wire.Message) bool { return m.From.UserID == "echo" })
			if err != nil || m.Body.Text != "Ada said: "+body.Text || m.ReplyTo == nil || m.ReplyTo.MessageID != asked.MessageID {
				t.Fatalf("the bot's reply %+v: %v", m, err)
			}
		}
	}
}
