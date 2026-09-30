package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/apron-chat/apron-server-go/internal/store"
)

// testMailbox is an EmailSender that keeps what it is asked to send.
type testMailbox chan SignInEmail

func (m testMailbox) SendSignInCode(_ context.Context, message SignInEmail) error {
	select {
	case m <- message:
	default: // A test that does not read its mail drops it.
	}
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

// setCode gives an address an outstanding sign-in code that expires after
// ttl.
func setCode(app *Server, email, code string, ttl time.Duration) {
	app.mu.Lock()
	defer app.mu.Unlock()
	app.email.createAddress(email, time.Now()).signIn = &emailCode{code: code, expires: time.Now().Add(ttl)}
}

// ageSends moves an address's sends back by d, as if time had passed.
func ageSends(app *Server, email string, d time.Duration) {
	app.mu.Lock()
	defer app.mu.Unlock()
	if state := app.email.addresses[email]; state != nil {
		for i := range state.sends {
			state.sends[i] = state.sends[i].Add(-d)
		}
	}
}

// addCode is the add code an account has outstanding, which is not sent
// when the address could not be added.
func addCode(t *testing.T, app *Server, userID string) string {
	t.Helper()
	app.mu.Lock()
	defer app.mu.Unlock()
	add := app.email.adds[userID]
	if add == nil {
		t.Fatalf("no add code for %s", userID)
	}
	return add.code
}

// dialFrom connects as a client at ip, for a server that reads
// X-Forwarded-For, and returns the connection.
func dialFrom(t *testing.T, httpServer *httptest.Server, ip string) *testClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, wsURL(httpServer), &websocket.DialOptions{HTTPHeader: http.Header{"X-Forwarded-For": {"203.0.113.9, " + ip}}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c := &testClient{ws: ws}
	t.Cleanup(func() { _ = ws.Close(websocket.StatusNormalClosure, "test finished") })
	c.read(t)
	return c
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

// A code request, or a denied code, changes no authentication, so on a
// connection signed in the requests behind it run as before (§3.2).
func TestEmailRequestsLeaveAuthenticationAlone(t *testing.T) {
	_, mailbox, httpServer := emailTestServer(t, nil)
	c := dialTestClient(t, httpServer)
	for _, params := range []map[string]any{
		{"scheme": "email", "email": "ada@example.com"},
		{"scheme": "email", "email": "ada@example.com", "token": "000000"},
	} {
		auth, list := c.nextID("auth"), c.nextID("list")
		c.write(t, map[string]any{"method": "auth", "id": auth, "params": params})
		c.write(t, map[string]any{"method": "room_list", "id": list, "params": map[string]any{"filter": "joined"}})
		c.read(t) // the auth reply
		reply := c.read(t)
		if reply["id"] != list || reply["result"] == nil {
			t.Fatalf("request behind %#v: %#v", params, reply)
		}
	}
	mailbox.receive(t)
	if you := c.result(t, "me", "me", map[string]any{})["you"].(map[string]any); you["user_id"] != c.userID {
		t.Fatalf("identity changed: %#v", you)
	}
}

// On a signed-in connection a code only adds the address to that account,
// and only a code that account asked for: a sign-in link for an attacker's
// address, presented by a signed-in victim, is denied, so the attacker
// cannot later sign in as the victim. An account's add code has no link, is
// not replaced by others' requests for the address, and never signs in.
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
	// Nor can an account take another account's address: no code is sent,
	// and the one kept is denied.
	ageSends(app, "mallory@evil.example", time.Hour)
	requestCode(t, victim, "mallory@evil.example")
	victim.expectError(t, "auth", "owned", map[string]any{"scheme": "email", "email": "mallory@evil.example", "token": addCode(t, app, victim.userID)}, codeDenied)

	// The victim asks to add their own address: the email has no link.
	requestCode(t, victim, "victim@example.com")
	message := mailbox.receive(t)
	if !message.Add || message.Link != "" || !strings.Contains(message.Text(), "add this address") {
		t.Fatalf("add code email: %#v", message)
	}
	// Someone else asking for a code for the address meanwhile does not
	// replace it, and the add code signs nobody in elsewhere.
	ageSends(app, "victim@example.com", time.Minute)
	other, _ := dialRaw(t, httpServer)
	requestCode(t, other, "victim@example.com")
	mailbox.receive(t)
	other.expectError(t, "auth", "elsewhere", map[string]any{"scheme": "email", "email": "victim@example.com", "token": message.Code}, codeDenied)
	// It adds the address to the guest, which becomes an account keeping
	// its messages, with the roles its address was granted and a token to
	// come back with.
	before, result := emailSignIn(t, victim, "victim@example.com", message.Code, nil)
	you := result["you"].(map[string]any)
	if you["user_id"] != victim.userID || !reflect.DeepEqual(you["roles"], []any{"admin"}) || len(before) != 0 {
		t.Fatalf("adding an address: %#v after %#v", result, before)
	}
	save(t, victim, "edit", map[string]any{"message_id": secret, "room_id": diary, "body": map[string]any{"text": "still mine"}})
	// An account holds one address.
	requestCode(t, victim, "second@example.com")
	victim.expectError(t, "auth", "second", map[string]any{"scheme": "email", "email": "second@example.com", "token": addCode(t, app, victim.userID)}, codeDenied)

	// A passkey registration on the signed-in account adds the passkey to
	// it (§4.9), and signs in to it later.
	authenticator := newTestAuthenticator(t)
	options := passkeyResult(t, passkeyCall(t, victim, "register-begin", "register", "begin", nil))
	registered := passkeyResult(t, passkeyCall(t, victim, "register-finish", "register", "finish", map[string]any{"credential": authenticator.registration(t, options, testPasskeyOrigin)}))
	if registered["you"].(map[string]any)["user_id"] != victim.userID {
		t.Fatalf("passkey registration: %#v", registered)
	}
	login, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	begin := passkeyResult(t, passkeyCall(t, login, "login-begin", "login", "begin", nil))
	loggedIn := passkeyResult(t, passkeyCall(t, login, "login-finish", "login", "finish", map[string]any{"credential": authenticator.assertion(t, begin, testPasskeyOrigin, "localhost", 0x05)}))
	if loggedIn["you"].(map[string]any)["user_id"] != victim.userID {
		t.Fatalf("passkey login: %#v", loggedIn)
	}
	// The address signs in on a connection not signed in.
	ageSends(app, "victim@example.com", time.Hour)
	fresh, _ := dialRaw(t, httpServer)
	requestCode(t, fresh, "victim@example.com")
	if _, again := emailSignIn(t, fresh, "victim@example.com", mailbox.receive(t).Code, nil); again["you"].(map[string]any)["user_id"] != victim.userID {
		t.Fatalf("sign-in with the account's address: %#v", again)
	}
}

// A passkey account adds an address with a code asked for on one of its
// connections and presented on another; the result keeps the connection's
// token.
func TestEmailAddsToAPasskeyAccount(t *testing.T) {
	app, mailbox, httpServer := emailTestServer(t, func(config *Config) { config.WebAuthn = testWebAuthn(t) })
	owner, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	registered := registerTestPasskey(t, owner, newTestAuthenticator(t))
	ownerID := registered["you"].(map[string]any)["user_id"]
	owner.drain(t)
	requestCode(t, owner, "owner@example.com")
	second, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	passkeyResult(t, passkeyCall(t, second, "resume", "token", "", map[string]any{"token": registered["token"]}))
	second.drain(t)
	result := second.result(t, "auth", "add", map[string]any{"scheme": "email", "email": "owner@example.com", "token": mailbox.receive(t).Code})
	if result["you"].(map[string]any)["user_id"] != ownerID || result["token"] != nil {
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
	// A valid code is kept when the rest of the request is invalid.
	setCode(app, "bob@example.com", "123456", time.Minute)
	c.expectError(t, "auth", "bad-name", map[string]any{"scheme": "email", "email": "bob@example.com", "token": "123456", "name": 7}, codeInvalidParams)
	emailSignIn(t, c, "bob@example.com", "123456", nil)
	// Email sign-in is not offered without a sender.
	_, plain := newTestServer(t, DefaultConfig())
	guest, _ := dialRaw(t, plain)
	guest.expectError(t, "auth", "email", map[string]any{"scheme": "email", "email": "ada@example.com"}, codeUnsupported)
	guest.expectError(t, "auth", "token", map[string]any{"scheme": "token", "token": "x"}, codeUnsupported)
}

// Wrong codes count against the guesser, tightly, and against the address,
// loosely, across codes; past either budget codes are retry_after. Codes
// sent are limited per address and per client, deliveries in progress are
// bounded, and a full table forgets its least recent address rather than
// refusing anyone.
func TestEmailBudgets(t *testing.T) {
	app, _, httpServer := emailTestServer(t, func(config *Config) { config.ClientIPHeader = "X-Forwarded-For" })
	wrong := func(c *testClient, email string) map[string]any {
		t.Helper()
		setCode(app, email, "123456", time.Minute)
		return c.call(t, "auth", c.nextID("wrong"), map[string]any{"scheme": "email", "email": email, "token": "000000"})["error"].(map[string]any)
	}
	// One client gets clientFailureBurst wrong codes, whatever the address.
	guesser := dialFrom(t, httpServer, "198.51.100.1")
	for i := range clientFailureBurst {
		if failure := wrong(guesser, fmt.Sprintf("a%d@example.com", i)); failure["code"] != float64(codeDenied) {
			t.Fatalf("wrong code %d: %#v", i, failure)
		}
	}
	if failure := wrong(guesser, "b@example.com"); failure["code"] != float64(codeRetryAfter) {
		t.Fatalf("past the client's budget: %#v", failure)
	}
	// Another client guessing the same address still may, until the
	// address's own budget runs out; then even its right code waits.
	for i := 0; i < maxAddressFailuresPerWindow; i++ {
		c := dialFrom(t, httpServer, fmt.Sprintf("198.51.100.%d", 10+i/clientFailureBurst))
		if failure := wrong(c, "target@example.com"); failure["code"] != float64(codeDenied) {
			t.Fatalf("wrong code %d for the address: %#v", i, failure)
		}
	}
	owner := dialFrom(t, httpServer, "192.0.2.1")
	setCode(app, "target@example.com", "123456", time.Minute)
	locked := owner.call(t, "auth", "locked", map[string]any{"scheme": "email", "email": "target@example.com", "token": "123456"})["error"].(map[string]any)
	if locked["code"] != float64(codeRetryAfter) || locked["data"].(map[string]any)["retry_after"].(float64) < 3000 {
		t.Fatalf("locked address: %#v", locked)
	}
	owner.expectError(t, "auth", "no-code", map[string]any{"scheme": "email", "email": "target@example.com"}, codeRetryAfter)
	// Guesses without an outstanding code cost nothing.
	for range clientFailureBurst + 1 {
		owner.expectError(t, "auth", owner.nextID("guess"), map[string]any{"scheme": "email", "email": "nocode@example.com", "token": "123456"}, codeDenied)
	}

	// At most maxEmailSendsPerWindow codes an hour to one address.
	for i := range maxEmailSendsPerWindow {
		requestCode(t, dialFrom(t, httpServer, fmt.Sprintf("203.0.113.%d", i)), "carol@example.com")
		ageSends(app, "carol@example.com", emailResendInterval)
	}
	dialFrom(t, httpServer, "203.0.113.100").expectError(t, "auth", "capped", map[string]any{"scheme": "email", "email": "carol@example.com"}, codeRetryAfter)
	// One client asks for at most clientSendBurst codes, across connections.
	for i := range clientSendBurst {
		requestCode(t, dialFrom(t, httpServer, "198.51.100.200"), fmt.Sprintf("c%d@example.com", i))
	}
	dialFrom(t, httpServer, "198.51.100.200").expectError(t, "auth", "client-sends", map[string]any{"scheme": "email", "email": "d@example.com"}, codeRetryAfter)

	// A full table forgets its least recently used address.
	app.mu.Lock()
	for i := 0; len(app.email.addresses) < maxTrackedEmailState; i++ {
		app.email.createAddress(fmt.Sprintf("filler%d@example.com", i), time.Now()).sends = []time.Time{time.Now()}
	}
	app.mu.Unlock()
	requestCode(t, dialFrom(t, httpServer, "192.0.2.50"), "late@example.com")
	app.mu.Lock()
	full := len(app.email.addresses)
	app.mu.Unlock()
	if full != maxTrackedEmailState {
		t.Fatalf("table size %d", full)
	}
}

// blockingMailbox holds each delivery until released.
type blockingMailbox struct{ release chan struct{} }

func (m blockingMailbox) SendSignInCode(ctx context.Context, _ SignInEmail) error {
	select {
	case <-m.release:
	case <-ctx.Done():
	}
	return nil
}

// Deliveries in progress are bounded; past the bound a request waits.
func TestEmailDeliveriesAreBounded(t *testing.T) {
	mailbox := blockingMailbox{release: make(chan struct{})}
	_, _, httpServer := emailTestServer(t, func(config *Config) {
		config.EmailSender = mailbox
		config.ClientIPHeader = "X-Forwarded-For"
	})
	defer close(mailbox.release)
	for i := range maxConcurrentEmailSends {
		requestCode(t, dialFrom(t, httpServer, fmt.Sprintf("198.51.100.%d", i)), fmt.Sprintf("q%d@example.com", i))
	}
	dialFrom(t, httpServer, "192.0.2.1").expectError(t, "auth", "busy", map[string]any{"scheme": "email", "email": "busy@example.com"}, codeRetryAfter)
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

// A guest who adds an address becomes an account that survives a restart,
// with its token and the roles its address was granted.
func TestEmailGuestBecomesAnAccountAcrossRestarts(t *testing.T) {
	kept := store.NewMemory()
	mailbox := make(testMailbox, 4)
	configure := func(config *Config) {
		config.EmailSender = mailbox
		config.Roles = map[string][]string{"admin": {"ada@example.com"}}
	}
	_, httpServer, stop := startWith(t, kept, t.TempDir(), configure)
	guest := dialTestClient(t, httpServer)
	id, _ := save(t, guest, "hello", map[string]any{"body": map[string]any{"text": "hello"}})
	requestCode(t, guest, "ada@example.com")
	_, result := emailSignIn(t, guest, "ada@example.com", mailbox.receive(t).Code, nil)
	if result["you"].(map[string]any)["user_id"] != guest.userID {
		t.Fatalf("adding an address: %#v", result)
	}
	stop()

	_, httpServer, _ = startWith(t, kept, t.TempDir(), configure)
	c, _ := dialRaw(t, httpServer)
	resumed := c.result(t, "auth", "resume", map[string]any{"scheme": "token", "token": result["token"]})
	if you := resumed["you"].(map[string]any); you["user_id"] != guest.userID || !reflect.DeepEqual(you["roles"], []any{"admin"}) {
		t.Fatalf("resumed after restart: %#v", resumed)
	}
	c.userID = guest.userID
	c.drain(t)
	save(t, c, "edit", map[string]any{"message_id": id, "body": map[string]any{"text": "edited"}})
}

// Refused code requests leave no entries behind, and a full table never
// forgets a live code or a lock: a flood of requests for other addresses
// from one client leaves the victim's code and a locked address as they
// were. A table full of live codes refuses new requests instead.
func TestEmailFloodKeepsCodesAndLocks(t *testing.T) {
	app, mailbox, httpServer := emailTestServer(t, func(config *Config) { config.ClientIPHeader = "X-Forwarded-For" })
	victim := dialFrom(t, httpServer, "192.0.2.1")
	requestCode(t, victim, "victim@example.com")
	code := mailbox.receive(t).Code
	for i := 0; i < maxAddressFailuresPerWindow; i++ {
		c := dialFrom(t, httpServer, fmt.Sprintf("198.51.100.%d", 10+i/clientFailureBurst))
		setCode(app, "target@example.com", "123456", time.Minute)
		c.call(t, "auth", c.nextID("wrong"), map[string]any{"scheme": "email", "email": "target@example.com", "token": "000000"})
	}

	attacker := dialFrom(t, httpServer, "203.0.113.66")
	for i := 0; i < maxTrackedEmailState+10; i++ {
		attacker.call(t, "auth", fmt.Sprintf("flood-%d", i), map[string]any{"scheme": "email", "email": fmt.Sprintf("junk%d@example.com", i)})
	}
	app.mu.Lock()
	entries := len(app.email.addresses)
	app.mu.Unlock()
	if entries > 10 {
		t.Fatalf("refused requests left %d entries", entries)
	}
	dialFrom(t, httpServer, "192.0.2.77").expectError(t, "auth", "locked", map[string]any{"scheme": "email", "email": "target@example.com"}, codeRetryAfter)
	emailSignIn(t, victim, "victim@example.com", code, nil)

	// A table full of live codes forgets none of them.
	app.mu.Lock()
	for i := 0; len(app.email.addresses) < maxTrackedEmailState; i++ {
		app.email.createAddress(fmt.Sprintf("live%d@example.com", i), time.Now())
	}
	for _, state := range app.email.addresses {
		state.signIn = &emailCode{code: "1", expires: time.Now().Add(time.Minute)}
	}
	app.mu.Unlock()
	dialFrom(t, httpServer, "192.0.2.99").expectError(t, "auth", "full", map[string]any{"scheme": "email", "email": "late@example.com"}, codeRetryAfter)
	app.mu.Lock()
	_, kept := app.email.addresses["live0@example.com"]
	app.mu.Unlock()
	if !kept {
		t.Fatal("a live code was forgotten")
	}
}

// The client address comes from the last entry across every line of the
// configured header, without a port, else from the connection.
func TestClientIPFromAProxyHeader(t *testing.T) {
	s := &Server{config: Config{ClientIPHeader: "X-Forwarded-For"}}
	for _, test := range []struct {
		lines []string
		want  string
	}{
		{nil, "10.0.0.1"},
		{[]string{"203.0.113.7"}, "203.0.113.7"},
		{[]string{"6.6.6.6, 203.0.113.7"}, "203.0.113.7"},
		{[]string{"6.6.6.6", "203.0.113.7"}, "203.0.113.7"},
		{[]string{"6.6.6.6", "198.51.100.1, 203.0.113.7:4711"}, "203.0.113.7"},
		{[]string{"[2001:db8::1]:4711"}, "2001:db8::1"},
		{[]string{"2001:db8::2"}, "2001:db8::2"},
		{[]string{"garbage-x"}, "10.0.0.1"},
		{[]string{"203.0.113.7, "}, "10.0.0.1"},
	} {
		r := httptest.NewRequest("GET", "/ws", nil)
		r.RemoteAddr = "10.0.0.1:5555"
		for _, line := range test.lines {
			r.Header.Add("X-Forwarded-For", line)
		}
		if got := s.clientIP(r); got != test.want {
			t.Errorf("clientIP(%q) = %q, want %q", test.lines, got, test.want)
		}
	}
	plain := &Server{}
	r := httptest.NewRequest("GET", "/ws", nil)
	r.RemoteAddr = "10.0.0.1:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	if got := plain.clientIP(r); got != "10.0.0.1" {
		t.Errorf("without a header configured: %q", got)
	}
}
