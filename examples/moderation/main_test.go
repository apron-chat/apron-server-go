package main

import (
	"strings"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/aprontest"
)

func TestModeration(t *testing.T) {
	m := &Moderation{}
	m.Settings.Words = []string{"darn", "ärger"}
	server := aprontest.Serve(t, apron.DefaultConfig(), m)
	if m.Settings.LinkAge != 24*time.Hour {
		t.Fatalf("LinkAge = %v, want its default", m.Settings.LinkAge)
	}
	ada, bob, mo := server.Guest("Ada"), server.Guest("Bob"), server.Member("mo", apron.RoleModerator)

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
	if got := bob.Message(func(m apron.Message) bool { return m.ID == id }); got.Text != "well d***, so much Ä****" {
		t.Fatalf("masked = %q", got.Text)
	}
	if _, err := ada.Command("general", "/slowmode 30"); err == nil {
		t.Fatal("a guest turned slow mode on")
	}

	// A moderator turns slow mode on; then each member posts once in 30s.
	if _, err := mo.Command("general", "/slowmode 30"); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.Post("general", "first"); err != nil {
		t.Fatal(err)
	}
	// Slow mode counts posts from the log, which the extension follows.
	server.Settle()
	if _, err := bob.Post("general", "second"); err == nil || !strings.Contains(err.Error(), "Slow mode") {
		t.Fatalf("second post in slow mode: %v", err)
	}
	if _, err := mo.Post("general", "moderators are not slowed"); err != nil {
		t.Fatal(err)
	}
}
