package apron

import (
	"context"
	"encoding/json/v2"
	"maps"
	"slices"
	"testing"

	"github.com/apron-chat/apron-server-go/internal/auth/email"
	"github.com/apron-chat/apron-server-go/internal/authn"
	"github.com/apron-chat/apron-server-go/store"
)

// Accounts stored with their address and external identity in fields of
// their own load with them as identities, keep them in the new form, and
// get the roles granted to either.
func TestStoredIdentitiesConvert(t *testing.T) {
	memory := store.NewMemory()
	if err := memory.Apply([]store.Entry{
		{Kind: entryUser, ID: "ada", Value: []byte(`{"name":"Ada","email":"ada@example.com"}`)},
		{Kind: entryUser, ID: "bob", Value: []byte(`{"name":"Bob","external":"github:7"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig()
	config.Store = memory
	config.EmailSender = LogEmailSender{}
	config.Roles = map[string][]string{RoleAdmin: {"ada@example.com"}, RoleModerator: {"external:github:7"}}
	app, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	app.mu.RLock()
	ada := app.accounts[authn.Identity{Scheme: "email", Subject: "ada@example.com"}]
	bob := app.accounts[authn.Identity{Scheme: "external", Subject: "github:7"}]
	app.mu.RUnlock()
	if ada == nil || ada.id != "ada" || !ada.hasRole(RoleAdmin) {
		t.Fatalf("ada = %#v", ada)
	}
	if bob == nil || bob.id != "bob" || !bob.hasRole(RoleModerator) {
		t.Fatalf("bob = %#v", bob)
	}
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	var stored storedUser
	if err := memory.Load(func(e store.Entry) error {
		if e.Kind == entryUser && e.ID == "ada" {
			return json.Unmarshal(e.Value, &stored)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if stored.Identities["email"] != "ada@example.com" || stored.Email != "" {
		t.Fatalf("stored = %#v, want the identity in the new form", stored)
	}
}

// An identity has one account: adding it to another is refused, and an
// account has one identity of each scheme.
func TestIdentitiesBelongToOneAccount(t *testing.T) {
	app, mailbox, httpServer := emailTestServer(t, nil)
	first := dialTestClient(t, httpServer)
	propose(t, first, "ada@example.com")
	approve(t, first, mailbox.receive(t).Code, nil)
	second := dialTestClient(t, httpServer)
	ageSends(app, "ada@example.com", email.ResendInterval)
	propose(t, second, "ada@example.com")
	frame := second.call(t, "auth", "a2", map[string]any{"scheme": "email", "token": pendingCode(t, app, second.userID)})
	if errorCode(frame) != CodeDenied {
		t.Fatalf("adding another account's address = %#v", frame)
	}
	app.mu.RLock()
	owner := app.accounts[authn.Identity{Scheme: "email", Subject: "ada@example.com"}]
	identities := slices.Collect(maps.Keys(owner.identities))
	app.mu.RUnlock()
	if owner.id != first.userID || !slices.Equal(identities, []string{"email"}) {
		t.Fatalf("owner %s with %v, want %s with its address", owner.id, identities, first.userID)
	}
}
