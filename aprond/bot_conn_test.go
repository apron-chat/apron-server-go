package aprond

import (
	"context"
	"encoding/json/jsontext"
	"reflect"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
)

// A bot's connection in package apron/server's terms makes requests with
// package apron's params and results, as the bot, and stays signed in as
// it.
func TestBotConn(t *testing.T) {
	var bot *Bot
	_, httpServer := newTestServer(t, extensionConfig(testExtension{"helper", func(r *Registrar) error {
		var err error
		bot, err = r.Bot("helper", BotOptions{Name: "Helper"})
		return err
	}}))
	c := dialTestClient(t, httpServer)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := bot.Conn()
	if err != nil {
		t.Fatal(err)
	}
	if conn.User().UserID != "helper" {
		t.Fatalf("Conn signed in as %q", conn.User().UserID)
	}
	posted, err := callAs[apron.MessageResult](ctx, conn, "message", apron.MessageParams{
		RoomID: "general",
		Body:   apron.Body{Text: "**hi** @" + c.userID, Format: apron.Markdown, Mentions: []string{c.userID}},
		Ext:    apron.Ext{}.With("helper", map[string]any{"n": 1}),
	})
	if err != nil || posted.MessageID == 0 {
		t.Fatalf("Post = %+v, %v", posted, err)
	}
	if got := notificationParams(t, c.read(t), "message"); got["message_id"] != posted.MessageID.String() {
		t.Fatalf("guest got %#v", got)
	}
	if _, err := conn.Call(ctx, "message", apron.MessageParams{RoomID: "nowhere", Body: apron.Body{Text: "x"}}); apron.CodeOf(err) != CodeInvalidParams {
		t.Fatalf("Post to no room: %v", err)
	}
	if _, err := conn.Call(ctx, "auth", map[string]any{"scheme": "guest"}); apron.CodeOf(err) != CodeDenied {
		t.Fatalf("auth on a bot's connection: %v", err)
	}
	if _, err := conn.Call(ctx, "command", apron.CommandParams{RoomID: "general", Body: apron.Body{Text: "/help"}}); err != nil {
		t.Fatalf("/help: %v", err)
	}

	page, err := callAs[apron.HistoryResult](ctx, conn, "history", apron.HistoryParams{RoomID: "general", Limit: 10})
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("History = %+v, %v", page, err)
	}
	history, err := bot.History(ctx, "general", HistoryOptions{Limit: 10})
	if err != nil || len(history) != 1 {
		t.Fatalf("bot.History = %+v, %v", history, err)
	}
	m := MessageOf(page.Messages[0])
	if !reflect.DeepEqual(m, history[0]) || m.ID != posted.MessageID.String() || m.Text != "**hi** @"+c.userID ||
		m.Format != apron.Markdown || !m.MentionsUser(c.userID) || string(m.Ext["helper"]) != `{"n":1}` {
		t.Fatalf("MessageOf = %#v, History has %#v", m, history[0])
	}
	if back := MessageOf(m.Apron()); !reflect.DeepEqual(back, m) {
		t.Fatalf("MessageOf(Apron()) = %#v, want %#v", back, m)
	}
}

// Message and apron.Message say the same of a message: a reply, a moved
// snapshot, a body with members apron has no field for, a tombstone, and
// a notice.
func TestMessageOfAndApron(t *testing.T) {
	reply := apron.Message{
		MessageID: 7, LogID: 9, RoomID: "general", PrevLogID: 8, PrevRoomID: "random",
		From:    apron.User{UserID: "ada", Name: "Ada"},
		Body:    apron.Body{Text: "yes", Mentions: []string{"bob"}, Unknown: jsontext.Value(`{"x":1}`)},
		ReplyTo: apron.MessageRef{MessageID: 3},
		Ext:     apron.Ext{"irc": jsontext.Value(`{"nick":"ada_"}`)},
	}
	m := MessageOf(reply)
	if m.ID != "7" || m.LogID != 9 || m.PrevLogID != 8 || m.PrevRoomID != "random" || m.ReplyTo != "3" ||
		m.Text != "yes" || m.Format != apron.Plain || m.Body()["x"] != float64(1) || m.From.Name != "Ada" || m.New() {
		t.Fatalf("MessageOf = %#v", m)
	}
	back := m.Apron()
	if back.MessageID != 7 || back.ReplyTo.MessageID != 3 || back.Body.Text != "yes" || string(back.Body.Unknown) != `{"x":1}` ||
		string(back.Ext["irc"]) != `{"nick":"ada_"}` {
		t.Fatalf("Apron = %#v", back)
	}
	if !reflect.DeepEqual(MessageOf(back), m) {
		t.Fatalf("MessageOf(Apron()) = %#v, want %#v", MessageOf(back), m)
	}

	tombstone := MessageOf(apron.Message{MessageID: 7, LogID: 10, PrevLogID: 9, RoomID: "general", From: apron.User{UserID: "ada"}, Deleted: true})
	if back := tombstone.Apron(); !tombstone.Deleted || tombstone.Body() != nil || !back.Body.IsZero() {
		t.Fatalf("tombstone = %#v", tombstone)
	}
	notice := MessageOf(apron.Notice("general", "hello"))
	if notice.ID != "" || notice.From.UserID != privateNoticeID || notice.Text != "hello" || notice.Apron().MessageID != 0 {
		t.Fatalf("notice = %#v", notice)
	}
	made := Message{ID: "12", RoomID: "general", Text: "hand made", Format: apron.Markdown}.Apron()
	if made.MessageID != 12 || made.Body.IsZero() || made.Body.Text != "hand made" || made.Body.Format != apron.Markdown {
		t.Fatalf("Apron of a Message without Body = %#v", made)
	}
}
