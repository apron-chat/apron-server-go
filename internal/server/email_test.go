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

// propose proposes an email sign-in or addition on c, which must be answered
// with {} (§4.11).
func propose(t *testing.T, c *testClient, email string) {
	t.Helper()
	if result := c.result(t, "auth", c.nextID("propose"), map[string]any{"scheme": "email", "email": email}); len(result) != 0 {
		t.Fatalf("proposal result: %#v", result)
	}
}

// approve approves a proposal on c with token and returns the notifications
// before the result, then the result.
func approve(t *testing.T, c *testClient, token string, extra map[string]any) ([]map[string]any, map[string]any) {
	t.Helper()
	params := map[string]any{"scheme": "email", "token": token}
	for key, value := range extra {
		params[key] = value
	}
	return c.request(t, "auth", c.nextID("approve"), params)
}

// signInByEmail approves a sign-in and returns the notifications before the
// result, then the result, which must carry you and a token.
func signInByEmail(t *testing.T, c *testClient, token string, extra map[string]any) ([]map[string]any, map[string]any) {
	t.Helper()
	before, result := approve(t, c, token, extra)
	if _, ok := result["token"].(string); !ok {
		t.Fatalf("sign-in result without a token: %#v", result)
	}
	c.userID = result["you"].(map[string]any)["user_id"].(string)
	return before, result
}

// linkToken is the token in a sign-in email's link.
func linkToken(t *testing.T, message SignInEmail) string {
	t.Helper()
	_, fragment, _ := strings.Cut(message.Link, "#token=")
	token, _, _ := strings.Cut(fragment, "&")
	if token == "" {
		t.Fatalf("no link token in %q", message.Link)
	}
	return token
}

// pendingCode is the code of the proposal pending on a connection of
// userID, which may not have been sent.
func pendingCode(t *testing.T, app *Server, userID string) string {
	t.Helper()
	app.mu.Lock()
	defer app.mu.Unlock()
	for c := range app.clients {
		if c.user != nil && c.user.id == userID && c.proposal != nil {
			return c.proposal.code
		}
	}
	t.Fatalf("no proposal pending for %s", userID)
	return ""
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

func TestEmailSignIn(t *testing.T) {
	app, mailbox, httpServer := emailTestServer(t, func(config *Config) { config.PublicURL = "https://chat.example/app/" })
	proposer, frame := dialRaw(t, httpServer)
	params := frame["params"].(map[string]any)
	if !reflect.DeepEqual(params["auth"], []any{"email", "token", "guest"}) || !reflect.DeepEqual(params["signup"], []any{"email", "guest"}) {
		t.Fatalf("auth schemes: %#v, signup %#v", params["auth"], params["signup"])
	}

	// A proposal authenticates nothing, so on a connection not signed in the
	// requests behind it are still denied; the address is normalized.
	proposer.write(t, map[string]any{"method": "auth", "id": "propose", "params": map[string]any{"scheme": "email", "email": " Ada@Example.com "}})
	proposer.write(t, map[string]any{"method": "room_list", "id": "list", "params": map[string]any{}})
	if reply := proposer.read(t); reply["id"] != "propose" || len(reply["result"].(map[string]any)) != 0 {
		t.Fatalf("proposal reply: %#v", reply)
	}
	if reply := proposer.read(t); reply["id"] != "list" || reply["error"].(map[string]any)["code"] != float64(codeDenied) {
		t.Fatalf("request behind a proposal: %#v", reply)
	}
	message := mailbox.receive(t)
	if message.To != "ada@example.com" || message.Add || len(message.Code) != 6 || strings.Trim(message.Code, "0123456789") != "" {
		t.Fatalf("sent: %#v", message)
	}
	// The link is built from the configured page, with an unguessable token
	// and this server's WebSocket URL in its fragment, and no address.
	link := linkToken(t, message)
	want := "https://chat.example/login#token=" + link + "&server=wss%3A%2F%2Fchat.example%2Fapp%2Fws"
	if message.Link != want || len(link) < 26 || strings.Contains(message.Link, "ada") {
		t.Fatalf("link %q, want %q", message.Link, want)
	}
	if !strings.Contains(message.Text(), message.Code) || !strings.Contains(message.Text(), message.Link) {
		t.Fatalf("text: %q", message.Text())
	}
	// Another proposal for the address so soon is retry_after.
	proposer.expectError(t, "auth", "again", map[string]any{"scheme": "email", "email": "ada@example.com"}, codeRetryAfter)
	proposer.expectError(t, "auth", "invalid", map[string]any{"scheme": "email", "email": "Ada <ada@example.com>"}, codeInvalidParams)
	proposer.expectError(t, "auth", "missing", map[string]any{"scheme": "email"}, codeInvalidParams)

	// The code works only on the proposing connection; the link token on
	// any connection not signed in, which it signs in: a new account, which
	// joins general, before the result.
	reader, _ := dialRaw(t, httpServer)
	reader.expectError(t, "auth", "code-elsewhere", map[string]any{"scheme": "email", "token": message.Code}, codeDenied)
	reader.expectError(t, "auth", "wrong", map[string]any{"scheme": "email", "token": "wrong"}, codeDenied)
	before, result := signInByEmail(t, reader, link, map[string]any{"name": "Ada", "user_id": "ada"})
	if you := result["you"].(map[string]any); you["user_id"] != "ada" || you["name"] != "Ada" {
		t.Fatalf("new account: %#v", result)
	}
	if len(before) != 1 {
		t.Fatalf("frames before the sign-in result: %#v", before)
	}
	checkMembership(t, membershipOnly(t, before[0]), "general", "ada", true)
	// The proposal is consumed: neither token works again.
	third, _ := dialRaw(t, httpServer)
	third.expectError(t, "auth", "reuse", map[string]any{"scheme": "email", "token": link}, codeDenied)
	proposer.expectError(t, "auth", "code-after", map[string]any{"scheme": "email", "token": message.Code}, codeDenied)
	// The bearer token resumes the account on later connections.
	resumed := third.result(t, "auth", "resume", map[string]any{"scheme": "token", "token": result["token"]})
	if resumed["you"].(map[string]any)["user_id"] != "ada" || resumed["token"] != result["token"] {
		t.Fatalf("token resume: %#v", resumed)
	}
	// Signing in again with the code, on the proposing connection, finds
	// the same account.
	ageSends(app, "ada@example.com", time.Minute)
	fourth, _ := dialRaw(t, httpServer)
	propose(t, fourth, "ada@example.com")
	if _, again := signInByEmail(t, fourth, mailbox.receive(t).Code, map[string]any{"user_id": "other"}); again["you"].(map[string]any)["user_id"] != "ada" {
		t.Fatalf("second sign-in: %#v", again)
	}
}

// A proposal is answered with the same {} and nothing else whether or not
// the address has an account.
func TestEmailProposalsDoNotRevealAccounts(t *testing.T) {
	app, mailbox, httpServer := emailTestServer(t, nil)
	c, _ := dialRaw(t, httpServer)
	propose(t, c, "ada@example.com")
	signInByEmail(t, c, mailbox.receive(t).Code, nil)
	ageSends(app, "ada@example.com", time.Minute)
	var replies [][]map[string]any
	for _, email := range []string{"ada@example.com", "nobody@example.com"} {
		asker, _ := dialRaw(t, httpServer)
		before, result := asker.request(t, "auth", "propose", map[string]any{"scheme": "email", "email": email})
		asker.expectQuiet(t)
		replies = append(replies, append(before, result))
		mailbox.receive(t)
	}
	if !reflect.DeepEqual(replies[0], replies[1]) || len(replies[0]) != 1 || len(replies[0][0]) != 0 {
		t.Fatalf("replies differ: %#v", replies)
	}
}

// A proposal, or a denied approval, changes no authentication, so on a
// connection signed in the requests behind it run as before (§3.2).
func TestEmailRequestsLeaveAuthenticationAlone(t *testing.T) {
	_, mailbox, httpServer := emailTestServer(t, nil)
	c := dialTestClient(t, httpServer)
	for _, params := range []map[string]any{
		{"scheme": "email", "email": "ada@example.com"},
		{"scheme": "email", "token": "000000"},
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

// On a signed-in connection a proposal is to add the address to that
// account, by a code only that connection can present, whose email has no
// link. A sign-in link presented by someone signed in is denied, so an
// attacker's link cannot join the attacker's address to their account, and
// an address is never taken from another account.
func TestEmailAdditions(t *testing.T) {
	app, mailbox, httpServer := emailTestServer(t, func(config *Config) {
		config.WebAuthn = testWebAuthn(t)
		config.Roles = map[string][]string{"admin": {"victim@example.com"}}
	})
	victim, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	guestAuth(t, victim)
	victim.drain(t)
	diary, _ := saveRoom(t, victim, "diary", map[string]any{"title": "Diary", "private": true})
	secret, _ := save(t, victim, "secret", map[string]any{"room_id": diary, "body": map[string]any{"text": "my secret"}})

	// The attacker's sign-in link, presented by the signed-in victim, is
	// denied and changes nothing; it still signs its own proposer in, to an
	// account of its own.
	attacker, _ := dialRaw(t, httpServer)
	propose(t, attacker, "mallory@evil.example")
	link := linkToken(t, mailbox.receive(t))
	victim.expectError(t, "auth", "link", map[string]any{"scheme": "email", "token": link}, codeDenied)
	later, _ := dialRaw(t, httpServer)
	if _, result := signInByEmail(t, later, link, nil); result["you"].(map[string]any)["user_id"] == victim.userID {
		t.Fatalf("the attacker became the victim: %#v", result)
	}
	later.expectError(t, "history", "diary", map[string]any{"room_id": diary}, codeInvalidParams)
	victim.drain(t)

	// Nor can an account take another account's address: nothing is sent,
	// and the pending code is denied.
	ageSends(app, "mallory@evil.example", time.Hour)
	propose(t, victim, "mallory@evil.example")
	approveOwned := pendingCode(t, app, victim.userID)
	victim.expectError(t, "auth", "owned", map[string]any{"scheme": "email", "token": approveOwned}, codeDenied)

	// The victim proposes adding their own address: the email has no link.
	propose(t, victim, "victim@example.com")
	message := mailbox.receive(t)
	if !message.Add || message.Link != "" || !strings.Contains(message.Text(), "add this address") {
		t.Fatalf("addition email: %#v", message)
	}
	// Someone else's proposal for the address does not touch it, and the
	// code works on no other connection.
	ageSends(app, "victim@example.com", time.Minute)
	other, _ := dialRaw(t, httpServer)
	propose(t, other, "victim@example.com")
	mailbox.receive(t)
	other.expectError(t, "auth", "elsewhere", map[string]any{"scheme": "email", "token": message.Code}, codeDenied)
	// Approving adds the address to the guest, which becomes an account
	// keeping its messages, with the roles its address was granted: {} and
	// a user notification with the new roles.
	before, result := approve(t, victim, message.Code, nil)
	if len(result) != 0 || len(before) != 1 {
		t.Fatalf("approving an addition: %#v after %#v", result, before)
	}
	if you := notificationParams(t, before[0], "user")["you"].(map[string]any); !reflect.DeepEqual(you["roles"], []any{"admin"}) {
		t.Fatalf("profile after the addition: %#v", you)
	}
	save(t, victim, "edit", map[string]any{"message_id": secret, "room_id": diary, "body": map[string]any{"text": "still mine"}})
	// An account holds one address.
	propose(t, victim, "second@example.com")
	victim.expectError(t, "auth", "second", map[string]any{"scheme": "email", "token": pendingCode(t, app, victim.userID)}, codeDenied)

	// A passkey registration on the signed-in account adds the passkey to
	// it (§4.10), and signs in to it later.
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
	propose(t, fresh, "victim@example.com")
	if _, again := signInByEmail(t, fresh, mailbox.receive(t).Code, nil); again["you"].(map[string]any)["user_id"] != victim.userID {
		t.Fatalf("sign-in with the account's address: %#v", again)
	}
}

// A proposal's name and user_id apply only when its own connection
// approves it: a link opened on another connection is the address's owner
// reading the email, whose new account the proposer must not name.
func TestEmailProposalsNameOnlyTheirOwnApprovals(t *testing.T) {
	_, mailbox, httpServer := emailTestServer(t, nil)
	squatter, _ := dialRaw(t, httpServer)
	if result := squatter.result(t, "auth", "propose", map[string]any{"scheme": "email", "email": "victim@example.com", "user_id": "squatted", "name": "Squatter"}); len(result) != 0 {
		t.Fatalf("proposal result: %#v", result)
	}
	victim, _ := dialRaw(t, httpServer)
	_, result := signInByEmail(t, victim, linkToken(t, mailbox.receive(t)), nil)
	if you := result["you"].(map[string]any); you["user_id"] == "squatted" || you["name"] == "Squatter" || !strings.HasPrefix(you["user_id"].(string), accountIDPrefix) {
		t.Fatalf("the proposer named the victim's account: %#v", you)
	}

	// On the proposing connection, the proposal's own values apply.
	proposer, _ := dialRaw(t, httpServer)
	if result := proposer.result(t, "auth", "propose", map[string]any{"scheme": "email", "email": "carol@example.com", "user_id": "carol", "name": "Carol"}); len(result) != 0 {
		t.Fatalf("proposal result: %#v", result)
	}
	_, result = signInByEmail(t, proposer, mailbox.receive(t).Code, nil)
	if you := result["you"].(map[string]any); you["user_id"] != "carol" || you["name"] != "Carol" {
		t.Fatalf("the proposal's own values: %#v", you)
	}
}

// A connection's pending proposal is dropped when its identity changes, so
// an addition proposed by one account cannot be approved by another, nor a
// sign-in link used once its connection signed in some other way.
func TestEmailProposalsEndWithTheirIdentity(t *testing.T) {
	app, mailbox, httpServer := emailTestServer(t, func(config *Config) { config.WebAuthn = testWebAuthn(t) })
	other, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	token, _ := registerTestPasskey(t, other, newTestAuthenticator(t))["token"].(string)
	if token == "" {
		t.Fatal("no token for the second account")
	}
	c, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	registerTestPasskey(t, c, newTestAuthenticator(t))
	c.drain(t)
	propose(t, c, "first@example.com")
	code := mailbox.receive(t).Code
	resumed := c.result(t, "auth", "switch", map[string]any{"scheme": "token", "token": token})
	if resumed["you"].(map[string]any)["user_id"] != other.userID {
		t.Fatalf("switching accounts: %#v", resumed)
	}
	c.expectError(t, "auth", "after-switch", map[string]any{"scheme": "email", "token": code}, codeDenied)
	app.mu.RLock()
	added := app.emails["first@example.com"]
	app.mu.RUnlock()
	if added != nil {
		t.Fatalf("the address was added to %s", added.id)
	}

	// A sign-in proposal ends when its connection signs in as a guest.
	fresh, _ := dialRaw(t, httpServer)
	propose(t, fresh, "fresh@example.com")
	link := linkToken(t, mailbox.receive(t))
	guestAuth(t, fresh)
	reader, _ := dialRaw(t, httpServer)
	reader.expectError(t, "auth", "stale-link", map[string]any{"scheme": "email", "token": link}, codeDenied)
}

// A passkey account adds an address on the connection that proposed it.
func TestEmailAddsToAPasskeyAccount(t *testing.T) {
	app, mailbox, httpServer := emailTestServer(t, func(config *Config) { config.WebAuthn = testWebAuthn(t) })
	owner, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	registered := registerTestPasskey(t, owner, newTestAuthenticator(t))
	ownerID := registered["you"].(map[string]any)["user_id"]
	owner.drain(t)
	propose(t, owner, "owner@example.com")
	if _, result := approve(t, owner, mailbox.receive(t).Code, nil); len(result) != 0 {
		t.Fatalf("adding an address to a passkey account: %#v", result)
	}
	app.mu.RLock()
	defer app.mu.RUnlock()
	if app.emails["owner@example.com"] == nil || app.emails["owner@example.com"].id != ownerID {
		t.Fatal("the address was not added")
	}
}

func TestEmailProposalsExpireAndAreInvalidated(t *testing.T) {
	app, mailbox, httpServer := emailTestServer(t, nil)
	c, _ := dialRaw(t, httpServer)
	propose(t, c, "ada@example.com")
	first := mailbox.receive(t)
	// A newer proposal replaces the older one, its link included.
	propose(t, c, "bob@example.com")
	second := mailbox.receive(t)
	if first.Code != second.Code {
		c.expectError(t, "auth", "old", map[string]any{"scheme": "email", "token": first.Code}, codeDenied)
	}
	c.expectError(t, "auth", "old-link", map[string]any{"scheme": "email", "token": linkToken(t, first)}, codeDenied)
	// A few wrong tokens invalidate it.
	for range maxEmailAttempts - 2 {
		c.expectError(t, "auth", c.nextID("wrong"), map[string]any{"scheme": "email", "token": "wrong"}, codeDenied)
	}
	c.expectError(t, "auth", "last-wrong", map[string]any{"scheme": "email", "token": "nope"}, codeDenied)
	c.expectError(t, "auth", "invalidated", map[string]any{"scheme": "email", "token": second.Code}, codeDenied)
	// An expired proposal is denied.
	propose(t, c, "carol@example.com")
	expired := mailbox.receive(t)
	app.mu.Lock()
	for client := range app.clients {
		if client.proposal != nil {
			client.proposal.expires = time.Now().Add(-time.Second)
		}
	}
	app.mu.Unlock()
	c.expectError(t, "auth", "expired", map[string]any{"scheme": "email", "token": expired.Code}, codeDenied)
	// A connection may propose only a few times.
	for i := range emailSendsPerConnection - 3 {
		propose(t, c, fmt.Sprintf("user%d@example.com", i))
		mailbox.receive(t)
	}
	c.expectError(t, "auth", "flood", map[string]any{"scheme": "email", "email": "flood@example.com"}, codeRetryAfter)
	// A valid token is kept when the rest of the request is invalid.
	other, _ := dialRaw(t, httpServer)
	propose(t, other, "dave@example.com")
	code := mailbox.receive(t).Code
	other.expectError(t, "auth", "bad-name", map[string]any{"scheme": "email", "token": code, "name": 7}, codeInvalidParams)
	signInByEmail(t, other, code, nil)
	// A sign-in code on a connection signed in is denied.
	signedIn := dialTestClient(t, httpServer)
	fresh, _ := dialRaw(t, httpServer)
	propose(t, fresh, "erin@example.com")
	signedIn.expectError(t, "auth", "signed-in", map[string]any{"scheme": "email", "token": linkToken(t, mailbox.receive(t))}, codeDenied)
	// Email sign-in is not offered without a sender.
	_, plain := newTestServer(t, DefaultConfig())
	guest, _ := dialRaw(t, plain)
	guest.expectError(t, "auth", "email", map[string]any{"scheme": "email", "email": "ada@example.com"}, codeUnsupported)
	guest.expectError(t, "auth", "token", map[string]any{"scheme": "token", "token": "x"}, codeUnsupported)
}

// Emails are limited per address and per client, deliveries in progress
// are bounded, refused proposals leave nothing behind, and a full table
// forgets only entries whose limits have lapsed.
func TestEmailSendLimits(t *testing.T) {
	app, _, httpServer := emailTestServer(t, func(config *Config) { config.ClientIPHeader = "X-Forwarded-For" })
	// At most maxEmailSendsPerWindow emails an hour to one address.
	for i := range maxEmailSendsPerWindow {
		propose(t, dialFrom(t, httpServer, fmt.Sprintf("203.0.113.%d", i)), "carol@example.com")
		ageSends(app, "carol@example.com", emailResendInterval)
	}
	dialFrom(t, httpServer, "203.0.113.100").expectError(t, "auth", "capped", map[string]any{"scheme": "email", "email": "carol@example.com"}, codeRetryAfter)
	// One client proposes at most clientSendBurst times, across connections.
	for i := range clientSendBurst {
		propose(t, dialFrom(t, httpServer, "198.51.100.200"), fmt.Sprintf("c%d@example.com", i))
	}
	flooder := dialFrom(t, httpServer, "198.51.100.200")
	flooder.expectError(t, "auth", "client-sends", map[string]any{"scheme": "email", "email": "d@example.com"}, codeRetryAfter)
	// Refused proposals are not tracked.
	app.mu.Lock()
	before := len(app.email.addresses)
	app.mu.Unlock()
	for i := range 100 {
		flooder.call(t, "auth", fmt.Sprintf("refused-%d", i), map[string]any{"scheme": "email", "email": fmt.Sprintf("junk%d@example.com", i)})
	}
	app.mu.Lock()
	after := len(app.email.addresses)
	app.mu.Unlock()
	if after != before {
		t.Fatalf("refused proposals made %d entries", after-before)
	}

	// A full table forgets an address whose emails are older than the
	// window, and refuses when every entry is within it.
	app.mu.Lock()
	for i := 0; len(app.email.addresses) < maxTrackedEmailState; i++ {
		app.email.createAddress(fmt.Sprintf("filler%d@example.com", i), time.Now()).sends = []time.Time{time.Now()}
	}
	app.mu.Unlock()
	dialFrom(t, httpServer, "192.0.2.50").expectError(t, "auth", "full", map[string]any{"scheme": "email", "email": "late@example.com"}, codeRetryAfter)
	ageSends(app, "filler0@example.com", time.Hour)
	propose(t, dialFrom(t, httpServer, "192.0.2.51"), "late@example.com")
	app.mu.Lock()
	_, kept := app.email.addresses["filler0@example.com"]
	_, keptLive := app.email.addresses["filler1@example.com"]
	app.mu.Unlock()
	if kept || !keptLive {
		t.Fatalf("eviction: lapsed kept %v, live kept %v", kept, keptLive)
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

// Deliveries in progress are bounded; past the bound a proposal waits.
func TestEmailDeliveriesAreBounded(t *testing.T) {
	mailbox := blockingMailbox{release: make(chan struct{})}
	_, _, httpServer := emailTestServer(t, func(config *Config) {
		config.EmailSender = mailbox
		config.ClientIPHeader = "X-Forwarded-For"
	})
	defer close(mailbox.release)
	for i := range maxConcurrentEmailSends {
		propose(t, dialFrom(t, httpServer, fmt.Sprintf("198.51.100.%d", i)), fmt.Sprintf("q%d@example.com", i))
	}
	dialFrom(t, httpServer, "192.0.2.1").expectError(t, "auth", "busy", map[string]any{"scheme": "email", "email": "busy@example.com"}, codeRetryAfter)
}

// Email accounts, their addresses, their tokens, and the roles granted to
// their addresses survive a restart; so does a guest that added an address,
// which signs in with it afterwards.
func TestEmailAccountsPersist(t *testing.T) {
	kept := store.NewMemory()
	mailbox := make(testMailbox, 4)
	configure := func(config *Config) {
		config.EmailSender = mailbox
		config.Roles = map[string][]string{"admin": {"Ada@example.com"}}
	}
	_, httpServer, stop := startWith(t, kept, t.TempDir(), configure)
	c, _ := dialRaw(t, httpServer)
	propose(t, c, "ada@example.com")
	_, result := signInByEmail(t, c, mailbox.receive(t).Code, nil)
	you := result["you"].(map[string]any)
	if you["user_id"] != "user_1" || !reflect.DeepEqual(you["roles"], []any{"admin"}) {
		t.Fatalf("sign-in: %#v", result)
	}
	guest := dialTestClient(t, httpServer)
	id, _ := save(t, guest, "hello", map[string]any{"body": map[string]any{"text": "hello"}})
	propose(t, guest, "bob@example.com")
	approve(t, guest, mailbox.receive(t).Code, nil)
	stop()

	_, httpServer, _ = startWith(t, kept, t.TempDir(), configure)
	c, _ = dialRaw(t, httpServer)
	resumed := c.result(t, "auth", "resume", map[string]any{"scheme": "token", "token": result["token"]})
	if resumed["you"].(map[string]any)["user_id"] != "user_1" || !reflect.DeepEqual(resumed["you"].(map[string]any)["roles"], []any{"admin"}) {
		t.Fatalf("resumed after restart: %#v", resumed)
	}
	back, _ := dialRaw(t, httpServer)
	propose(t, back, "bob@example.com")
	if _, again := signInByEmail(t, back, mailbox.receive(t).Code, nil); again["you"].(map[string]any)["user_id"] != guest.userID {
		t.Fatalf("the guest's address after restart: %#v", again)
	}
	back.drain(t)
	save(t, back, "edit", map[string]any{"message_id": id, "body": map[string]any{"text": "edited"}})
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
