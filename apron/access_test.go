package apron

import (
	"context"
	"errors"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/apron-chat/apron-server-go/store"
)

// lobbyConfigure puts a server in lobby mode, with ada@example.com an admin
// and mo@example.com a moderator.
func lobbyConfigure(config *Config) {
	config.Access = Access{Mode: AccessLobby}
	config.Roles = map[string][]string{"admin": {"ada@example.com"}, "moderator": {"mo@example.com"}}
}

// lobbyServer starts a lobby mode server with email sign-in.
func lobbyServer(t *testing.T) (*Server, testMailbox, *httptest.Server) {
	t.Helper()
	return emailTestServer(t, lobbyConfigure)
}

// emailSignUp signs a new connection in by email as address, asking for
// userID, and reads its join to roomID, which follows the result.
func emailSignUp(t *testing.T, httpServer *httptest.Server, mailbox testMailbox, address, userID, roomID string) (*testClient, map[string]any) {
	t.Helper()
	c, _ := dialRaw(t, httpServer)
	propose(t, c, address)
	_, result := signInByEmail(t, c, mailbox.receive(t).Code, map[string]any{"user_id": userID})
	expectJoined(t, c, roomID, c.userID)
	return c, result
}

// lobbyGuest signs in a guest, which joins the Lobby.
func lobbyGuest(t *testing.T, httpServer *httptest.Server) (*testClient, map[string]any) {
	t.Helper()
	c, _ := dialRaw(t, httpServer)
	_, result := c.request(t, "auth", c.nextID("auth"), map[string]any{"scheme": "guest"})
	c.userID = result["you"].(map[string]any)["user_id"].(string)
	expectJoined(t, c, DefaultLobbyRoom, c.userID)
	return c, result
}

// expectJoined reads the room_update that joins userID to roomID.
func expectJoined(t *testing.T, c *testClient, roomID, userID string) {
	t.Helper()
	frame := c.read(t)
	if record := joinedRecord(t, frame, userID); record["room_id"] != roomID {
		t.Fatalf("joined %#v, want %s", record, roomID)
	}
	checkMembership(t, membershipOf(t, frame), roomID, userID, true)
}

// permissionsOf is a user object's ext.permissions.
func permissionsOf(user map[string]any) map[string]any {
	ext, _ := user["ext"].(map[string]any)
	permissions, _ := ext["permissions"].(map[string]any)
	return permissions
}

// rolesOf is a user object's roles, nil when it has none.
func rolesOf(user map[string]any) []any {
	roles, _ := user["roles"].([]any)
	return roles
}

// commandIn runs a command line in a room and returns the frames before its
// reply, and the reply.
func commandIn(t *testing.T, c *testClient, roomID, line string) ([]map[string]any, map[string]any) {
	t.Helper()
	id := c.nextID("command")
	c.write(t, map[string]any{"method": "command", "id": id, "params": map[string]any{"room_id": roomID, "body": map[string]any{"text": line}}})
	var before []map[string]any
	for {
		frame := c.read(t)
		if frame["id"] == id {
			return before, frame
		}
		before = append(before, frame)
	}
}

func TestLobbyServerFrame(t *testing.T) {
	app, _, httpServer := lobbyServer(t)
	_, frame := dialRaw(t, httpServer)
	params := frame["params"].(map[string]any)
	capabilities := params["capabilities"].([]any)
	if !slices.Contains(capabilities, any("ext:permissions")) || !slices.Contains(capabilities, any("ext:settings")) {
		t.Fatalf("capabilities %v", capabilities)
	}
	lobby := params["ext"].(map[string]any)["settings"].(map[string]any)["lobby"]
	if !reflect.DeepEqual(lobby, map[string]any{"room_id": "lobby", "open": true}) {
		t.Fatalf("ext.settings.lobby = %#v", lobby)
	}
	if !reflect.DeepEqual(params["auth"], []any{"email", "token", "guest"}) || !reflect.DeepEqual(params["signup"], []any{"email", "guest"}) {
		t.Fatalf("auth %v, signup %v", params["auth"], params["signup"])
	}
	app.mu.RLock()
	title := app.rooms["lobby"].title()
	app.mu.RUnlock()
	if title != "Public Lobby" {
		t.Fatalf("the Lobby is titled %q", title)
	}

	// A public server says nothing of a lobby.
	_, _, public := emailTestServer(t, nil)
	_, frame = dialRaw(t, public)
	params = frame["params"].(map[string]any)
	if ext, _ := params["ext"].(map[string]any); slices.Contains(params["capabilities"].([]any), any("ext:permissions")) || ext["settings"] != nil {
		t.Fatalf("public server frame %#v", params)
	}
}

// The description of a server frame for a newcomer is the Lobby's; for a
// member it is general's, which a sign-in sends.
func TestLobbyDescription(t *testing.T) {
	_, mailbox, httpServer := lobbyServer(t)
	ada, signedIn := emailSignUp(t, httpServer, mailbox, "ada@example.com", "ada", "general")
	edit := func(roomID, description string) {
		t.Helper()
		if _, result := ada.request(t, "room_set", ada.nextID("edit"), map[string]any{"room_id": roomID, "description": description}); result["room_id"] != roomID {
			t.Fatalf("edit %s: %#v", roomID, result)
		}
	}
	edit("general", "For members")
	edit("lobby", "Say hello")
	newcomer, frame := dialRaw(t, httpServer)
	if description := frame["params"].(map[string]any)["description"]; description != "Say hello" {
		t.Fatalf("a newcomer's description %v", description)
	}
	newcomer.result(t, "auth", "resume", map[string]any{"scheme": "token", "token": signedIn["token"]})
	frame = newcomer.read(t)
	if params := notificationParams(t, frame, "server"); params["description"] != "For members" {
		t.Fatalf("after signing in as a member: %#v", params)
	}
}

// A guest, or a waiting account, sees the Lobby and its threads: every
// other room answers as one that does not exist, and it starts nothing.
func TestLobbyVisibility(t *testing.T) {
	app, mailbox, httpServer := lobbyServer(t)
	ada, _ := emailSignUp(t, httpServer, mailbox, "ada@example.com", "ada", "general")
	inGeneral, _ := save(t, ada, "in-general", map[string]any{"room_id": "general", "body": map[string]any{"text": "members only"}})
	thread, _ := saveRoom(t, ada, "thread", map[string]any{"parent_room_id": "general", "description": "Side talk"})
	joinRoom(t, ada, "lobby")
	lobbyThread, _ := saveRoom(t, ada, "lobby-thread", map[string]any{"parent_room_id": "lobby", "description": "Intros"})
	inLobby, _ := save(t, ada, "in-lobby", map[string]any{"room_id": "lobby", "body": map[string]any{"text": "welcome"}})
	ada.drain(t)

	guest, guestAuth := lobbyGuest(t, httpServer)
	if you := guestAuth["you"].(map[string]any); !reflect.DeepEqual(permissionsOf(you), map[string]any{"lobby": "waiting"}) || you["roles"] != nil {
		t.Fatalf("a guest's you: %#v", you)
	}
	waiting, waitingAuth := emailSignUp(t, httpServer, mailbox, "wes@example.com", "wes", "lobby")
	if you := waitingAuth["you"].(map[string]any); !reflect.DeepEqual(permissionsOf(you), map[string]any{"lobby": "waiting"}) || !reflect.DeepEqual(rolesOf(you), []any{"waiting"}) {
		t.Fatalf("a waiting account's you: %#v", you)
	}
	if !app.Waiting("wes") || !app.Waiting(guest.userID) || app.Waiting("ada") {
		t.Fatal("Waiting disagrees")
	}
	ada.drain(t)
	// The admin may invite, and is not limited.
	if you := ada.result(t, "me", "me", map[string]any{})["you"].(map[string]any); !reflect.DeepEqual(permissionsOf(you), map[string]any{"invite": true}) {
		t.Fatalf("the admin's you: %#v", you)
	}
	guest.drain(t)

	for _, c := range []*testClient{guest, waiting} {
		c.drain(t)
		list := listRooms(t, c, map[string]any{})
		if joined, others := roomIDs(t, list["joined"]), roomIDs(t, list["not_joined"]); !reflect.DeepEqual(joined, []string{"lobby"}) || len(others) != 0 {
			t.Fatalf("%s lists joined %v, not_joined %v", c.userID, joined, others)
		}
		if threads := roomIDs(t, listRooms(t, c, map[string]any{"parent_room_id": "lobby"})["not_joined"]); !reflect.DeepEqual(threads, []string{lobbyThread}) {
			t.Fatalf("%s lists the Lobby's threads %v", c.userID, threads)
		}
		historyPage(t, c, "lobby", map[string]any{})
		historyPage(t, c, lobbyThread, map[string]any{})
		for name, request := range map[string]struct {
			method string
			params map[string]any
		}{
			"list general":    {"room_list", map[string]any{"room_id": "general"}},
			"list threads":    {"room_list", map[string]any{"parent_room_id": "general"}},
			"history":         {"history", map[string]any{"room_id": "general"}},
			"thread history":  {"history", map[string]any{"room_id": thread}},
			"join":            {"room_join", map[string]any{"room_id": "general"}},
			"join thread":     {"room_join", map[string]any{"room_id": thread}},
			"leave":           {"room_leave", map[string]any{"room_id": "general"}},
			"edit":            {"room_set", map[string]any{"room_id": "general", "title": "Mine"}},
			"new thread":      {"room_set", map[string]any{"parent_room_id": "general"}},
			"post":            {"message", map[string]any{"room_id": "general", "body": map[string]any{"text": "hi"}}},
			"reply":           {"message", map[string]any{"room_id": "lobby", "reply_to": inGeneral, "body": map[string]any{"text": "hi"}}},
			"react":           {"reactions", map[string]any{"message_id": inGeneral, "emojis": []any{"👍"}}},
			"command":         {"command", map[string]any{"room_id": "general", "body": map[string]any{"text": "/help"}}},
			"lobby edit":      {"room_set", map[string]any{"room_id": "lobby", "title": "Mine"}},
			"lobby thread":    {"room_set", map[string]any{"parent_room_id": "lobby"}},
			"new room":        {"room_set", map[string]any{"title": "Mine"}},
			"new private":     {"room_set", map[string]any{"private": true}},
			"add someone":     {"room_join", map[string]any{"room_id": "lobby", "user_id": "ada"}},
			"emoji":           {"command", map[string]any{"room_id": "lobby", "body": map[string]any{"text": "/emoji add :wave:", "embeds": []any{map[string]any{"kind": "upload", "content_type": "image/png", "size": 10}}}}},
			"invite":          {"command", map[string]any{"room_id": "lobby", "body": map[string]any{"text": "/invite ada"}}},
			"toggle":          {"command", map[string]any{"room_id": "lobby", "body": map[string]any{"text": "/toggle lobby"}}},
			"read elsewhere":  {"activity", map[string]any{"room_id": "lobby", "read_message_id": inGeneral}},
			"history in room": {"history", map[string]any{"room_id": "general", "after": inGeneral, "before": inGeneral}},
		} {
			want := CodeInvalidParams
			switch name {
			case "lobby edit", "lobby thread", "new room", "new private", "add someone", "emoji", "invite", "toggle":
				want = CodeDenied
			case "read elsewhere":
				// activity is never answered (§4.6).
				continue
			}
			frame := c.call(t, request.method, c.nextID(name), request.params)
			if code := errorCode(frame); code != want {
				t.Errorf("%s: %s = %#v, want error %d", c.userID, name, frame, want)
			}
		}
		// A move out of the Lobby is to a room it does not see.
		mine, _ := save(t, c, c.nextID("mine"), map[string]any{"room_id": "lobby", "body": map[string]any{"text": "hello from " + c.userID}})
		c.expectError(t, "message", c.nextID("move"), map[string]any{"message_id": mine, "room_id": "general", "body": map[string]any{"text": "moved"}}, CodeInvalidParams)
		// What it may do: post, reply, and react in the Lobby, and a
		// request without room_id goes there.
		if _, result := c.request(t, "message", c.nextID("default"), map[string]any{"body": map[string]any{"text": "no room"}}); result["message_id"] == nil {
			t.Fatalf("%s: a message without room_id: %#v", c.userID, result)
		}
		c.request(t, "reactions", c.nextID("react"), map[string]any{"message_id": inLobby, "emojis": []any{"👋"}})
		if _, reply := commandIn(t, c, "lobby", "/help"); errorCode(reply) != 0 {
			t.Fatalf("%s: /help = %#v", c.userID, reply)
		}
		c.drain(t)
	}
	// A guest keeps no avatar or push registration; a waiting account may.
	guest.drain(t)
	guest.expectError(t, "command", "avatar", map[string]any{"room_id": "lobby", "body": map[string]any{"text": "/avatar", "embeds": []any{map[string]any{"kind": "upload", "content_type": "image/png", "size": 10}}}}, CodeDenied)
	guest.expectError(t, "push_register", "push", map[string]any{"kind": "relay", "url": "https://push.example/relay"}, CodeDenied)
	// No member can add a restricted user elsewhere.
	ada.drain(t)
	ada.expectError(t, "room_join", "add-guest", map[string]any{"room_id": "general", "user_id": guest.userID}, CodeDenied)
	ada.expectError(t, "room_join", "add-waiting", map[string]any{"room_id": "general", "user_id": "wes"}, CodeDenied)
	// Who shares the Lobby sees a waiting account's role.
	list := listRooms(t, ada, map[string]any{"room_id": "lobby", "members": true, "user_id": "wes"})
	if users := list["users"].([]any); len(users) != 1 || !reflect.DeepEqual(rolesOf(users[0].(map[string]any)), []any{"waiting"}) || permissionsOf(users[0].(map[string]any)) != nil {
		t.Fatalf("the waiting account to others: %#v", users)
	}
}

// /invite lets a guest in once it signs up, and a waiting account at once.
func TestLobbyInvite(t *testing.T) {
	app, mailbox, httpServer := lobbyServer(t)
	ada, _ := emailSignUp(t, httpServer, mailbox, "ada@example.com", "ada", "general")
	joinRoom(t, ada, "lobby")
	ada.drain(t)
	guest, _ := lobbyGuest(t, httpServer)
	expectMembership(t, ada, "lobby", guest.userID, true)

	// /help lists /invite and /toggle to admins and moderators only.
	before, _ := commandIn(t, ada, "general", "/help")
	if text := noticeText(t, before[0]); !strings.Contains(text, "`/invite <user_id>`") || !strings.Contains(text, "`/toggle [lobby]`") {
		t.Fatalf("an admin's /help: %q", text)
	}
	guest.drain(t)
	before, _ = commandIn(t, guest, "lobby", "/help")
	if text := noticeText(t, before[0]); strings.Contains(text, "/invite ") || strings.Contains(text, "/toggle") {
		t.Fatalf("a guest's /help: %q", text)
	}

	// Unknown users, and members, change nothing.
	_, reply := commandIn(t, ada, "general", "/invite nobody")
	if errorCode(reply) != CodeInvalidParams {
		t.Fatalf("/invite nobody = %#v", reply)
	}
	if _, reply := commandIn(t, ada, "general", "/invite"); errorCode(reply) != CodeInvalidParams {
		t.Fatalf("/invite = %#v", reply)
	}
	if before, reply := commandIn(t, ada, "general", "/invite @ada"); errorCode(reply) != 0 || len(before) != 1 || !strings.Contains(noticeText(t, before[0]), "already a member") {
		t.Fatalf("/invite @ada = %#v after %#v", reply, before)
	}

	// The guest is invited: its you, then a notice; those in the Lobby see
	// its role; the inviter a notice before the result.
	before, reply = commandIn(t, ada, "general", "/invite @"+guest.userID)
	if errorCode(reply) != 0 || len(before) != 2 {
		t.Fatalf("/invite guest = %#v after %#v", reply, before)
	}
	if user := notificationParams(t, before[0], "user")["new"].(map[string]any); !reflect.DeepEqual(rolesOf(user), []any{"invited"}) || permissionsOf(user) != nil {
		t.Fatalf("others see the invited guest as %#v", user)
	}
	if text := noticeText(t, before[1]); !strings.Contains(text, "is invited") {
		t.Fatalf("the inviter's notice %q", text)
	}
	you := notificationParams(t, guest.read(t), "user")["you"].(map[string]any)
	if !reflect.DeepEqual(permissionsOf(you), map[string]any{"lobby": "invited"}) || !reflect.DeepEqual(rolesOf(you), []any{"invited"}) {
		t.Fatalf("the invited guest's you: %#v", you)
	}
	if text := noticeText(t, guest.read(t)); !strings.HasPrefix(text, "@ada invited you. Add a way to sign in to finish joining") {
		t.Fatalf("the invited guest's notice %q", text)
	}
	if app.Waiting(guest.userID) {
		t.Fatal("an invited guest is waiting")
	}
	// It is still limited to the Lobby.
	guest.expectError(t, "history", "general", map[string]any{"room_id": "general"}, CodeInvalidParams)

	// It adds an address: a member, which joins general after the result.
	propose(t, guest, "gus@example.com")
	_, result := approve(t, guest, mailbox.receive(t).Code, nil)
	if you := result["you"].(map[string]any); you["user_id"] != guest.userID || permissionsOf(you) != nil || !reflect.DeepEqual(rolesOf(you), []any{}) {
		t.Fatalf("the invited guest signed up as %#v", you)
	}
	expectJoined(t, guest, "general", guest.userID)
	historyPage(t, guest, "general", map[string]any{})
	if app.Waiting(guest.userID) {
		t.Fatal("the member is waiting")
	}

	// A guest that signs up uninvited waits; inviting it lets it in at once.
	other, _ := lobbyGuest(t, httpServer)
	propose(t, other, "otto@example.com")
	_, result = approve(t, other, mailbox.receive(t).Code, nil)
	if you := result["you"].(map[string]any); !reflect.DeepEqual(permissionsOf(you), map[string]any{"lobby": "waiting"}) || !reflect.DeepEqual(rolesOf(you), []any{"waiting"}) {
		t.Fatalf("an uninvited guest signed up as %#v", you)
	}
	if !app.Waiting(other.userID) {
		t.Fatal("the waiting account is not waiting")
	}
	other.drain(t)
	ada.drain(t)
	before, reply = commandIn(t, ada, "general", "/invite "+other.userID)
	if errorCode(reply) != 0 || !strings.Contains(noticeText(t, before[len(before)-1]), "is in") {
		t.Fatalf("/invite waiting = %#v after %#v", reply, before)
	}
	frame := other.read(t)
	you = notificationParams(t, frame, "user")["you"].(map[string]any)
	if ext, _ := you["ext"].(map[string]any); !reflect.DeepEqual(ext["permissions"], map[string]any{}) || !reflect.DeepEqual(rolesOf(you), []any{}) {
		t.Fatalf("the admitted account's you: %#v", you)
	}
	expectJoined(t, other, "general", other.userID)
	if text := noticeText(t, other.read(t)); text != "@ada invited you in." {
		t.Fatalf("the admitted account's notice %q", text)
	}
	if app.Waiting(other.userID) {
		t.Fatal("the admitted account still waits")
	}
}

// An invited guest that leaves before signing up is gone, and so is its
// invitation.
func TestLobbyInvitedGuestLeaves(t *testing.T) {
	_, mailbox, httpServer := lobbyServer(t)
	mo, _ := emailSignUp(t, httpServer, mailbox, "mo@example.com", "mo", "general")
	guest, _ := lobbyGuest(t, httpServer)
	if _, reply := commandIn(t, mo, "general", "/invite "+guest.userID); errorCode(reply) != 0 {
		t.Fatalf("a moderator's /invite = %#v", reply)
	}
	_ = guest.ws.Close(websocket.StatusNormalClosure, "bye")
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, reply := commandIn(t, mo, "general", "/invite "+guest.userID)
		if errorCode(reply) == CodeInvalidParams {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("/invite of a departed guest = %#v", reply)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A new guest is no one invited.
	_, result := lobbyGuest(t, httpServer)
	if permissions := permissionsOf(result["you"].(map[string]any)); !reflect.DeepEqual(permissions, map[string]any{"lobby": "waiting"}) {
		t.Fatalf("a new guest's permissions %#v", permissions)
	}
}

// A passkey registration on a new connection makes a waiting account, and
// an invited guest's passkey a member.
func TestLobbyPasskeySignUp(t *testing.T) {
	config := DefaultConfig()
	config.WebAuthn = testWebAuthn(t)
	lobbyConfigure(&config)
	app, httpServer := newTestServer(t, config)

	fresh, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	options := passkeyResult(t, passkeyCall(t, fresh, "begin", "register", "begin", map[string]any{"name": "Fay", "user_id": "fay"}))
	result := passkeyResult(t, passkeyCall(t, fresh, "finish", "register", "finish", map[string]any{"credential": newTestAuthenticator(t).registration(t, options, testPasskeyOrigin)}))
	if you := result["you"].(map[string]any); you["user_id"] != "fay" || !reflect.DeepEqual(rolesOf(you), []any{"waiting"}) {
		t.Fatalf("a fresh registration: %#v", you)
	}
	expectJoined(t, fresh, "lobby", "fay")
	if !app.Waiting("fay") {
		t.Fatal("a fresh registration does not wait")
	}

	guest, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	_, auth := guest.request(t, "auth", "guest", map[string]any{"scheme": "guest"})
	guest.userID = auth["you"].(map[string]any)["user_id"].(string)
	expectJoined(t, guest, "lobby", guest.userID)
	if err := app.Admit(context.Background(), guest.userID); err != nil {
		t.Fatal(err)
	}
	guest.drain(t)
	options = passkeyResult(t, passkeyCall(t, guest, "begin2", "register", "begin", nil))
	result = passkeyResult(t, passkeyCall(t, guest, "finish2", "register", "finish", map[string]any{"credential": newTestAuthenticator(t).registration(t, options, testPasskeyOrigin)}))
	if you := result["you"].(map[string]any); you["user_id"] != guest.userID || permissionsOf(you) != nil || !reflect.DeepEqual(rolesOf(you), []any{}) {
		t.Fatalf("an invited guest's passkey: %#v", you)
	}
	expectJoined(t, guest, "general", guest.userID)
	if err := app.Admit(context.Background(), "nobody"); err == nil {
		t.Fatal("Admit of nobody succeeded")
	}
}

// A waiting account resumes as waiting, with its session token or a device
// token, and stays so across a restart until it is invited.
func TestLobbyWaitingAccountResumes(t *testing.T) {
	memory := store.NewMemory()
	mailbox := make(testMailbox, 16)
	configure := func(config *Config) {
		lobbyConfigure(config)
		config.EmailSender = mailbox
		config.WebAuthn = nil
	}
	_, httpServer, stop := startWith(t, memory, t.TempDir(), configure)
	wes, signedIn := emailSignUp(t, httpServer, mailbox, "wes@example.com", "wes", "lobby")
	before, reply := commandIn(t, wes, "lobby", "/invite-device phone")
	if errorCode(reply) != 0 || len(before) != 1 {
		t.Fatalf("/invite-device = %#v after %#v", reply, before)
	}
	text := noticeText(t, before[0])
	device := text[strings.Index(text, "apron_device_") : strings.Index(text, "apron_device_")+len("apron_device_")+43]
	stop()

	app, httpServer, _ := startWith(t, memory, t.TempDir(), configure)
	for _, token := range []any{signedIn["token"], device} {
		c, _ := dialRaw(t, httpServer)
		you := c.result(t, "auth", "resume", map[string]any{"scheme": "token", "token": token})["you"].(map[string]any)
		if you["user_id"] != "wes" || !reflect.DeepEqual(permissionsOf(you), map[string]any{"lobby": "waiting"}) || !reflect.DeepEqual(rolesOf(you), []any{"waiting"}) {
			t.Fatalf("resumed as %#v", you)
		}
		c.expectError(t, "history", "general", map[string]any{"room_id": "general"}, CodeInvalidParams)
		if joined := roomIDs(t, listRooms(t, c, map[string]any{})["joined"]); !reflect.DeepEqual(joined, []string{"lobby"}) {
			t.Fatalf("joined %v", joined)
		}
	}
	if !app.Waiting("wes") {
		t.Fatal("not waiting after a restart")
	}
}

// /toggle lobby closes the Lobby to newcomers: its guests go, with 1008,
// new connections are offered no guests or sign-ups, and waiting accounts
// stay. It stays closed across a restart, and opens again.
func TestLobbyToggle(t *testing.T) {
	memory := store.NewMemory()
	mailbox := make(testMailbox, 16)
	configure := func(config *Config) {
		lobbyConfigure(config)
		config.EmailSender = mailbox
		config.WebAuthn = nil
	}
	app, httpServer, stop := startWith(t, memory, t.TempDir(), configure)
	ada, _ := emailSignUp(t, httpServer, mailbox, "ada@example.com", "ada", "general")
	_, waiting := emailSignUp(t, httpServer, mailbox, "wes@example.com", "wes", "lobby")
	guest, _ := lobbyGuest(t, httpServer)
	if before, reply := commandIn(t, ada, "general", "/toggle"); errorCode(reply) != 0 || !strings.Contains(noticeText(t, before[len(before)-1]), "`lobby`: open") {
		t.Fatalf("/toggle = %#v after %#v", reply, before)
	}
	ada.drain(t)

	before, reply := commandIn(t, ada, "general", "/toggle lobby")
	if errorCode(reply) != 0 || len(before) != 2 {
		t.Fatalf("/toggle lobby = %#v after %#v", reply, before)
	}
	if settings := notificationParams(t, before[0], "server")["ext"].(map[string]any)["settings"].(map[string]any); !reflect.DeepEqual(settings["lobby"], map[string]any{"room_id": "lobby", "open": false}) {
		t.Fatalf("the admin's server frame: %#v", settings)
	}
	if text := noticeText(t, before[1]); !strings.HasPrefix(text, "The Public Lobby is closed") {
		t.Fatalf("the admin's notice %q", text)
	}
	if text := noticeText(t, guest.read(t)); text != "The Public Lobby is closed" {
		t.Fatalf("the guest's notice %q", text)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, _, err := guest.ws.Read(ctx)
	cancel()
	if status := websocket.CloseStatus(err); status != websocket.StatusPolicyViolation {
		t.Fatalf("the guest's connection ended with %v, want 1008", err)
	}
	if app.LobbyOpen() {
		t.Fatal("LobbyOpen after closing it")
	}

	checkClosed := func(httpServer *httptest.Server) {
		t.Helper()
		newcomer, frame := dialRaw(t, httpServer)
		params := frame["params"].(map[string]any)
		if !reflect.DeepEqual(params["auth"], []any{"email", "token"}) || !reflect.DeepEqual(params["signup"], []any{}) || params["ext"] != nil {
			t.Fatalf("a newcomer's server frame while closed: %#v", params)
		}
		newcomer.expectError(t, "auth", "guest", map[string]any{"scheme": "guest"}, CodeUnsupported)
		// A new address is mailed no code: nobody new may sign up.
		propose(t, newcomer, "new@example.com")
		select {
		case message := <-mailbox:
			t.Fatalf("a code was sent to %s", message.To)
		case <-time.After(100 * time.Millisecond):
		}
		// A waiting account still signs in, to the Lobby.
		c, _ := dialRaw(t, httpServer)
		you := c.result(t, "auth", "resume", map[string]any{"scheme": "token", "token": waiting["token"]})["you"].(map[string]any)
		if !reflect.DeepEqual(permissionsOf(you), map[string]any{"lobby": "waiting"}) {
			t.Fatalf("a waiting account while closed: %#v", you)
		}
		frame = notificationParams(t, c.read(t), "server")
		if lobby := frame["ext"].(map[string]any)["settings"].(map[string]any)["lobby"]; !reflect.DeepEqual(lobby, map[string]any{"room_id": "lobby", "open": false}) {
			t.Fatalf("a waiting account's server frame %#v", frame)
		}
		historyPage(t, c, "lobby", map[string]any{})
	}
	checkClosed(httpServer)
	stop()

	app, httpServer, _ = startWith(t, memory, t.TempDir(), configure)
	checkClosed(httpServer)
	ada, _ = dialRaw(t, httpServer)
	ageSends(app, "ada@example.com", time.Minute)
	propose(t, ada, "ada@example.com")
	signInByEmail(t, ada, mailbox.receive(t).Code, nil)
	ada.drain(t)
	if before, reply := commandIn(t, ada, "general", "/toggle lobby"); errorCode(reply) != 0 || !strings.HasPrefix(noticeText(t, before[len(before)-1]), "The Public Lobby is open again") {
		t.Fatalf("/toggle lobby = %#v after %#v", reply, before)
	}
	lobbyGuest(t, httpServer)
	if !app.LobbyOpen() {
		t.Fatal("not open")
	}
}

// Switching modes: existing accounts are members in lobby mode, the waiting
// marker is ignored but kept in public mode, and waiting accounts may not
// sign in in private mode.
func TestAccessModeSwitches(t *testing.T) {
	memory := store.NewMemory()
	mailbox := make(testMailbox, 16)
	start := func(mode AccessMode) (*Server, *httptest.Server, func()) {
		return startWith(t, memory, t.TempDir(), func(config *Config) {
			config.Access = Access{Mode: mode}
			config.EmailSender = mailbox
			config.WebAuthn = nil
		})
	}
	resume := func(httpServer *httptest.Server, token any) (*testClient, map[string]any) {
		t.Helper()
		c, _ := dialRaw(t, httpServer)
		frame := c.call(t, "auth", "resume", map[string]any{"scheme": "token", "token": token})
		if result, ok := frame["result"].(map[string]any); ok {
			c.userID = result["you"].(map[string]any)["user_id"].(string)
		}
		return c, frame
	}

	_, httpServer, stop := start(AccessPublic)
	_, bob := emailSignUp(t, httpServer, mailbox, "bob@example.com", "bob", "general")
	stop()

	// Public to lobby: bob is a member, and the Lobby is made.
	app, httpServer, stop := start(AccessLobby)
	c, frame := resume(httpServer, bob["token"])
	if you := frame["result"].(map[string]any)["you"].(map[string]any); permissionsOf(you) != nil || !reflect.DeepEqual(rolesOf(you), []any{}) || app.Waiting("bob") {
		t.Fatalf("bob in lobby mode: %#v", you)
	}
	historyPage(t, c, "general", map[string]any{})
	historyPage(t, c, "lobby", map[string]any{})
	_, wes := emailSignUp(t, httpServer, mailbox, "wes@example.com", "wes", "lobby")
	stop()

	// Lobby to public: wes signs in as a member, and may join general.
	app, httpServer, stop = start(AccessPublic)
	c, frame = resume(httpServer, wes["token"])
	if you := frame["result"].(map[string]any)["you"].(map[string]any); permissionsOf(you) != nil || !reflect.DeepEqual(rolesOf(you), []any{}) || app.Waiting("wes") {
		t.Fatalf("wes in public mode: %#v", you)
	}
	joinRoom(t, c, "general")
	if lobby := listRooms(t, c, map[string]any{"room_id": "lobby"}); len(roomIDs(t, lobby["joined"])) != 1 {
		t.Fatalf("the Lobby in public mode: %#v", lobby)
	}
	_, server := dialRaw(t, httpServer)
	if ext, _ := server["params"].(map[string]any)["ext"].(map[string]any); ext["settings"] != nil {
		t.Fatalf("a public server frame %#v", server)
	}
	stop()

	// Back to lobby: wes waits again, taken out of general.
	app, httpServer, stop = start(AccessLobby)
	c, frame = resume(httpServer, wes["token"])
	if you := frame["result"].(map[string]any)["you"].(map[string]any); !reflect.DeepEqual(rolesOf(you), []any{"waiting"}) || !app.Waiting("wes") {
		t.Fatalf("wes back in lobby mode: %#v", you)
	}
	if joined := roomIDs(t, listRooms(t, c, map[string]any{})["joined"]); !reflect.DeepEqual(joined, []string{"lobby"}) {
		t.Fatalf("wes joined %v", joined)
	}
	stop()

	// Lobby to private: wes may not sign in; bob may.
	_, httpServer, stop = start(AccessPrivate)
	if _, frame = resume(httpServer, wes["token"]); errorCode(frame) != CodeDenied {
		t.Fatalf("wes in private mode: %#v", frame)
	}
	c, _ = dialRaw(t, httpServer)
	propose(t, c, "wes@example.com")
	select {
	case message := <-mailbox:
		t.Fatalf("a code was sent to %s", message.To)
	case <-time.After(100 * time.Millisecond):
	}
	if _, frame = resume(httpServer, bob["token"]); errorCode(frame) != 0 {
		t.Fatalf("bob in private mode: %#v", frame)
	}
	stop()

	// Private to lobby: wes signs in again, waiting.
	app, httpServer, _ = start(AccessLobby)
	ageSends(app, "wes@example.com", time.Minute)
	c, _ = dialRaw(t, httpServer)
	propose(t, c, "wes@example.com")
	if _, result := signInByEmail(t, c, mailbox.receive(t).Code, nil); !reflect.DeepEqual(rolesOf(result["you"].(map[string]any)), []any{"waiting"}) {
		t.Fatalf("wes after private mode: %#v", result)
	}
}

// Private mode is what DisableGuests was.
func TestPrivateMode(t *testing.T) {
	for _, configure := range []func(*Config){
		func(config *Config) { config.Access.Mode = AccessPrivate },
		func(config *Config) { config.DisableGuests = true },
	} {
		_, _, httpServer := emailTestServer(t, configure)
		c, frame := dialRaw(t, httpServer)
		if params := frame["params"].(map[string]any); !reflect.DeepEqual(params["auth"], []any{"email", "token"}) || params["description"] != nil {
			t.Fatalf("private server frame %#v", params)
		}
		c.expectError(t, "auth", "guest", map[string]any{"scheme": "guest"}, CodeUnsupported)
	}
}

func TestAccessConfiguration(t *testing.T) {
	for name, access := range map[string]Access{
		"unknown mode": {Mode: "secret"},
		"lobby room":   {Mode: AccessLobby, LobbyRoom: "two words"},
	} {
		config := DefaultConfig()
		config.Access = access
		if _, err := Open(config); err == nil {
			t.Errorf("%s: Open succeeded", name)
		}
	}
	config := DefaultConfig()
	config.Access.Mode = AccessLobby
	config.DisableGuests = true
	if _, err := Open(config); err == nil {
		t.Error("Open took DisableGuests in lobby mode")
	}
	config = DefaultConfig()
	lobbyConfigure(&config)
	config.Roles = map[string][]string{"waiting": {"ada"}}
	if _, err := Open(config); err == nil {
		t.Error("Open granted the waiting role")
	}

	// A room that exists becomes the Lobby as it is, but not a private one.
	memory := store.NewMemory()
	_, httpServer, stop := startWith(t, memory, t.TempDir(), func(*Config) {})
	c := dialTestClient(t, httpServer)
	private, _ := saveRoom(t, c, "private", map[string]any{"private": true, "title": "Secret"})
	stop()
	app, _, stop := startWith(t, memory, t.TempDir(), func(config *Config) { config.Access = Access{Mode: AccessLobby, LobbyRoom: "general"} })
	app.mu.RLock()
	title := app.rooms["general"].title()
	app.mu.RUnlock()
	if title != "General" {
		t.Fatalf("the adopted Lobby is titled %q", title)
	}
	stop()
	config = DefaultConfig()
	config.Store = memory
	config.Access = Access{Mode: AccessLobby, LobbyRoom: private}
	if _, err := Open(config); err == nil {
		t.Error("a private room became the Lobby")
	}
}

// In lobby mode only the Lobby and its threads are public to extensions,
// while the Lobby is open.
func TestLobbyPublicRooms(t *testing.T) {
	var host *Host
	config := extensionConfig(testExtension{"web", func(r *Registrar) error {
		host = r.Host
		return nil
	}})
	lobbyConfigure(&config)
	mailbox := make(testMailbox, 16)
	config.EmailSender = mailbox
	app, httpServer := newTestServer(t, config)
	ada, _ := emailSignUp(t, httpServer, mailbox, "ada@example.com", "ada", "general")
	thread, _ := saveRoom(t, ada, "thread", map[string]any{"parent_room_id": "lobby", "description": "Intros"})
	if !host.Public("lobby") || !host.Public(thread) || host.Public("general") {
		t.Fatal("Public disagrees with the Lobby")
	}
	if guest, ok := host.User("ada"); !ok || host.Waiting(guest.ID) {
		t.Fatal("the admin waits")
	}
	app.mu.Lock()
	app.setLobbyClosedLocked(true)
	app.unlock()
	if host.Public("lobby") {
		t.Fatal("a closed Lobby is public")
	}
	if err := host.Admit(context.Background(), "nobody"); err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("Admit(nobody) = %v", err)
	}
}
