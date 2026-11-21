package apron

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/apron-chat/apron-server-go/internal/oauthtest"
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

// expectAdmitted reads the room_update that tells a restricted user made a
// member of its rooms: general in joined, with its membership, and in
// updated the other top-level rooms it sees now and has not joined.
func expectAdmitted(t *testing.T, c *testClient, updated []string) {
	t.Helper()
	update := notificationParams(t, c.read(t), "room_update")
	joined, _ := update["joined"].([]any)
	if len(joined) != 1 || joined[0].(map[string]any)["room_id"] != "general" || !slices.Contains(memberIDs(joined[0].(map[string]any)), c.userID) || update["users"] == nil {
		t.Fatalf("room_update %#v, want general joined", update)
	}
	checkMembership(t, update["memberships"].([]any)[0].(map[string]any), "general", c.userID, true)
	if got := roomIDs(t, update["updated"]); !reflect.DeepEqual(got, updated) {
		t.Fatalf("updated %v, want %v", got, updated)
	}
}

// permissionsOf is a user object's ext.permissions, which this server
// never sends.
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
	// Lobby mode changes nothing in the protocol: no ext:permissions or
	// ext:settings.
	if ext, _ := params["ext"].(map[string]any); slices.Contains(capabilities, any("ext:permissions")) || slices.Contains(capabilities, any("ext:settings")) || ext["settings"] != nil {
		t.Fatalf("capabilities %v, ext %#v", capabilities, params["ext"])
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
	if you := guestAuth["you"].(map[string]any); permissionsOf(you) != nil || you["roles"] != nil {
		t.Fatalf("a guest's you: %#v", you)
	}
	waiting, waitingAuth := emailSignUp(t, httpServer, mailbox, "wes@example.com", "wes", "lobby")
	if you := waitingAuth["you"].(map[string]any); permissionsOf(you) != nil || !reflect.DeepEqual(rolesOf(you), []any{"waiting"}) {
		t.Fatalf("a waiting account's you: %#v", you)
	}
	if !app.Waiting("wes") || !app.Waiting(guest.userID) || app.Waiting("ada") {
		t.Fatal("Waiting disagrees")
	}
	ada.drain(t)
	// The admin is not limited, and has no ext.permissions.
	if you := ada.result(t, "me", "me", map[string]any{})["you"].(map[string]any); permissionsOf(you) != nil {
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
			"reply":           {"message", map[string]any{"room_id": "lobby", "reply_to": map[string]any{"message_id": inGeneral}, "body": map[string]any{"text": "hi"}}},
			"react":           {"reactions", map[string]any{"message_id": inGeneral, "emojis": []any{"👍"}}},
			"command":         {"command", map[string]any{"room_id": "general", "body": map[string]any{"text": "/help"}}},
			"lobby edit":      {"room_set", map[string]any{"room_id": "lobby", "title": "Mine"}},
			"lobby thread":    {"room_set", map[string]any{"parent_room_id": "lobby"}},
			"new room":        {"room_set", map[string]any{"title": "Mine"}},
			"new private":     {"room_set", map[string]any{"private": true}},
			"add someone":     {"room_join", map[string]any{"room_id": "lobby", "user_id": "ada"}},
			"emoji":           {"command", map[string]any{"room_id": "lobby", "body": map[string]any{"text": "/emoji add :wave:", "embeds": []any{map[string]any{"kind": "upload", "content_type": "image/png", "size": 10}}}}},
			"welcome":         {"command", map[string]any{"room_id": "lobby", "body": map[string]any{"text": "/welcome ada"}}},
			"lobby":           {"command", map[string]any{"room_id": "lobby", "body": map[string]any{"text": "/lobby close"}}},
			"read elsewhere":  {"activity", map[string]any{"room_id": "lobby", "read_message_id": inGeneral}},
			"history in room": {"history", map[string]any{"room_id": "general", "after": inGeneral, "before": inGeneral}},
		} {
			want := CodeInvalidParams
			switch name {
			case "lobby edit", "lobby thread", "new room", "new private", "add someone", "emoji", "welcome", "lobby":
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

// /welcome lets a guest in once it signs up, and a waiting account at once.
func TestLobbyInvite(t *testing.T) {
	app, mailbox, httpServer := lobbyServer(t)
	ada, _ := emailSignUp(t, httpServer, mailbox, "ada@example.com", "ada", "general")
	joinRoom(t, ada, "lobby")
	// Rooms a member sees: other, but not the private room, nor general's
	// thread, which room_list lists only by its parent.
	other, _ := saveRoom(t, ada, "other", map[string]any{"title": "Other"})
	saveRoom(t, ada, "private", map[string]any{"title": "Secret", "private": true})
	saveRoom(t, ada, "thread", map[string]any{"parent_room_id": "general", "description": "Side talk"})
	ada.drain(t)
	guest, _ := lobbyGuest(t, httpServer)
	expectMembership(t, ada, "lobby", guest.userID, true)

	// /help lists /welcome and /lobby to admins and moderators only, and
	// there is no /toggle.
	before, _ := commandIn(t, ada, "general", "/help")
	if text := noticeText(t, before[0]); !strings.Contains(text, "`/welcome <user_id>`") || !strings.Contains(text, "`/lobby [open|close]`") || strings.Contains(text, "/toggle") {
		t.Fatalf("an admin's /help: %q", text)
	}
	guest.drain(t)
	before, _ = commandIn(t, guest, "lobby", "/help")
	if text := noticeText(t, before[0]); strings.Contains(text, "/welcome ") || strings.Contains(text, "/lobby") {
		t.Fatalf("a guest's /help: %q", text)
	}

	// Unknown users, and members, change nothing.
	_, reply := commandIn(t, ada, "general", "/welcome nobody")
	if errorCode(reply) != CodeInvalidParams {
		t.Fatalf("/welcome nobody = %#v", reply)
	}
	if _, reply := commandIn(t, ada, "general", "/welcome"); errorCode(reply) != CodeInvalidParams {
		t.Fatalf("/welcome = %#v", reply)
	}
	if before, reply := commandIn(t, ada, "general", "/welcome @ada"); errorCode(reply) != 0 || len(before) != 1 || !strings.Contains(noticeText(t, before[0]), "already a member") {
		t.Fatalf("/welcome @ada = %#v after %#v", reply, before)
	}

	// The guest is welcomed: its you, then a notice; those in the Lobby see
	// its role; the welcomer a notice before the result.
	before, reply = commandIn(t, ada, "general", "/welcome @"+guest.userID)
	if errorCode(reply) != 0 || len(before) != 2 {
		t.Fatalf("/welcome guest = %#v after %#v", reply, before)
	}
	if user := notificationParams(t, before[0], "user")["new"].(map[string]any); !reflect.DeepEqual(rolesOf(user), []any{"welcomed"}) || permissionsOf(user) != nil {
		t.Fatalf("others see the welcomed guest as %#v", user)
	}
	if text := noticeText(t, before[1]); !strings.Contains(text, "is welcomed") {
		t.Fatalf("the welcomer's notice %q", text)
	}
	you := notificationParams(t, guest.read(t), "user")["you"].(map[string]any)
	if permissionsOf(you) != nil || !reflect.DeepEqual(rolesOf(you), []any{"welcomed"}) {
		t.Fatalf("the welcomed guest's you: %#v", you)
	}
	if text := noticeText(t, guest.read(t)); !strings.HasPrefix(text, "@ada welcomed you. Add a way to sign in to finish joining") {
		t.Fatalf("the welcomed guest's notice %q", text)
	}
	if app.Waiting(guest.userID) {
		t.Fatal("a welcomed guest is waiting")
	}
	// It is still limited to the Lobby.
	guest.expectError(t, "history", "general", map[string]any{"room_id": "general"}, CodeInvalidParams)

	// It adds an address: a member, a separate account with a user_id of
	// its own (§3.3.3), which joins general after the result; who shares
	// the Lobby with it sees the guest leave and the account join.
	ada.drain(t)
	propose(t, guest, "gus@example.com")
	_, result := approve(t, guest, mailbox.receive(t).Code, nil)
	if you := result["you"].(map[string]any); you["user_id"] == guest.userID || strings.HasPrefix(you["user_id"].(string), "guest_") || permissionsOf(you) != nil || !reflect.DeepEqual(rolesOf(you), []any{}) {
		t.Fatalf("the welcomed guest signed up as %#v", you)
	}
	guest.guestID, guest.userID = guest.userID, result["you"].(map[string]any)["user_id"].(string)
	// The guest leaves, and its account, a separate user, joins the Lobby,
	// then general, with the other rooms it sees now.
	expectSwitched(t, ada, guest, "lobby")
	expectJoined(t, guest, "lobby", guest.userID)
	expectAdmitted(t, guest, []string{other})
	historyPage(t, guest, "general", map[string]any{})
	if app.Waiting(guest.userID) {
		t.Fatal("the member is waiting")
	}

	// A guest that signs up unwelcomed waits; welcoming it lets it in at once.
	otto, _ := lobbyGuest(t, httpServer)
	propose(t, otto, "otto@example.com")
	_, result = approve(t, otto, mailbox.receive(t).Code, nil)
	if you := result["you"].(map[string]any); you["user_id"] == otto.userID || permissionsOf(you) != nil || !reflect.DeepEqual(rolesOf(you), []any{"waiting"}) {
		t.Fatalf("an unwelcomed guest signed up as %#v", you)
	}
	ottoGuest := otto.userID
	otto.userID = result["you"].(map[string]any)["user_id"].(string)
	if !app.Waiting(otto.userID) {
		t.Fatal("the waiting account is not waiting")
	}
	otto.drain(t)
	ada.drain(t)
	before, reply = commandIn(t, ada, "general", "/welcome "+otto.userID)
	if errorCode(reply) != 0 || !strings.Contains(noticeText(t, before[len(before)-1]), "is in") {
		t.Fatalf("/welcome waiting = %#v after %#v", reply, before)
	}
	frame := otto.read(t)
	you = notificationParams(t, frame, "user")["you"].(map[string]any)
	if permissionsOf(you) != nil || !reflect.DeepEqual(rolesOf(you), []any{}) {
		t.Fatalf("the admitted account's you: %#v", you)
	}
	expectAdmitted(t, otto, []string{other})
	if text := noticeText(t, otto.read(t)); text != "@ada welcomed you in." {
		t.Fatalf("the admitted account's notice %q", text)
	}
	if app.Waiting(otto.userID) {
		t.Fatal("the admitted account still waits")
	}
	// The guest's user_id is the guest's for good.
	if _, reply := commandIn(t, ada, "general", "/welcome "+ottoGuest); errorCode(reply) != CodeInvalidParams {
		t.Fatalf("/welcome of the guest otto was = %#v", reply)
	}
}

// /welcome takes its user as typed with an autocompleted @, or from the
// message it replies to.
func TestLobbyWelcomeByReply(t *testing.T) {
	_, mailbox, httpServer := lobbyServer(t)
	ada, _ := emailSignUp(t, httpServer, mailbox, "ada@example.com", "ada", "general")
	joinRoom(t, ada, "lobby")
	guest, _ := lobbyGuest(t, httpServer)
	ada.drain(t)
	guest.drain(t)

	// The guest introduces itself; an admin replies to it with /welcome.
	_, posted := guest.request(t, "message", guest.nextID("intro"), map[string]any{"room_id": "lobby", "body": map[string]any{"text": "Hi, I'm Gus"}})
	intro, _ := posted["message_id"].(string)
	if intro == "" {
		t.Fatalf("the introduction = %#v", posted)
	}
	ada.drain(t)
	id := ada.nextID("command")
	before, _ := ada.request(t, "command", id, map[string]any{"room_id": "lobby", "reply_to": map[string]any{"message_id": intro}, "body": map[string]any{"text": "/welcome"}})
	if len(before) == 0 || !strings.Contains(noticeText(t, before[len(before)-1]), "@"+guest.userID+" is welcomed") {
		t.Fatalf("/welcome in reply: %#v", before)
	}
	// Typed with the @ autocompletion leaves, and a trailing space: the same user, already welcomed.
	if before, reply := commandIn(t, ada, "lobby", "/welcome @"+guest.userID+" "); errorCode(reply) != 0 || !strings.Contains(noticeText(t, before[len(before)-1]), "welcomed already") {
		t.Fatalf("/welcome @user = %#v after %#v", reply, before)
	}
	// A mention whose text is a display name of several words: the mention.
	before, _ = ada.request(t, "command", ada.nextID("command"), map[string]any{"room_id": "lobby", "body": map[string]any{"text": "/welcome @Guest Gus", "mentions": []any{guest.userID}}})
	if !strings.Contains(noticeText(t, before[len(before)-1]), "@"+guest.userID+" is welcomed already") {
		t.Fatalf("/welcome with a mention: %#v", before)
	}
	// Several words and no mention: no one.
	if _, reply := commandIn(t, ada, "lobby", "/welcome Guest Gus"); errorCode(reply) != CodeInvalidParams {
		t.Fatalf("/welcome of several words = %#v", reply)
	}
	// A reply to one's own message names oneself, a member already.
	_, own := ada.request(t, "message", ada.nextID("hello"), map[string]any{"room_id": "lobby", "body": map[string]any{"text": "Welcome, all"}})
	mine := own["message_id"].(string)
	ada.drain(t)
	before, _ = ada.request(t, "command", ada.nextID("command"), map[string]any{"room_id": "lobby", "reply_to": map[string]any{"message_id": mine}, "body": map[string]any{"text": "/welcome"}})
	if !strings.Contains(noticeText(t, before[len(before)-1]), "already a member") {
		t.Fatalf("/welcome in reply to oneself: %#v", before)
	}
}

// A welcomed guest that leaves before signing up is gone, and so is its
// invitation.
func TestLobbyInvitedGuestLeaves(t *testing.T) {
	_, mailbox, httpServer := lobbyServer(t)
	mo, _ := emailSignUp(t, httpServer, mailbox, "mo@example.com", "mo", "general")
	guest, _ := lobbyGuest(t, httpServer)
	if _, reply := commandIn(t, mo, "general", "/welcome "+guest.userID); errorCode(reply) != 0 {
		t.Fatalf("a moderator's /welcome = %#v", reply)
	}
	_ = guest.ws.Close(websocket.StatusNormalClosure, "bye")
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, reply := commandIn(t, mo, "general", "/welcome "+guest.userID)
		if errorCode(reply) == CodeInvalidParams {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("/welcome of a departed guest = %#v", reply)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A new guest is no one welcomed.
	_, result := lobbyGuest(t, httpServer)
	if you := result["you"].(map[string]any); you["roles"] != nil {
		t.Fatalf("a new guest is %#v", you)
	}
}

// A passkey registration on a new connection makes a waiting account, and
// a welcomed guest's passkey a member.
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
	if you := result["you"].(map[string]any); you["user_id"] == guest.userID || permissionsOf(you) != nil || !reflect.DeepEqual(rolesOf(you), []any{}) {
		t.Fatalf("a welcomed guest's passkey: %#v", you)
	}
	guest.userID = result["you"].(map[string]any)["user_id"].(string)
	// The account joins the Lobby, the guest's room, then general.
	expectJoined(t, guest, "lobby", guest.userID)
	expectJoined(t, guest, "general", guest.userID)
	if err := app.Admit(context.Background(), "nobody"); err == nil {
		t.Fatal("Admit of nobody succeeded")
	}
}

// A waiting account resumes as waiting, with its session token or a device
// token, and stays so across a restart until it is welcomed.
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
		if you["user_id"] != "wes" || permissionsOf(you) != nil || !reflect.DeepEqual(rolesOf(you), []any{"waiting"}) {
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

// /lobby close closes the Lobby to newcomers: its guests go, with 1008,
// new connections are offered no guests or sign-ups, and waiting accounts
// stay. It stays closed across a restart, and opens again.
func TestLobbyOpenAndClose(t *testing.T) {
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
	if before, reply := commandIn(t, ada, "general", "/lobby"); errorCode(reply) != 0 || !strings.HasPrefix(noticeText(t, before[len(before)-1]), "The Public Lobby is open;") {
		t.Fatalf("/lobby = %#v after %#v", reply, before)
	}
	if _, reply := commandIn(t, ada, "general", "/toggle lobby"); errorCode(reply) != CodeInvalidParams {
		t.Fatalf("/toggle lobby = %#v", reply)
	}
	ada.drain(t)

	if before, reply := commandIn(t, ada, "general", "/lobby open"); errorCode(reply) != 0 || noticeText(t, before[0]) != "The Public Lobby is open already." {
		t.Fatalf("/lobby open = %#v after %#v", reply, before)
	}
	for _, line := range []string{"/lobby shut", "/lobby lobby", "/lobby open now"} {
		if _, reply := commandIn(t, ada, "general", line); errorCode(reply) != CodeInvalidParams {
			t.Fatalf("%s = %#v", line, reply)
		}
	}
	before, reply := commandIn(t, ada, "general", "/lobby close")
	if errorCode(reply) != 0 || len(before) != 2 {
		t.Fatalf("/lobby close = %#v after %#v", reply, before)
	}
	if params := notificationParams(t, before[0], "server"); slices.Contains(params["auth"].([]any), any("guest")) {
		t.Fatalf("the admin's server frame: %#v", params)
	}
	if text := noticeText(t, before[1]); !strings.HasPrefix(text, "The Public Lobby is closed") {
		t.Fatalf("the admin's notice %q", text)
	}
	if text := noticeText(t, guest.read(t)); text != "The Public Lobby is closed" {
		t.Fatalf("the guest's notice %q", text)
	}
	// Then the error about the connection as a whole (§1.1), without id.
	if frame := guest.read(t); !reflect.DeepEqual(frame, map[string]any{"error": map[string]any{"code": float64(CodeDenied), "message": "The Public Lobby is closed. Ask for an invite link."}}) {
		t.Fatalf("the guest's error %#v", frame)
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
		if permissionsOf(you) != nil {
			t.Fatalf("a waiting account while closed: %#v", you)
		}
		// The whole server frame follows the sign-in.
		notificationParams(t, c.read(t), "server")
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
	if before, reply := commandIn(t, ada, "general", "/lobby close"); errorCode(reply) != 0 || noticeText(t, before[0]) != "The Public Lobby is closed already." {
		t.Fatalf("/lobby close when closed = %#v after %#v", reply, before)
	}
	if before, reply := commandIn(t, ada, "general", "/lobby"); errorCode(reply) != 0 || !strings.HasPrefix(noticeText(t, before[0]), "The Public Lobby is closed;") {
		t.Fatalf("/lobby when closed = %#v after %#v", reply, before)
	}
	if before, reply := commandIn(t, ada, "general", "/lobby open"); errorCode(reply) != 0 || !strings.HasPrefix(noticeText(t, before[len(before)-1]), "The Public Lobby is open again") {
		t.Fatalf("/lobby open = %#v after %#v", reply, before)
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

// Private mode, like DisableGuests, offers no guests.
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

// In private mode nobody makes an account by themselves: a passkey
// registration, an email address no account has, and an external identity
// no account has are refused, unless an invitation comes with the
// registration. Existing accounts sign in by every scheme, and add to
// themselves.
func TestPrivateModeIsInviteOnly(t *testing.T) {
	memory := store.NewMemory()
	mailbox := make(testMailbox, 16)
	identity := &Identity{Subject: "idp:1", Name: "Ext", UserID: "ext"}
	start := func(mode AccessMode) (*Server, *httptest.Server, func()) {
		return startWith(t, memory, t.TempDir(), func(config *Config) {
			config.Access.Mode = mode
			config.EmailSender = mailbox
			config.EmailLinkURL = "https://chat.example/login"
			config.Extensions = []Extension{ssoExtension(identity)}
		})
	}
	external := func(httpServer *httptest.Server) (*testClient, map[string]any) {
		t.Helper()
		c, _ := dialRaw(t, httpServer)
		page := c.result(t, "auth", "begin", map[string]any{"scheme": "external"})["external"].(map[string]any)["url"].(string)
		app := oauthtest.New(t, httpServer.URL)
		screen := app.Get(app.URL(page, nil))
		if screen.Status != http.StatusOK {
			return c, map[string]any{"status": screen.Status, "body": screen.Body}
		}
		return c, c.call(t, "auth", "finish", redemption(app, app.Code(app.Press(screen, "allow"), httpServer.URL)))
	}

	// Public mode: an account by email, by passkey, and by external sign-in.
	_, httpServer, stop := start(AccessPublic)
	emailSignUp(t, httpServer, mailbox, "eve@example.com", "eve", "general")
	pat, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	passkey := newTestAuthenticator(t)
	options := passkeyResult(t, passkeyCall(t, pat, "begin", "register", "begin", map[string]any{"user_id": "pat"}))
	passkeyResult(t, passkeyCall(t, pat, "finish", "register", "finish", map[string]any{"credential": passkey.registration(t, options, testPasskeyOrigin)}))
	if _, frame := external(httpServer); errorCode(frame) != 0 || frame["result"] == nil {
		t.Fatalf("external sign-up in public mode: %#v", frame)
	}
	stop()

	app, httpServer, _ := start(AccessPrivate)
	newcomer, frame := dialOrigin(t, httpServer, testPasskeyOrigin)
	// signup lists nothing: only an invitation makes an account, which
	// webauthn in auth carries.
	if params := frame["params"].(map[string]any); !reflect.DeepEqual(params["auth"], []any{"webauthn", "email", "external", "token"}) || !reflect.DeepEqual(params["signup"], []any{}) {
		t.Fatalf("auth %v, signup %v", params["auth"], params["signup"])
	}

	// A passkey registration without an invitation is refused.
	refusal := passkeyCall(t, newcomer, "begin", "register", "begin", map[string]any{"name": "New"})
	if message := refusal["error"].(map[string]any)["message"]; errorCode(refusal) != CodeDenied || message != "This server is invite-only; ask for an invite link" {
		t.Fatalf("a registration without an invitation: %#v", refusal)
	}
	// An address no account has is mailed no code.
	propose(t, newcomer, "new@example.com")
	select {
	case message := <-mailbox:
		t.Fatalf("a code was sent to %s", message.To)
	case <-time.After(100 * time.Millisecond):
	}
	// An external identity no account has is refused on the page.
	identity.Subject, identity.UserID = "idp:2", "other"
	if _, refused := external(httpServer); refused["status"] != http.StatusForbidden {
		t.Fatalf("a new external identity: %#v", refused)
	}

	// An invitation makes an account.
	invitation, err := app.Invite(context.Background(), InvitationOptions{Name: "Ivy"})
	if err != nil {
		t.Fatal(err)
	}
	invited, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	if you := passkeyResult(t, registerInvited(t, invited, newTestAuthenticator(t), invitation.Token, nil))["you"].(map[string]any); you["name"] != "Ivy" {
		t.Fatalf("an invited registration: %#v", you)
	}

	// Existing accounts sign in by every scheme.
	identity.Subject, identity.UserID = "idp:1", "ext"
	if _, signedIn := external(httpServer); errorCode(signedIn) != 0 || signedIn["result"].(map[string]any)["you"].(map[string]any)["user_id"] != "ext" {
		t.Fatalf("an existing external identity: %#v", signedIn)
	}
	login, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	options = passkeyResult(t, passkeyCall(t, login, "begin", "login", "begin", nil))
	if you := passkeyResult(t, passkeyCall(t, login, "finish", "login", "finish", map[string]any{"credential": passkey.assertion(t, options, testPasskeyOrigin, "localhost", 0x05)}))["you"].(map[string]any); you["user_id"] != "pat" {
		t.Fatalf("a passkey login: %#v", you)
	}
	eve, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	ageSends(app, "eve@example.com", time.Minute)
	propose(t, eve, "eve@example.com")
	signInByEmail(t, eve, mailbox.receive(t).Code, nil)
	if eve.userID != "eve" {
		t.Fatalf("an email sign-in as %s", eve.userID)
	}
	eve.drain(t)
	// And add to themselves: eve's first passkey.
	options = passkeyResult(t, passkeyCall(t, eve, "add-begin", "register", "begin", nil))
	if you := passkeyResult(t, passkeyCall(t, eve, "add-finish", "register", "finish", map[string]any{"credential": newTestAuthenticator(t).registration(t, options, testPasskeyOrigin)}))["you"].(map[string]any); you["user_id"] != "eve" {
		t.Fatalf("eve's passkey: %#v", you)
	}
}

// An allowlist is the operator's own choice: in private mode an address it
// names may still make its account, and signup lists email.
func TestPrivateModeWithAnAllowlist(t *testing.T) {
	_, mailbox, httpServer := emailTestServer(t, func(config *Config) {
		config.Access.Mode = AccessPrivate
		config.Extensions = []Extension{testExtension{"private", func(r *Registrar) error {
			p, err := ParsePrincipal("ann@example.com")
			if err != nil {
				return err
			}
			return r.RestrictSignIn().Allow(p)
		}}}
	})
	ann, frame := dialRaw(t, httpServer)
	if params := frame["params"].(map[string]any); !reflect.DeepEqual(params["signup"], []any{"email"}) {
		t.Fatalf("signup %v", params["signup"])
	}
	propose(t, ann, "ann@example.com")
	signInByEmail(t, ann, mailbox.receive(t).Code, map[string]any{"user_id": "ann"})
	// The whole server frame follows the sign-in, then the join.
	notificationParams(t, ann.read(t), "server")
	expectJoined(t, ann, "general", "ann")
	bob, _ := dialRaw(t, httpServer)
	propose(t, bob, "bob@example.com")
	select {
	case message := <-mailbox:
		t.Fatalf("a code was sent to %s", message.To)
	case <-time.After(100 * time.Millisecond):
	}
}

// /welcome in private mode admits a waiting account left from lobby mode,
// which then signs in as a member.
func TestPrivateModeWelcome(t *testing.T) {
	memory := store.NewMemory()
	mailbox := make(testMailbox, 16)
	start := func(mode AccessMode) (*Server, *httptest.Server, func()) {
		return startWith(t, memory, t.TempDir(), func(config *Config) {
			lobbyConfigure(config)
			config.Access.Mode = mode
			config.EmailSender = mailbox
			config.WebAuthn = nil
		})
	}
	_, httpServer, stop := start(AccessLobby)
	emailSignUp(t, httpServer, mailbox, "wes@example.com", "wes", "lobby")
	stop()

	app, httpServer, _ := start(AccessPrivate)
	if !app.Waiting("wes") {
		t.Fatal("wes does not wait in private mode")
	}
	c, _ := dialRaw(t, httpServer)
	propose(t, c, "wes@example.com")
	select {
	case message := <-mailbox:
		t.Fatalf("a code was sent to the waiting account's %s", message.To)
	case <-time.After(100 * time.Millisecond):
	}
	// An identity a role is granted to, by the operator, makes its account.
	ada, _ := dialRaw(t, httpServer)
	propose(t, ada, "ada@example.com")
	signInByEmail(t, ada, mailbox.receive(t).Code, map[string]any{"user_id": "ada"})
	ada.drain(t)
	before, _ := commandIn(t, ada, "general", "/help")
	if text := noticeText(t, before[0]); !strings.Contains(text, "`/welcome <user_id>`") || strings.Contains(text, "/lobby") || strings.Contains(text, "/toggle") {
		t.Fatalf("/help in private mode: %q", text)
	}
	if before, reply := commandIn(t, ada, "general", "/welcome @ada"); errorCode(reply) != 0 || !strings.Contains(noticeText(t, before[0]), "already a member") {
		t.Fatalf("/welcome @ada = %#v after %#v", reply, before)
	}
	if _, reply := commandIn(t, ada, "general", "/welcome nobody"); errorCode(reply) != CodeInvalidParams {
		t.Fatalf("/welcome nobody = %#v", reply)
	}
	if before, reply := commandIn(t, ada, "general", "/welcome wes"); errorCode(reply) != 0 || !strings.Contains(noticeText(t, before[len(before)-1]), "is in") {
		t.Fatalf("/welcome wes = %#v after %#v", reply, before)
	}
	if app.Waiting("wes") {
		t.Fatal("wes still waits")
	}
	ageSends(app, "wes@example.com", time.Minute)
	wes, _ := dialRaw(t, httpServer)
	propose(t, wes, "wes@example.com")
	_, result := signInByEmail(t, wes, mailbox.receive(t).Code, nil)
	if you := result["you"].(map[string]any); you["user_id"] != "wes" || !reflect.DeepEqual(rolesOf(you), []any{}) {
		t.Fatalf("wes signed in as %#v", you)
	}
	wes.drain(t)
	historyPage(t, wes, "general", map[string]any{})
	if joined := roomIDs(t, listRooms(t, wes, map[string]any{})["joined"]); !slices.Contains(joined, "general") {
		t.Fatalf("wes joined %v", joined)
	}
}

// /welcome's target: the one mention, whatever the text; else the one word
// typed; else the author of the message it replies to. Several mentions,
// or no target, are invalid_params, and a guest that left is named so.
func TestLobbyWelcomeTarget(t *testing.T) {
	_, mailbox, httpServer := lobbyServer(t)
	ada, _ := emailSignUp(t, httpServer, mailbox, "ada@example.com", "ada", "general")
	joinRoom(t, ada, "lobby")
	first, _ := lobbyGuest(t, httpServer)
	second, _ := lobbyGuest(t, httpServer)
	third, _ := lobbyGuest(t, httpServer)
	first.drain(t)
	_, posted := first.request(t, "message", first.nextID("intro"), map[string]any{"room_id": "lobby", "body": map[string]any{"text": "Hi"}})
	intro := posted["message_id"].(string)
	ada.drain(t)
	welcome := func(params map[string]any) (string, int) {
		t.Helper()
		params["room_id"] = "lobby"
		id := ada.nextID("welcome")
		ada.write(t, map[string]any{"method": "command", "id": id, "params": params})
		var notices []string
		for {
			frame := ada.read(t)
			if frame["id"] == id {
				if code := errorCode(frame); code != 0 {
					return frame["error"].(map[string]any)["message"].(string), code
				}
				return notices[len(notices)-1], 0
			}
			if params, _ := frame["params"].(map[string]any); frame["method"] == "message" && params["from"].(map[string]any)["user_id"] == privateNoticeID {
				notices = append(notices, noticeText(t, frame))
			}
		}
	}
	// The mention wins over the typed word.
	if text, code := welcome(map[string]any{"body": map[string]any{"text": "/welcome @" + second.userID, "mentions": []any{first.userID}}}); code != 0 || !strings.Contains(text, "@"+first.userID+" is welcomed") {
		t.Fatalf("mention over the typed word: %q %d", text, code)
	}
	// The typed word wins over the reply.
	if text, code := welcome(map[string]any{"reply_to": map[string]any{"message_id": intro}, "body": map[string]any{"text": "/welcome " + second.userID}}); code != 0 || !strings.Contains(text, "@"+second.userID+" is welcomed") {
		t.Fatalf("typed word over the reply: %q %d", text, code)
	}
	// The reply's author, without either.
	if text, code := welcome(map[string]any{"reply_to": map[string]any{"message_id": intro}, "body": map[string]any{"text": "/welcome"}}); code != 0 || !strings.Contains(text, "@"+first.userID+" is welcomed already") {
		t.Fatalf("reply: %q %d", text, code)
	}
	// Several mentions, with or without a reply, and nothing at all.
	for name, params := range map[string]map[string]any{
		"mentions":            {"body": map[string]any{"text": "/welcome @a @b", "mentions": []any{first.userID, third.userID}}},
		"mentions with reply": {"reply_to": map[string]any{"message_id": intro}, "body": map[string]any{"text": "/welcome", "mentions": []any{first.userID, third.userID}}},
		"nothing":             {"body": map[string]any{"text": "/welcome"}},
	} {
		if text, code := welcome(params); code != CodeInvalidParams {
			t.Errorf("%s: %q %d", name, text, code)
		}
	}
	// A guest that left is named as one.
	_ = third.ws.Close(websocket.StatusNormalClosure, "bye")
	deadline := time.Now().Add(5 * time.Second)
	for {
		text, code := welcome(map[string]any{"body": map[string]any{"text": "/welcome " + third.userID}})
		if code == CodeInvalidParams {
			if text != "No guest "+third.userID+" is connected; send them an invite link instead" {
				t.Fatalf("a departed guest: %q", text)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("/welcome of a departed guest: %q %d", text, code)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if text, code := welcome(map[string]any{"body": map[string]any{"text": "/welcome nobody"}}); code != CodeInvalidParams || text != `Unknown user "nobody"` {
		t.Fatalf("nobody: %q %d", text, code)
	}
}

// The Lobby's edge is like a private room's: nothing logged in the Lobby
// names a room outside it. A move either way, and a reply in the Lobby to a
// message outside it, are denied; a reply outside it to a Lobby message is
// not, since members see the Lobby.
func TestLobbyEdge(t *testing.T) {
	_, mailbox, httpServer := lobbyServer(t)
	ada, _ := emailSignUp(t, httpServer, mailbox, "ada@example.com", "ada", "general")
	joinRoom(t, ada, "lobby")
	thread, _ := saveRoom(t, ada, "thread", map[string]any{"parent_room_id": "lobby", "description": "Intros"})
	inLobby, _ := save(t, ada, "in-lobby", map[string]any{"room_id": "lobby", "body": map[string]any{"text": "lobby"}})
	inGeneral, _ := save(t, ada, "in-general", map[string]any{"room_id": "general", "body": map[string]any{"text": "general"}})
	ada.drain(t)
	for name, params := range map[string]map[string]any{
		"move out":           {"message_id": inLobby, "room_id": "general", "body": map[string]any{"text": "lobby"}},
		"move in":            {"message_id": inGeneral, "room_id": "lobby", "body": map[string]any{"text": "general"}},
		"move to its thread": {"message_id": inGeneral, "room_id": thread, "body": map[string]any{"text": "general"}},
		"reply":              {"room_id": "lobby", "reply_to": map[string]any{"message_id": inGeneral}, "body": map[string]any{"text": "re"}},
		"reply in thread":    {"room_id": thread, "reply_to": map[string]any{"message_id": inGeneral}, "body": map[string]any{"text": "re"}},
	} {
		if frame := ada.call(t, "message", ada.nextID(name), params); errorCode(frame) != CodeDenied {
			t.Errorf("%s = %#v", name, frame)
		}
	}
	if frame := ada.call(t, "command", ada.nextID("append"), map[string]any{"room_id": "lobby", "reply_to": map[string]any{"message_id": inGeneral}, "body": map[string]any{"text": "/embed-append hi"}}); errorCode(frame) != CodeDenied {
		t.Errorf("/embed-append reply = %#v", frame)
	}
	// Within the Lobby, and replies from outside into it, as before.
	if _, result := ada.request(t, "message", ada.nextID("into thread"), map[string]any{"message_id": inLobby, "room_id": thread, "body": map[string]any{"text": "lobby"}}); result["message_id"] != inLobby {
		t.Fatalf("a move within the Lobby: %#v", result)
	}
	if _, result := ada.request(t, "message", ada.nextID("reply out"), map[string]any{"room_id": "general", "reply_to": map[string]any{"message_id": inLobby}, "body": map[string]any{"text": "re"}}); result["message_id"] == nil {
		t.Fatalf("a reply in general to the Lobby: %#v", result)
	}
}

// In private mode the operator's word lets in a waiting account left from
// lobby mode: a --role grant or an allowlist entry naming it clears its
// mark, and it signs in as a member, in general.
func TestPrivateModeAdmitsNamedWaitingAccounts(t *testing.T) {
	for name, configure := range map[string]func(*Config){
		"role": func(config *Config) {
			config.Roles = map[string][]string{"staff": {"wes@example.com"}}
		},
		"allowlist": func(config *Config) {
			config.Extensions = []Extension{testExtension{"private", func(r *Registrar) error {
				p, err := ParsePrincipal("wes@example.com")
				if err != nil {
					return err
				}
				return r.RestrictSignIn().Allow(p)
			}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			memory := store.NewMemory()
			mailbox := make(testMailbox, 16)
			_, httpServer, stop := startWith(t, memory, t.TempDir(), func(config *Config) {
				config.Access.Mode = AccessLobby
				config.EmailSender = mailbox
				config.WebAuthn = nil
			})
			emailSignUp(t, httpServer, mailbox, "wes@example.com", "wes", "lobby")
			stop()

			app, httpServer, _ := startWith(t, memory, t.TempDir(), func(config *Config) {
				config.Access.Mode = AccessPrivate
				config.EmailSender = mailbox
				config.WebAuthn = nil
				configure(config)
			})
			if app.Waiting("wes") {
				t.Fatal("wes still waits")
			}
			ageSends(app, "wes@example.com", time.Minute)
			wes, _ := dialRaw(t, httpServer)
			propose(t, wes, "wes@example.com")
			_, result := signInByEmail(t, wes, mailbox.receive(t).Code, nil)
			if you := result["you"].(map[string]any); you["user_id"] != "wes" || slices.Contains(rolesOf(you), any("waiting")) {
				t.Fatalf("wes signed in as %#v", you)
			}
			wes.drain(t)
			if joined := roomIDs(t, listRooms(t, wes, map[string]any{})["joined"]); !slices.Contains(joined, "general") {
				t.Fatalf("wes joined %v", joined)
			}
		})
	}
}

// A waiting account is never in no room: one the Lobby's move, or its own
// leave while the server was public, took out of every room joins the
// Lobby at the next start in lobby mode.
func TestLobbyWaitingAccountsAreNeverRoomless(t *testing.T) {
	memory := store.NewMemory()
	mailbox := make(testMailbox, 16)
	start := func(mode AccessMode, lobby string) (*Server, *httptest.Server, func()) {
		return startWith(t, memory, t.TempDir(), func(config *Config) {
			config.Access = Access{Mode: mode, LobbyRoom: lobby}
			config.EmailSender = mailbox
			config.WebAuthn = nil
		})
	}
	joinedBy := func(httpServer *httptest.Server, token any) []string {
		t.Helper()
		c, _ := dialRaw(t, httpServer)
		c.result(t, "auth", "resume", map[string]any{"scheme": "token", "token": token})
		c.drain(t)
		return roomIDs(t, listRooms(t, c, map[string]any{})["joined"])
	}
	_, httpServer, stop := start(AccessLobby, "lobby")
	_, wes := emailSignUp(t, httpServer, mailbox, "wes@example.com", "wes", "lobby")
	stop()

	// The Lobby is another room now.
	_, httpServer, stop = start(AccessLobby, "hall")
	if joined := joinedBy(httpServer, wes["token"]); !reflect.DeepEqual(joined, []string{"hall"}) {
		t.Fatalf("after the Lobby moved, wes joined %v", joined)
	}
	stop()

	// It leaves the Lobby while the server is public.
	_, httpServer, stop = start(AccessPublic, "")
	c, _ := dialRaw(t, httpServer)
	c.result(t, "auth", "resume", map[string]any{"scheme": "token", "token": wes["token"]})
	c.userID = "wes"
	c.drain(t)
	leaveRoom(t, c, "hall")
	stop()
	_, httpServer, _ = start(AccessLobby, "hall")
	if joined := joinedBy(httpServer, wes["token"]); !reflect.DeepEqual(joined, []string{"hall"}) {
		t.Fatalf("after leaving the Lobby, wes joined %v", joined)
	}
}

// A waiting account that was a member while the server was public keeps
// none of a member's powers once back in lobby mode: the rooms it made
// and joined are gone from its sight, so it kicks, edits, and posts in
// none, and it welcomes, opens or closes the Lobby, and touches custom emoji
// no more.
func TestLobbyAfterAPublicSpell(t *testing.T) {
	memory := store.NewMemory()
	mailbox := make(testMailbox, 16)
	start := func(mode AccessMode) (*Server, *httptest.Server, func()) {
		return startWith(t, memory, t.TempDir(), func(config *Config) {
			config.Access.Mode = mode
			config.EmailSender = mailbox
			config.WebAuthn = nil
		})
	}
	resume := func(httpServer *httptest.Server, token any) *testClient {
		t.Helper()
		c, _ := dialRaw(t, httpServer)
		c.result(t, "auth", "resume", map[string]any{"scheme": "token", "token": token})
		c.userID = "wes"
		c.drain(t)
		return c
	}
	_, httpServer, stop := start(AccessLobby)
	_, wes := emailSignUp(t, httpServer, mailbox, "wes@example.com", "wes", "lobby")
	stop()

	_, httpServer, stop = start(AccessPublic)
	c := resume(httpServer, wes["token"])
	mine, _ := saveRoom(t, c, "mine", map[string]any{"title": "Wes's"})
	joinRoom(t, c, "general")
	stop()

	app, httpServer, _ := start(AccessLobby)
	c = resume(httpServer, wes["token"])
	if joined := roomIDs(t, listRooms(t, c, map[string]any{})["joined"]); !reflect.DeepEqual(joined, []string{"lobby"}) {
		t.Fatalf("wes joined %v", joined)
	}
	for name, request := range map[string]struct {
		method string
		params map[string]any
		code   int
	}{
		"history of its room": {"history", map[string]any{"room_id": mine}, CodeInvalidParams},
		"edit its room":       {"room_set", map[string]any{"room_id": mine, "title": "Mine"}, CodeInvalidParams},
		"kick in its room":    {"command", map[string]any{"room_id": mine, "body": map[string]any{"text": "/kick @ada", "mentions": []any{"ada"}}}, CodeInvalidParams},
		"post in general":     {"message", map[string]any{"room_id": "general", "body": map[string]any{"text": "hi"}}, CodeInvalidParams},
		"welcome":             {"command", map[string]any{"room_id": "lobby", "body": map[string]any{"text": "/welcome wes"}}, CodeDenied},
		"lobby":               {"command", map[string]any{"room_id": "lobby", "body": map[string]any{"text": "/lobby close"}}, CodeDenied},
		"emoji remove":        {"command", map[string]any{"room_id": "lobby", "body": map[string]any{"text": "/emoji remove :wave:"}}, CodeDenied},
		"new room":            {"room_set", map[string]any{"title": "Another"}, CodeDenied},
	} {
		if frame := c.call(t, request.method, c.nextID(name), request.params); errorCode(frame) != request.code {
			t.Errorf("%s = %#v, want %d", name, frame, request.code)
		}
	}
	if !app.Waiting("wes") {
		t.Fatal("wes does not wait")
	}
}

// waiting and welcomed are the server's: no invitation or identity grants
// them as stored roles.
func TestServerRolesAreNotGranted(t *testing.T) {
	app, httpServer := externalTestServer(t, &Identity{Subject: "idp:1", Name: "Ext", UserID: "ext", Roles: []string{"Welcomed", "staff", "waiting"}})
	if _, err := app.Invite(context.Background(), InvitationOptions{Roles: []string{"Waiting"}}); err == nil {
		t.Fatal("an invitation granted waiting")
	}
	c, _ := dialRaw(t, httpServer)
	page := c.result(t, "auth", "begin", map[string]any{"scheme": "external"})["external"].(map[string]any)["url"].(string)
	client := oauthtest.New(t, httpServer.URL)
	result := c.result(t, "auth", "finish", redemption(client, externalCode(t, client, page, httpServer.URL)))
	if roles := rolesOf(result["you"].(map[string]any)); !reflect.DeepEqual(roles, []any{"staff"}) {
		t.Fatalf("roles %v", roles)
	}
}

// A passkey registration without an invitation, on a server without
// guests, is refused in words that fit why.
func TestPasskeySignUpRefusals(t *testing.T) {
	for want, configure := range map[string]func(*Config){
		"This server is invite-only; ask for an invite link":                             func(c *Config) { c.Access.Mode = AccessPrivate },
		"The Public Lobby is closed; ask for an invite link":                             func(c *Config) { c.Access.Mode = AccessLobby },
		"Make an account another way, or present an invitation, before adding a passkey": func(c *Config) { c.DisableGuests = true },
		"Only those this server lets in may make an account; ask for an invite link": func(c *Config) {
			c.Extensions = []Extension{testExtension{"private", func(r *Registrar) error {
				r.RestrictSignIn()
				return nil
			}}}
		},
	} {
		config := DefaultConfig()
		config.WebAuthn = testWebAuthn(t)
		configure(&config)
		app, httpServer := newTestServer(t, config)
		if config.Access.Mode == AccessLobby {
			app.mu.Lock()
			app.setLobbyClosedLocked(true)
			app.unlock()
		}
		c, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
		refusal := passkeyCall(t, c, "begin", "register", "begin", nil)
		if message := refusal["error"].(map[string]any)["message"]; errorCode(refusal) != CodeDenied || message != want {
			t.Errorf("refusal %#v, want %q", refusal, want)
		}
	}
}

// A guest that signs up, by any scheme, makes an account: a separate user
// with a user_id of its own, the one it asked for, else user_<n>,
// decoupled from the guest's history (§3.3.3). The guest departs, as one
// that signs in to an existing account does, and the account joins the
// rooms the guest was in as itself; no `user` carries `old`. What the
// guest posted stays the guest's, and its user_id is never given out again.
func TestGuestSignUpMakesASeparateAccount(t *testing.T) {
	identity := &Identity{Subject: "idp:1", Name: "Ext", UserID: "extuser"}
	config := extensionConfig(ssoExtension(identity))
	config.WebAuthn = testWebAuthn(t)
	mailbox := make(testMailbox, 16)
	config.EmailSender = mailbox
	app, httpServer := newTestServer(t, config)
	watcher := dialTestClient(t, httpServer)

	signUp := map[string]func(c *testClient) map[string]any{
		"passkey": func(c *testClient) map[string]any {
			options := passkeyResult(t, passkeyCall(t, c, "begin", "register", "begin", map[string]any{"user_id": "pat"}))
			return passkeyResult(t, passkeyCall(t, c, "finish", "register", "finish", map[string]any{"credential": newTestAuthenticator(t).registration(t, options, testPasskeyOrigin)}))
		},
		"email": func(c *testClient) map[string]any {
			propose(t, c, "eve@example.com")
			_, result := approve(t, c, mailbox.receive(t).Code, nil)
			return result
		},
		"external": func(c *testClient) map[string]any {
			client := oauthtest.New(t, httpServer.URL)
			_, result := c.request(t, "auth", "external", redemption(client, externalCode(t, client, httpServer.URL+"/oauth/authorize", httpServer.URL)))
			return result
		},
	}
	want := map[string]func(string) bool{
		"passkey":  func(id string) bool { return id == "pat" },
		"email":    func(id string) bool { return strings.HasPrefix(id, "user_") },
		"external": func(id string) bool { return id == "extuser" },
	}
	for _, scheme := range []string{"passkey", "email", "external"} {
		c, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
		guestAuth(t, c)
		c.request(t, "me", c.nextID("name"), map[string]any{"name": "Gus"})
		expectMembership(t, watcher, "general", c.userID, true)
		// The guest posts, and makes a private room with the watcher in it.
		room, _ := saveRoom(t, c, c.nextID("room"), map[string]any{"title": "Mine", "private": true})
		c.request(t, "room_join", c.nextID("add"), map[string]any{"room_id": room, "user_id": watcher.userID})
		message, _ := save(t, c, c.nextID("hello"), map[string]any{"room_id": room, "body": map[string]any{"text": "hello"}})
		c.drain(t)
		watcher.drain(t)

		result := signUp[scheme](c)
		you := result["you"].(map[string]any)
		id, _ := you["user_id"].(string)
		if !want[scheme](id) || you["name"] != "Gus" {
			t.Fatalf("%s: the account is %#v", scheme, you)
		}
		c.guestID, c.userID = c.userID, id
		// The watcher sees the guest leave and the account join, and no
		// `old`.
		var left, joined []string
		for _, frame := range watcher.drain(t) {
			params, _ := frame["params"].(map[string]any)
			if _, has := params["old"]; has {
				t.Fatalf("%s: a user_id change was announced: %#v", scheme, frame)
			}
			for _, record := range asList(params["memberships"]) {
				for _, member := range asList(record.(map[string]any)["members"]) {
					entry := member.(map[string]any)
					who := entry["user"].(map[string]any)["user_id"].(string)
					switch {
					case who == c.guestID && entry["joined"] == false:
						left = append(left, record.(map[string]any)["room_id"].(string))
					case who == id && entry["joined"] == true:
						joined = append(joined, record.(map[string]any)["room_id"].(string))
					}
				}
			}
		}
		if !reflect.DeepEqual(left, []string{"general", room}) || !reflect.DeepEqual(joined, []string{"general", room}) {
			t.Fatalf("%s: the guest left %v, the account joined %v", scheme, left, joined)
		}
		// The guest's id is retired for good, and its message is its own:
		// the account may not edit it.
		app.mu.RLock()
		_, live := app.users[c.guestID]
		used := app.usedIDs[strings.ToLower(c.guestID)]
		owner := app.messages[message].owner
		app.mu.RUnlock()
		if live || !used || owner != c.guestID {
			t.Fatalf("%s: the guest's id is live %v, used %v; the message is %s's", scheme, live, used, owner)
		}
		c.drain(t)
		c.expectError(t, "message", c.nextID("edit"), map[string]any{"message_id": message, "room_id": room, "body": map[string]any{"text": "mine?"}}, CodeDenied)
		// The account is in the guest's rooms, as itself.
		list := listRooms(t, c, map[string]any{"room_id": room, "members": true})
		if members := memberIDs(list["joined"].([]any)[0].(map[string]any)); !slices.Contains(members, id) || slices.Contains(members, c.guestID) {
			t.Fatalf("%s: the room's members %v", scheme, members)
		}
		watcher.drain(t)
	}
}
