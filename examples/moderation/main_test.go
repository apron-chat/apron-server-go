package main

import (
	"strings"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/internal/wstest"
)

// mod is a test extension whose bot is a moderator.
type mod struct{ bot *apron.Bot }

func (*mod) Name() string { return "mod" }
func (m *mod) Init(r *apron.Registrar) (err error) {
	m.bot, err = r.Bot("mod", apron.BotOptions{})
	return err
}

func TestModeration(t *testing.T) {
	m := &Moderation{}
	m.Settings.Words = []string{"darn", "ärger"}
	config := apron.DefaultConfig()
	config.Roles = map[string][]string{apron.RoleModerator: {"mod"}}
	moderator := &mod{}
	_, server := wstest.Serve(t, config, m, moderator)
	if m.Settings.LinkAge != 24*time.Hour {
		t.Fatalf("LinkAge = %v, want its default", m.Settings.LinkAge)
	}
	ada, bob := wstest.Guest(t, server, "Ada"), wstest.Guest(t, server, "Bob")

	if _, err := ada.Post("general", "see https://example.com"); err == nil || !strings.Contains(err.Error(), "links") {
		t.Fatalf("a guest posted a link: %v", err)
	}
	if _, err := ada.Command("general", "/embed-append see https://example.com"); err == nil {
		t.Fatal("a guest posted a link through /embed-append")
	}
	id, err := ada.Post("general", "well darn, so much Ärger")
	if err != nil {
		t.Fatal(err)
	}
	if got := bob.Message(func(p map[string]any) bool { return p["message_id"] == id }); wstest.Text(got) != "well d***, so much Ä****" {
		t.Fatalf("masked = %q", wstest.Text(got))
	}
	if _, err := ada.Command("general", "/slowmode 30"); err == nil {
		t.Fatal("a guest turned slow mode on")
	}

	// A moderator turns slow mode on; then each member posts once in 30s.
	if _, err := moderator.bot.Call(t.Context(), "command", map[string]any{"room_id": "general", "body": map[string]any{"text": "/slowmode 30"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.Post("general", "first"); err != nil {
		t.Fatal(err)
	}
	// Slow mode counts posts from the log, which the extension follows.
	for deadline := time.Now().Add(5 * time.Second); m.slowed("general", bob.UserID) == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the post was not noted")
		}
	}
	if _, err := bob.Post("general", "second"); err == nil || !strings.Contains(err.Error(), "Slow mode") {
		t.Fatalf("second post in slow mode: %v", err)
	}
}
