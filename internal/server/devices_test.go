package server

import (
	"slices"
	"testing"

	"github.com/apron-chat/apron-server-go/internal/store"
)

// addDevice issues a token for another device on c, which must succeed.
func addDevice(t *testing.T, c *testClient, id string) (token, origin string) {
	t.Helper()
	result := c.result(t, deviceAddMethod, id, map[string]any{})
	token, _ = result["token"].(string)
	origin, _ = result["origin"].(string)
	if token == "" || len(result) != 2 {
		t.Fatalf("%s result: %#v", deviceAddMethod, result)
	}
	return token, origin
}

// A passkey account issues a token for another device: a session of its own,
// bound to the issuing connection's origin, which leaves the connection's
// own token alone. A retry of the same request answers with the same token.
func TestDeviceAddIssuesATokenBoundToTheOrigin(t *testing.T) {
	app, httpServer := passkeyTestServer(t)
	owner, frame := dialOrigin(t, httpServer, testPasskeyOrigin)
	if capabilities := frame["params"].(map[string]any)["capabilities"].([]any); !slices.Contains(capabilities, any(deviceAddMethod)) {
		t.Fatalf("capabilities without %s: %#v", deviceAddMethod, capabilities)
	}
	a := newTestAuthenticator(t)
	guest, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	guestAuth(t, guest)
	guest.expectError(t, deviceAddMethod, "guest", map[string]any{}, codeDenied)

	registered := registerTestPasskey(t, owner, a)
	userID := registered["you"].(map[string]any)["user_id"]
	owner.drain(t)
	app.mu.RLock()
	before := len(app.sessions)
	app.mu.RUnlock()
	token, origin := addDevice(t, owner, "phone")
	if origin != testPasskeyOrigin || token == registered["token"] {
		t.Fatalf("token %q for origin %q, sign-in token %q", token, origin, registered["token"])
	}
	if again, _ := addDevice(t, owner, "phone"); again != token {
		t.Fatalf("retry issued %q, want %q", again, token)
	}
	app.mu.RLock()
	after := len(app.sessions)
	app.mu.RUnlock()
	if after != before+1 {
		t.Fatalf("sessions: %d, then %d", before, after)
	}

	phone, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	resumed := phone.result(t, "auth", "resume", map[string]any{"scheme": "token", "token": token})
	if resumed["you"].(map[string]any)["user_id"] != userID || resumed["token"] != token {
		t.Fatalf("device resume: %#v", resumed)
	}
	browser, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	if kept := browser.result(t, "auth", "resume", map[string]any{"scheme": "token", "token": registered["token"]}); kept["you"].(map[string]any)["user_id"] != userID {
		t.Fatalf("issuing connection's token: %#v", kept)
	}
	elsewhere, _ := dialOrigin(t, httpServer, "http://localhost:8080")
	elsewhere.expectError(t, "auth", "other-origin", map[string]any{"scheme": "token", "token": token}, codeDenied)
	native, _ := dialRaw(t, httpServer)
	native.expectError(t, "auth", "no-origin", map[string]any{"scheme": "token", "token": token}, codeDenied)
}

// An account adds deviceAddBurst devices at once; the next is retry_after,
// on any of its connections, and issues nothing.
func TestDeviceAddLimit(t *testing.T) {
	app, mailbox, httpServer := emailTestServer(t, nil)
	c, _ := dialRaw(t, httpServer)
	propose(t, c, "ada@example.com")
	_, signedIn := signInByEmail(t, c, mailbox.receive(t).Code, nil)
	c.drain(t)
	for range deviceAddBurst {
		addDevice(t, c, c.nextID("device"))
	}
	other, _ := dialRaw(t, httpServer)
	other.result(t, "auth", "resume", map[string]any{"scheme": "token", "token": signedIn["token"]})
	other.drain(t)
	app.mu.RLock()
	before := len(app.sessions)
	app.mu.RUnlock()
	frame := other.call(t, deviceAddMethod, "over", map[string]any{})
	failure, _ := frame["error"].(map[string]any)
	data, _ := failure["data"].(map[string]any)
	if failure["code"] != float64(codeRetryAfter) || data["retry_after"].(float64) <= 0 {
		t.Fatalf("over the limit: %#v", frame)
	}
	app.mu.RLock()
	after := len(app.sessions)
	app.mu.RUnlock()
	if after != before {
		t.Fatalf("a refused request kept a session: %d, then %d", before, after)
	}
}

// A server without sign-in sessions neither advertises nor answers the
// method: guests have no token scheme to resume one with.
func TestDeviceAddNeedsSessions(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	c, frame := dialRaw(t, httpServer)
	if capabilities := frame["params"].(map[string]any)["capabilities"].([]any); slices.Contains(capabilities, any(deviceAddMethod)) {
		t.Fatalf("capabilities: %#v", capabilities)
	}
	guestAuth(t, c)
	c.drain(t)
	c.expectError(t, deviceAddMethod, "unknown", map[string]any{}, codeUnsupported)
}

// A device's token is kept in the store like a sign-in's.
func TestDeviceAddTokensPersist(t *testing.T) {
	kept := store.NewMemory()
	mailbox := make(testMailbox, 4)
	configure := func(config *Config) { config.EmailSender = mailbox }
	_, httpServer, stop := startWith(t, kept, t.TempDir(), configure)
	c, _ := dialRaw(t, httpServer)
	propose(t, c, "ada@example.com")
	_, signedIn := signInByEmail(t, c, mailbox.receive(t).Code, nil)
	c.drain(t)
	token, origin := addDevice(t, c, "phone")
	if origin != "" {
		t.Fatalf("origin of a connection without one: %q", origin)
	}
	stop()

	_, httpServer, _ = startWith(t, kept, t.TempDir(), configure)
	phone, _ := dialRaw(t, httpServer)
	resumed := phone.result(t, "auth", "resume", map[string]any{"scheme": "token", "token": token})
	if resumed["you"].(map[string]any)["user_id"] != signedIn["you"].(map[string]any)["user_id"] {
		t.Fatalf("resumed after restart: %#v", resumed)
	}
}
