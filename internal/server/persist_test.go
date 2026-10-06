package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/apron-chat/apron-server-go/internal/store"
)

// startWithStore starts a passkey-enabled server on a store and returns a
// function that shuts it down, closing the store.
func startWithStore(t *testing.T, s store.Store, uploadDir string) (*Server, *httptest.Server, func()) {
	t.Helper()
	return startWith(t, s, uploadDir, func(*Config) {})
}

// startWith is startWithStore with configure applied to the configuration.
func startWith(t *testing.T, s store.Store, uploadDir string, configure func(*Config)) (*Server, *httptest.Server, func()) {
	t.Helper()
	config := DefaultConfig()
	config.WebAuthn = testWebAuthn(t)
	config.Store = s
	config.UploadDir = uploadDir
	configure(&config)
	app, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(app.Handler())
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := app.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		httpServer.Close()
	}
	t.Cleanup(stop)
	return app, httpServer, stop
}

func TestStateSurvivesRestart(t *testing.T) {
	// Each opener returns a function that opens the same store again after a
	// restart.
	for name, opener := range map[string]func() func(t *testing.T, path string) store.Store{
		"memory": func() func(*testing.T, string) store.Store {
			memory := store.NewMemory()
			return func(*testing.T, string) store.Store { return memory }
		},
		"sqlite": func() func(*testing.T, string) store.Store {
			return func(t *testing.T, path string) store.Store {
				s, err := store.OpenSQLite(path)
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			open := opener()
			dir := t.TempDir()
			dbPath, uploadDir := filepath.Join(dir, "aprond.db"), filepath.Join(dir, "uploads")

			app, httpServer, stop := startWithStore(t, open(t, dbPath), uploadDir)
			owner, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
			registered := registerTestPasskey(t, owner, newTestAuthenticator(t))
			ownerID := registered["you"].(map[string]any)["user_id"].(string)
			token := registered["token"].(string)
			guest := dialTestClient(t, httpServer)
			owner.drain(t)

			hello, _ := save(t, owner, "hello", map[string]any{"body": map[string]any{"text": "Hello <world> & all"}})
			guest.drain(t)
			react(t, guest, "react", hello, "👍")
			owner.drain(t)
			gone, _ := save(t, owner, "gone", map[string]any{"body": map[string]any{"text": "Secret first line\nmore"}})
			guest.drain(t)
			thread, _ := saveRoom(t, owner, "thread", map[string]any{"parent_room_id": "general", "description": "Hello thread"})
			ops, _ := saveRoom(t, owner, "ops", map[string]any{"title": "Ops", "private": true})
			owner.request(t, "message", "delete", map[string]any{"room_id": "general", "message_id": gone, "deleted": true})
			attached := postEmbeds(t, owner, "attach", map[string]any{"room_id": ops, "body": map[string]any{"text": "file", "embeds": []any{map[string]any{"kind": "upload", "title": "dots.png"}}}})
			writeURL := attached["embeds"].([]any)[0].(map[string]any)["write_url"].(string)
			image := testPNG(t)
			if status, _, _ := httpDo(t, http.MethodPut, writeURL, bytes.NewReader(image), "image/png"); status != http.StatusCreated {
				t.Fatalf("upload: %d", status)
			}
			fileURL := embedsOf(t, owner.notification(t, "message"))[0]["url"].(string)
			owner.drain(t)
			guest.drain(t)

			// The guest leaves first, so that shutdown changes nothing more.
			_ = guest.ws.Close(websocket.StatusNormalClosure, "bye")
			expectMembership(t, owner, "general", guest.userID, false)
			owner.expectQuiet(t)
			// Every change reached the store: after shutdown it holds exactly
			// the state the server had.
			app.mu.Lock()
			want := make(map[string]string)
			for _, entry := range app.dumpLocked() {
				want[entry.Kind+"/"+entry.ID] = string(entry.Value)
			}
			app.mu.Unlock()

			before := map[string]map[string]any{}
			for _, room := range []string{"general", thread, ops} {
				before[room] = historyPage(t, owner, room, map[string]any{"limit": 1000})
			}
			stop()
			reopened := open(t, dbPath)
			stored := make(map[string]string)
			if err := reopened.Load(func(e store.Entry) error {
				stored[e.Kind+"/"+e.ID] = string(e.Value)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			_ = reopened.Close()
			for key, value := range want {
				if stored[key] != value {
					t.Errorf("stored %s = %s\nwant %s", key, stored[key], value)
				}
			}
			for key := range stored {
				if _, ok := want[key]; !ok {
					t.Errorf("stored %s is not in the server's state", key)
				}
			}
			if t.Failed() {
				t.FailNow()
			}

			_, httpServer, _ = startWithStore(t, open(t, dbPath), uploadDir)
			resumed, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
			result := passkeyResult(t, passkeyCall(t, resumed, "resume", "token", "", map[string]any{"token": token}))
			if result["you"].(map[string]any)["user_id"] != ownerID {
				t.Fatalf("resumed as %v, want %s", result["you"], ownerID)
			}
			for room, page := range before {
				after := historyPage(t, resumed, room, map[string]any{"limit": 1000})
				for _, key := range []string{"rooms", "messages", "reactions"} {
					if !reflect.DeepEqual(after[key], page[key]) {
						t.Fatalf("%s %s after restart:\n%#v\nwant\n%#v", room, key, after[key], page[key])
					}
				}
				if room == "general" {
					// The guest's leave is in the log.
					membership := records(t, after, "memberships")
					last := membership[len(membership)-1].(map[string]any)["members"].([]any)[0].(map[string]any)
					if last["user"].(map[string]any)["user_id"] != guest.userID || last["joined"] != false {
						t.Fatalf("guest after restart: %#v", last)
					}
				}
			}
			listed := listRooms(t, resumed, map[string]any{"filter": "joined"})
			if ids := roomIDs(t, listed["joined"]); len(ids) != 3 {
				t.Fatalf("joined rooms after restart: %v", ids)
			}
			parsed, _ := url.Parse(fileURL)
			status, _, content := httpDo(t, http.MethodGet, httpServer.URL+parsed.Path, nil, "")
			if status != http.StatusOK || !bytes.Equal(content, image) {
				t.Fatalf("upload after restart: %d", status)
			}
			// Guest IDs are never reissued, across restarts too.
			next := dialTestClient(t, httpServer)
			if next.userID == guest.userID {
				t.Fatalf("guest ID %s reissued", next.userID)
			}
			// A private room stays private.
			next.expectError(t, "history", "private", map[string]any{"room_id": ops}, codeInvalidParams)
			// A new message's log_id follows every restored record.
			resumed.drain(t)
			id, _ := save(t, resumed, "after", map[string]any{"body": map[string]any{"text": "after"}})
			if parseID(t, id) <= parseID(t, before["general"]["latest_log_id"]) {
				t.Fatalf("log_id %s does not follow %v", id, before["general"]["latest_log_id"])
			}
		})
	}
}

// TestRestoreAfterACrash restores a store written while a guest was
// connected and an upload was pending, as after a crash: the guest is
// retired and the pending embed leaves its message.
func TestRestoreAfterACrash(t *testing.T) {
	app, httpServer, _ := startWithStore(t, store.NewMemory(), t.TempDir())
	guest := dialTestClient(t, httpServer)
	pending := postEmbeds(t, guest, "pending", map[string]any{"room_id": "general", "body": map[string]any{"text": "soon", "embeds": []any{map[string]any{"kind": "upload"}}}})
	app.mu.Lock()
	crashed := store.NewMemory()
	if err := crashed.Apply(app.dumpLocked()); err != nil {
		t.Fatal(err)
	}
	app.mu.Unlock()

	restored, httpServer, _ := startWithStore(t, crashed, t.TempDir())
	restored.mu.RLock()
	_, guestKept := restored.users[guest.userID]
	restored.mu.RUnlock()
	if guestKept {
		t.Fatal("a guest outlived the restart")
	}
	c := dialTestClient(t, httpServer)
	page := historyPage(t, c, "general", map[string]any{})
	messages := records(t, page, "messages")
	last := messages[len(messages)-1].(map[string]any)
	if last["message_id"] != pending["message_id"] || len(embedsOf(t, last)) != 0 {
		t.Fatalf("pending upload after restart: %#v", last)
	}
	membership := records(t, page, "memberships")
	var left bool
	for _, value := range membership {
		for _, member := range value.(map[string]any)["members"].([]any) {
			m := member.(map[string]any)
			left = left || (m["user"].(map[string]any)["user_id"] == guest.userID && m["joined"] == false)
		}
	}
	if !left {
		t.Fatalf("no leave logged for the retired guest: %#v", membership)
	}
}

// TestRestoreMigratesProtocolV6State restores entries as a protocol v6
// server stored them: a thread whose intro_message was a message, embedded
// by reference in its logged record, and a @room notice. The intro text
// becomes the room's description and the notice is from ~room, in memory
// and, rewritten once, in the store.
func TestRestoreMigratesProtocolV6State(t *testing.T) {
	v6 := store.NewMemory()
	entry := func(kind, id, value string) store.Entry {
		return store.Entry{Kind: kind, ID: id, Value: []byte(value)}
	}
	intro := `{"body":{"text":"Why *the* deploy failed\n- 4pm"},"from":{"user_id":"alice"},"log_id":"1000","message_id":"1000","room_id":"general"}`
	edited := `{"body":{"text":"Why the 4pm deploy failed"},"from":{"user_id":"alice"},"log_id":"1003","message_id":"1000","prev_log_id":"1000","room_id":"general"}`
	notice := `{"body":{"text":"@bob was removed by @alice"},"from":{"name":"General","user_id":"@room"},"log_id":"1002","message_id":"1002","room_id":"general"}`
	if err := v6.Apply([]store.Entry{
		entry(entryMeta, "counters", `{"last_id":1004}`),
		entry(entryRecord, "999", `{"kind":0,"raw":{"log_id":"999","room_id":"general","title":"General"},"rooms":["general"]}`),
		entry(entryRecord, "1000", `{"kind":1,"raw":`+intro+`,"rooms":["general"]}`),
		entry(entryRecord, "1001", `{"kind":0,"raw":{"log_id":"1001","parent_room_id":"general","room_id":"1001","title":"Why the deploy failed"},"intro":1000,"rooms":["1001"]}`),
		entry(entryRecord, "1004", `{"kind":0,"raw":{"log_id":"1004","prev_log_id":"1001","parent_room_id":"general","room_id":"1001","title":"Deploy"},"intro":1000,"rooms":["1001"]}`),
		entry(entryRecord, "1002", `{"kind":1,"raw":`+notice+`,"rooms":["general"]}`),
		entry(entryRecord, "1003", `{"kind":1,"raw":`+edited+`,"rooms":["general"]}`),
		entry(entryRoom, "general", `{"record":{"log_id":"999","room_id":"general","title":"General"},"record_log_id":999,"created_id":999,"latest_id":1003,"members":[]}`),
		entry(entryRoom, "1001", `{"parent":"general","record":{"intro_message":{"message_id":"1000"},"log_id":"1004","prev_log_id":"1001","parent_room_id":"general","room_id":"1001","title":"Deploy"},"record_log_id":1004,"created_id":1001,"latest_id":1004,"members":[],"title_from":"1000"}`),
		entry(entryMessage, "1000", `{"from":{"user_id":"alice"},"log_id":1003,"owner":"alice","room_id":"general","records":[1000,1003],"titled_rooms":["1001"]}`),
		entry(entryMessage, "1002", `{"from":{"name":"General","user_id":"@room"},"log_id":1002,"owner":"@room","room_id":"general","records":[1002]}`),
	}); err != nil {
		t.Fatal(err)
	}
	check := func(httpServer *httptest.Server) {
		t.Helper()
		c := dialTestClient(t, httpServer)
		// An earlier logged record takes the text of the intro snapshot it
		// embedded, escaped as Markdown since it was plain text; the current
		// record takes the message's current text, as v6 showed it, and so
		// does the latest logged record, at the same log_id.
		rooms := records(t, historyPage(t, c, "1001", map[string]any{}), "rooms")
		first := map[string]any{"log_id": "1001", "parent_room_id": "general", "room_id": "1001", "title": "Why the deploy failed", "description": "Why \\*the\\* deploy failed\n\\- 4pm"}
		want := map[string]any{"log_id": "1004", "prev_log_id": "1001", "parent_room_id": "general", "room_id": "1001", "title": "Deploy", "description": "Why the 4pm deploy failed"}
		if len(rooms) != 2 || !reflect.DeepEqual(rooms[0], any(first)) || !reflect.DeepEqual(rooms[1], any(want)) {
			t.Fatalf("migrated room records: %#v", rooms)
		}
		listed := listRooms(t, c, map[string]any{"room_id": "1001"})["not_joined"].([]any)
		room := listed[0].(map[string]any)
		delete(room, "latest_log_id")
		delete(room, "history_log_id")
		if !reflect.DeepEqual(room, want) {
			t.Fatalf("migrated current record: %#v", room)
		}
		messages := records(t, historyPage(t, c, "general", map[string]any{}), "messages")
		from := messages[1].(map[string]any)["from"]
		if !reflect.DeepEqual(from, map[string]any{"user_id": "~room", "name": "General"}) {
			t.Fatalf("migrated notice sender: %#v", messages)
		}
	}
	_, httpServer, stop := startWithStore(t, v6, t.TempDir())
	check(httpServer)
	stop()
	// The migration was written back: a second start finds only v7 state.
	var legacy []string
	_ = v6.Load(func(e store.Entry) error {
		if bytes.Contains(e.Value, []byte("intro")) || bytes.Contains(e.Value, []byte(`"@room"`)) {
			legacy = append(legacy, e.Kind+" "+e.ID)
		}
		return nil
	})
	if len(legacy) != 0 {
		t.Fatalf("v6 state left in the store: %v", legacy)
	}
	_, httpServer, _ = startWithStore(t, v6, t.TempDir())
	check(httpServer)
}

// Roles from the configuration are granted to accounts, shown in their
// current user objects, and never settable with `me`; a granted user_id
// that was never used is not given to a guest. server.welcome is the
// configured Markdown.
func TestRolesAndWelcome(t *testing.T) {
	kept := store.NewMemory()
	_, httpServer, stop := startWithStore(t, kept, t.TempDir())
	ada, frame := dialOrigin(t, httpServer, testPasskeyOrigin)
	if _, has := frame["params"].(map[string]any)["welcome"]; has {
		t.Fatalf("welcome without configuration: %#v", frame)
	}
	registered := registerTestPasskey(t, ada, newTestAuthenticator(t))
	adaID := registered["you"].(map[string]any)["user_id"].(string)
	if roles := registered["you"].(map[string]any)["roles"]; !reflect.DeepEqual(roles, []any{}) {
		t.Fatalf("an account without roles: %#v", registered)
	}
	stop()

	welcome := "Chat as a guest, or **sign in** to keep your name."
	_, httpServer, _ = startWith(t, kept, t.TempDir(), func(config *Config) {
		config.Welcome = welcome
		config.Roles = map[string][]string{"admin": {adaID, "Newbie"}, "moderator": {adaID}}
	})
	ada, frame = dialOrigin(t, httpServer, testPasskeyOrigin)
	if frame["params"].(map[string]any)["welcome"] != welcome {
		t.Fatalf("server frame: %#v", frame)
	}
	resumed := passkeyResult(t, passkeyCall(t, ada, "resume", "token", "", map[string]any{"token": registered["token"]}))
	if roles := resumed["you"].(map[string]any)["roles"]; !reflect.DeepEqual(roles, []any{"admin", "moderator"}) {
		t.Fatalf("resumed account's roles: %#v", resumed)
	}
	// `me` cannot set roles, nor clear them.
	if you := ada.result(t, "me", "me", map[string]any{"roles": []any{}, "name": "Ada"})["you"].(map[string]any); !reflect.DeepEqual(you["roles"], []any{"admin", "moderator"}) {
		t.Fatalf("me changed roles: %#v", you)
	}

	guest, _ := dialRaw(t, httpServer)
	_, authed := guest.request(t, "auth", "auth", map[string]any{"scheme": "guest", "user_id": "newbie"})
	you := authed["you"].(map[string]any)
	guest.userID = you["user_id"].(string)
	expectMembership(t, guest, "general", guest.userID, true)
	if guest.userID == "newbie" {
		t.Fatal("a guest took a granted user_id")
	}
	if you := guest.result(t, "me", "me", map[string]any{"roles": []any{"admin"}})["you"].(map[string]any); you["roles"] != nil {
		t.Fatalf("me granted roles: %#v", you)
	}
	// Roles travel in current objects, such as room_list's users, and not in
	// recorded ones.
	ada.drain(t)
	listed := listRooms(t, guest, map[string]any{"room_id": "general", "members": true})
	var roles any
	for _, user := range listed["users"].([]any) {
		if user.(map[string]any)["user_id"] == adaID {
			roles = user.(map[string]any)["roles"]
		}
	}
	if !reflect.DeepEqual(roles, []any{"admin", "moderator"}) {
		t.Fatalf("listed users: %#v", listed["users"])
	}
	_, snapshot := save(t, ada, "hello", map[string]any{"body": map[string]any{"text": "hi"}})
	guest.drain(t)
	if !reflect.DeepEqual(snapshot["from"], map[string]any{"user_id": adaID, "name": "Ada"}) {
		t.Fatalf("recorded sender: %#v", snapshot["from"])
	}
	// room_update joined carries them too.
	ada.userID = adaID
	opsRoom, _ := saveRoom(t, ada, "ops", map[string]any{"title": "Ops"})
	before, _ := guest.request(t, "room_join", "join-ops", map[string]any{"room_id": opsRoom})
	var joinedRoles any
	for _, user := range notificationParams(t, before[0], "room_update")["users"].([]any) {
		if user.(map[string]any)["user_id"] == adaID {
			joinedRoles = user.(map[string]any)["roles"]
		}
	}
	if !reflect.DeepEqual(joinedRoles, []any{"admin", "moderator"}) {
		t.Fatalf("room_update joined users: %#v", before)
	}
	ada.drain(t)
	// An admin may remove others from any room they can see.
	room, _ := saveRoom(t, guest, "room", map[string]any{"title": "Mine"})
	ada.result(t, "room_leave", "remove", map[string]any{"room_id": room, "user_id": guest.userID})
	expectLeft(t, guest, room, guest.userID)
	guest.expectError(t, "room_leave", "remove-admin", map[string]any{"room_id": "general", "user_id": adaID}, codeDenied)
}

func TestEscapeMarkdown(t *testing.T) {
	for text, want := range map[string]string{
		"plain words":            "plain words",
		"*bold* _it_ `code` [x]": `\*bold\* \_it\_ \` + "`" + `code\` + "`" + ` \[x\]`,
		"# title\n> quote\n- item\n  + sub\n1. one\n2) two": "\\# title\n\\> quote\n\\- item\n  \\+ sub\n1\\. one\n2\\) two",
		"a - b 1. c <tag> ~x~ |p|":                          `a - b 1. c \<tag\> \~x\~ \|p\|`,
		`back\slash`:                                        `back\\slash`,
	} {
		if got := escapeMarkdown(text); got != want {
			t.Errorf("escapeMarkdown(%q) = %q, want %q", text, got, want)
		}
	}
}
