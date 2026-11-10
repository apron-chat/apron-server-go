package main

import (
	"context"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/internal/wstest"
	"github.com/apron-chat/apron-server-go/store"
)

// mailbox hands the test the codes email sign-in sends.
type mailbox chan apron.SignInEmail

func (m mailbox) SendSignInCode(_ context.Context, message apron.SignInEmail) error {
	m <- message
	return nil
}

// code returns the code mailed to address, or "" if none came.
func (m mailbox) code(t *testing.T, address string) string {
	t.Helper()
	select {
	case message := <-m:
		if message.To != address {
			t.Fatalf("mailed %s, want %s", message.To, address)
		}
		return message.Code
	case <-time.After(200 * time.Millisecond):
		return ""
	}
}

// signIn signs in with an email address on a new connection, or fails
// the test when it cannot, and returns the connection and its session
// token.
func signIn(t *testing.T, server *httptest.Server, mail mailbox, address string) (*wstest.Client, string) {
	t.Helper()
	c, _ := wstest.Dial(t, server)
	if _, err := c.Call("auth", map[string]any{"scheme": "email", "email": address}); err != nil {
		t.Fatal(err)
	}
	code := mail.code(t, address)
	if code == "" {
		t.Fatalf("no code was mailed to %s", address)
	}
	result, err := c.Call("auth", map[string]any{"scheme": "email", "token": code})
	if err != nil {
		t.Fatal(err)
	}
	c.UserID = result["you"].(map[string]any)["user_id"].(string)
	return c, result["token"].(string)
}

func serve(t *testing.T, memory store.Store, mail mailbox) (*apron.Server, *httptest.Server) {
	p := &Private{}
	p.Settings.Allow = []string{"ada@example.com"}
	config := apron.DefaultConfig()
	config.Store = memory
	config.EmailSender = mail
	config.Roles = map[string][]string{apron.RoleAdmin: {"ada@example.com"}}
	return wstest.Serve(t, config, p)
}

func TestPrivate(t *testing.T) {
	memory, mail := store.NewMemory(), make(mailbox, 8)
	app, server := serve(t, memory, mail)

	// Before signing in, nobody learns more than how to sign in.
	stranger, frame := wstest.Dial(t, server)
	params := frame.Params()
	if auth := params["auth"].([]any); slices.Contains(auth, any("guest")) || params["ext"] != nil {
		t.Fatalf("server frame before sign-in = %#v", params)
	}
	if _, err := stranger.Call("auth", map[string]any{"scheme": "guest"}); err == nil {
		t.Fatal("a guest signed in")
	}
	if _, err := stranger.Call("auth", map[string]any{"scheme": "email", "email": "mallory@example.com"}); err != nil {
		t.Fatal(err)
	}
	if code := mail.code(t, "mallory@example.com"); code != "" {
		t.Fatal("a code was mailed to an address not on the list")
	}

	// Ada, on the list by flag and an admin, lets Bob in.
	ada, _ := signIn(t, server, mail, "ada@example.com")
	if _, err := ada.Command("general", "/allow bob@example.com"); err != nil {
		t.Fatal(err)
	}
	bob, _ := signIn(t, server, mail, "bob@example.com")
	if _, err := bob.Command("general", "/allow mallory@example.com"); err == nil {
		t.Fatal("someone who is not an admin changed the list")
	}
	if _, err := ada.Command("general", "/disallow ada@example.com"); err == nil || !strings.Contains(err.Error(), "--private.allow") {
		t.Fatalf("disallowing an entry from the flags: %v", err)
	}
	if notices, err := ada.Command("general", "/invite"); err != nil || len(notices) != 1 || !strings.Contains(notices[0], "apron_invite_") {
		t.Fatalf("/invite = %q, %v", notices, err)
	}
	if invitations := app.Invitations(); len(invitations) != 1 || invitations[0].By != "private" {
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
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, server = serve(t, memory, mail)
	ada, _ = signIn(t, server, mail, "ada@example.com")
	bob, token := signIn(t, server, mail, "bob@example.com")

	// Taken off the list, Bob is signed out, and stays out.
	if _, err := ada.Command("general", "/disallow bob@example.com"); err != nil {
		t.Fatal(err)
	}
	if !bob.Closed() {
		t.Fatal("Bob's connection stayed open")
	}
	again, _ := wstest.Dial(t, server)
	if _, err := again.Call("auth", map[string]any{"scheme": "token", "token": token}); err == nil {
		t.Fatal("Bob's session token still signs in")
	}
}
