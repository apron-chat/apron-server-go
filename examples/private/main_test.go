package main

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/aprontest"
	"github.com/apron-chat/apron-server-go/store"
)

func serve(t *testing.T, memory store.Store, mail *aprontest.Mailbox) *aprontest.Server {
	p := &Private{}
	p.Settings.Allow = []string{"ada@example.com"}
	config := apron.DefaultConfig()
	config.Store = memory
	config.EmailSender = mail
	config.Roles = map[string][]string{apron.RoleAdmin: {"ada@example.com"}}
	return aprontest.Serve(t, config, p)
}

func TestPrivate(t *testing.T) {
	memory, mail := store.NewMemory(), &aprontest.Mailbox{}
	server := serve(t, memory, mail)

	// Before signing in, nobody learns more than how to sign in.
	stranger := server.Dial()
	if hello := stranger.Hello(); slices.Contains(hello.Auth, "guest") || hello.Ext != nil {
		t.Fatalf("server frame before sign-in = %#v", hello)
	}
	if err := stranger.SignInAsGuest("Mallory"); err == nil {
		t.Fatal("a guest signed in")
	}
	if err := stranger.AskForCode("mallory@example.com"); err != nil {
		t.Fatal(err)
	}
	if code := mail.Code("mallory@example.com", 200*time.Millisecond); code != "" {
		t.Fatal("a code was mailed to an address not on the list")
	}

	// Ada, on the list by flag and an admin, lets Bob in.
	ada := server.Account("ada@example.com")
	if _, err := ada.Command("general", "/allow bob@example.com"); err != nil {
		t.Fatal(err)
	}
	bob := server.Account("bob@example.com")
	if _, err := bob.Command("general", "/allow mallory@example.com"); err == nil {
		t.Fatal("someone who is not an admin changed the list")
	}
	if _, err := ada.Command("general", "/disallow ada@example.com"); err == nil || !strings.Contains(err.Error(), "--private.allow") {
		t.Fatalf("disallowing an entry from the flags: %v", err)
	}
	if notices, err := ada.Command("general", "/invite"); err != nil || len(notices) != 1 || !strings.Contains(notices[0], "apron_invite_") {
		t.Fatalf("/invite = %q, %v", notices, err)
	}
	if invitations := server.Invitations(); len(invitations) != 1 || invitations[0].By != "private" {
		t.Fatalf("invitations = %#v", invitations)
	}
	if _, err := bob.Command("general", "/invite"); err == nil {
		t.Fatal("someone who is not an admin invited someone")
	}
	notices, err := ada.Command("general", "/allowed")
	if err != nil || len(notices) != 1 || !strings.Contains(notices[0], "`email:bob@example.com`") {
		t.Fatalf("/allowed = %q, %v", notices, err)
	}

	// The list survives a restart.
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	server = serve(t, memory, mail)
	ada, bob = server.Account("ada@example.com"), server.Account("bob@example.com")

	// Taken off the list, Bob is signed out, and stays out.
	if _, err := ada.Command("general", "/disallow bob@example.com"); err != nil {
		t.Fatal(err)
	}
	if !bob.Closed() {
		t.Fatal("Bob's connection stayed open")
	}
	if err := server.Dial().SignInWithToken(bob.Token); bob.Token == "" || err == nil {
		t.Fatal("Bob's session token still signs in")
	}
}
