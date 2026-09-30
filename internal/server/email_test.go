package server

import (
	"context"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/internal/store"
)

// testMailbox is an EmailSender that keeps what it is asked to send.
type testMailbox chan SignInEmail

func (m testMailbox) SendSignInCode(_ context.Context, message SignInEmail) error {
	m <- message
	return nil
}

// receive returns the next message sent to the mailbox.
func (m testMailbox) receive(t *testing.T) SignInEmail {
	t.Helper()
	select {
	case message := <-m:
		return message
	case <-time.After(time.Second):
		t.Fatal("no email was sent")
		return SignInEmail{}
	}
}

func emailTestServer(t *testing.T, configure func(*Config)) (*Server, testMailbox, *httptest.Server) {
	t.Helper()
	mailbox := make(testMailbox, 16)
	config := DefaultConfig()
	config.EmailSender = mailbox
	config.EmailLinkURL = "https://chat.example/login#ignored"
	if configure != nil {
		configure(&config)
	}
	app, httpServer := newTestServer(t, config)
	return app, mailbox, httpServer
}

// requestCode asks for a code for email on c, which must be answered with {}.
func requestCode(t *testing.T, c *testClient, email string) {
	t.Helper()
	if result := c.result(t, "auth", c.nextID("code"), map[string]any{"scheme": "email", "email": email}); len(result) != 0 {
		t.Fatalf("code request result: %#v", result)
	}
}

// emailSignIn signs c in with a code and returns the notifications before
// the result, then the result.
func emailSignIn(t *testing.T, c *testClient, email, code string, extra map[string]any) ([]map[string]any, map[string]any) {
	t.Helper()
	params := map[string]any{"scheme": "email", "email": email, "token": code}
	for key, value := range extra {
		params[key] = value
	}
	before, result := c.request(t, "auth", c.nextID("email"), params)
	if _, ok := result["token"].(string); !ok {
		t.Fatalf("email sign-in result without a token: %#v", result)
	}
	c.userID = result["you"].(map[string]any)["user_id"].(string)
	return before, result
}

func TestEmailSignIn(t *testing.T) {
	app, mailbox, httpServer := emailTestServer(t, nil)
	requester, frame := dialRaw(t, httpServer)
	if auth := frame["params"].(map[string]any)["auth"]; !reflect.DeepEqual(auth, []any{"email", "token", "guest"}) {
		t.Fatalf("auth schemes: %#v", auth)
	}

	// Asking for a code authenticates nothing, so requests behind it are
	// denied; the address is normalized.
	requester.write(t, map[string]any{"method": "auth", "id": "code", "params": map[string]any{"scheme": "email", "email": " Ada@Example.com "}})
	requester.write(t, map[string]any{"method": "room_list", "id": "list", "params": map[string]any{}})
	if reply := requester.read(t); reply["id"] != "code" || len(reply["result"].(map[string]any)) != 0 {
		t.Fatalf("code request reply: %#v", reply)
	}
	if reply := requester.read(t); reply["id"] != "list" || reply["error"].(map[string]any)["code"] != float64(codeDenied) {
		t.Fatalf("request behind a code request: %#v", reply)
	}
	message := mailbox.receive(t)
	if message.To != "ada@example.com" || len(message.Code) != 6 || strings.Trim(message.Code, "0123456789") != "" {
		t.Fatalf("sent: %#v", message)
	}
	// The link is built from the configured page, with the address and code
	// in its fragment.
	link, _ := url.Parse(message.Link)
	fragment, _ := url.ParseQuery(link.Fragment)
	if link.Host != "chat.example" || link.Path != "/login" || link.RawQuery != "" ||
		fragment.Get("email") != "ada@example.com" || fragment.Get("token") != message.Code {
		t.Fatalf("link: %q", message.Link)
	}
	if !strings.Contains(message.Text(), message.Code) || !strings.Contains(message.Text(), message.Link) {
		t.Fatalf("text: %q", message.Text())
	}
	// Another code for the address so soon is retry_after.
	requester.expectError(t, "auth", "again", map[string]any{"scheme": "email", "email": "ada@example.com"}, codeRetryAfter)
	requester.expectError(t, "auth", "invalid", map[string]any{"scheme": "email", "email": "Ada <ada@example.com>"}, codeInvalidParams)
	requester.expectError(t, "auth", "missing", map[string]any{"scheme": "email"}, codeInvalidParams)

	// The sign-in happens on the connection that presents the code: a new
	// account, which joins general like a new guest, before the result.
	reader, _ := dialRaw(t, httpServer)
	reader.expectError(t, "auth", "wrong", map[string]any{"scheme": "email", "email": "ada@example.com", "token": "wrong"}, codeDenied)
	reader.expectError(t, "auth", "other", map[string]any{"scheme": "email", "email": "bob@example.com", "token": message.Code}, codeDenied)
	before, result := emailSignIn(t, reader, "ada@example.com", message.Code, map[string]any{"name": "Ada", "user_id": "ada"})
	if you := result["you"].(map[string]any); you["user_id"] != "ada" || you["name"] != "Ada" {
		t.Fatalf("new account: %#v", result)
	}
	if len(before) != 1 {
		t.Fatalf("frames before the sign-in result: %#v", before)
	}
	checkMembership(t, notificationParams(t, before[0], "membership"), "general", "ada", true)
	// A code works once.
	third, _ := dialRaw(t, httpServer)
	third.expectError(t, "auth", "reuse", map[string]any{"scheme": "email", "email": "ada@example.com", "token": message.Code}, codeDenied)
	// The bearer token resumes the account on later connections.
	resumed := third.result(t, "auth", "resume", map[string]any{"scheme": "token", "token": result["token"]})
	if resumed["you"].(map[string]any)["user_id"] != "ada" || resumed["token"] != result["token"] {
		t.Fatalf("token resume: %#v", resumed)
	}
	// Signing in again with the address finds the same account.
	app.mu.Lock()
	app.emailCodes["ada@example.com"] = &emailCode{code: "123456", expires: time.Now().Add(time.Minute), sent: time.Now()}
	app.mu.Unlock()
	fourth, _ := dialRaw(t, httpServer)
	if _, again := emailSignIn(t, fourth, "ada@example.com", "123456", map[string]any{"user_id": "other"}); again["you"].(map[string]any)["user_id"] != "ada" {
		t.Fatalf("second sign-in: %#v", again)
	}
}

func TestEmailCodesExpireAndLockOut(t *testing.T) {
	app, mailbox, httpServer := emailTestServer(t, nil)
	c, _ := dialRaw(t, httpServer)
	requestCode(t, c, "ada@example.com")
	first := mailbox.receive(t).Code
	// A newer code replaces the older one.
	app.mu.Lock()
	app.emailCodes["ada@example.com"].sent = time.Now().Add(-time.Hour)
	app.mu.Unlock()
	requestCode(t, c, "ada@example.com")
	second := mailbox.receive(t).Code
	if first != second {
		c.expectError(t, "auth", "old", map[string]any{"scheme": "email", "email": "ada@example.com", "token": first}, codeDenied)
	}
	// A few failed attempts invalidate the code.
	for i := range maxEmailAttempts - 1 {
		c.expectError(t, "auth", c.nextID("wrong"), map[string]any{"scheme": "email", "email": "ada@example.com", "token": "x" + string(rune('a'+i))}, codeDenied)
	}
	c.expectError(t, "auth", "last-wrong", map[string]any{"scheme": "email", "email": "ada@example.com", "token": "nope"}, codeDenied)
	c.expectError(t, "auth", "locked", map[string]any{"scheme": "email", "email": "ada@example.com", "token": second}, codeDenied)
	// An expired code is denied.
	app.mu.Lock()
	app.emailCodes["ada@example.com"] = &emailCode{code: "123456", expires: time.Now().Add(-time.Second), sent: time.Now().Add(-time.Hour)}
	app.mu.Unlock()
	c.expectError(t, "auth", "expired", map[string]any{"scheme": "email", "email": "ada@example.com", "token": "123456"}, codeDenied)
	// A connection may ask for only a few codes.
	for i := range emailSendsPerConnection - 2 {
		requestCode(t, c, "user"+string(rune('a'+i))+"@example.com")
	}
	c.expectError(t, "auth", "flood", map[string]any{"scheme": "email", "email": "flood@example.com"}, codeRetryAfter)
	// Email sign-in is not offered without a sender.
	_, plain := newTestServer(t, DefaultConfig())
	guest, _ := dialRaw(t, plain)
	guest.expectError(t, "auth", "email", map[string]any{"scheme": "email", "email": "ada@example.com"}, codeUnsupported)
	guest.expectError(t, "auth", "token", map[string]any{"scheme": "token", "token": "x"}, codeUnsupported)
}

// A guest who signs in with a new address keeps their identity: the address
// is added to it, so their messages stay theirs, and it becomes an account
// with the roles its address was granted.
func TestEmailSignInKeepsTheGuestIdentity(t *testing.T) {
	kept := store.NewMemory()
	mailbox := make(testMailbox, 4)
	configure := func(config *Config) {
		config.EmailSender = mailbox
		config.Roles = map[string][]string{"admin": {"Ada@example.com"}}
	}
	app, httpServer, stop := startWith(t, kept, t.TempDir(), configure)
	guest := dialTestClient(t, httpServer)
	id, _ := save(t, guest, "hello", map[string]any{"body": map[string]any{"text": "hello"}})
	requestCode(t, guest, "ada@example.com")
	before, result := emailSignIn(t, guest, "ada@example.com", mailbox.receive(t).Code, nil)
	you := result["you"].(map[string]any)
	if you["user_id"] != "guest_1" || !reflect.DeepEqual(you["roles"], []any{"admin"}) || len(before) != 0 {
		t.Fatalf("guest sign-in: %#v after %#v", result, before)
	}
	save(t, guest, "edit", map[string]any{"message_id": id, "body": map[string]any{"text": "edited"}})
	app.mu.RLock()
	account := app.users["guest_1"].account()
	app.mu.RUnlock()
	if !account {
		t.Fatal("the guest did not become an account")
	}
	stop()

	// The account, its address, and its token survive a restart.
	_, httpServer, _ = startWith(t, kept, t.TempDir(), configure)
	c, _ := dialRaw(t, httpServer)
	resumed := c.result(t, "auth", "resume", map[string]any{"scheme": "token", "token": result["token"]})
	if resumed["you"].(map[string]any)["user_id"] != "guest_1" || !reflect.DeepEqual(resumed["you"].(map[string]any)["roles"], []any{"admin"}) {
		t.Fatalf("resumed after restart: %#v", resumed)
	}
	other, _ := dialRaw(t, httpServer)
	requestCode(t, other, "ada@example.com")
	if _, again := emailSignIn(t, other, "ada@example.com", mailbox.receive(t).Code, nil); again["you"].(map[string]any)["user_id"] != "guest_1" {
		t.Fatalf("sign-in after restart: %#v", again)
	}
}
