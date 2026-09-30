package server

import (
	"context"
	"net/http/httptest"
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
	app, mailbox, httpServer := emailTestServer(t, func(config *Config) { config.PublicURL = "https://chat.example/app/" })
	requester, frame := dialRaw(t, httpServer)
	params := frame["params"].(map[string]any)
	if !reflect.DeepEqual(params["auth"], []any{"email", "token", "guest"}) || !reflect.DeepEqual(params["signup"], []any{"email", "guest"}) {
		t.Fatalf("auth schemes: %#v, signup %#v", params["auth"], params["signup"])
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
	// The link is built from the configured page, with the address, the
	// code, and this server's WebSocket URL in its fragment, in that order.
	want := "https://chat.example/login#email=ada%40example.com&token=" + message.Code + "&server=wss%3A%2F%2Fchat.example%2Fapp%2Fws"
	if message.Link != want {
		t.Fatalf("link %q, want %q", message.Link, want)
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
	setCode(app, "ada@example.com", "123456", time.Minute)
	fourth, _ := dialRaw(t, httpServer)
	if _, again := emailSignIn(t, fourth, "ada@example.com", "123456", map[string]any{"user_id": "other"}); again["you"].(map[string]any)["user_id"] != "ada" {
		t.Fatalf("second sign-in: %#v", again)
	}
}

// setCode gives an address an outstanding code that expires after ttl.
func setCode(app *Server, email, code string, ttl time.Duration) {
	app.mu.Lock()
	defer app.mu.Unlock()
	state := app.emailAddresses[email]
	if state == nil {
		state = &emailAddress{}
		app.emailAddresses[email] = state
	}
	state.code, state.expires, state.attempts = code, time.Now().Add(ttl), 0
}

// ageSends moves an address's sends back by d, as if time had passed.
func ageSends(app *Server, email string, d time.Duration) {
	app.mu.Lock()
	defer app.mu.Unlock()
	if state := app.emailAddresses[email]; state != nil {
		for i := range state.sends {
			state.sends[i] = state.sends[i].Add(-d)
		}
	}
}

// A code request is answered with the same {} and nothing else whether or
// not the address has an account.
func TestEmailCodeRequestsDoNotRevealAccounts(t *testing.T) {
	app, mailbox, httpServer := emailTestServer(t, nil)
	setCode(app, "ada@example.com", "123456", time.Minute)
	c, _ := dialRaw(t, httpServer)
	emailSignIn(t, c, "ada@example.com", "123456", nil)
	var replies [][]map[string]any
	for _, email := range []string{"ada@example.com", "nobody@example.com"} {
		asker, _ := dialRaw(t, httpServer)
		before, result := asker.request(t, "auth", "code", map[string]any{"scheme": "email", "email": email})
		asker.expectQuiet(t)
		replies = append(replies, append(before, result))
		mailbox.receive(t)
	}
	if !reflect.DeepEqual(replies[0], replies[1]) || len(replies[0]) != 1 || len(replies[0][0]) != 0 {
		t.Fatalf("replies differ: %#v", replies)
	}
}

// On a signed-in connection a code adds the address to that account, but
// only a code that account asked for: a link for an attacker's address,
// presented by a signed-in victim, is denied, so the attacker cannot later
// sign in as the victim. An address is never moved between accounts.
func TestEmailCodesAddToTheAccountThatAskedForThem(t *testing.T) {
	app, mailbox, httpServer := emailTestServer(t, func(config *Config) {
		config.WebAuthn = testWebAuthn(t)
		config.Roles = map[string][]string{"admin": {"victim@example.com"}}
	})
	victim, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	guestAuth(t, victim)
	victim.drain(t)
	diary, _ := saveRoom(t, victim, "diary", map[string]any{"title": "Diary", "private": true})
	secret, _ := save(t, victim, "secret", map[string]any{"room_id": diary, "body": map[string]any{"text": "my secret"}})

	// The takeover: the attacker's code, presented by the signed-in victim,
	// is denied and changes nothing.
	attacker, _ := dialRaw(t, httpServer)
	requestCode(t, attacker, "mallory@evil.example")
	code := mailbox.receive(t).Code
	victim.expectError(t, "auth", "link", map[string]any{"scheme": "email", "email": "mallory@evil.example", "token": code}, codeDenied)
	app.mu.RLock()
	stolen := app.emails["mallory@evil.example"]
	app.mu.RUnlock()
	if stolen != nil {
		t.Fatalf("the attacker's address was added to %s", stolen.id)
	}
	// The code still signs its own requester in, to an account of its own.
	later, _ := dialRaw(t, httpServer)
	if _, result := emailSignIn(t, later, "mallory@evil.example", code, nil); result["you"].(map[string]any)["user_id"] == victim.userID {
		t.Fatalf("the attacker became the victim: %#v", result)
	}
	later.expectError(t, "history", "diary", map[string]any{"room_id": diary}, codeInvalidParams)
	victim.drain(t)
	// Nor can an account take another account's address.
	ageSends(app, "mallory@evil.example", time.Hour)
	requestCode(t, victim, "mallory@evil.example")
	victim.expectError(t, "auth", "owned", map[string]any{"scheme": "email", "email": "mallory@evil.example", "token": mailbox.receive(t).Code}, codeDenied)

	// A code the victim asked for while signed in adds the address to the
	// guest, which becomes an account and keeps its messages, with the
	// roles its address was granted.
	requestCode(t, victim, "victim@example.com")
	before, result := emailSignIn(t, victim, "victim@example.com", mailbox.receive(t).Code, nil)
	you := result["you"].(map[string]any)
	if you["user_id"] != victim.userID || !reflect.DeepEqual(you["roles"], []any{"admin"}) || len(before) != 0 {
		t.Fatalf("adding an address: %#v after %#v", result, before)
	}
	save(t, victim, "edit", map[string]any{"message_id": secret, "room_id": diary, "body": map[string]any{"text": "still mine"}})
	// An account holds one address.
	ageSends(app, "victim@example.com", time.Hour)
	requestCode(t, victim, "second@example.com")
	victim.expectError(t, "auth", "second", map[string]any{"scheme": "email", "email": "second@example.com", "token": mailbox.receive(t).Code}, codeDenied)

	// A passkey registration on the signed-in account adds the passkey to
	// it (§4.9), and signs in to it later.
	authenticator := newTestAuthenticator(t)
	options := passkeyResult(t, passkeyCall(t, victim, "register-begin", "register", "begin", nil))
	registered := passkeyResult(t, passkeyCall(t, victim, "register-finish", "register", "finish", map[string]any{"credential": authenticator.registration(t, options, testPasskeyOrigin)}))
	if registered["you"].(map[string]any)["user_id"] != victim.userID {
		t.Fatalf("passkey registration: %#v", registered)
	}
	other, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	login := passkeyResult(t, passkeyCall(t, other, "login-begin", "login", "begin", nil))
	loggedIn := passkeyResult(t, passkeyCall(t, other, "login-finish", "login", "finish", map[string]any{"credential": authenticator.assertion(t, login, testPasskeyOrigin, "localhost", 0x05)}))
	if loggedIn["you"].(map[string]any)["user_id"] != victim.userID {
		t.Fatalf("passkey login: %#v", loggedIn)
	}
	// A code asked for while signed in also signs in on a new connection.
	ageSends(app, "victim@example.com", time.Hour)
	requestCode(t, victim, "victim@example.com")
	fresh, _ := dialRaw(t, httpServer)
	if _, again := emailSignIn(t, fresh, "victim@example.com", mailbox.receive(t).Code, nil); again["you"].(map[string]any)["user_id"] != victim.userID {
		t.Fatalf("sign-in with the account's address: %#v", again)
	}
}

// A passkey account signed in on the connection that asked for a code gains
// the address.
func TestEmailAddsToAPasskeyAccount(t *testing.T) {
	app, mailbox, httpServer := emailTestServer(t, func(config *Config) { config.WebAuthn = testWebAuthn(t) })
	owner, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	registered := registerTestPasskey(t, owner, newTestAuthenticator(t))
	ownerID := registered["you"].(map[string]any)["user_id"]
	owner.drain(t)
	requestCode(t, owner, "owner@example.com")
	if _, result := emailSignIn(t, owner, "owner@example.com", mailbox.receive(t).Code, nil); result["you"].(map[string]any)["user_id"] != ownerID {
		t.Fatalf("adding an address to a passkey account: %#v", result)
	}
	app.mu.RLock()
	defer app.mu.RUnlock()
	if app.emails["owner@example.com"] == nil || app.emails["owner@example.com"].id != ownerID {
		t.Fatal("the address was not added")
	}
}

func TestEmailCodesExpireAndLockOut(t *testing.T) {
	app, mailbox, httpServer := emailTestServer(t, nil)
	c, _ := dialRaw(t, httpServer)
	requestCode(t, c, "ada@example.com")
	first := mailbox.receive(t).Code
	// A newer code replaces the older one.
	ageSends(app, "ada@example.com", time.Minute)
	requestCode(t, c, "ada@example.com")
	second := mailbox.receive(t).Code
	if first != second {
		c.expectError(t, "auth", "old", map[string]any{"scheme": "email", "email": "ada@example.com", "token": first}, codeDenied)
	}
	// A few failed attempts invalidate the code.
	for range maxEmailAttempts - 2 {
		c.expectError(t, "auth", c.nextID("wrong"), map[string]any{"scheme": "email", "email": "ada@example.com", "token": "wrong"}, codeDenied)
	}
	c.expectError(t, "auth", "last-wrong", map[string]any{"scheme": "email", "email": "ada@example.com", "token": "nope"}, codeDenied)
	c.expectError(t, "auth", "invalidated", map[string]any{"scheme": "email", "email": "ada@example.com", "token": second}, codeDenied)
	// An expired code is denied.
	setCode(app, "ada@example.com", "123456", -time.Second)
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

// Wrong codes count against the address across its codes: past the budget,
// even the right code is denied with retry_after until the window passes,
// and no more codes are sent. Codes to one address are also capped per hour.
func TestEmailBudgetsSpanCodes(t *testing.T) {
	app, _, httpServer := emailTestServer(t, nil)
	c, _ := dialRaw(t, httpServer)
	wrong := func(id string) map[string]any {
		t.Helper()
		reply := c.call(t, "auth", id, map[string]any{"scheme": "email", "email": "ada@example.com", "token": "000000x"})
		return reply["error"].(map[string]any)
	}
	for i := 0; i < maxEmailFailuresPerWindow; i++ {
		if i%maxEmailAttempts == 0 {
			setCode(app, "ada@example.com", "123456", time.Minute)
		}
		if failure := wrong(c.nextID("wrong")); failure["code"] != float64(codeDenied) || failure["data"] != nil {
			t.Fatalf("wrong code %d: %#v", i, failure)
		}
	}
	setCode(app, "ada@example.com", "123456", time.Minute)
	locked := c.call(t, "auth", "locked", map[string]any{"scheme": "email", "email": "ada@example.com", "token": "123456"})["error"].(map[string]any)
	if locked["code"] != float64(codeDenied) || locked["data"].(map[string]any)["retry_after"].(float64) < 3000 {
		t.Fatalf("locked address: %#v", locked)
	}
	c.expectError(t, "auth", "no-code", map[string]any{"scheme": "email", "email": "ada@example.com"}, codeRetryAfter)
	// Guesses without an outstanding code cost nothing.
	for range maxEmailFailuresPerWindow {
		c.expectError(t, "auth", c.nextID("guess"), map[string]any{"scheme": "email", "email": "bob@example.com", "token": "123456"}, codeDenied)
	}
	app.mu.Lock()
	failures := app.emailAddresses["bob@example.com"]
	app.mu.Unlock()
	if failures != nil {
		t.Fatalf("budget spent without a code: %#v", failures)
	}

	// At most maxEmailSendsPerWindow codes an hour to one address.
	for i := range maxEmailSendsPerWindow {
		asker, _ := dialRaw(t, httpServer)
		requestCode(t, asker, "carol@example.com")
		ageSends(app, "carol@example.com", emailResendInterval)
		_ = i
	}
	asker, _ := dialRaw(t, httpServer)
	asker.expectError(t, "auth", "capped", map[string]any{"scheme": "email", "email": "carol@example.com"}, codeRetryAfter)
}

func TestNormalizeEmail(t *testing.T) {
	for value, want := range map[string]string{
		" Ada@Example.COM ":      "ada@example.com",
		"a.b+c@mail.example.org": "a.b+c@mail.example.org",
		"Ada <ada@example.com>":  "",
		"ada@localhost":          "",
		"ada@[10.0.0.5]":         "",
		"ada@10.0.0.5":           "",
		"ada@-bad.example":       "",
		"ada@exa_mple.com":       "",
		"ada":                    "",
		"":                       "",
	} {
		if got := normalizeEmail(value); got != want {
			t.Errorf("normalizeEmail(%q) = %q, want %q", value, got, want)
		}
	}
}

// Email accounts, their addresses, their tokens, and the roles granted to
// their addresses survive a restart.
func TestEmailAccountsPersistWithRoles(t *testing.T) {
	kept := store.NewMemory()
	mailbox := make(testMailbox, 4)
	configure := func(config *Config) {
		config.EmailSender = mailbox
		config.Roles = map[string][]string{"admin": {"Ada@example.com"}}
	}
	_, httpServer, stop := startWith(t, kept, t.TempDir(), configure)
	c, _ := dialRaw(t, httpServer)
	requestCode(t, c, "ada@example.com")
	_, result := emailSignIn(t, c, "ada@example.com", mailbox.receive(t).Code, nil)
	you := result["you"].(map[string]any)
	if you["user_id"] != "user_1" || !reflect.DeepEqual(you["roles"], []any{"admin"}) {
		t.Fatalf("sign-in: %#v", result)
	}
	stop()

	_, httpServer, _ = startWith(t, kept, t.TempDir(), configure)
	c, _ = dialRaw(t, httpServer)
	resumed := c.result(t, "auth", "resume", map[string]any{"scheme": "token", "token": result["token"]})
	if resumed["you"].(map[string]any)["user_id"] != "user_1" || !reflect.DeepEqual(resumed["you"].(map[string]any)["roles"], []any{"admin"}) {
		t.Fatalf("resumed after restart: %#v", resumed)
	}
	other, _ := dialRaw(t, httpServer)
	requestCode(t, other, "ada@example.com")
	if _, again := emailSignIn(t, other, "ada@example.com", mailbox.receive(t).Code, nil); again["you"].(map[string]any)["user_id"] != "user_1" {
		t.Fatalf("sign-in after restart: %#v", again)
	}
}
