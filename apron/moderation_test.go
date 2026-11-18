package apron

import (
	"context"
	"testing"
	"time"
)

// An admin or moderator deletes any message it can see; others may not,
// and nobody but the author edits.
func TestModeratorsDeleteMessages(t *testing.T) {
	var mod *Bot
	config := extensionConfig(testExtension{"mods", func(r *Registrar) error {
		var err error
		mod, err = r.Bot("mod", BotOptions{Name: "Mod"})
		return err
	}})
	config.Roles = map[string][]string{RoleModerator: {"mod"}}
	_, httpServer := newTestServer(t, config)
	clients := dialGroup(t, httpServer, 2)
	author, other := clients[0], clients[1]
	id, _ := save(t, author, "m1", map[string]any{"body": map[string]any{"text": "spam"}})
	other.drain(t)
	other.expectError(t, "message", "d1", map[string]any{"room_id": "general", "message_id": id, "deleted": true}, CodeDenied)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := mod.Call(ctx, "message", map[string]any{"room_id": "general", "message_id": id, "body": map[string]any{"text": "edited"}}); err == nil {
		t.Fatal("a moderator edited another's message")
	}
	if _, err := mod.Call(ctx, "message", map[string]any{"room_id": "general", "message_id": id, "deleted": true}); err != nil {
		t.Fatalf("a moderator's delete: %v", err)
	}
	if tombstone := notificationParams(t, author.read(t), "message"); tombstone["deleted"] != true || tombstone["from"].(map[string]any)["user_id"] != author.userID {
		t.Fatalf("tombstone = %#v", tombstone)
	}
}
