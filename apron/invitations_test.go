package apron

import (
	"context"
	"encoding/json/v2"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/store"
)

// invitedServer is a passkey server whose sign-in an extension restricts to
// an empty allowlist, on backing, with the extension's Host and Allowlist.
func invitedServer(t *testing.T, backing store.Store) (*Server, *httptest.Server, *Host, *Allowlist) {
	t.Helper()
	var host *Host
	var allowlist *Allowlist
	config := extensionConfig(testExtension{"closed", func(r *Registrar) error {
		host, allowlist = r.Host, r.RestrictSignIn()
		return nil
	}})
	config.WebAuthn = testWebAuthn(t)
	config.Store = backing
	app, httpServer := newTestServer(t, config)
	return app, httpServer, host, allowlist
}

// registerInvited registers a passkey on a connection that is not signed
// in, with an invitation, and returns the finish frame.
func registerInvited(t *testing.T, c *testClient, a *testAuthenticator, token string, extra map[string]any) map[string]any {
	t.Helper()
	begin := map[string]any{"invitation": token}
	for key, value := range extra {
		begin[key] = value
	}
	options := passkeyResult(t, passkeyCall(t, c, c.nextID("begin"), "register", "begin", begin))
	return passkeyCall(t, c, c.nextID("finish"), "register", "finish", map[string]any{
		"invitation": token, "credential": a.registration(t, options, testPasskeyOrigin),
	})
}

func TestAnInvitationMakesOneAccount(t *testing.T) {
	backing := store.NewMemory()
	app, httpServer, host, allowlist := invitedServer(t, backing)
	ctx := context.Background()
	invitation, err := host.Invite(ctx, InvitationOptions{
		Name: "Owner", UserID: "owner", Roles: []string{RoleAdmin}, Rooms: []string{defaultRoomID},
		Data: map[string]any{"grant": "owner"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if invitation.Token == "" || invitation.ID == "" || invitation.By != "closed" {
		t.Fatalf("invitation: %#v", invitation)
	}
	if listed := app.Invitations(); len(listed) != 1 || listed[0].Token != "" || listed[0].ID != invitation.ID {
		t.Fatalf("listed: %#v", listed)
	}

	// Without an invitation, a connection that is not signed in cannot
	// register, and a wrong token is refused.
	c, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	passkeyDenied(t, passkeyCall(t, c, "bare", "register", "begin", nil))
	passkeyDenied(t, passkeyCall(t, c, "wrong", "register", "begin", map[string]any{"invitation": "apron_invite_wrong"}))

	// A registration that fails, or is abandoned, leaves the invitation.
	abandoned, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	passkeyResult(t, passkeyCall(t, abandoned, "begin", "register", "begin", map[string]any{"invitation": invitation.Token}))
	options := passkeyResult(t, passkeyCall(t, c, "begin", "register", "begin", map[string]any{"invitation": invitation.Token}))
	bad := newTestAuthenticator(t).registration(t, options, "http://evil.example")
	passkeyDenied(t, passkeyCall(t, c, "bad", "register", "finish", map[string]any{"invitation": invitation.Token, "credential": bad}))
	if len(app.Invitations()) != 1 {
		t.Fatal("a failed registration used the invitation")
	}

	a := newTestAuthenticator(t)
	result := passkeyResult(t, registerInvited(t, c, a, invitation.Token, map[string]any{"name": "Ada"}))
	you := result["you"].(map[string]any)
	if you["user_id"] != "owner" || you["name"] != "Ada" || result["token"] == nil || !slices.Contains(asList(you["roles"]), any(RoleAdmin)) {
		t.Fatalf("invited account: %#v", result)
	}
	// The reply came after the account and the used invitation were stored.
	var storedUser, storedInvitation bool
	_ = backing.Load(func(e store.Entry) error {
		storedUser = storedUser || (e.Kind == entryUser && e.ID == "owner")
		storedInvitation = storedInvitation || e.Kind == entryInvitation
		return nil
	})
	if !storedUser || storedInvitation {
		t.Fatalf("stored user %v, invitation %v", storedUser, storedInvitation)
	}
	enrollment, ok := host.Enrollment("owner")
	var data map[string]any
	_ = json.Unmarshal(enrollment.Data, &data)
	if !ok || !enrollment.Admitted || enrollment.Invitation != invitation.ID || enrollment.By != "closed" || data["grant"] != "owner" {
		t.Fatalf("enrollment: %#v", enrollment)
	}
	if len(app.Invitations()) != 0 {
		t.Fatal("the invitation outlived its use")
	}

	// It is used up: the abandoned registration, finished now, fails.
	passkeyDenied(t, passkeyCall(t, abandoned, "finish", "register", "finish", map[string]any{
		"invitation": invitation.Token, "credential": newTestAuthenticator(t).registration(t, map[string]any{"public_key": map[string]any{"challenge": "x", "user": map[string]any{"id": "x"}}}, testPasskeyOrigin),
	}))
	again, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	passkeyDenied(t, passkeyCall(t, again, "reuse", "register", "begin", map[string]any{"invitation": invitation.Token}))

	// The account signs in again, the allowlist naming nobody.
	login := passkeyResult(t, passkeyCall(t, again, "login", "login", "begin", nil))
	passkeyResult(t, passkeyCall(t, again, "login-end", "login", "finish", map[string]any{"credential": a.assertion(t, login, testPasskeyOrigin, "localhost", 0x05)}))

	// Disallowing it ends the invitation's admission, and signs it out.
	allowlist.Disallow(UserID("owner"))
	if enrollment, _ := host.Enrollment("owner"); enrollment.Admitted {
		t.Fatal("still admitted")
	}
	fresh, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	login = passkeyResult(t, passkeyCall(t, fresh, "login", "login", "begin", nil))
	passkeyDenied(t, passkeyCall(t, fresh, "login-end", "login", "finish", map[string]any{"credential": a.assertion(t, login, testPasskeyOrigin, "localhost", 0x05)}))
}

func TestInvitationsLastAndExpire(t *testing.T) {
	backing := store.NewMemory()
	app, _, host, _ := invitedServer(t, backing)
	ctx := context.Background()
	kept, err := app.Invite(ctx, InvitationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	short, err := host.Invite(ctx, InvitationOptions{Lifetime: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	revoked, _ := host.Invite(ctx, InvitationOptions{})
	if ok, err := host.RevokeInvitation(ctx, revoked.ID); !ok || err != nil {
		t.Fatalf("revoke: %v %v", ok, err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := app.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	restarted, httpServer, _, _ := invitedServer(t, backing)
	listed := restarted.Invitations()
	if len(listed) != 1 || listed[0].ID != kept.ID || listed[0].By != "" {
		t.Fatalf("after a restart: %#v (short %s)", listed, short.ID)
	}
	c, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	passkeyDenied(t, passkeyCall(t, c, "expired", "register", "begin", map[string]any{"invitation": short.Token}))
	passkeyResult(t, registerInvited(t, c, newTestAuthenticator(t), kept.Token, nil))
}

// A reply that makes an account waits for the store: if the store fails,
// the connection closes with no result.
func TestAnInvitedAccountIsStoredBeforeItsReply(t *testing.T) {
	backing := &failingStore{Memory: store.NewMemory()}
	_, httpServer, host, _ := invitedServer(t, backing)
	invitation, err := host.Invite(context.Background(), InvitationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	options := passkeyResult(t, passkeyCall(t, c, "begin", "register", "begin", map[string]any{"invitation": invitation.Token}))
	backing.failing.Store(true)
	c.write(t, map[string]any{"method": "auth", "id": "finish", "params": map[string]any{
		"scheme": "webauthn", "action": "register", "step": "finish", "challenge_id": c.passkeyChallenge,
		"invitation": invitation.Token, "credential": newTestAuthenticator(t).registration(t, options, testPasskeyOrigin),
	}})
	frame := c.read(t)
	if frame["id"] != nil || frame["error"] == nil {
		t.Fatalf("frame after a failed store: %#v", frame)
	}
}
