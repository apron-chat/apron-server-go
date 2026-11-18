package apron

import (
	"context"
	"encoding/json/v2"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/store"
)

const testRemote = "ws://b.example:8787/ws"

func TestShadowIDs(t *testing.T) {
	for _, tc := range []struct{ server, user, want string }{
		{"wss://B.example/ws", "carol", "carol@b.example"},
		{"ws://127.0.0.1:8787/ws", "carol", "carol@127.0.0.1:8787"},
		{"ws://[::1]:8081/ws", "Dave", "Dave@[::1]:8081"},
		{"wss://b.example/ws", "dave@c.example", "dave@c.example@b.example"},
	} {
		if got, err := ShadowID(tc.server, tc.user); err != nil || got != tc.want {
			t.Errorf("ShadowID(%q, %q) = %q, %v; want %q", tc.server, tc.user, got, err, tc.want)
		}
	}
	for _, bad := range [][2]string{{"b.example", "carol"}, {"wss:///ws", "carol"}, {"wss://u@b.example/ws", "carol"}, {testRemote, ""}, {testRemote, "~server"}, {testRemote, "a b"}} {
		if id, err := ShadowID(bad[0], bad[1]); err == nil {
			t.Errorf("ShadowID(%q, %q) = %q, want an error", bad[0], bad[1], id)
		}
	}
}

// call sends a request on a session's bot and decodes its result.
func callAs(t *testing.T, b *Bot, method string, params map[string]any) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := b.Call(ctx, method, params)
	if err != nil {
		t.Fatalf("%s as %s: %v", method, b.UserID(), err)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

// nextNotification reads frames until a notification with method, skipping
// others, such as memberships.
func nextNotification(t *testing.T, c *testClient, method string) map[string]any {
	t.Helper()
	for {
		frame := c.read(t)
		if frame["method"] == method {
			return notificationParams(t, frame, method)
		}
	}
}

func remoteOfJSON(t *testing.T, object map[string]any) map[string]any {
	t.Helper()
	ext, _ := object["ext"].(map[string]any)
	remote, _ := ext["remote"].(map[string]any)
	return remote
}

func TestShadowUsers(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "aprond.db")
	var host *Host
	var bot *Bot
	extension := testExtension{"fed", func(r *Registrar) error {
		host = r.Host
		if room, created, err := r.CreateRoom("ops", RoomOptions{Title: "Ops"}); err != nil || room.ID != "ops" {
			return err
		} else if again, createdAgain, err := r.CreateRoom("ops", RoomOptions{}); err != nil || createdAgain || again.Title != "Ops" {
			t.Errorf("CreateRoom again = %#v, %v, %v (first created %v)", again, createdAgain, err, created)
		}
		if _, _, err := r.CreateRoom("OPS", RoomOptions{}); err == nil {
			t.Error("CreateRoom made a room that differs from another in case only")
		}
		var err error
		bot, err = r.Bot("fedbot", BotOptions{Name: "Federation"})
		return err
	}}
	var otherHost *Host
	other := testExtension{"other", func(r *Registrar) error {
		otherHost = r.Host
		return nil
	}}
	open := func() (*Server, *httptest.Server, func()) {
		s, err := store.OpenSQLite(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		app, httpServer, stop := startWith(t, s, filepath.Join(dir, "uploads"), func(c *Config) {
			c.Extensions = []Extension{extension, other}
		})
		return app, httpServer, stop
	}
	app, httpServer, stop := open()

	shadow, err := host.Shadow(testRemote, "carol")
	if err != nil {
		t.Fatal(err)
	}
	const carol = "carol@b.example:8787"
	if shadow.UserID() != carol {
		t.Fatalf("shadow user_id = %q", shadow.UserID())
	}
	if again, err := host.Shadow(testRemote, "carol"); err != nil || again.UserID() != carol {
		t.Fatalf("Shadow again = %v, %v", again, err)
	}
	if _, err := host.Shadow(testRemote, "Carol"); err == nil {
		t.Fatal("a shadow took a user_id that differs from another in case only")
	}
	if _, err := otherHost.Shadow(testRemote, "carol"); err == nil {
		t.Fatal("another extension took the shadow")
	}
	if _, err := otherHost.Shadow(testRemote, "dave"); err != nil {
		t.Fatalf("another extension's own shadow: %v", err)
	}
	user, ok := host.User(carol)
	if !ok || !user.Account || user.Bot || len(user.Roles) != 0 || user.Remote == nil || *user.Remote != (RemoteUser{testRemote, "carol"}) {
		t.Fatalf("shadow user = %#v", user)
	}
	if _, err := shadow.IssueToken(context.Background(), ServiceTokenOptions{}); err == nil {
		t.Fatal("a shadow got a service token")
	}

	// Nobody may request the shadow's user_id.
	c, _ := dialRaw(t, httpServer)
	result := c.result(t, "auth", "a1", map[string]any{"scheme": "guest", "user_id": carol, "name": "Mallory"})
	if c.userID = result["you"].(map[string]any)["user_id"].(string); c.userID == carol {
		t.Fatal("a guest took the shadow's user_id")
	}
	c.drain(t)
	joinRoom(t, c, "ops")

	// The shadow joins, renames itself, and posts: its records are its own,
	// with where it is from.
	callAs(t, shadow, "room_join", map[string]any{"room_id": "ops"})
	expectMembership(t, c, "ops", carol, true)
	callAs(t, shadow, "me", map[string]any{"name": "Carol", "ext": map[string]any{"remote": "", "tz": "UTC"}})
	profile := nextNotification(t, c, "user")["new"].(map[string]any)
	if profile["name"] != "Carol" || !reflect.DeepEqual(remoteOfJSON(t, profile), map[string]any{"server": testRemote, "user_id": "carol"}) {
		t.Fatalf("shadow profile = %#v", profile)
	}
	posted := callAs(t, shadow, "message", map[string]any{
		"room_id": "ops", "body": map[string]any{"text": "hello from b"},
		"ext": map[string]any{"remote": map[string]any{"server": "wss://evil.example/ws", "user_id": "eve", "room_id": "r1", "message_id": "1724803599120", "extra": true}},
	})
	messageID := posted["message_id"].(string)
	snapshot := nextNotification(t, c, "message")
	from := snapshot["from"].(map[string]any)
	if from["user_id"] != carol || from["name"] != "Carol" || !reflect.DeepEqual(remoteOfJSON(t, from), map[string]any{"server": testRemote, "user_id": "carol"}) {
		t.Fatalf("from = %#v", from)
	}
	// The server sets it: what the session wrote is ignored.
	wantRemote := map[string]any{"server": testRemote, "user_id": "carol"}
	if !reflect.DeepEqual(remoteOfJSON(t, snapshot), wantRemote) {
		t.Fatalf("message ext = %#v", snapshot["ext"])
	}

	// A message it posts without ext.remote has the shadow's.
	plain := callAs(t, shadow, "message", map[string]any{"room_id": "ops", "body": map[string]any{"text": "plain"}})
	if got := remoteOfJSON(t, nextNotification(t, c, "message")); !reflect.DeepEqual(got, map[string]any{"server": testRemote, "user_id": "carol"}) {
		t.Fatalf("plain message ext.remote = %#v", got)
	}

	// It edits its own message, which keeps ext.remote, and reacts.
	callAs(t, shadow, "message", map[string]any{"room_id": "ops", "message_id": messageID, "body": map[string]any{"text": "hello from b (edited)"}})
	if edited := nextNotification(t, c, "message"); wstestText(edited) != "hello from b (edited)" || !reflect.DeepEqual(remoteOfJSON(t, edited), wantRemote) {
		t.Fatalf("edited = %#v", edited)
	}
	callAs(t, shadow, "reactions", map[string]any{"message_id": messageID, "emojis": []any{"👍"}})
	reaction := nextNotification(t, c, "reactions")["reactions"].([]any)[0].(map[string]any)
	if reactionFrom := reaction["from"].(map[string]any); reactionFrom["user_id"] != carol || remoteOfJSON(t, reactionFrom) == nil {
		t.Fatalf("reaction = %#v", reaction)
	}

	// People cannot write ext.remote, nor change the shadow's messages.
	mine, _ := save(t, c, "m1", map[string]any{"room_id": "ops", "body": map[string]any{"text": "local"}, "ext": map[string]any{"remote": map[string]any{"server": testRemote, "user_id": "carol"}, "x": 1}})
	if snapshot := lastSnapshot(t, app, mine); remoteOfJSON(t, snapshot) != nil || snapshot["ext"] == nil {
		t.Fatalf("a client wrote ext.remote: %#v", snapshot)
	}
	c.expectError(t, "message", "m2", map[string]any{"room_id": "ops", "message_id": messageID, "body": map[string]any{"text": "mine now"}}, CodeDenied)
	if you := c.result(t, "me", "me1", map[string]any{"ext": map[string]any{"remote": map[string]any{"server": testRemote}}})["you"].(map[string]any); you["ext"] != nil {
		t.Fatalf("a client wrote its own ext.remote: %#v", you)
	}
	c.drain(t)
	c.request(t, "room_set", "r1", map[string]any{"room_id": "ops", "title": "Ops", "ext": map[string]any{"remote": map[string]any{"server": testRemote}}})
	app.mu.RLock()
	roomExt := app.rooms["ops"].record["ext"]
	app.mu.RUnlock()
	if roomExt != nil {
		t.Fatalf("a client wrote a room's ext.remote: %#v", roomExt)
	}
	c.drain(t)
	// The extension's bot writes it as it is.
	callAs(t, bot, "room_join", map[string]any{"room_id": "ops"})
	botPost := callAs(t, bot, "message", map[string]any{"room_id": "ops", "body": map[string]any{"text": "relay"}, "ext": map[string]any{"remote": map[string]any{"server": testRemote, "room_id": "r1"}}})
	if got := remoteOfJSON(t, lastSnapshot(t, app, botPost["message_id"].(string))); !reflect.DeepEqual(got, map[string]any{"server": testRemote, "room_id": "r1"}) {
		t.Fatalf("bot's ext.remote = %#v", got)
	}

	// It deletes its own message.
	callAs(t, shadow, "message", map[string]any{"room_id": "ops", "message_id": plain["message_id"], "deleted": true})
	if snapshot := lastSnapshot(t, app, plain["message_id"].(string)); snapshot["deleted"] != true {
		t.Fatalf("delete = %#v", snapshot)
	}
	// It shows as offline, though its extension's session is connected.
	app.mu.RLock()
	status := app.users[carol].status
	app.mu.RUnlock()
	if status != statusOffline {
		t.Fatalf("shadow status = %q", status)
	}

	// The shadow is kept across a restart, as the same account.
	stop()
	app, _, stop = open()
	defer stop()
	user, ok = host.User(carol)
	if !ok || user.Name != "Carol" || user.Remote == nil || user.Remote.UserID != "carol" || len(user.Roles) != 0 {
		t.Fatalf("restored shadow = %#v", user)
	}
	if room, ok := host.Room("ops"); !ok || room.Title != "Ops" {
		t.Fatalf("restored room = %#v", room)
	}
	shadow, err = host.Shadow(testRemote, "carol")
	if err != nil {
		t.Fatal(err)
	}
	again := callAs(t, shadow, "message", map[string]any{"room_id": "ops", "message_id": messageID, "body": map[string]any{"text": "after restart"}})
	if again["message_id"] != messageID || !reflect.DeepEqual(remoteOfJSON(t, lastSnapshot(t, app, messageID)), wantRemote) {
		t.Fatalf("save after restart = %#v", again)
	}
	app.mu.RLock()
	member := app.rooms["ops"].members[carol] != nil
	app.mu.RUnlock()
	if !member {
		t.Fatal("the shadow left ops at the restart")
	}

	// A shadow that left a room writes there no more, until it is added
	// again.
	callAs(t, shadow, "room_leave", map[string]any{"room_id": "ops"})
	if member := app.rooms["ops"].members[carol] != nil; member {
		t.Fatal("the shadow is still in the room")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for method, params := range map[string]map[string]any{
		"message":   {"room_id": "ops", "body": map[string]any{"text": "still here?"}},
		"reactions": {"message_id": messageID, "emojis": []any{"👀"}},
	} {
		if _, err := shadow.Call(ctx, method, params); err == nil || err.(*Error).Code != CodeDenied {
			t.Fatalf("%s after leaving: %v", method, err)
		}
	}
	callAs(t, bot, "room_join", map[string]any{"room_id": "ops", "user_id": carol})
	if member := app.rooms["ops"].members[carol] != nil; !member {
		t.Fatal("the shadow was not added")
	}
	callAs(t, shadow, "message", map[string]any{"room_id": "ops", "body": map[string]any{"text": "back"}})
}

// lastSnapshot is a message's current snapshot.
func lastSnapshot(t *testing.T, app *Server, messageID string) map[string]any {
	t.Helper()
	app.mu.RLock()
	defer app.mu.RUnlock()
	m := app.messages[messageID]
	if m == nil {
		t.Fatalf("no message %s", messageID)
	}
	snapshot := m.snapshot()
	// ext values are raw JSON; decode them as clients would.
	encoded, _ := json.Marshal(snapshot)
	var decoded map[string]any
	_ = json.Unmarshal(encoded, &decoded)
	return decoded
}

func wstestText(snapshot map[string]any) string {
	body, _ := snapshot["body"].(map[string]any)
	text, _ := body["text"].(string)
	return text
}
