package apron

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// federated is a test server with trusted federation, and an admin bot
// whose session hears its command notices.
type federated struct {
	app     *Server
	http    *httptest.Server
	self    string
	admin   *Bot
	notices chan string
}

func startFederated(t *testing.T) *federated {
	t.Helper()
	fed := &federated{notices: make(chan string, 64)}
	var handler http.Handler
	fed.http = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	fed.self = "ws://" + fed.http.Listener.Addr().String() + "/ws"
	config := extensionConfig(testExtension{"tadmin", func(r *Registrar) error {
		var err error
		fed.admin, err = r.Bot("admin", BotOptions{Name: "Admin"})
		return err
	}})
	config.Roles = map[string][]string{RoleAdmin: {"admin"}}
	config.Remote = RemoteConfig{Self: fed.self, Linger: 300 * time.Millisecond}
	app, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	fed.app, handler = app, app.Handler()
	fed.http.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := app.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		fed.http.Close()
	})
	return fed
}

// command runs a command as the admin and returns its notices.
func (fed *federated) command(t *testing.T, roomID, line string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The admin's own connection runs the command; its notices go to that
	// connection, so read them from a session that runs it.
	session, err := fed.admin.Connect(SessionOptions{OnFrame: func(frame jsontext.Value) {
		var note struct {
			Method string `json:"method"`
			Params struct {
				From struct {
					UserID string `json:"user_id"`
				} `json:"from"`
				Body struct {
					Text string `json:"text"`
				} `json:"body"`
			} `json:"params"`
		}
		if json.Unmarshal(frame, &note) == nil && note.Method == "message" && note.Params.From.UserID == privateNoticeID {
			fed.notices <- note.Params.Body.Text
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.Call(ctx, "command", map[string]any{"room_id": roomID, "body": map[string]any{"text": line}}); err != nil {
		t.Fatalf("%s: %v", line, err)
	}
	select {
	case text := <-fed.notices:
		return text
	case <-ctx.Done():
		t.Fatalf("%s: no notice", line)
	}
	return ""
}

func (fed *federated) call(t *testing.T, method string, params map[string]any) map[string]any {
	t.Helper()
	return callAs(t, fed.admin, method, params)
}

// waitCopy waits for a message in a room whose ext.remote.message_id is
// remoteID, and returns it.
func (fed *federated) waitCopy(t *testing.T, roomID, remoteID string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		page := fed.call(t, "history", map[string]any{"room_id": roomID, "limit": 500})
		for _, value := range asList(page["messages"]) {
			m := value.(map[string]any)
			if remote := remoteOfJSON(t, m); remote != nil && remote["message_id"] == remoteID {
				return m
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no copy of %s in %s", remoteID, roomID)
	return nil
}

var inviteInNotice = regexp.MustCompile("`(https://link\\.apron\\.chat/#[^`]+)`")

func TestFederationSharesRooms(t *testing.T) {
	a, b := startFederated(t), startFederated(t)
	roomA := a.call(t, "room_set", map[string]any{"title": "Ops"})["room_id"].(string)
	roomB := b.call(t, "room_set", map[string]any{"title": "Ops B", "private": true})["room_id"].(string)

	notice := a.command(t, roomA, "/remote invite "+b.self)
	invite := inviteInNotice.FindStringSubmatch(notice)
	if invite == nil {
		t.Fatalf("invite notice = %q", notice)
	}
	if notice := b.command(t, roomB, "/remote connect "+invite[1]); !strings.HasPrefix(notice, "Connected") {
		t.Fatalf("connect notice = %q", notice)
	}
	authorityA := strings.TrimSuffix(strings.TrimPrefix(a.self, "ws://"), "/ws")
	authorityB := strings.TrimSuffix(strings.TrimPrefix(b.self, "ws://"), "/ws")

	alice := dialTestClient(t, a.http)
	joinRoom(t, alice, roomA)
	hello, _ := save(t, alice, "m1", map[string]any{"room_id": roomA, "body": map[string]any{"text": "hello"}})
	copyOnB := b.waitCopy(t, roomB, hello)
	if from := copyOnB["from"].(map[string]any); from["user_id"] != alice.userID+"@"+authorityA {
		t.Fatalf("copy on B from %#v", from)
	}
	// B's user answers the copy, mentioning Alice's remote user there.
	bob := b.call(t, "message", map[string]any{"room_id": roomB, "reply_to": map[string]any{"message_id": copyOnB["message_id"]},
		"body": map[string]any{"text": "@" + alice.userID + "@" + authorityA + " hi", "mentions": []any{alice.userID + "@" + authorityA}}})
	copyOnA := a.waitCopy(t, roomA, bob["message_id"].(string))
	if wstestText(copyOnA) != "@"+alice.userID+" hi" || replyOfJSON(copyOnA) != hello || copyOnA["from"].(map[string]any)["user_id"] != "admin@"+authorityB {
		t.Fatalf("copy on A = %#v", copyOnA)
	}
	// A's edit reaches B's copy; the duplicate create of a resend does not
	// make another.
	alice.request(t, "message", "m2", map[string]any{"room_id": roomA, "message_id": hello, "body": map[string]any{"text": "hello (edited)"}})
	deadline := time.Now().Add(20 * time.Second)
	for wstestText(lastSnapshot(t, b.app, copyOnB["message_id"].(string))) != "hello (edited)" {
		if time.Now().After(deadline) {
			t.Fatal("the edit did not reach B")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A rename reaches B without a record after it.
	alice.request(t, "me", "n1", map[string]any{"name": "Alice R"})
	for {
		b.app.mu.RLock()
		name := b.app.users[alice.userID+"@"+authorityA].name
		b.app.mu.RUnlock()
		if name == "Alice R" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the rename did not reach B: %q", name)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A thread of the shared room is shared with it: created there through
	// the link, then its messages and title both ways.
	_, created := alice.request(t, "room_set", "t1", map[string]any{"parent_room_id": roomA, "title": "Deploy"})
	threadA := created["room_id"].(string)
	var threadB string
	for threadB == "" {
		b.app.mu.RLock()
		for id, r := range b.app.rooms {
			if r.parent != nil && r.parent.id == roomB && strings.Contains(string(encodeJSON(r.record["ext"])), threadA) {
				threadB = id
			}
		}
		b.app.mu.RUnlock()
		if time.Now().After(deadline) {
			t.Fatal("the thread did not reach B")
		}
		time.Sleep(20 * time.Millisecond)
	}
	inThread, _ := save(t, alice, "t2", map[string]any{"room_id": threadA, "body": map[string]any{"text": "in the thread"}})
	if copied := b.waitCopy(t, threadB, inThread); copied["room_id"] != threadB {
		t.Fatalf("thread message on B = %#v", copied)
	}
	fromB := b.call(t, "message", map[string]any{"room_id": threadB, "body": map[string]any{"text": "thread reply from B"}})
	a.waitCopy(t, threadA, fromB["message_id"].(string))
	alice.request(t, "room_set", "t3", map[string]any{"room_id": threadA, "title": "Deploy (renamed)"})
	for {
		b.app.mu.RLock()
		title := b.app.rooms[threadB].title()
		b.app.mu.RUnlock()
		if title == "Deploy (renamed)" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the title did not reach B: %q", title)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// B unshares: A hears of it, ends its pair, and its link for B leaves.
	if notice := b.command(t, roomB, "/remote unshare"); !strings.Contains(notice, "no longer shared") {
		t.Fatalf("unshare = %q", notice)
	}
	for {
		joined, _ := a.remoteHostMember(roomA, b.self)
		if list := a.command(t, roomA, "/remote list"); !joined && !strings.Contains(list, roomA+" with room") && !strings.Contains(list, threadA+" with room") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("A did not end the pair")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// remoteHostMember reports whether the link for a server is in a room.
func (fed *federated) remoteHostMember(roomID, server string) (bool, bool) {
	fed.app.mu.RLock()
	link := fed.app.links[server]
	fed.app.mu.RUnlock()
	return fed.admin.host.Member(roomID, link.id)
}

func replyOfJSON(m map[string]any) string {
	ref, _ := m["reply_to"].(map[string]any)
	id, _ := ref["message_id"].(string)
	return id
}

func TestLinkRequests(t *testing.T) {
	a := startFederated(t)
	const other = "ws://other.test:9000/ws"
	room := a.call(t, "room_set", map[string]any{"title": "Ops"})["room_id"].(string)
	elsewhere := a.call(t, "room_set", map[string]any{"title": "Elsewhere"})["room_id"].(string)
	invite := inviteInNotice.FindStringSubmatch(a.command(t, room, "/remote invite "+other))
	_, _, token, err := parseInvite(invite[1])
	if err != nil {
		t.Fatal(err)
	}

	// The link signs in with the token, and is told where it is bound.
	link, _ := dialRaw(t, a.http)
	you := link.result(t, "auth", "a1", map[string]any{"scheme": "token", "token": token})["you"].(map[string]any)
	if !reflectEqual(remoteOfJSON(t, you), map[string]any{"server": other}) || you["roles"].([]any)[0] != "bot" {
		t.Fatalf("link you = %#v", you)
	}
	link.drain(t)
	act := func(id, method string, params map[string]any) map[string]any {
		link.write(t, map[string]any{"method": method, "id": id, "params": params})
		for {
			if frame := link.read(t); frame["id"] == id {
				return frame
			}
		}
	}
	// A create as a remote user, and its duplicate.
	create := map[string]any{"room_id": room, "body": map[string]any{"text": "from far"}, "ext": map[string]any{"remote": map[string]any{"user_id": "zed", "room_id": "r9", "message_id": "1700000000001"}}}
	first := act("c1", "message", create)
	second := act("c2", "message", create)
	firstID := first["result"].(map[string]any)["message_id"]
	if second["result"].(map[string]any)["message_id"] != firstID {
		t.Fatalf("duplicate create = %#v, first %#v", second, first)
	}
	snapshot := lastSnapshot(t, a.app, firstID.(string))
	if snapshot["from"].(map[string]any)["user_id"] != "zed@other.test:9000" || !reflectEqual(remoteOfJSON(t, snapshot), map[string]any{"server": other, "user_id": "zed", "room_id": "r9", "message_id": "1700000000001"}) {
		t.Fatalf("remote user's message = %#v", snapshot)
	}
	// me answers with the remote user.
	if you := act("me1", "me", map[string]any{"name": "Zed", "ext": map[string]any{"remote": map[string]any{"user_id": "zed"}}})["result"].(map[string]any)["you"].(map[string]any); you["user_id"] != "zed@other.test:9000" || you["name"] != "Zed" {
		t.Fatalf("me as zed = %#v", you)
	}
	// Not in a room the link has not joined, which its error says.
	if frame := act("c3", "message", map[string]any{"room_id": elsewhere, "body": map[string]any{"text": "x"}, "ext": map[string]any{"remote": map[string]any{"user_id": "zed"}}}); frame["error"].(map[string]any)["code"] != float64(CodeDenied) ||
		!reflectEqual(frame["error"].(map[string]any)["data"], map[string]any{"ext": map[string]any{"remote": "not_joined"}}) {
		t.Fatalf("write elsewhere = %#v", frame)
	}
	// A link sets only threads of rooms it joined, as a remote user.
	for id, params := range map[string]map[string]any{
		"rs1": {"title": "mine"},
		"rs2": {"title": "top", "ext": map[string]any{"remote": map[string]any{"user_id": "zed"}}},
		"rs3": {"room_id": room, "title": "renamed", "ext": map[string]any{"remote": map[string]any{"user_id": "zed"}}},
		"rs4": {"parent_room_id": elsewhere, "title": "t", "ext": map[string]any{"remote": map[string]any{"user_id": "zed"}}},
	} {
		if frame := act(id, "room_set", params); frame["error"] == nil {
			t.Fatalf("%s: %#v", id, frame)
		}
	}
	thread := map[string]any{"parent_room_id": room, "title": "far thread", "ext": map[string]any{"remote": map[string]any{"user_id": "zed", "room_id": "rt1"}}}
	made := act("rs5", "room_set", thread)["result"].(map[string]any)["room_id"]
	if again := act("rs6", "room_set", thread)["result"].(map[string]any)["room_id"]; again != made {
		t.Fatalf("duplicate thread = %v, %v", again, made)
	}
	if frame := act("rs7", "message", map[string]any{"room_id": made, "body": map[string]any{"text": "in far thread"}, "ext": map[string]any{"remote": map[string]any{"user_id": "zed"}}}); frame["result"] == nil {
		t.Fatalf("write in a thread = %#v", frame)
	}
	// The key of a copy outlives its deletion: a create sent again returns
	// it, and a save restores it.
	act("d1", "message", map[string]any{"room_id": room, "message_id": firstID, "deleted": true, "ext": map[string]any{"remote": map[string]any{"user_id": "zed"}}})
	if again := act("c6", "message", create); again["result"].(map[string]any)["message_id"] != firstID {
		t.Fatalf("create after the delete = %#v", again)
	}
	act("s1", "message", map[string]any{"room_id": room, "message_id": firstID, "body": map[string]any{"text": "from far"}, "ext": map[string]any{"remote": map[string]any{"user_id": "zed", "room_id": "r9", "message_id": "1700000000001"}}})
	if restored := lastSnapshot(t, a.app, firstID.(string)); restored["deleted"] == true || wstestText(restored) != "from far" || remoteOfJSON(t, restored)["message_id"] != "1700000000001" {
		t.Fatalf("restored = %#v", restored)
	}
	// A remote user that left is joined again; one that was removed is not.
	act("l1", "room_leave", map[string]any{"room_id": room, "ext": map[string]any{"remote": map[string]any{"user_id": "zed"}}})
	if frame := act("c4", "message", map[string]any{"room_id": room, "body": map[string]any{"text": "back"}, "ext": map[string]any{"remote": map[string]any{"user_id": "zed"}}}); frame["result"] == nil {
		t.Fatalf("write after leaving = %#v", frame)
	}
	a.call(t, "room_leave", map[string]any{"room_id": room, "user_id": "zed@other.test:9000"})
	if frame := act("c5", "message", map[string]any{"room_id": room, "body": map[string]any{"text": "again"}, "ext": map[string]any{"remote": map[string]any{"user_id": "zed"}}}); frame["error"] == nil || frame["error"].(map[string]any)["code"] != float64(CodeDenied) {
		t.Fatalf("write after removal = %#v", frame)
	}
	// ext:remote pairs only a room the link joined, with a link of its own
	// server; anyone else is denied.
	for id, tc := range map[string]struct {
		params map[string]any
		code   int
	}{
		"p1": {map[string]any{"room_id": elsewhere, "link": "https://link.apron.chat/#server=" + other + "&room=x&token=t"}, CodeInvalidParams},
		"p2": {map[string]any{"room_id": room, "link": "https://link.apron.chat/#server=ws://evil.test/ws&room=x&token=t"}, CodeInvalidParams},
		"p3": {map[string]any{"room_id": room, "link": "not a link"}, CodeInvalidParams},
	} {
		if frame := act(id, "ext:remote", tc.params); frame["error"] == nil || frame["error"].(map[string]any)["code"] != float64(tc.code) {
			t.Fatalf("%s: %#v", id, frame)
		}
	}
	if frame := act("p4", "ext:remote", map[string]any{"room_id": room, "link": "https://link.apron.chat/#server=" + strings.ToUpper("ws://OTHER.test:9000") + "/ws&room=x&token=t"}); frame["result"] == nil {
		t.Fatalf("pairing = %#v", frame)
	}
	if notice := a.command(t, room, "/remote list"); !strings.Contains(notice, room+" with room x of "+other) {
		t.Fatalf("list = %q", notice)
	}
	// Without link, the request ends the pair, and the link leaves.
	if frame := act("p5", "ext:remote", map[string]any{"room_id": room}); frame["result"] == nil {
		t.Fatalf("unpairing = %#v", frame)
	}
	if notice := a.command(t, room, "/remote list"); strings.Contains(notice, room+" with room") {
		t.Fatalf("list after unpairing = %q", notice)
	}
	if joined, _ := a.remoteHostMember(room, other); joined {
		t.Fatal("the link stayed in the room")
	}
	guest := dialTestClient(t, a.http)
	guest.expectError(t, "ext:remote", "g1", map[string]any{"room_id": room, "link": "https://link.apron.chat/#server=" + other + "&room=x&token=t"}, CodeDenied)
	// A client cannot act as a remote user.
	_, posted := guest.request(t, "message", "g2", map[string]any{"room_id": "general", "body": map[string]any{"text": "me"}, "ext": map[string]any{"remote": map[string]any{"user_id": "zed"}}})
	if mine := lastSnapshot(t, a.app, posted["message_id"].(string)); mine["from"].(map[string]any)["user_id"] != guest.userID || mine["ext"] != nil {
		t.Fatalf("a guest's message with ext.remote = %#v", mine)
	}
	// Revoking the link closes its connection.
	if notice := a.command(t, room, "/remote revoke "+other); !strings.HasPrefix(notice, "Revoked") {
		t.Fatalf("revoke = %q", notice)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := link.ws.Read(ctx); err != nil {
			break
		}
	}
}

func reflectEqual(a, b any) bool { return string(encodeJSON(a)) == string(encodeJSON(b)) }
