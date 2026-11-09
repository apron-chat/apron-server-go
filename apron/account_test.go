package apron

import (
	"context"
	"errors"
	"testing"
	"time"
)

// expectClosed reads until the connection closes.
func expectClosed(t *testing.T, c *testClient) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := c.ws.Read(ctx); err != nil {
			if ctx.Err() != nil {
				t.Fatal("the connection stayed open")
			}
			return
		}
	}
}

// loginTestPasskey signs c in with a's passkey, and returns the result.
func loginTestPasskey(t *testing.T, c *testClient, a *testAuthenticator) map[string]any {
	t.Helper()
	options := passkeyResult(t, passkeyCall(t, c, c.nextID("login"), "login", "begin", nil))
	return passkeyResult(t, passkeyCall(t, c, c.nextID("login-end"), "login", "finish", map[string]any{"credential": a.assertion(t, options, testPasskeyOrigin, "localhost", 0x05)}))
}

// addTestPasskey adds a's passkey to c's account.
func addTestPasskey(t *testing.T, c *testClient, a *testAuthenticator) map[string]any {
	t.Helper()
	options := passkeyResult(t, passkeyCall(t, c, c.nextID("add"), "register", "begin", nil))
	return passkeyCall(t, c, c.nextID("add-end"), "register", "finish", map[string]any{"credential": a.registration(t, options, testPasskeyOrigin)})
}

func credentialIDs(t *testing.T, c *testClient) ([]map[string]any, string) {
	t.Helper()
	result := c.result(t, "account.credentials", c.nextID("credentials"), nil)
	var list []map[string]any
	current := ""
	for _, value := range asList(result["credentials"]) {
		entry := value.(map[string]any)
		list = append(list, entry)
		if entry["current"] == true {
			current = entry["id"].(string)
		}
	}
	return list, current
}

func TestManagingPasskeys(t *testing.T) {
	app, httpServer := passkeyTestServer(t)
	a, b := newTestAuthenticator(t), newTestAuthenticator(t)
	first, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	registered := registerTestPasskey(t, first, a)
	userID := registered["you"].(map[string]any)["user_id"].(string)
	first.drain(t)
	passkeyResult(t, addTestPasskey(t, first, b))
	first.drain(t)

	list, current := credentialIDs(t, first)
	if len(list) != 2 || current != b64(b.id) || list[0]["id"] != b64(a.id) || list[0]["scheme"] != "webauthn" || list[0]["created"] == nil {
		t.Fatalf("credentials: %#v (current %s)", list, current)
	}
	first.result(t, "account.credential_rename", "rename", map[string]any{"id": b64(b.id), "name": "Backup key"})
	if list, _ := credentialIDs(t, first); list[1]["name"] != "Backup key" {
		t.Fatalf("renamed: %#v", list)
	}

	// One connection signs in with each passkey, and another resumes the
	// first passkey's session.
	withA, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	tokenA := loginTestPasskey(t, withA, a)["token"].(string)
	resumedA, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	passkeyResult(t, passkeyCall(t, resumedA, "resume", "token", "", map[string]any{"token": tokenA}))
	withB, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	tokenB := loginTestPasskey(t, withB, b)["token"].(string)
	withB.drain(t)

	// A connection that resumed a session must reauthenticate first.
	resumedB, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	passkeyResult(t, passkeyCall(t, resumedB, "resume", "token", "", map[string]any{"token": tokenB}))
	resumedB.drain(t)
	resumedB.expectError(t, "account.credential_remove", "remove", map[string]any{"id": b64(a.id)}, CodeDenied)

	// The lost passkey goes: the connections signed in with it close, and
	// its sessions end; the account and the other passkey stay.
	withB.result(t, "account.credential_remove", "remove", map[string]any{"id": b64(a.id)})
	expectClosed(t, withA)
	expectClosed(t, resumedA)
	gone, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	passkeyDenied(t, passkeyCall(t, gone, "resume", "token", "", map[string]any{"token": tokenA}))
	options := passkeyResult(t, passkeyCall(t, gone, "login", "login", "begin", nil))
	passkeyDenied(t, passkeyCall(t, gone, "login-end", "login", "finish", map[string]any{"credential": a.assertion(t, options, testPasskeyOrigin, "localhost", 0x05)}))
	if credentials := app.Credentials(userID); len(credentials) != 1 || credentials[0].ID != b64(b.id) || credentials[0].Name != "Backup key" {
		t.Fatalf("credentials left: %#v", credentials)
	}

	// The last way to sign in stays.
	withB.expectError(t, "account.credential_remove", "last", map[string]any{"id": b64(b.id)}, CodeDenied)
	if err := app.RemoveCredential(context.Background(), userID, "webauthn", b64(b.id)); !errors.Is(err, ErrLastCredential) {
		t.Fatalf("RemoveCredential of the last = %v", err)
	}

	// Signing out the others keeps this connection's session.
	withB.result(t, "account.sign_out", "others", map[string]any{"others": true})
	expectClosed(t, resumedB)
	again, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	passkeyDenied(t, passkeyCall(t, again, "resume", "token", "", map[string]any{"token": registered["token"]}))
	app.mu.RLock()
	sessions := 0
	for _, session := range app.sessions {
		if session.user.id == userID {
			sessions++
		}
	}
	app.mu.RUnlock()
	if sessions != 1 {
		t.Fatalf("sessions left: %d", sessions)
	}
	// Signing out this connection forgets its token too.
	withB.result(t, "account.sign_out", "self", nil)
	expectClosed(t, withB)
	passkeyDenied(t, passkeyCall(t, again, "resume-b", "token", "", map[string]any{"token": tokenB}))

	// A guest has nothing to manage.
	guest := dialTestClient(t, httpServer)
	guest.expectError(t, "account.credentials", "guest", nil, CodeDenied)
}

func TestAddingAPasskeyCanNeedReauthentication(t *testing.T) {
	config := DefaultConfig()
	config.WebAuthn = testWebAuthn(t)
	config.ReauthToAddCredential = true
	app, httpServer := newTestServer(t, config)
	a, b := newTestAuthenticator(t), newTestAuthenticator(t)
	first, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	registered := registerTestPasskey(t, first, a)
	token, userID := registered["token"].(string), registered["you"].(map[string]any)["user_id"].(string)

	resumed, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	passkeyResult(t, passkeyCall(t, resumed, "resume", "token", "", map[string]any{"token": token}))
	resumed.drain(t)
	passkeyDenied(t, addTestPasskey(t, resumed, b))
	loginTestPasskey(t, resumed, a)
	resumed.drain(t)
	passkeyResult(t, addTestPasskey(t, resumed, b))
	if n := len(app.Credentials(userID)); n != 2 {
		t.Fatalf("credentials: %d", n)
	}
}
