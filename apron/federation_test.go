package apron

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/store"
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
	// stop shuts it down, once.
	stop func()
}

func startFederated(t *testing.T, linger time.Duration) *federated {
	t.Helper()
	return startFederatedWith(t, linger, nil, nil)
}

// startFederatedWith starts a federated server on listener, if not nil,
// such as the address of one that stopped, and with a store.
func startFederatedWith(t *testing.T, linger time.Duration, listener net.Listener, st store.Store) *federated {
	t.Helper()
	fed := &federated{notices: make(chan string, 64)}
	var handler http.Handler
	fed.http = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	if listener != nil {
		fed.http.Listener.Close()
		fed.http.Listener = listener
	}
	fed.self = "ws://" + fed.http.Listener.Addr().String() + "/ws"
	config := extensionConfig(testExtension{"tadmin", func(r *Registrar) error {
		var err error
		fed.admin, err = r.Bot("admin", BotOptions{Name: "Admin"})
		return err
	}})
	config.Roles = map[string][]string{RoleAdmin: {"admin"}}
	config.Remote = RemoteConfig{Self: fed.self, Linger: linger}
	config.Store = st
	app, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	fed.app, handler = app, app.Handler()
	fed.http.Start()
	fed.stop = sync.OnceFunc(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := app.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		fed.http.Close()
	})
	t.Cleanup(fed.stop)
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

// refusedAfterNotes sends a request that must fail with code, skipping
// the notifications that come before its reply, such as a mirror thread's.
func refusedAfterNotes(t *testing.T, c *testClient, method, id string, params map[string]any, code int) {
	t.Helper()
	c.write(t, map[string]any{"method": method, "id": id, "params": params})
	for {
		frame := c.read(t)
		if frame["id"] == nil {
			continue
		}
		failure, _ := frame["error"].(map[string]any)
		if frame["id"] != id || failure == nil || failure["code"] != float64(code) {
			t.Fatalf("%s reply = %#v, want error %d", method, frame, code)
		}
		return
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
	// A mirror thread's first members are those of the owner's
	// room_update: its creator joined before the link.
	eventually(t, "the thread", func() error {
		b.app.mu.RLock()
		defer b.app.mu.RUnlock()
		for _, r := range b.app.rooms {
			if r.parent != nil && r.parent.id == mirrorRoom && r.title() == "Deploy" {
				if r.members[alice.userID+"@"+authorityA] == nil {
					return fmt.Errorf("the creator is not in the mirror thread")
				}
				return nil
			}
		}
		return fmt.Errorf("none")
	})
	_, madeHere := bob.request(t, "room_set", "t2", map[string]any{"parent_room_id": mirrorRoom, "title": "From B"})
	threadHere := madeHere["room_id"].(string)
	b.app.mu.RLock()
	if b.app.rooms[threadHere].members[bob.userID] == nil {
		b.app.mu.RUnlock()
		t.Fatal("bob is not in his mirror thread")
	}
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
	made := act("s5", "room_set", map[string]any{"parent_room_id": room, "title": "far thread", "private": true, "ext": map[string]any{"remote": map[string]any{"user_id": "yan"}}})
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

// fakeOwner is an owner that serves room ops to b's link. It answers a
// request that handle takes with the reply's result or error that handle
// returns, or closes the connection for a nil reply. write sends a frame
// to the mirror.
func fakeOwner(t *testing.T, b *federated, handle func(method string, params map[string]any, write func(any)) (reply map[string]any, took bool)) (address string) {
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
			if handled, took := handle(frame.Method, frame.Params, write); took {
				if handled == nil {
					return
				}
				handled["id"] = frame.ID
				write(handled)
				continue
			}
			switch frame.Method {
			case "auth":
				reply["result"] = map[string]any{"you": map[string]any{"user_id": "link_1", "ext": map[string]any{"remote": map[string]any{"server": b.self}}}}
			case "room_list":
				reply["result"] = map[string]any{"joined": []any{map[string]any{"room_id": "ops", "title": "Ops", "latest_log_id": "100"}}}
			case "me":
				reply["result"] = map[string]any{"you": map[string]any{"user_id": "bob@b"}}
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
	address := fakeOwner(t, b, func(method string, params map[string]any, write func(any)) (map[string]any, bool) {
		if method != "message" {
			return nil, false
		}
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
		return map[string]any{"result": map[string]any{"message_id": id}}, true
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
	address := fakeOwner(t, b, func(method string, _ map[string]any, _ func(any)) (map[string]any, bool) {
		if method != "message" {
			return nil, false
		}
		return map[string]any{"error": map[string]any{"code": CodeDenied, "message": "not joined", "data": map[string]any{"ext": map[string]any{"remote": "not_joined"}}}}, true
	})
	mirrorRoom, bob := connectFake(t, b, address)
	refusedAfterNotes(t, bob, "message", "m1", map[string]any{"room_id": mirrorRoom, "body": map[string]any{"text": "hello?"}}, CodeDenied)
	b.app.fed.mu.Lock()
	unshared := b.app.fed.rooms[mirrorRoom].Unshared
	b.app.fed.mu.Unlock()
	if !unshared {
		t.Fatal("the mirror room is still shared")
	}
}

// A join, leave, or rename that the owner did not answer stays queued,
// kept in the KV, and goes on the next connection; a newer change of the
// same user and room replaces an older one.
func TestMirrorResendsMemberships(t *testing.T) {
	b := startFederated(t, 0)
	var dropping atomic.Bool
	dropping.Store(true)
	var mu sync.Mutex
	got := make(map[string]string) // user_id → method
	address := fakeOwner(t, b, func(method string, params map[string]any, _ func(any)) (map[string]any, bool) {
		if method != "room_join" && method != "room_leave" && method != "me" {
			return nil, false
		}
		if dropping.Load() {
			return nil, true // the connection closes before the answer
		}
		user := fmt.Sprint(remoteExtOf(params)["user_id"])
		mu.Lock()
		defer mu.Unlock()
		if method == "me" {
			got["name of "+user] = fmt.Sprint(params["name"])
			return map[string]any{"result": map[string]any{"you": map[string]any{"user_id": user + "@b"}}}, true
		}
		got[user] = method
		return map[string]any{"result": map[string]any{}}, true
	})
	mirrorRoom, bob := connectFake(t, b, address)
	carol := dialTestClient(t, b.http)
	b.call(t, "room_join", map[string]any{"room_id": mirrorRoom, "user_id": carol.userID})
	carol.request(t, "room_leave", "l1", map[string]any{"room_id": mirrorRoom})
	bob.request(t, "me", "n1", map[string]any{"name": "Robert"})
	server, _ := normalizeAddress(address)
	queued := func() []membershipChange {
		var changes []membershipChange
		_, _ = b.app.fed.kv.Get("memberships "+server, &changes)
		return changes
	}
	eventually(t, "the kept queue", func() error {
		var users []string
		for _, change := range queued() {
			users = append(users, fmt.Sprintf("%s:%v:%v", change.UserID, change.Joined, change.Profile))
		}
		if want := []string{"admin:true:false", bob.userID + ":true:false", carol.userID + ":false:false", bob.userID + ":false:true"}; !slices.Equal(users, want) {
			return fmt.Errorf("%v, want %v", users, want)
		}
		return nil
	})
	dropping.Store(false)
	eventually(t, "the joins on the owner", func() error {
		mu.Lock()
		defer mu.Unlock()
		if got["admin"] != "room_join" || got[bob.userID] != "room_join" || got["name of "+bob.userID] != "Robert" || len(queued()) != 0 {
			return fmt.Errorf("owner %v, queue %v", got, queued())
		}
		return nil
	})
	// Carol never reached the owner: her leave needs no request.
	mu.Lock()
	defer mu.Unlock()
	if _, sent := got[carol.userID]; sent {
		t.Fatalf("carol's change was sent: %v", got)
	}
}

// A link's wake push is a relay with a token, on the link's own server,
// and is sent with the bearer token and §4.9.2's TTL and Urgency.
func TestWakePush(t *testing.T) {
	a := startFederated(t, 0)
	pushes := make(chan *http.Request, 8)
	var gone atomic.Bool
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pushes <- r
		if gone.Load() {
			w.WriteHeader(http.StatusGone)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer relay.Close()
	host := strings.TrimPrefix(relay.URL, "http://")
	room := a.call(t, "room_set", map[string]any{"title": "Ops"})["room_id"].(string)
	invite := inviteInNotice.FindStringSubmatch(a.command(t, room, "/remote invite ws://"+host+"/ws"))
	_, _, token, err := parseInvite(invite[1])
	if err != nil {
		t.Fatal(err)
	}
	link, _ := dialRaw(t, a.http)
	link.result(t, "auth", "a1", map[string]any{"scheme": "token", "token": token})
	link.drain(t)
	register := map[string]any{"kind": "relay", "url": relay.URL + "/remote/wake/x", "wake": []any{"ext:remote"}}
	link.expectError(t, "push_register", "w1", register, CodeInvalidParams)
	register["token"] = "secret"
	link.result(t, "push_register", "w2", register)
	a.call(t, "message", map[string]any{"room_id": room, "body": map[string]any{"text": "wake"}})
	select {
	case push := <-pushes:
		if push.Method != http.MethodPost || push.Header.Get("Authorization") != "Bearer secret" || push.Header.Get("TTL") != "86400" ||
			push.Header.Get("Urgency") != "low" || push.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("push %s %v", push.Method, push.Header)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no wake push")
	}
	// A URL that answers 410 is gone: its registration is removed.
	gone.Store(true)
	a.call(t, "message", map[string]any{"room_id": room, "body": map[string]any{"text": "wake again"}})
	select {
	case <-pushes:
	case <-time.After(10 * time.Second):
		t.Fatal("no second wake push")
	}
	eventually(t, "the removal", func() error {
		a.app.fed.mu.Lock()
		defer a.app.fed.mu.Unlock()
		if len(a.app.fed.wakes) != 0 {
			return fmt.Errorf("%d registrations", len(a.app.fed.wakes))
		}
		return nil
	})
}

// A membership record of the link's own leave stops the mirror room.
func TestMirrorLinkLeaveRecord(t *testing.T) {
	b := startFederated(t, 0)
	address := fakeOwner(t, b, func(method string, _ map[string]any, write func(any)) (map[string]any, bool) {
		if method != "message" {
			return nil, false
		}
		write(map[string]any{"method": "room_update", "params": map[string]any{"memberships": []any{map[string]any{
			"log_id": "200", "room_id": "ops", "members": []any{map[string]any{
				"user": map[string]any{"user_id": "link_1", "ext": map[string]any{"remote": map[string]any{"server": b.self}}}, "joined": false,
			}},
		}}}})
		return map[string]any{"error": map[string]any{"code": CodeDenied, "message": "no"}}, true
	})
	mirrorRoom, bob := connectFake(t, b, address)
	refusedAfterNotes(t, bob, "message", "m1", map[string]any{"room_id": mirrorRoom, "body": map[string]any{"text": "hello?"}}, CodeDenied)
	eventually(t, "the unshare", func() error {
		b.app.fed.mu.Lock()
		defer b.app.fed.mu.Unlock()
		if !b.app.fed.rooms[mirrorRoom].Unshared {
			return fmt.Errorf("still shared")
		}
		return nil
	})
}

// A copy keeps its original apart from its ext: after a restart, an
// owner's restore of a deleted message reaches the same copy.
func TestMirrorRestoreAfterRestart(t *testing.T) {
	memory := store.NewMemory()
	a := startFederated(t, 0)
	b := startFederatedWith(t, 0, nil, memory)
	ownerRoom := a.call(t, "room_set", map[string]any{"title": "Ops"})["room_id"].(string)
	mirrorRoom := share(t, a, b, ownerRoom)
	id := a.call(t, "message", map[string]any{"room_id": ownerRoom, "body": map[string]any{"text": "oops"}})["message_id"].(string)
	var copyID string
	eventually(t, "the copy", func() error {
		if m := copyIn(b.app, mirrorRoom, id); m != nil {
			copyID = m["message_id"].(string)
			return nil
		}
		return fmt.Errorf("none")
	})
	a.call(t, "message", map[string]any{"room_id": ownerRoom, "message_id": id, "deleted": true})
	eventually(t, "the delete", func() error {
		if snapshotOf(b.app, copyID)["deleted"] != true {
			return fmt.Errorf("not deleted")
		}
		return nil
	})
	address := b.http.Listener.Addr().String()
	b.stop()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	b = startFederatedWith(t, 0, listener, memory)
	a.call(t, "message", map[string]any{"room_id": ownerRoom, "message_id": id, "body": map[string]any{"text": "restored"}})
	eventually(t, "the restore", func() error {
		if text := wstestText(snapshotOf(b.app, copyID)); text != "restored" {
			return fmt.Errorf("%q", text)
		}
		return nil
	})
	if m := copyIn(b.app, mirrorRoom, id); m == nil || m["message_id"] != copyID {
		t.Fatalf("copy %#v, want %s", m, copyID)
	}
}

// A mirror thread's first members come with the link's join, also when the
// thread's updated record came first.
func TestMirrorThreadMembers(t *testing.T) {
	b := startFederated(t, 0)
	address := fakeOwner(t, b, func(method string, _ map[string]any, write func(any)) (map[string]any, bool) {
		if method != "message" {
			return nil, false
		}
		thread := map[string]any{"room_id": "t1", "log_id": "300", "parent_room_id": "ops", "title": "Deploy", "latest_log_id": "301"}
		write(map[string]any{"method": "room_update", "params": map[string]any{"updated": []any{thread}}})
		joined := maps.Clone(thread)
		joined["members"] = []any{map[string]any{"user_id": "carol"}, map[string]any{"user_id": "link_1"}}
		write(map[string]any{"method": "room_update", "params": map[string]any{"joined": []any{joined}, "users": []any{
			map[string]any{"user_id": "carol", "name": "Carol"},
			map[string]any{"user_id": "link_1", "ext": map[string]any{"remote": map[string]any{"server": b.self}}},
		}}})
		return map[string]any{"error": map[string]any{"code": CodeDenied, "message": "no"}}, true
	})
	mirrorRoom, bob := connectFake(t, b, address)
	refusedAfterNotes(t, bob, "message", "m1", map[string]any{"room_id": mirrorRoom, "body": map[string]any{"text": "go"}}, CodeDenied)
	carol := "carol@" + authorityOf(address)
	eventually(t, "the thread's members", func() error {
		b.app.mu.RLock()
		defer b.app.mu.RUnlock()
		for _, r := range b.app.rooms {
			if r.parent != nil && r.parent.id == mirrorRoom {
				if r.members[carol] == nil || len(r.members) != 1 {
					return fmt.Errorf("members %v", slices.Collect(maps.Keys(r.members)))
				}
				return nil
			}
		}
		return fmt.Errorf("no thread")
	})
}

// A user object of the owner's without remote is known by its user_id's
// form: no remote user is made for an ID that has an @.
func TestMirrorUserObjectsWithoutRemote(t *testing.T) {
	b := startFederated(t, 0)
	var bobOnOwner string
	address := fakeOwner(t, b, func(method string, _ map[string]any, write func(any)) (map[string]any, bool) {
		if method != "message" {
			return nil, false
		}
		for _, id := range []string{bobOnOwner, "x@c.example", "zed"} {
			write(map[string]any{"method": "user", "params": map[string]any{"new": map[string]any{"user_id": id, "name": "Renamed " + id}}})
		}
		return map[string]any{"error": map[string]any{"code": CodeDenied, "message": "no"}}, true
	})
	mirrorRoom, bob := connectFake(t, b, address)
	bobOnOwner = bob.userID + "@" + authorityOf(b.self)
	refusedAfterNotes(t, bob, "message", "m1", map[string]any{"room_id": mirrorRoom, "body": map[string]any{"text": "go"}}, CodeDenied)
	zed := "zed@" + authorityOf(address)
	eventually(t, "the owner's user", func() error {
		b.app.mu.RLock()
		defer b.app.mu.RUnlock()
		if u := b.app.users[zed]; u == nil || u.name != "Renamed zed" {
			return fmt.Errorf("no %s", zed)
		}
		return nil
	})
	b.app.mu.RLock()
	defer b.app.mu.RUnlock()
	for id := range b.app.users {
		if strings.Count(id, "@") > 1 {
			t.Errorf("a remote user %s", id)
		}
	}
}

// A second /remote connect of a room that is mirrored here already is
// refused.
func TestMirrorConnectTwice(t *testing.T) {
	a, b, ownerRoom, mirrorRoom := mirrored(t, 0)
	invite := inviteInNotice.FindStringSubmatch(a.command(t, ownerRoom, "/remote invite "+b.self))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := b.admin.Call(ctx, "command", map[string]any{"room_id": "general", "body": map[string]any{"text": "/remote connect " + invite[1]}})
	if e, ok := errors.AsType[*Error](err); !ok || e.Code != CodeInvalidParams || !strings.Contains(e.Message, mirrorRoom) {
		t.Fatalf("a second connect: %v", err)
	}
}

// A record of a newly connected room that the open connection brought
// before the mirror room existed is read again, once.
func TestMirrorCatchesUpANewRoom(t *testing.T) {
	b := startFederated(t, 0)
	var mu sync.Mutex
	var live func(any)
	message := func(id, text string) map[string]any {
		return map[string]any{"method": "message", "params": map[string]any{
			"message_id": id, "log_id": id, "room_id": "ops2", "from": map[string]any{"user_id": "carol"}, "body": map[string]any{"text": text},
		}}
	}
	address := fakeOwner(t, b, func(method string, params map[string]any, write func(any)) (map[string]any, bool) {
		switch {
		case method == "history" && params["after"] == "101":
			mu.Lock()
			live = write
			mu.Unlock()
		case method == "history" && params["after"] == "501":
			return map[string]any{"result": map[string]any{"messages": []any{message("501", "early")["params"]}, "last_log_id": "501"}}, true
		case method == "room_list" && params["room_id"] == "ops2":
			// The room's first record reaches the open connection first.
			mu.Lock()
			live(message("501", "early"))
			mu.Unlock()
			time.Sleep(200 * time.Millisecond)
			return map[string]any{"result": map[string]any{"joined": []any{map[string]any{"room_id": "ops2", "title": "Ops 2", "latest_log_id": "500"}}}}, true
		}
		return nil, false
	})
	connectFake(t, b, address)
	eventually(t, "the open connection", func() error {
		mu.Lock()
		defer mu.Unlock()
		if live == nil {
			return fmt.Errorf("not yet")
		}
		return nil
	})
	notice := b.command(t, "general", "/remote connect https://link.apron.chat/#server="+address+"&room=ops2&token=t")
	found := mirrorInNotice.FindStringSubmatch(notice)
	if found == nil {
		t.Fatalf("connect = %q", notice)
	}
	mu.Lock()
	live(message("502", "later"))
	mu.Unlock()
	eventually(t, "the copies", func() error {
		b.app.mu.RLock()
		defer b.app.mu.RUnlock()
		texts := make(map[string]int)
		for _, m := range b.app.messages {
			if m.roomID == found[1] {
				texts[wstestText(m.snapshot())]++
			}
		}
		if texts["early"] != 1 || texts["later"] != 1 || len(texts) != 2 {
			return fmt.Errorf("%v", texts)
		}
		return nil
	})
}

// When the owner's history starts after the mirror's place, the mirror
// reads from there.
func TestMirrorHistoryBound(t *testing.T) {
	b := startFederated(t, 0)
	address := fakeOwner(t, b, func(method string, params map[string]any, _ func(any)) (map[string]any, bool) {
		if method != "history" || params["after"] != "101" {
			return nil, false
		}
		return map[string]any{"result": map[string]any{"history_log_id": "300", "latest_log_id": "350"}}, true
	})
	connectFake(t, b, address)
	server, _ := normalizeAddress(address)
	eventually(t, "the cursor", func() error {
		b.app.fed.mu.Lock()
		ms := b.app.fed.mirrors[server]
		b.app.fed.mu.Unlock()
		ms.mu.Lock()
		defer ms.mu.Unlock()
		if ms.cursor != 299 {
			return fmt.Errorf("cursor %d", ms.cursor)
		}
		return nil
	})
}

// linkOf signs in as a's link for server, invited to room, and returns a
// function that sends a request as it and returns its reply.
func linkOf(t *testing.T, a *federated, room, server string) func(id, method string, params map[string]any) map[string]any {
	t.Helper()
	invite := inviteInNotice.FindStringSubmatch(a.command(t, room, "/remote invite "+server))
	_, _, token, err := parseInvite(invite[1])
	if err != nil {
		t.Fatal(err)
	}
	link, _ := dialRaw(t, a.http)
	link.result(t, "auth", "a1", map[string]any{"scheme": "token", "token": token})
	link.drain(t)
	return func(id, method string, params map[string]any) map[string]any {
		link.write(t, map[string]any{"method": method, "id": id, "params": params})
		for {
			if frame := link.read(t); frame["id"] == id {
				return frame
			}
		}
	}
}

// errorOf is a reply's error, its code, and its data.ext.remote.
func errorOf(frame map[string]any) (failure map[string]any, code any, reason any) {
	failure, _ = frame["error"].(map[string]any)
	data, _ := failure["data"].(map[string]any)
	ext, _ := data["ext"].(map[string]any)
	return failure, failure["code"], ext["remote"]
}

// A link's room_set creates or saves a thread of a room it joined, as a
// remote user, and nothing else: anything else is denied, and a room it
// cannot see unknown. The thread a remote user creates is one every member
// of the room sees, whatever private says.
func TestLinkRoomSet(t *testing.T) {
	a := startFederated(t, 0)
	const other = "ws://other.test:9000/ws"
	room := a.call(t, "room_set", map[string]any{"title": "Ops", "private": true})["room_id"].(string)
	elsewhere := a.call(t, "room_set", map[string]any{"title": "Elsewhere"})["room_id"].(string)
	hidden := a.call(t, "room_set", map[string]any{"title": "Hidden", "private": true})["room_id"].(string)
	member := dialTestClient(t, a.http)
	a.call(t, "room_join", map[string]any{"room_id": room, "user_id": member.userID})
	act := linkOf(t, a, room, other)
	asYan := map[string]any{"remote": map[string]any{"user_id": "yan"}}
	for _, tc := range []struct {
		id     string
		params map[string]any
		code   float64
		reason any
	}{
		// The link's own room_set, of anything.
		{"own1", map[string]any{"title": "mine"}, CodeDenied, nil},
		{"own2", map[string]any{"parent_room_id": room, "title": "mine"}, CodeDenied, nil},
		// No thread: neither room_id nor parent_room_id, or a room that is
		// no thread, joined or not.
		{"top", map[string]any{"title": "top", "ext": asYan}, CodeDenied, nil},
		{"save", map[string]any{"room_id": room, "title": "renamed", "ext": asYan}, CodeDenied, nil},
		{"other", map[string]any{"room_id": elsewhere, "title": "renamed", "ext": asYan}, CodeDenied, nil},
		// A thread of a room the link has not joined.
		{"under", map[string]any{"parent_room_id": elsewhere, "title": "t", "ext": asYan}, CodeDenied, "not_joined"},
		{"unseen", map[string]any{"room_id": hidden, "title": "t", "ext": asYan}, CodeInvalidParams, "not_joined"},
		{"unseen2", map[string]any{"parent_room_id": hidden, "title": "t", "ext": asYan}, CodeInvalidParams, "not_joined"},
	} {
		frame := act(tc.id, "room_set", tc.params)
		if _, code, reason := errorOf(frame); code != tc.code || reason != tc.reason {
			t.Errorf("%s: %#v, want error %v with %v", tc.id, frame, tc.code, tc.reason)
		}
	}
	a.app.mu.RLock()
	titles := []string{a.app.rooms[room].title(), a.app.rooms[elsewhere].title(), a.app.rooms[hidden].title()}
	a.app.mu.RUnlock()
	if !slices.Equal(titles, []string{"Ops", "Elsewhere", "Hidden"}) {
		t.Fatalf("titles %v", titles)
	}
	// A thread of a private room, asked private: every member sees it.
	made := act("make", "room_set", map[string]any{"parent_room_id": room, "title": "far thread", "private": true, "ext": asYan})
	thread, _ := made["result"].(map[string]any)["room_id"].(string)
	saved := act("resave", "room_set", map[string]any{"room_id": thread, "title": "far thread 2", "private": true, "ext": asYan})
	if saved["result"] == nil {
		t.Fatalf("a save of the thread = %#v", saved)
	}
	a.app.mu.RLock()
	defer a.app.mu.RUnlock()
	r := a.app.rooms[thread]
	if r == nil || r.private || r.title() != "far thread 2" || r.members[member.userID] != nil || !r.visibleTo(a.app.users[member.userID]) {
		t.Fatalf("the thread %#v is not one every member of the room sees", r)
	}
}

// replyAfterNotes sends a request and returns its reply, skipping the
// notifications that come before it.
func replyAfterNotes(t *testing.T, c *testClient, method, id string, params map[string]any) map[string]any {
	t.Helper()
	c.write(t, map[string]any{"method": method, "id": id, "params": params})
	for {
		if frame := c.read(t); frame["id"] == id {
			return frame
		}
	}
}

// mirrorThreadOf is the mirror thread here of an owner's thread.
func mirrorThreadOf(b *federated, ownerThread string) *roomState {
	b.app.fed.mu.Lock()
	var id string
	for local, room := range b.app.fed.rooms {
		if room.OwnerRoom == ownerThread && room.Parent != "" {
			id = local
		}
	}
	b.app.fed.mu.Unlock()
	b.app.mu.RLock()
	defer b.app.mu.RUnlock()
	return b.app.rooms[id]
}

// Each later record of the owner's thread sets the mirror thread's title
// and description; the mirror room keeps its own.
func TestMirrorThreadTitles(t *testing.T) {
	a, b, ownerRoom, mirrorRoom := mirrored(t, 0)
	thread := a.call(t, "room_set", map[string]any{"parent_room_id": ownerRoom, "title": "Deploy", "description": "Monday", "private": false})["room_id"].(string)
	titled := func(title, description string) func() error {
		return func() error {
			r := mirrorThreadOf(b, thread)
			if r == nil {
				return fmt.Errorf("no mirror thread")
			}
			b.app.mu.RLock()
			defer b.app.mu.RUnlock()
			if r.title() != title || r.record["description"] != description {
				return fmt.Errorf("title %q, description %v", r.title(), r.record["description"])
			}
			return nil
		}
	}
	eventually(t, "the mirror thread", titled("Deploy", "Monday"))
	a.call(t, "room_set", map[string]any{"room_id": thread, "title": "Deploy v2", "description": "Tuesday"})
	eventually(t, "the thread's new title", titled("Deploy v2", "Tuesday"))
	// The owner's room changes, then its thread: the mirror room keeps its
	// title and description, while the thread follows.
	a.call(t, "room_set", map[string]any{"room_id": ownerRoom, "title": "Ops renamed", "description": "Elsewhere"})
	a.call(t, "room_set", map[string]any{"room_id": thread, "title": "Deploy v3", "description": "Wednesday"})
	eventually(t, "the thread's third title", titled("Deploy v3", "Wednesday"))
	b.app.mu.RLock()
	r := b.app.rooms[mirrorRoom]
	title, description := r.title(), r.record["description"]
	b.app.mu.RUnlock()
	if title != "Ops" || description != "Deploys" {
		t.Fatalf("the mirror room took the owner's change: %q, %v", title, description)
	}
}

// The owner's removal of a mirror's own user is not copied into the mirror
// room: the user stays, and its next write gets the owner's error, with its
// code, message, and data.
func TestMirrorOwnerRemovesMirrorUser(t *testing.T) {
	a, b, ownerRoom, mirrorRoom := mirrored(t, 0)
	bob := dialTestClient(t, b.http)
	b.call(t, "room_join", map[string]any{"room_id": mirrorRoom, "user_id": bob.userID})
	bobOnA := bob.userID + "@" + authorityOf(b.self)
	eventually(t, "bob on the owner", func() error {
		a.app.mu.RLock()
		defer a.app.mu.RUnlock()
		if a.app.rooms[ownerRoom].members[bobOnA] == nil {
			return fmt.Errorf("not there")
		}
		return nil
	})
	a.call(t, "room_leave", map[string]any{"room_id": ownerRoom, "user_id": bobOnA})
	// A later record of the owner's: the removal's came before it.
	later := a.call(t, "message", map[string]any{"room_id": ownerRoom, "body": map[string]any{"text": "after the removal"}})["message_id"].(string)
	eventually(t, "the later copy", func() error {
		if copyIn(b.app, mirrorRoom, later) == nil {
			return fmt.Errorf("none")
		}
		return nil
	})
	b.app.mu.RLock()
	stayed := b.app.rooms[mirrorRoom].members[bob.userID] != nil
	b.app.mu.RUnlock()
	if !stayed {
		t.Fatal("the owner's removal of bob was copied")
	}
	bob.drain(t)
	frame := replyAfterNotes(t, bob, "message", "m1", map[string]any{"room_id": mirrorRoom, "body": map[string]any{"text": "still here?"}})
	failure, code, reason := errorOf(frame)
	if code != float64(CodeDenied) || reason != "removed" || !strings.Contains(fmt.Sprint(failure["message"]), bobOnA+" was removed") {
		t.Fatalf("bob's write = %#v", frame)
	}
	// Still in the mirror room, which is still shared.
	b.app.fed.mu.Lock()
	unshared := b.app.fed.rooms[mirrorRoom].Unshared
	b.app.fed.mu.Unlock()
	if unshared {
		t.Fatal("the removal stopped the mirror room")
	}
}

// The owner's error reaches the mirror's user as it is: code, message, and
// data.
func TestMirrorPassesOwnerErrors(t *testing.T) {
	b := startFederated(t, 0)
	data := map[string]any{"limit": float64(3), "ext": map[string]any{"remote": "removed", "other": []any{"x"}}}
	address := fakeOwner(t, b, func(method string, _ map[string]any, _ func(any)) (map[string]any, bool) {
		if method != "message" {
			return nil, false
		}
		return map[string]any{"error": map[string]any{"code": CodeTooLarge, "message": "The owner says no", "data": data}}, true
	})
	mirrorRoom, bob := connectFake(t, b, address)
	frame := replyAfterNotes(t, bob, "message", "m1", map[string]any{"room_id": mirrorRoom, "body": map[string]any{"text": "hello?"}})
	failure, code, _ := errorOf(frame)
	if code != float64(CodeTooLarge) || failure["message"] != "The owner says no" || fmt.Sprint(failure["data"]) != fmt.Sprint(data) {
		t.Fatalf("bob's error = %#v", frame)
	}
}

// A wake push covers every record committed before it is sent, and a
// record after it gets another push: none waits for the mirror's poll.
func TestWakePushCoversRecords(t *testing.T) {
	a := startFederated(t, 0)
	room := a.call(t, "room_set", map[string]any{"title": "Ops"})["room_id"].(string)
	// Each push carries the messages of the room when it arrived.
	pushes := make(chan map[string]bool, 16)
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen := make(map[string]bool)
		a.app.mu.RLock()
		for id, m := range a.app.messages {
			if m.roomID == room {
				seen[id] = true
			}
		}
		a.app.mu.RUnlock()
		pushes <- seen
		w.WriteHeader(http.StatusNoContent)
	}))
	defer relay.Close()
	act := linkOf(t, a, room, "ws://"+strings.TrimPrefix(relay.URL, "http://")+"/ws")
	if frame := act("w1", "push_register", map[string]any{"kind": "relay", "url": relay.URL + "/remote/wake/x", "token": "secret", "wake": []any{"ext:remote"}}); frame["result"] == nil {
		t.Fatalf("register = %#v", frame)
	}
	post := func(text string) string {
		return a.call(t, "message", map[string]any{"room_id": room, "body": map[string]any{"text": text}})["message_id"].(string)
	}
	// covered waits for a push that covers a record.
	covered := func(id string) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case seen := <-pushes:
				if seen[id] {
					return
				}
			case <-deadline:
				t.Fatalf("no push covers %s", id)
			}
		}
	}
	covered(post("first"))
	// Records right after a push, while the next waits: one push after
	// them covers them all.
	post("second")
	covered(post("third"))
	// And a record after that push gets one of its own.
	covered(post("fourth"))
}
