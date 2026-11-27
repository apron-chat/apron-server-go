package aprond

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/apron-chat/apron-server-go/aprond/internal/roomlog"
	"github.com/apron-chat/apron-server-go/aprond/internal/rooms"
	"github.com/apron-chat/apron-server-go/aprond/internal/users"
	"github.com/apron-chat/apron-server-go/store"
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
		shutdown(t, app)
		httpServer.Close()
	}
	t.Cleanup(stop)
	return app, httpServer, stop
}

// shutdown shuts app down and waits until it is done. Closing a SQLite
// store checkpoints and syncs it, which a busy disk can hold up for seconds,
// so a short deadline would fail a shutdown that is only slow, and leave its
// store open for the test's next one. A shutdown not done in a minute is
// stuck, and the goroutines' stacks show where.
func shutdown(t *testing.T, app *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		var stacks strings.Builder
		_ = pprof.Lookup("goroutine").WriteTo(&stacks, 1)
		t.Errorf("shutdown: %v\n%s", err, stacks.String())
	}
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
			stopping := time.Now().UnixMilli()
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
			// Shutdown closes the owner's connection, which shows them
			// offline, so it adds their seen and stores them as no longer
			// shown otherwise (§4.5.2).
			var ownerUser users.Stored
			if err := json.Unmarshal([]byte(stored["user/"+owner.userID]), &ownerUser); err != nil || ownerUser.Seen < stopping || ownerUser.Shown {
				t.Fatalf("owner after shutdown: seen %d, shown %v (%v), want seen at least %d, not shown", ownerUser.Seen, ownerUser.Shown, err, stopping)
			}
			ownerUser.Seen, ownerUser.Shown = 0, true
			stored["user/"+owner.userID] = string(encodeJSON(ownerUser))
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
			next.expectError(t, "history", "private", map[string]any{"room_id": ops}, CodeInvalidParams)
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
	guestKept := restored.users.Get(guest.userID) != nil
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
	expectJoinedDefault(t, guest, guest.userID)
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
	guest.expectError(t, "room_leave", "remove-admin", map[string]any{"room_id": "general", "user_id": adaID}, CodeDenied)
}

// dumpLocked renders the whole state as the entries a store holding it
// would have, for checking that every change reaches the store.
func (s *Server) dumpLocked() []store.Entry {
	var records []store.Entry
	for _, r := range s.rooms.All() {
		records = append(records, rooms.Entry(r))
	}
	logged, _ := s.log.After(0, 1<<30)
	for _, record := range logged {
		records = append(records, roomlog.Entry(record))
	}
	for _, m := range s.rooms.Messages() {
		records = append(records, rooms.MessageEntry(m.Message))
	}
	records = append(records, s.storedVAPID.Snapshot()...)
	meta := s.metaLocked()
	records = append(records, s.users.Snapshot(avatarEmbedID)...)
	records = append(records, s.pushes.Snapshot()...)
	records = append(records, s.sessions.Snapshot(userID)...)
	records = append(records, s.ids.Snapshot()...)
	records = append(records, s.devices.Snapshot(userID)...)
	records = append(records, s.services.Snapshot(userID)...)
	records = append(records, s.invitations.Snapshot()...)
	records = append(records, s.embeds.Snapshot(avatarOwner)...)
	if s.emoji.Len() > 0 {
		// An empty catalog is stored only once one was emptied.
		records = append(records, s.emoji.Snapshot()...)
	}
	records = append(records, s.profile.Snapshot()...)
	return append(records, store.Entry{Kind: entryMeta, ID: "counters", Value: encodeJSON(meta)})
}

// A large room's entry, which lists every member, is not stored again for
// every message: it lags behind its latest_log_id, activity, and read
// cursors for a moment (rooms.EntryDelay), and is stored once that passes,
// at Sync, and at shutdown; a restart in between restores the latest_log_id
// from the records.
func TestLargeRoomEntryLags(t *testing.T) {
	st := store.NewMemory()
	bs := newBenchServer(t, Config{Store: st})
	for i := range rooms.LaterMembers {
		bs.member(i)
	}
	poster := bs.guest()
	storedLatest := func() string {
		var latest string
		_ = st.Load(func(e store.Entry) error {
			if e.Kind == entryRoom && e.ID == defaultRoomID {
				var room struct {
					LatestID int64 `json:"latest_id"`
				}
				_ = json.Unmarshal(e.Value, &room)
				latest = formatID(room.LatestID)
			}
			return nil
		})
		return latest
	}
	post := func() string {
		return stringField(poster.call(t, "message", map[string]any{"body": map[string]any{"text": "hi"}}), "message_id")
	}
	first := post()
	if err := bs.s.Sync(bs.ctx); err != nil {
		t.Fatal(err)
	}
	if got := storedLatest(); got != first {
		t.Fatalf("stored latest_id %s at Sync, want %s", got, first)
	}
	second := post()
	deadline := time.Now().Add(10 * time.Second)
	for storedLatest() != second {
		if time.Now().After(deadline) {
			t.Fatalf("stored latest_id %s, never %s", storedLatest(), second)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A server restored from records newer than its room's entry.
	third := post()
	snapshot := store.NewMemory()
	var entries []store.Entry
	_ = st.Load(func(e store.Entry) error { entries = append(entries, e); return nil })
	if storedLatest() == third {
		t.Skip("the room's entry was stored before it could be seen lagging")
	}
	_ = snapshot.Apply(entries)
	restored, err := Open(Config{Store: snapshot})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Shutdown(context.Background())
	// The restart retires the guest, whose leave is the latest record.
	restored.mu.RLock()
	log := restored.rooms.Get(defaultRoomID).Log()
	latest, last := log.Latest(), log.Since(0, 1)
	restored.mu.RUnlock()
	if len(last) != 1 || latest != last[0].ID || formatID(latest) < third {
		t.Fatalf("restored latest_log_id %d, want the latest record's, at least %s", latest, third)
	}
}
