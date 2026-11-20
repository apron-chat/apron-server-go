package apron

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
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

func startFederated(t *testing.T, linger time.Duration) *federated {
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
	config.Remote = RemoteConfig{Self: fed.self, Linger: linger}
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

var (
	inviteInNotice = regexp.MustCompile("`(https://link\\.apron\\.chat/#[^`]+)`")
	mirrorInNotice = regexp.MustCompile("in room `([^`]+)`")
)

// eventually retries check until it returns nil, or fails the test.
func eventually(t *testing.T, what string, check func() error) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		err := check()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %v", what, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// snapshotOf is a message's current snapshot, decoded as clients see it.
func snapshotOf(app *Server, messageID string) map[string]any {
	app.mu.RLock()
	defer app.mu.RUnlock()
	m := app.messages[messageID]
	if m == nil || len(m.records) == 0 {
		return nil
	}
	var decoded map[string]any
	_ = json.Unmarshal(encodeJSON(m.snapshot()), &decoded)
	return decoded
}

// copyIn finds the copy in a room of an owner's message, by its ext.remote.
func copyIn(app *Server, roomID, ownerID string) map[string]any {
	app.mu.RLock()
	var ids []string
	for id, m := range app.messages {
		if m.roomID == roomID {
			ids = append(ids, id)
		}
	}
	app.mu.RUnlock()
	for _, id := range ids {
		if m := snapshotOf(app, id); remoteExtOf(m)["message_id"] == ownerID {
			return m
		}
	}
	return nil
}

func remoteExtOf(object map[string]any) map[string]any {
	ext, _ := object["ext"].(map[string]any)
	remote, _ := ext["remote"].(map[string]any)
	return remote
}

func authorityOf(address string) string {
	authority, _ := remoteAuthority(address)
	return authority
}

// TestFederationOff: without Config.Remote nothing of it shows.
func TestFederationOff(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	c, frame := dialRaw(t, httpServer)
	for _, capability := range frame["params"].(map[string]any)["capabilities"].([]any) {
		if capability == "ext:remote" {
			t.Fatal("ext:remote advertised")
		}
	}
	guestAuth(t, c)
	c.drain(t)
	c.expectError(t, "command", "c1", map[string]any{"room_id": "general", "body": map[string]any{"text": "/remote list"}}, CodeInvalidParams)
	// The message's record comes before its result.
	before, _ := c.request(t, "message", "m1", map[string]any{"body": map[string]any{"text": "x"}, "ext": map[string]any{"remote": map[string]any{"user_id": "zed", "server": "ws://b.test/ws"}}})
	seen := false
	for _, frame := range before {
		if params, _ := frame["params"].(map[string]any); frame["method"] == "message" {
			if seen = true; params["ext"] != nil {
				t.Fatalf("ext.remote kept: %#v", params)
			}
		}
	}
	if !seen {
		t.Fatal("no message record")
	}
}

// mirrored starts an owner, A, and a mirror, B, of a private room of A's.
func mirrored(t *testing.T, linger time.Duration) (a, b *federated, ownerRoom, mirrorRoom string) {
	t.Helper()
	a, b = startFederated(t, linger), startFederated(t, linger)
	ownerRoom = a.call(t, "room_set", map[string]any{"title": "Ops", "description": "Deploys", "private": true})["room_id"].(string)
	mirrorRoom = share(t, a, b, ownerRoom)
	b.app.mu.RLock()
	r := b.app.rooms[mirrorRoom]
	b.app.mu.RUnlock()
	if r == nil || !r.private || r.title() != "Ops" || r.record["description"] != "Deploys" || remoteExtOf(snapshotRoom(b.app, mirrorRoom))["room_id"] != ownerRoom {
		t.Fatalf("mirror room = %#v", r)
	}
	return a, b, ownerRoom, mirrorRoom
}

// share invites b to a's room, connects it, and returns the mirror room.
func share(t *testing.T, a, b *federated, ownerRoom string) string {
	t.Helper()
	invite := inviteInNotice.FindStringSubmatch(a.command(t, ownerRoom, "/remote invite "+b.self))
	if invite == nil {
		t.Fatal("no invite")
	}
	notice := b.command(t, "general", "/remote connect "+invite[1])
	found := mirrorInNotice.FindStringSubmatch(notice)
	if found == nil {
		t.Fatalf("connect = %q", notice)
	}
	return found[1]
}

func snapshotRoom(app *Server, roomID string) map[string]any {
	app.mu.RLock()
	defer app.mu.RUnlock()
	var decoded map[string]any
	_ = json.Unmarshal(encodeJSON(app.rooms[roomID].record), &decoded)
	return decoded
}

func TestMirrorRoom(t *testing.T) {
	a, b, ownerRoom, mirrorRoom := mirrored(t, 0)
	authorityA, authorityB := authorityOf(a.self), authorityOf(b.self)

	alice := dialTestClient(t, a.http)
	a.call(t, "room_join", map[string]any{"room_id": ownerRoom, "user_id": alice.userID})
	bob := dialTestClient(t, b.http)
	b.call(t, "room_join", map[string]any{"room_id": mirrorRoom, "user_id": bob.userID})
	alice.drain(t)
	bob.drain(t)

	// Both rooms list the same people: the owner's members join the mirror
	// room as remote users, and the mirror's members the owner's room.
	members := func(app *Server, roomID string) map[string]bool {
		app.mu.RLock()
		defer app.mu.RUnlock()
		set := make(map[string]bool)
		for id, u := range app.rooms[roomID].members {
			if u.link == nil {
				set[id] = true
			}
		}
		return set
	}
	eventually(t, "the member lists", func() error {
		onA, onB := members(a.app, ownerRoom), members(b.app, mirrorRoom)
		want := map[string]bool{"admin": true, alice.userID: true, "admin@" + authorityB: true, bob.userID + "@" + authorityB: true}
		wantB := map[string]bool{"admin": true, bob.userID: true, "admin@" + authorityA: true, alice.userID + "@" + authorityA: true}
		if fmt.Sprint(onA) != fmt.Sprint(want) || fmt.Sprint(onB) != fmt.Sprint(wantB) {
			return fmt.Errorf("owner %v, mirror %v", onA, onB)
		}
		return nil
	})

	// The owner's message is copied into the mirror room.
	_, posted := alice.request(t, "message", "m1", map[string]any{"room_id": ownerRoom, "body": map[string]any{"text": "hello"}})
	hello := posted["message_id"].(string)
	var helloCopy map[string]any
	eventually(t, "the copy", func() error {
		if helloCopy = copyIn(b.app, mirrorRoom, hello); helloCopy == nil {
			return fmt.Errorf("none")
		}
		return nil
	})
	if from := helloCopy["from"].(map[string]any); from["user_id"] != alice.userID+"@"+authorityA || remoteExtOf(helloCopy)["server"] != a.self {
		t.Fatalf("copy = %#v", helloCopy)
	}

	// A mirror user's reply goes to the owner as its remote user, and is
	// answered with its copy here.
	mention := alice.userID + "@" + authorityA
	_, replied := bob.request(t, "message", "m2", map[string]any{"room_id": mirrorRoom, "reply_to": map[string]any{"message_id": helloCopy["message_id"]},
		"body": map[string]any{"text": "@" + mention + " hi", "mentions": []any{mention}}})
	replyHere := replied["message_id"].(string)
	mine := snapshotOf(b.app, replyHere)
	if mine["from"].(map[string]any)["user_id"] != bob.userID || mine["reply_to"].(map[string]any)["message_id"] != helloCopy["message_id"] {
		t.Fatalf("the copy of bob's reply = %#v", mine)
	}
	ownerReply := remoteExtOf(mine)["message_id"].(string)
	onA := snapshotOf(a.app, ownerReply)
	if onA["from"].(map[string]any)["user_id"] != bob.userID+"@"+authorityB || onA["reply_to"].(map[string]any)["message_id"] != hello ||
		wstestText(onA) != "@"+alice.userID+" hi" {
		t.Fatalf("bob's reply on the owner = %#v", onA)
	}

	// The author edits on each side; a reaction goes to the owner.
	bob.request(t, "message", "m3", map[string]any{"room_id": mirrorRoom, "message_id": replyHere, "body": map[string]any{"text": "hi (edited)"}})
	if wstestText(snapshotOf(a.app, ownerReply)) != "hi (edited)" || wstestText(snapshotOf(b.app, replyHere)) != "hi (edited)" {
		t.Fatal("bob's edit")
	}
	alice.request(t, "message", "m4", map[string]any{"room_id": ownerRoom, "message_id": hello, "body": map[string]any{"text": "hello (edited)"}})
	eventually(t, "alice's edit", func() error {
		if text := wstestText(snapshotOf(b.app, helloCopy["message_id"].(string))); text != "hello (edited)" {
			return fmt.Errorf("%q", text)
		}
		return nil
	})
	bob.request(t, "reactions", "r1", map[string]any{"message_id": helloCopy["message_id"], "emojis": []any{"👍"}})
	a.app.mu.RLock()
	set := a.app.messages[hello].reactions[bob.userID+"@"+authorityB].emojis
	a.app.mu.RUnlock()
	b.app.mu.RLock()
	mirrorSet := b.app.messages[helloCopy["message_id"].(string)].reactions[bob.userID].emojis
	b.app.mu.RUnlock()
	if len(set) != 1 || len(mirrorSet) != 1 {
		t.Fatalf("reactions: owner %v, mirror %v", set, mirrorSet)
	}

	// Threads, both ways.
	// A thread of a private room here is private unless it says otherwise,
	// and only a thread every member sees is mirrored.
	alice.request(t, "room_set", "t1", map[string]any{"parent_room_id": ownerRoom, "title": "Deploy", "private": false})
	eventually(t, "the thread", func() error {
		b.app.mu.RLock()
		defer b.app.mu.RUnlock()
		for _, r := range b.app.rooms {
			if r.parent != nil && r.parent.id == mirrorRoom && r.title() == "Deploy" {
				return nil
			}
		}
		return fmt.Errorf("none")
	})
	_, madeHere := bob.request(t, "room_set", "t2", map[string]any{"parent_room_id": mirrorRoom, "title": "From B"})
	threadHere := madeHere["room_id"].(string)
	b.app.mu.RLock()
	parent := b.app.rooms[threadHere].parent.id
	b.app.mu.RUnlock()
	if parent != mirrorRoom {
		t.Fatalf("bob's thread is under %s", parent)
	}
	a.app.mu.RLock()
	threadsOnA := len(a.app.rooms[ownerRoom].children)
	a.app.mu.RUnlock()
	if threadsOnA != 2 {
		t.Fatalf("threads on the owner: %d", threadsOnA)
	}

	// The owner's admin deletes bob's message: the copy is deleted too.
	a.call(t, "message", map[string]any{"room_id": ownerRoom, "message_id": ownerReply, "deleted": true})
	eventually(t, "the delete", func() error {
		if snapshotOf(b.app, replyHere)["deleted"] != true {
			return fmt.Errorf("not deleted")
		}
		return nil
	})
	// The mirror's admin hides a copy, which an owner's edit leaves hidden.
	b.call(t, "message", map[string]any{"room_id": mirrorRoom, "message_id": helloCopy["message_id"], "deleted": true})
	alice.request(t, "message", "m5", map[string]any{"room_id": ownerRoom, "message_id": hello, "body": map[string]any{"text": "hello again"}})
	alice.request(t, "message", "m6", map[string]any{"room_id": ownerRoom, "body": map[string]any{"text": "later"}})
	eventually(t, "the later message", func() error {
		b.app.mu.RLock()
		defer b.app.mu.RUnlock()
		for _, m := range b.app.messages {
			if m.roomID == mirrorRoom && strings.Contains(string(encodeJSON(m.snapshot())), "later") {
				return nil
			}
		}
		return fmt.Errorf("none")
	})
	if snapshotOf(b.app, helloCopy["message_id"].(string))["deleted"] != true {
		t.Fatal("the hidden copy came back")
	}

	// A rename reaches the owner without a message.
	bob.request(t, "me", "n1", map[string]any{"name": "Robert"})
	eventually(t, "the rename", func() error {
		a.app.mu.RLock()
		defer a.app.mu.RUnlock()
		if name := a.app.users[bob.userID+"@"+authorityB].name; name != "Robert" {
			return fmt.Errorf("%q", name)
		}
		return nil
	})

	// Leaves travel too.
	carol := dialTestClient(t, b.http)
	b.call(t, "room_join", map[string]any{"room_id": mirrorRoom, "user_id": carol.userID})
	eventually(t, "carol on the owner", func() error {
		if !members(a.app, ownerRoom)[carol.userID+"@"+authorityB] {
			return fmt.Errorf("not there")
		}
		return nil
	})
	carol.request(t, "room_leave", "l1", map[string]any{"room_id": mirrorRoom})
	alice.request(t, "room_leave", "l2", map[string]any{"room_id": ownerRoom})
	eventually(t, "the leaves", func() error {
		if members(a.app, ownerRoom)[carol.userID+"@"+authorityB] || members(b.app, mirrorRoom)[alice.userID+"@"+authorityA] {
			return fmt.Errorf("owner %v, mirror %v", members(a.app, ownerRoom), members(b.app, mirrorRoom))
		}
		return nil
	})

	// Unshared from the mirror: the link leaves, and writes are refused.
	if notice := b.command(t, mirrorRoom, "/remote unshare"); !strings.Contains(notice, "no longer mirrored") {
		t.Fatalf("unshare = %q", notice)
	}
	bob.drain(t)
	bob.expectError(t, "message", "m7", map[string]any{"room_id": mirrorRoom, "body": map[string]any{"text": "after"}}, CodeDenied)
	a.app.mu.RLock()
	linked := false
	for _, link := range a.app.fed.links {
		linked = linked || a.app.rooms[ownerRoom].members[link.id] != nil
	}
	a.app.mu.RUnlock()
	if linked {
		t.Fatal("the link stayed in the owner's room")
	}
	// Its remote users went with it.
	for id := range members(a.app, ownerRoom) {
		if strings.HasSuffix(id, "@"+authorityB) {
			t.Fatalf("%s stayed in the owner's room", id)
		}
	}
}

// A mirror with a linger closes its connection, and the owner's wake push
// brings it back.
func TestMirrorWakes(t *testing.T) {
	a, b, ownerRoom, mirrorRoom := mirrored(t, 200*time.Millisecond)
	time.Sleep(time.Second)
	b.app.fed.mu.Lock()
	ms := b.app.fed.mirrors[a.self]
	b.app.fed.mu.Unlock()
	ms.mu.Lock()
	connected := ms.conn != nil
	ms.mu.Unlock()
	if connected {
		t.Fatal("the mirror stayed connected")
	}
	posted := a.call(t, "message", map[string]any{"room_id": ownerRoom, "body": map[string]any{"text": "wake up"}})
	eventually(t, "the copy after the wake", func() error {
		if copyIn(b.app, mirrorRoom, posted["message_id"].(string)) == nil {
			return fmt.Errorf("none")
		}
		return nil
	})
	// The owner unshares while the mirror sleeps: at its next connection,
	// the mirror room stops taking writes.
	time.Sleep(time.Second)
	a.command(t, ownerRoom, "/remote unshare")
	ms.wakeUp()
	eventually(t, "the unshare", func() error {
		b.app.fed.mu.Lock()
		defer b.app.fed.mu.Unlock()
		if !b.app.fed.rooms[mirrorRoom].Unshared {
			return fmt.Errorf("still shared")
		}
		return nil
	})
}

// A thread that existed before the room was shared gets a mirror thread
// at connect, which copies its records from the mirror room's start.
func TestMirrorExistingThreads(t *testing.T) {
	a, b := startFederated(t, 0), startFederated(t, 0)
	ownerRoom := a.call(t, "room_set", map[string]any{"title": "Ops"})["room_id"].(string)
	thread := a.call(t, "room_set", map[string]any{"parent_room_id": ownerRoom, "title": "Old thread", "description": "From before"})["room_id"].(string)
	nested := a.call(t, "room_set", map[string]any{"parent_room_id": thread, "title": "Nested"})["room_id"].(string)
	before := a.call(t, "message", map[string]any{"room_id": thread, "body": map[string]any{"text": "before"}})["message_id"].(string)
	mirrorRoom := share(t, a, b, ownerRoom)
	mirrorThread := func(parent, title string) *roomState {
		b.app.mu.RLock()
		defer b.app.mu.RUnlock()
		for _, r := range b.app.rooms {
			if r.parent != nil && r.parent.id == parent && r.title() == title {
				return r
			}
		}
		return nil
	}
	local := mirrorThread(mirrorRoom, "Old thread")
	if local == nil || local.record["description"] != "From before" || mirrorThread(local.id, "Nested") == nil {
		t.Fatalf("mirror thread %#v", local)
	}
	after := a.call(t, "message", map[string]any{"room_id": thread, "body": map[string]any{"text": "after"}})["message_id"].(string)
	inNested := a.call(t, "message", map[string]any{"room_id": nested, "body": map[string]any{"text": "nested"}})["message_id"].(string)
	eventually(t, "the copies", func() error {
		if copyIn(b.app, local.id, after) == nil || copyIn(b.app, mirrorThread(local.id, "Nested").id, inNested) == nil {
			return fmt.Errorf("none")
		}
		return nil
	})
	if copyIn(b.app, local.id, before) != nil {
		t.Fatal("a record from before the start was copied")
	}
}

// The owner's side: what a link may do as remote users.
func TestLinkRequests(t *testing.T) {
	a := startFederated(t, 0)
	const other = "ws://other.test:9000/ws"
	room := a.call(t, "room_set", map[string]any{"title": "Ops"})["room_id"].(string)
	elsewhere := a.call(t, "room_set", map[string]any{"title": "Elsewhere"})["room_id"].(string)
	invite := inviteInNotice.FindStringSubmatch(a.command(t, room, "/remote invite "+other))
	_, _, token, err := parseInvite(invite[1])
	if err != nil {
		t.Fatal(err)
	}
	link, _ := dialRaw(t, a.http)
	you := link.result(t, "auth", "a1", map[string]any{"scheme": "token", "token": token})["you"].(map[string]any)
	if remoteExtOf(you)["server"] != other || you["roles"].([]any)[0] != "bot" {
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
	reason := func(frame map[string]any) any {
		failure, _ := frame["error"].(map[string]any)
		data, _ := failure["data"].(map[string]any)
		ext, _ := data["ext"].(map[string]any)
		return ext["remote"]
	}
	asZed := map[string]any{"remote": map[string]any{"user_id": "zed"}}
	first := act("c1", "message", map[string]any{"room_id": room, "body": map[string]any{"text": "from far"}, "ext": map[string]any{"remote": map[string]any{"user_id": "zed", "room_id": "r9", "message_id": "1"}}})
	id := first["result"].(map[string]any)["message_id"].(string)
	if m := snapshotOf(a.app, id); m["from"].(map[string]any)["user_id"] != "zed@other.test:9000" || len(remoteExtOf(m)) != 2 || remoteExtOf(m)["user_id"] != "zed" {
		t.Fatalf("remote user's message = %#v", m)
	}
	if you := act("me1", "me", map[string]any{"name": "Zed", "ext": asZed})["result"].(map[string]any)["you"].(map[string]any); you["user_id"] != "zed@other.test:9000" || you["name"] != "Zed" {
		t.Fatalf("me as zed = %#v", you)
	}
	if frame := act("c2", "message", map[string]any{"room_id": elsewhere, "body": map[string]any{"text": "x"}, "ext": asZed}); reason(frame) != "not_joined" {
		t.Fatalf("write elsewhere = %#v", frame)
	}
	// A remote user that left is joined again; one that was removed is not.
	act("l1", "room_leave", map[string]any{"room_id": room, "ext": asZed})
	if frame := act("c3", "message", map[string]any{"room_id": room, "body": map[string]any{"text": "back"}, "ext": asZed}); frame["result"] == nil {
		t.Fatalf("write after leaving = %#v", frame)
	}
	act("l2", "room_leave", map[string]any{"room_id": room, "ext": asZed})
	if frame := act("j1", "room_join", map[string]any{"room_id": room, "ext": asZed}); frame["result"] == nil {
		t.Fatalf("join = %#v", frame)
	}
	a.call(t, "room_leave", map[string]any{"room_id": room, "user_id": "zed@other.test:9000"})
	if frame := act("c4", "message", map[string]any{"room_id": room, "body": map[string]any{"text": "again"}, "ext": asZed}); reason(frame) != "removed" {
		t.Fatalf("write after removal = %#v", frame)
	}
	if frame := act("j2", "room_join", map[string]any{"room_id": room, "ext": asZed}); reason(frame) != "removed" {
		t.Fatalf("join after removal = %#v", frame)
	}
	// A link sets only threads of its rooms, as a remote user; the thread is
	// one every member of the room sees, which the link joins.
	for id, params := range map[string]map[string]any{
		"s1": {"title": "mine"},
		"s2": {"title": "top", "ext": map[string]any{"remote": map[string]any{"user_id": "yan"}}},
		"s3": {"room_id": room, "title": "renamed", "ext": map[string]any{"remote": map[string]any{"user_id": "yan"}}},
		"s4": {"parent_room_id": elsewhere, "title": "t", "ext": map[string]any{"remote": map[string]any{"user_id": "yan"}}},
	} {
		if frame := act(id, "room_set", params); frame["error"] == nil {
			t.Fatalf("%s: %#v", id, frame)
		}
	}
	made := act("s5", "room_set", map[string]any{"parent_room_id": room, "title": "far thread", "ext": map[string]any{"remote": map[string]any{"user_id": "yan"}}})
	thread := made["result"].(map[string]any)["room_id"].(string)
	a.app.mu.RLock()
	r := a.app.rooms[thread]
	linked := false
	for _, l := range a.app.fed.links {
		linked = linked || r.members[l.id] != nil
	}
	a.app.mu.RUnlock()
	if r.private || !linked {
		t.Fatalf("the thread: private %v, link in it %v", r.private, linked)
	}
	// A wake push is a relay on the link's own server.
	for id, url := range map[string]string{"w1": "https://evil.test/wake", "w2": "http://other.test:9000/wake"} {
		if frame := act(id, "push_register", map[string]any{"kind": "relay", "url": url, "wake": []any{"ext:remote"}}); frame["error"] == nil {
			t.Fatalf("%s: %#v", id, frame)
		}
	}
	if frame := act("w3", "push_register", map[string]any{"kind": "relay", "url": "https://other.test:9000/remote/wake/x", "token": "t", "wake": []any{"ext:remote"}}); frame["result"] == nil {
		t.Fatalf("wake push = %#v", frame)
	}
	// Anyone else cannot act as a remote user.
	guest := dialTestClient(t, a.http)
	_, posted := guest.request(t, "message", "g1", map[string]any{"body": map[string]any{"text": "me"}, "ext": asZed})
	if m := snapshotOf(a.app, posted["message_id"].(string)); m["from"].(map[string]any)["user_id"] != guest.userID || m["ext"] != nil {
		t.Fatalf("a guest's message with ext.remote = %#v", m)
	}
	// Nor write ext.remote on itself or a room.
	if you := guest.result(t, "me", "g2", map[string]any{"ext": asZed})["you"].(map[string]any); you["ext"] != nil {
		t.Fatalf("a guest wrote its own ext.remote: %#v", you)
	}
	guest.request(t, "room_set", "g3", map[string]any{"room_id": room, "title": "Ops", "ext": map[string]any{"remote": map[string]any{"server": other}}})
	a.app.mu.RLock()
	roomExt := a.app.rooms[room].record["ext"]
	// A remote user shows as offline, though its session is connected.
	status := a.app.users["zed@other.test:9000"].status
	a.app.mu.RUnlock()
	if roomExt != nil || status != statusOffline {
		t.Fatalf("room ext %#v, remote user's status %q", roomExt, status)
	}
}

// fakeOwner is an owner that serves room ops to b's link, and answers each
// message request with what message returns: the reply's result or error.
// write sends a frame to the mirror.
func fakeOwner(t *testing.T, b *federated, message func(params map[string]any, write func(any)) map[string]any) (address string) {
	t.Helper()
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ctx := context.Background()
		var mu sync.Mutex
		write := func(v any) {
			mu.Lock()
			defer mu.Unlock()
			_ = ws.Write(ctx, websocket.MessageText, encodeJSON(v))
		}
		write(map[string]any{"method": "server", "params": map[string]any{"apron": 10, "auth": []string{"token"}, "capabilities": []string{"ext:remote"}}})
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var frame struct {
				Method string         `json:"method"`
				ID     string         `json:"id"`
				Params map[string]any `json:"params"`
			}
			if json.Unmarshal(data, &frame) != nil || frame.ID == "" {
				continue
			}
			reply := map[string]any{"id": frame.ID, "result": map[string]any{}}
			switch frame.Method {
			case "auth":
				reply["result"] = map[string]any{"you": map[string]any{"user_id": "link_1", "ext": map[string]any{"remote": map[string]any{"server": b.self}}}}
			case "room_list":
				reply["result"] = map[string]any{"joined": []any{map[string]any{"room_id": "ops", "title": "Ops", "latest_log_id": "100"}}}
			case "me":
				reply["result"] = map[string]any{"you": map[string]any{"user_id": "bob@b"}}
			case "message":
				reply = message(frame.Params, write)
				reply["id"] = frame.ID
			}
			write(reply)
		}
	}))
	t.Cleanup(owner.Close)
	return "ws" + strings.TrimPrefix(owner.URL, "http") + "/ws"
}

// connectFake mirrors room ops of a fake owner in b, and returns the mirror
// room and a member of it.
func connectFake(t *testing.T, b *federated, address string) (string, *testClient) {
	t.Helper()
	notice := b.command(t, "general", "/remote connect https://link.apron.chat/#server="+address+"&room=ops&token=t")
	found := mirrorInNotice.FindStringSubmatch(notice)
	if found == nil {
		t.Fatalf("connect = %q", notice)
	}
	mirrorRoom := found[1]
	bob := dialTestClient(t, b.http)
	b.call(t, "room_join", map[string]any{"room_id": mirrorRoom, "user_id": bob.userID})
	bob.drain(t)
	return mirrorRoom, bob
}

// A mirror answers a request the owner took with the copy, also when the
// owner's record comes after the result: here a fake owner sends it late.
func TestMirrorAnswersWithTheCopy(t *testing.T) {
	b := startFederated(t, 0)
	var n atomic.Int64
	address := fakeOwner(t, b, func(params map[string]any, write func(any)) map[string]any {
		id := fmt.Sprint(1000 + n.Add(1))
		body := params["body"]
		ext, _ := params["ext"].(map[string]any)
		act, _ := ext["remote"].(map[string]any)
		author := fmt.Sprint(act["user_id"])
		// The result first, and the record late.
		go func() {
			time.Sleep(300 * time.Millisecond)
			write(map[string]any{"method": "message", "params": map[string]any{
				"message_id": id, "log_id": id, "room_id": "ops", "body": body,
				"from": map[string]any{"user_id": author + "@b", "ext": map[string]any{"remote": map[string]any{"server": b.self, "user_id": author}}},
			}})
		}()
		return map[string]any{"result": map[string]any{"message_id": id}}
	})
	mirrorRoom, bob := connectFake(t, b, address)
	_, result := bob.request(t, "message", "m1", map[string]any{"room_id": mirrorRoom, "body": map[string]any{"text": "late record"}})
	local, _ := result["message_id"].(string)
	if m := snapshotOf(b.app, local); m == nil || wstestText(m) != "late record" || m["from"].(map[string]any)["user_id"] != bob.userID {
		t.Fatalf("answered %#v, copy %#v", result, m)
	}
}

// An owner's not_joined refusal stops the mirror room: the link is no
// longer in the owner's room.
func TestMirrorNotJoined(t *testing.T) {
	b := startFederated(t, 0)
	address := fakeOwner(t, b, func(map[string]any, func(any)) map[string]any {
		return map[string]any{"error": map[string]any{"code": CodeDenied, "message": "not joined", "data": map[string]any{"ext": map[string]any{"remote": "not_joined"}}}}
	})
	mirrorRoom, bob := connectFake(t, b, address)
	bob.expectError(t, "message", "m1", map[string]any{"room_id": mirrorRoom, "body": map[string]any{"text": "hello?"}}, CodeDenied)
	b.app.fed.mu.Lock()
	unshared := b.app.fed.rooms[mirrorRoom].Unshared
	b.app.fed.mu.Unlock()
	if !unshared {
		t.Fatal("the mirror room is still shared")
	}
}
