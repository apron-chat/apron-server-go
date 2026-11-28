package main

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/aprontest"
)

// signIn connects a guest, and takes the welcome notice after the result.
func signIn(t *testing.T, ctx context.Context, chat *Chat, name string) (*aprontest.Client, apron.User) {
	t.Helper()
	c, you := aprontest.SignIn(t, chat.Server, map[string]any{"scheme": "guest", "name": name})
	if hello := c.Hello(); hello.Title != "Minimal" || you.Name != name {
		t.Fatalf("server frame %+v, you %+v", hello, you)
	}
	if welcome, err := aprontest.Expect[apron.Message](ctx, c, nil); err != nil || welcome.From.UserID != apron.PrivateNotice.UserID || !strings.HasPrefix(welcome.Body.Text, "Welcome, "+name) {
		t.Fatalf("welcome: %+v %v", welcome, err)
	}
	return c, you.User
}

// text is a message request's params with text.
func text(s string) apron.MessageParams {
	return apron.MessageParams{Body: apron.Body{Text: s}}
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
	for _, c := range []*aprontest.Client{ada, bob} {
		if m, err := aprontest.Expect[apron.Message](ctx, c, nil); err != nil || m.MessageID != sent.MessageID || m.Body.Text != "hi" || m.From.Name != "Ada" {
			t.Fatalf("message: %+v %v", m, err)
		}
	}
	elsewhere := text("x")
	elsewhere.RoomID = "elsewhere"
	if _, err := ada.Post(ctx, elsewhere); apron.CodeOf(err) != apron.CodeInvalidParams {
		t.Fatalf("another room: %v", err)
	}
	toNothing := text("x")
	toNothing.ReplyTo = &apron.MessageRef{MessageID: 999}
	if _, err := ada.Post(ctx, toNothing); apron.CodeOf(err) != apron.CodeInvalidParams {
		t.Fatalf("a reply to nothing: %v", err)
	}

	// A profile change is the result, complete, with what it clears as its
	// empty value (§3.3.2), and goes to the others as new (§3.3.3).
	empty := ""
	you, err := bob.Me(ctx, apron.MeParams{Name: &empty})
	if err != nil || you.UserID != bobUser.UserID || !slices.Equal(you.Cleared, []string{"name"}) {
		t.Fatalf("me: %+v %v", you, err)
	}
	update, err := aprontest.Expect(ctx, ada, func(u apron.UserUpdate) bool { return u.New != nil })
	if err != nil || update.New.UserID != bobUser.UserID || !slices.Equal(update.New.Cleared, []string{"name"}) {
		t.Fatalf("ada's user frame: %+v %v", update, err)
	}
	// The result told Bob's connection: it gets no user frame besides.
	if err := bob.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(bob.Pending(), aprontest.Method("user")); i >= 0 {
		t.Fatalf("bob's user frame %s besides the result", bob.Pending()[i].Raw)
	}

	if notices, err := ada.Command(ctx, "", "/help"); err != nil || len(notices) != 1 || !strings.Contains(notices[0], "/help") || !strings.Contains(notices[0], "/nick <name>") {
		t.Fatalf("/help: %q %v", notices, err)
	}
	if _, err := ada.Command(ctx, "", "/nope"); apron.CodeOf(err) != apron.CodeInvalidParams {
		t.Fatalf("an unknown command: %v", err)
	}
	if _, err := ada.Command(ctx, "", "/nick"); apron.CodeOf(err) != apron.CodeInvalidParams {
		t.Fatalf("/nick without a name: %v", err)
	}

	// /nick renames, as me does: the user update comes before the result.
	_, before, err := ada.CallFrames(ctx, "command", apron.CommandParams{Body: apron.Body{Text: "/nick Ada L."}})
	var renamed apron.UserUpdate
	if err != nil || len(before) != 1 || before[0].Decode(&renamed) != nil || renamed.You == nil || renamed.You.Name != "Ada L." {
		t.Fatalf("/nick: %v %v", before, err)
	}
	if update, err := aprontest.Expect[apron.UserUpdate](ctx, bob, nil); err != nil || update.New == nil || update.New.Name != "Ada L." {
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
	for _, body := range []apron.Body{
		{Text: "@echo hi", Mentions: []string{"echo"}},
		{Text: "echo: hi"},
	} {
		asked, err := ada.Post(ctx, apron.MessageParams{Body: body})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []*aprontest.Client{ada, bob} {
			m, err := aprontest.Expect(ctx, c, func(m apron.Message) bool { return m.From.UserID == "echo" })
			if err != nil || m.Body.Text != "Ada said: "+body.Text || m.ReplyTo == nil || m.ReplyTo.MessageID != asked.MessageID {
				t.Fatalf("the bot's reply %+v: %v", m, err)
			}
		}
	}
}
