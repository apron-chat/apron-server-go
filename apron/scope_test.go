package apron

import (
	"context"
	"encoding/json/v2"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// scopedBotServer starts a server whose extension's bot, "agent", is
// limited to general, with the extension's Host and the bot.
func scopedBotServer(t *testing.T) (*Server, *httptest.Server, *Host, *Bot) {
	t.Helper()
	var host *Host
	var bot *Bot
	app, httpServer := newTestServer(t, extensionConfig(testExtension{"agents", func(r *Registrar) error {
		host = r.Host
		var err error
		bot, err = r.Bot("agent", BotOptions{Name: "Agent", Scope: &RoomScope{Rooms: []string{defaultRoomID}}})
		return err
	}}))
	return app, httpServer, host, bot
}

func roomListIDs(t *testing.T, raw []byte) []string {
	t.Helper()
	var result struct {
		Joined []struct {
			RoomID string `json:"room_id"`
		} `json:"joined"`
		Others []struct {
			RoomID string `json:"room_id"`
		} `json:"others"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range append(result.Joined, result.Others...) {
		ids = append(ids, r.RoomID)
	}
	return ids
}

func TestAScopedBotSeesOnlyItsRooms(t *testing.T) {
	app, httpServer, host, bot := scopedBotServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := dialTestClient(t, httpServer)
	other, _ := saveRoom(t, c, "other", map[string]any{"title": "Other"})
	thread, _ := saveRoom(t, c, "thread", map[string]any{"parent_room_id": defaultRoomID, "description": "Side talk"})
	threadMessage, _ := save(t, c, "in-thread", map[string]any{"room_id": thread, "body": map[string]any{"text": "psst @agent"}})

	if user, _ := host.User("agent"); !host.Visible(user, defaultRoomID) || host.Visible(user, other) || host.Visible(user, thread) {
		t.Fatal("Visible disagrees with the scope")
	}
	if _, err := bot.Post(ctx, defaultRoomID, "in scope"); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"post":     func() error { _, err := bot.Post(ctx, other, "out of scope"); return err },
		"join":     func() error { return bot.Join(ctx, other) },
		"history":  func() error { _, err := bot.History(ctx, other, HistoryOptions{}); return err },
		"thread":   func() error { _, err := bot.History(ctx, thread, HistoryOptions{}); return err },
		"new room": func() error { _, err := bot.Call(ctx, "room_set", map[string]any{"title": "Mine"}); return err },
		"new thread": func() error {
			_, err := bot.Call(ctx, "room_set", map[string]any{"parent_room_id": defaultRoomID})
			return err
		},
		"react": func() error {
			_, err := bot.Call(ctx, "reactions", map[string]any{"room_id": thread, "message_id": threadMessage, "add": []string{"👍"}})
			return err
		},
		"live text": func() error { _, err := bot.StreamAppend(ctx, other, "hi"); return err },
	} {
		if err := call(); err == nil {
			t.Errorf("%s: an out-of-scope request succeeded", name)
		}
	}
	listed, err := bot.Call(ctx, "room_list", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if ids := roomListIDs(t, listed); len(ids) != 1 || ids[0] != defaultRoomID {
		t.Fatalf("room_list = %v", ids)
	}
	threads, err := bot.Call(ctx, "room_list", map[string]any{"parent_room_id": defaultRoomID})
	if err != nil || strings.Contains(string(threads), thread) {
		t.Fatalf("threads listed: %s, %v", threads, err)
	}
	// Nobody can add it to a room outside its scope.
	c.drain(t)
	c.expectError(t, "room_join", "add", map[string]any{"room_id": other, "user_id": "agent"}, CodeDenied)

	// Taking threads in lets it read and start them.
	if err := host.SetScope(ctx, "agent", &RoomScope{Rooms: []string{defaultRoomID}, Threads: true}); err != nil {
		t.Fatal(err)
	}
	if history, err := bot.History(ctx, thread, HistoryOptions{}); err != nil || len(history) != 1 {
		t.Fatalf("thread history = %#v, %v", history, err)
	}
	if _, err := bot.Call(ctx, "room_set", map[string]any{"parent_room_id": defaultRoomID}); err != nil {
		t.Fatal(err)
	}

	// A scope without general takes the bot out of it.
	if err := host.SetScope(ctx, "agent", &RoomScope{Rooms: []string{other}}); err != nil {
		t.Fatal(err)
	}
	app.mu.RLock()
	_, inGeneral := app.users["agent"].joined[defaultRoomID]
	app.mu.RUnlock()
	if inGeneral || host.Scope("agent").Rooms[0] != other {
		t.Fatalf("still in general: %v, scope %#v", inGeneral, host.Scope("agent"))
	}
	if err := bot.Join(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := host.SetScope(ctx, "agent", nil); err != nil || host.Scope("agent") != nil {
		t.Fatalf("lifting the scope: %v", err)
	}
	if err := bot.Join(ctx, defaultRoomID); err != nil {
		t.Fatal(err)
	}
}

func TestServiceTokens(t *testing.T) {
	_, httpServer, _, bot := scopedBotServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	issued, err := bot.IssueToken(ctx, ServiceTokenOptions{Name: "python agent", Lifetime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(issued.Token, serviceTokenPrefix) || issued.UserID != "agent" || time.Until(issued.Expires) > time.Hour {
		t.Fatalf("issued: %#v", issued)
	}
	if listed := bot.ServiceTokens(); len(listed) != 1 || listed[0].Token != "" || listed[0].Name != "python agent" {
		t.Fatalf("listed: %#v", listed)
	}

	// It signs in from any origin, as the bot, with the bot's scope, and
	// may not manage the account.
	agent, _ := dialRaw(t, httpServer)
	result := agent.result(t, "auth", "auth", map[string]any{"scheme": "token", "token": issued.Token})
	if result["you"].(map[string]any)["user_id"] != "agent" {
		t.Fatalf("signed in as %#v", result)
	}
	agent.drain(t)
	agent.expectError(t, "history", "other", map[string]any{"room_id": "nowhere"}, CodeInvalidParams)
	agent.expectError(t, "account.sign_out", "manage", map[string]any{"others": true}, CodeDenied)
	agent.expectError(t, "auth", "again", map[string]any{"scheme": "guest"}, CodeDenied)
	if _, reply := runCommand(t, agent, "/invite-device laptop"); reply["error"] == nil {
		t.Fatalf("/invite-device = %#v", reply)
	}

	// Rotating it with no overlap ends the old one at once.
	rotated, err := bot.RotateToken(ctx, issued.ID, 0, ServiceTokenOptions{Name: "python agent"})
	if err != nil {
		t.Fatal(err)
	}
	expectClosed(t, agent)
	stale, _ := dialRaw(t, httpServer)
	stale.expectError(t, "auth", "stale", map[string]any{"scheme": "token", "token": issued.Token}, CodeDenied)
	fresh, _ := dialRaw(t, httpServer)
	fresh.result(t, "auth", "fresh", map[string]any{"scheme": "token", "token": rotated.Token})

	// Revoking it closes its connections.
	if ok, err := bot.RevokeToken(ctx, rotated.ID); !ok || err != nil {
		t.Fatalf("revoke: %v %v", ok, err)
	}
	expectClosed(t, fresh)

	// A token expires on time, however it is used, and its connections
	// close then.
	short, err := bot.IssueToken(ctx, ServiceTokenOptions{Lifetime: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	brief, _ := dialRaw(t, httpServer)
	brief.result(t, "auth", "brief", map[string]any{"scheme": "token", "token": short.Token})
	expectClosed(t, brief)
	late, _ := dialRaw(t, httpServer)
	late.expectError(t, "auth", "late", map[string]any{"scheme": "token", "token": short.Token}, CodeDenied)
	if listed := bot.ServiceTokens(); len(listed) != 0 {
		t.Fatalf("tokens left: %#v", listed)
	}
}
