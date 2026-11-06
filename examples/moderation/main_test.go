package main

import (
	"strings"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/internal/wstest"
)

func TestModeration(t *testing.T) {
	m := &Moderation{}
	m.Settings.Words = []string{"darn"}
	m.Settings.LinkAge = 24 * time.Hour
	_, server := wstest.Serve(t, apron.DefaultConfig(), m)
	ada, bob := wstest.Guest(t, server, "Ada"), wstest.Guest(t, server, "Bob")

	if _, err := ada.Post("general", "see https://example.com"); err == nil || !strings.Contains(err.Error(), "links") {
		t.Fatalf("a guest posted a link: %v", err)
	}
	id, err := ada.Post("general", "well darn")
	if err != nil {
		t.Fatal(err)
	}
	if got := bob.Message(func(p map[string]any) bool { return p["message_id"] == id }); wstest.Text(got) != "well d***" {
		t.Fatalf("masked = %q", wstest.Text(got))
	}
	if _, err := ada.Command("general", "/slowmode 30"); err == nil {
		t.Fatal("a guest turned slow mode on")
	}

	// Slow mode, as a moderator's /slowmode 30 keeps it.
	if err := m.host.Store().Put("slowmode/general", 30); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.Post("general", "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.Post("general", "second"); err == nil || !strings.Contains(err.Error(), "Slow mode") {
		t.Fatalf("second post in slow mode: %v", err)
	}
}
