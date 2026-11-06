package apron

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/store"
)

func TestParsePrincipal(t *testing.T) {
	for text, want := range map[string]Principal{
		"Ada@Example.com":           Email("ada@example.com"),
		"email:ada@example.com":     Email("ada@example.com"),
		"external:github:583231":    ExternalID("github:583231"),
		"user:ada":                  UserID("ada"),
		"user:Ada":                  UserID("ada"),
		"  email:bob@example.com  ": Email("bob@example.com"),
	} {
		if got, err := ParsePrincipal(text); err != nil || got != want {
			t.Errorf("ParsePrincipal(%q) = %v, %v; want %v", text, got, err, want)
		}
	}
	for _, text := range []string{"ada", "email:not-an-address", "user:", "external:", "phone:123"} {
		if _, err := ParsePrincipal(text); err == nil {
			t.Errorf("ParsePrincipal(%q) succeeded", text)
		}
	}
}

func TestRestrictedSignIn(t *testing.T) {
	var allowlist *Allowlist
	app, mailbox, httpServer := emailTestServer(t, func(config *Config) {
		config.Extensions = []Extension{testExtension{"private", func(r *Registrar) error {
			allowlist = r.RestrictSignIn()
			if err := allowlist.Allow(Email("ada@example.com")); err != nil {
				return err
			}
			return r.Advertise(map[string]any{"secret": true})
		}}}
	})
	app.mu.Lock()
	app.rooms[defaultRoomID].record["description"] = "Our plans for the quarter"
	app.unlock()

	// Before a sign-in, the server frame holds what signing in needs.
	c, frame := dialRaw(t, httpServer)
	params := frame["params"].(map[string]any)
	capabilities, auth := asList(params["capabilities"]), asList(params["auth"])
	if slices.Contains(auth, any("guest")) || params["ext"] != nil || params["description"] != nil ||
		slices.Contains(capabilities, any("ext:emoji")) || slices.Contains(capabilities, any("ext:private")) {
		t.Fatalf("server frame before sign-in = %#v", params)
	}
	c.expectError(t, "auth", "g1", map[string]any{"scheme": "guest"}, CodeUnsupported)

	// An address not on the list is never mailed, and the result reads
	// the same.
	propose(t, c, "mallory@example.com")
	select {
	case message := <-mailbox:
		t.Fatalf("mailed an address not on the list: %#v", message)
	case <-time.After(100 * time.Millisecond):
	}

	// An address on it signs in, and gets the whole server frame.
	propose(t, c, "ada@example.com")
	code := mailbox.receive(t).Code
	_, result := signInByEmail(t, c, code, nil)
	token := result["token"].(string)
	params = notificationParams(t, c.read(t), "server")
	if params["description"] != "Our plans for the quarter" || !slices.Contains(asList(params["capabilities"]), any("ext:private")) {
		t.Fatalf("server frame after sign-in = %#v", params)
	}
	resumed, _ := dialRaw(t, httpServer)
	resumed.result(t, "auth", "r1", map[string]any{"scheme": "token", "token": token})

	// Taken off the list, the account is signed out everywhere.
	allowlist.Disallow(Email("ada@example.com"))
	for _, conn := range []*testClient{c, resumed} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		for {
			if _, _, err := conn.ws.Read(ctx); err != nil {
				break
			}
		}
		cancel()
	}
	again, _ := dialRaw(t, httpServer)
	again.expectError(t, "auth", "r2", map[string]any{"scheme": "token", "token": token}, CodeDenied)
	if principals := allowlist.Principals(); len(principals) != 0 {
		t.Fatalf("principals = %v", principals)
	}

	// Back on the list by user_id, the existing account signs in again;
	// a user_id never admits a new account.
	if err := allowlist.Allow(UserID(c.userID), UserID("newcomer")); err != nil {
		t.Fatal(err)
	}
	ageSends(app, "ada@example.com", time.Hour)
	propose(t, again, "ada@example.com")
	signInByEmail(t, again, mailbox.receive(t).Code, nil)
	stranger, _ := dialRaw(t, httpServer)
	propose(t, stranger, "newcomer@example.com")
	select {
	case message := <-mailbox:
		t.Fatalf("mailed an address for a new account by user_id: %#v", message)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestRestrictedSignInAtStart(t *testing.T) {
	memory := store.NewMemory()
	restricted := func(allow ...Principal) Config {
		config := DefaultConfig()
		config.Store = memory
		config.EmailSender = LogEmailSender{}
		config.Extensions = []Extension{testExtension{"private", func(r *Registrar) error {
			return r.RestrictSignIn().Allow(allow...)
		}}}
		return config
	}
	app, err := Open(restricted(UserID("Ada")))
	if err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	ada := newUserState("ada", "Ada")
	ada.email = "ada@example.com"
	app.users["ada"], app.emails["ada@example.com"] = ada, ada
	app.touchUser("ada")
	key := [32]byte{1}
	app.sessions[key] = session{user: ada, expires: time.Now().Add(time.Hour)}
	app.touchSession(key)
	admitted := app.admittedLocked(ada)
	app.unlock()
	if !admitted {
		t.Fatal("user:Ada does not admit ada")
	}
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Started again without Ada on the list, the server signs her out.
	app, err = Open(restricted(Email("bob@example.com")))
	if err != nil {
		t.Fatal(err)
	}
	defer app.Shutdown(context.Background())
	app.mu.RLock()
	_, kept := app.sessions[key]
	app.mu.RUnlock()
	if kept {
		t.Fatal("a session of an account taken off the list survived the restart")
	}
}

func TestSomeoneCanSignIn(t *testing.T) {
	config := DefaultConfig()
	config.DisableGuests = true
	if app, err := Open(config); err == nil {
		_ = app.Shutdown(context.Background())
		t.Fatal("Open accepted a server nobody can sign in to")
	}
}

func TestClosingConnectionsDoNothing(t *testing.T) {
	app, err := Open(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Shutdown(context.Background())
	c := &client{server: app, out: make(chan outboundBatch, 4), done: make(chan struct{}), user: newUserState("ada", "Ada")}
	c.closing.Store(true)
	app.processFrame(c, []byte(`{"method":"push_register","id":"p1","params":{"kind":"relay","url":"https://push.example/x","token":"t"}}`))
	if len(c.out) != 0 || len(app.pushes) != 0 {
		t.Fatalf("a closing connection's request ran: %d frames, %d pushes", len(c.out), len(app.pushes))
	}
}
