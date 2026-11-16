package apron

import (
	"context"
	"encoding/json/v2"
	"maps"
	"slices"
	"testing"
	"time"

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
	// The address is stored in both forms, so a server from before
	// identities reads it too.
	if stored.Identities["email"] != "ada@example.com" || stored.Email != "ada@example.com" {
		t.Fatalf("stored = %#v, want the identity in both forms", stored)
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

// Config.Roles names holders as principals: a bare holder is a user_id, or
// an email address too, and no other scheme's subject, which matches only
// as scheme:subject, exactly.
func TestRolesNameHoldersByPrincipal(t *testing.T) {
	config := DefaultConfig()
	config.EmailSender = LogEmailSender{}
	config.Roles = map[string][]string{
		"admin":     {"ada", "Boss@Example.com"},
		"moderator": {"external:github:7"},
	}
	app, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Shutdown(context.Background())
	roles := func(id string, identities ...authn.Identity) []string {
		app.mu.Lock()
		defer app.unlock()
		u := newUserState(id, id)
		for _, identity := range identities {
			app.users[id] = u
			app.setIdentityLocked(u, identity, nil)
		}
		app.grantRolesLocked(u)
		return u.roles
	}
	for name, test := range map[string]struct {
		id         string
		identities []authn.Identity
		want       []string
	}{
		"user_id":                      {"ada", []authn.Identity{{Scheme: "email", Subject: "ada@example.org"}}, []string{"admin"}},
		"address":                      {"x1", []authn.Identity{{Scheme: "email", Subject: "boss@example.com"}}, []string{"admin"}},
		"external subject as user_id":  {"x2", []authn.Identity{{Scheme: "external", Subject: "ada"}}, nil},
		"external subject as address":  {"x3", []authn.Identity{{Scheme: "external", Subject: "boss@example.com"}}, nil},
		"external by scheme:subject":   {"x4", []authn.Identity{{Scheme: "external", Subject: "github:7"}}, []string{"moderator"}},
		"external subjects keep case":  {"x5", []authn.Identity{{Scheme: "external", Subject: "GitHub:7"}}, nil},
		"passkey subject is no holder": {"x6", []authn.Identity{{Scheme: "webauthn", Subject: "ada"}}, nil},
	} {
		if got := roles(test.id, test.identities...); !slices.Equal(got, test.want) {
			t.Errorf("%s: roles %v, want %v", name, got, test.want)
		}
	}

	for _, holder := range []string{"not a holder", "sso:ada", "Email:ada@example.com", "email:", "external:"} {
		config.Roles = map[string][]string{"admin": {holder}}
		if bad, err := Open(config); err == nil {
			_ = bad.Shutdown(context.Background())
			t.Errorf("Open accepted the role holder %q, which names nobody", holder)
		}
	}
}

// Proposals made at once are limited as those made one after another are:
// of many connections proposing one address together, one mails it.
func TestEmailLimitsHoldForProposalsMadeAtOnce(t *testing.T) {
	_, mailbox, httpServer := emailTestServer(t, nil)
	const n = 12
	clients := make([]*testClient, n)
	for i := range clients {
		clients[i] = dialFrom(t, httpServer, "203.0.113."+string(rune('1'+i%9)))
	}
	results := make(chan map[string]any, n)
	for i, c := range clients {
		go func() {
			c.write(t, map[string]any{"method": "auth", "id": "p", "params": map[string]any{"scheme": "email", "email": "victim@example.com"}})
			results <- clients[i].read(t)
		}()
	}
	accepted := 0
	for range n {
		if frame := <-results; frame["result"] != nil {
			accepted++
		} else if errorCode(frame) != CodeRetryAfter {
			t.Fatalf("proposal = %#v", frame)
		}
	}
	if accepted != 1 {
		t.Fatalf("%d proposals accepted at once, want 1", accepted)
	}
	mailbox.receive(t)
	select {
	case message := <-mailbox:
		t.Fatalf("a second email went out: %#v", message)
	case <-time.After(100 * time.Millisecond):
	}
}

// A scheme's name is its own: reserved and repeated names are refused.
func TestSchemeNamesAreChecked(t *testing.T) {
	app, err := Open(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Shutdown(context.Background())
	app.mu.Lock()
	defer app.unlock()
	if err := app.addSchemeLocked(namedScheme("custom")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "guest", "token", "user", "custom", "a:b", "a b"} {
		if err := app.addSchemeLocked(namedScheme(name)); err == nil {
			t.Errorf("scheme %q was added", name)
		}
	}
}

type namedScheme string

func (n namedScheme) Name() string  { return string(n) }
func (n namedScheme) Label() string { return string(n) }
func (namedScheme) Auth(context.Context, *authn.Request) (authn.Result, error) {
	return &authn.Reply{Result: map[string]any{}}, nil
}
