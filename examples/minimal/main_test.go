package main

import (
	"context"
	"encoding/json/v2"
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
	welcome, err := c.Next(ctx)
	if err != nil || !strings.Contains(string(welcome.Params), "Welcome, "+name) {
		t.Fatalf("welcome: %s %v", welcome.Raw, err)
	}
	return c, result.You.User
}

func TestChat(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	chat := NewChat()
	ada, _ := signIn(t, ctx, chat, "Ada")
	bob, bobUser := signIn(t, ctx, chat, "Bob")

	// A message goes to everyone, the sender's copy before the result.
	result, err := ada.Call(ctx, "message", map[string]any{"body": map[string]any{"text": "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	var sent wire.MessageResult
	if err := json.Unmarshal(result, &sent); err != nil || sent.MessageID == 0 {
		t.Fatalf("message result %s: %v", result, err)
	}
	for _, c := range []*wiretest.Client{ada, bob} {
		f, err := c.Next(ctx)
		var m wire.Message
		if err != nil || f.Decode(&m) != nil || m.MessageID != sent.MessageID || m.Body.Text != "hi" || m.From.Name != "Ada" {
			t.Fatalf("message frame %s: %v", f.Raw, err)
		}
	}
	if _, err := ada.Call(ctx, "message", map[string]any{"room_id": "elsewhere", "body": map[string]any{"text": "x"}}); wire.CodeOf(err) != wire.CodeInvalidParams {
		t.Fatalf("another room: %v", err)
	}
	if _, err := ada.Call(ctx, "message", map[string]any{"body": map[string]any{"text": "x"}, "reply_to": map[string]any{"message_id": "999"}}); wire.CodeOf(err) != wire.CodeInvalidParams {
		t.Fatalf("a reply to nothing: %v", err)
	}

	// A profile change goes to the user's own connections as you, to the
	// others as new, and clears what it empties.
	raw, err := bob.Call(ctx, "me", map[string]any{"name": ""})
	if err != nil || !strings.Contains(string(raw), `"name":""`) {
		t.Fatalf("me: %s %v", raw, err)
	}
	if f, err := bob.Expect(ctx, wiretest.Method("user")); err != nil || !strings.Contains(string(f.Params), `"you"`) {
		t.Fatalf("bob's user frame: %s %v", f.Raw, err)
	}
	f, err := ada.Expect(ctx, wiretest.Method("user"))
	var update wire.UserUpdate
	if err != nil || f.Decode(&update) != nil || update.New == nil || update.New.UserID != bobUser.UserID {
		t.Fatalf("ada's user frame: %s %v", f.Raw, err)
	}

	if _, err := ada.Call(ctx, "command", map[string]any{"body": map[string]any{"text": "/help"}}); err != nil {
		t.Fatal(err)
	}
	if f, err := ada.Next(ctx); err != nil || !strings.Contains(string(f.Params), "/help") || !strings.Contains(string(f.Params), "/nick <name>") {
		t.Fatalf("help: %s %v", f.Raw, err)
	}
	if _, err := ada.Call(ctx, "command", map[string]any{"body": map[string]any{"text": "/nope"}}); wire.CodeOf(err) != wire.CodeInvalidParams {
		t.Fatalf("an unknown command: %v", err)
	}
	if _, err := ada.Call(ctx, "command", map[string]any{"body": map[string]any{"text": "/nick"}}); wire.CodeOf(err) != wire.CodeInvalidParams {
		t.Fatalf("/nick without a name: %v", err)
	}

	// /nick renames, as me does: the user update comes before the result.
	if _, before, err := ada.CallFrames(ctx, "command", map[string]any{"body": map[string]any{"text": "/nick Ada L."}}); err != nil || len(before) != 1 || !strings.Contains(string(before[0].Params), `"name":"Ada L."`) {
		t.Fatalf("/nick: %v %v", before, err)
	}
	if f, err := bob.Expect(ctx, wiretest.Method("user")); err != nil || !strings.Contains(string(f.Params), `"new"`) {
		t.Fatalf("bob's user frame: %s %v", f.Raw, err)
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
	for _, body := range []map[string]any{
		{"text": "@echo hi", "mentions": []string{"echo"}},
		{"text": "echo: hi"},
	} {
		result, err := ada.Call(ctx, "message", map[string]any{"body": body})
		if err != nil {
			t.Fatal(err)
		}
		var asked wire.MessageResult
		if err := json.Unmarshal(result, &asked); err != nil {
			t.Fatal(err)
		}
		for _, c := range []*wiretest.Client{ada, bob} {
			f, err := c.Expect(ctx, func(f wiretest.Frame) bool {
				var m wire.Message
				return f.Method == "message" && f.Decode(&m) == nil && m.From.UserID == "echo"
			})
			var m wire.Message
			if err != nil || f.Decode(&m) != nil || m.Body.Text != "Ada said: "+body["text"].(string) || m.ReplyTo == nil || m.ReplyTo.MessageID != asked.MessageID {
				t.Fatalf("the bot's reply %s: %v", f.Raw, err)
			}
		}
	}
}
