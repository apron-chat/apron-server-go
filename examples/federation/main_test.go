package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/internal/wstest"
	"github.com/apron-chat/apron-server-go/store"
)

// selfAddress is the address of the bridge's server (A), as B knows it.
const selfAddress = "ws://a.test:8081/ws"

// modExtension gives A a moderator bot, mod.
type modExtension struct{ bot **apron.Bot }

func (modExtension) Name() string { return "mod" }
func (m modExtension) Init(r *apron.Registrar) error {
	var err error
	*m.bot, err = r.Bot("mod", apron.BotOptions{Name: "Mod"})
	return err
}

// startA opens server A with the bridge on a store, with self as
// --federation.self, and returns its moderator bot, its server, and a
// function that shuts it down.
func startA(t *testing.T, memory store.Store, link, self string) (*apron.Bot, *httptest.Server, func()) {
	t.Helper()
	f := &Federation{}
	f.Settings.Link, f.Settings.Room, f.Settings.Self = link, "ops", self
	var mod *apron.Bot
	config := apron.DefaultConfig()
	config.Store = memory
	config.Roles = map[string][]string{"moderator": {"mod"}}
	config.Extensions = []apron.Extension{f, modExtension{&mod}}
	app, err := apron.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app.Handler())
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := app.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		server.Close()
	}
	t.Cleanup(stop)
	return mod, server, stop
}

// person signs in to A as a guest with this user_id and joins ops.
func person(t *testing.T, server *httptest.Server, userID, name string) *wstest.Client {
	t.Helper()
	c, _ := wstest.Dial(t, server)
	result, err := c.Call("auth", map[string]any{"scheme": "guest", "user_id": userID, "name": name})
	if err != nil {
		t.Fatal(err)
	}
	if c.UserID = result["you"].(map[string]any)["user_id"].(string); c.UserID != userID {
		t.Fatalf("signed in as %s", c.UserID)
	}
	if _, err := c.Call("room_join", map[string]any{"room_id": "ops"}); err != nil {
		t.Fatal(err)
	}
	return c
}

// eventually retries check until it returns "" or time runs out.
func eventually(t *testing.T, what string, check func() string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		problem := check()
		if problem == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %s", what, problem)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// opsMessages reads ops on A, as c, for the current snapshot of each
// message, and the reactions by message and user.
func opsMessages(t *testing.T, c *wstest.Client) (map[string]map[string]any, map[string]map[string][]any) {
	t.Helper()
	result, err := c.Call("history", map[string]any{"room_id": "ops", "limit": 1000})
	if err != nil {
		t.Fatal(err)
	}
	messages := make(map[string]map[string]any)
	for _, value := range asList(result["messages"]) {
		m := value.(map[string]any)
		messages[m["message_id"].(string)] = m
	}
	reactions := make(map[string]map[string][]any)
	for _, value := range asList(result["reactions"]) {
		r := value.(map[string]any)
		id := r["message_id"].(string)
		if reactions[id] == nil {
			reactions[id] = make(map[string][]any)
		}
		for _, set := range asList(r["reactions"]) {
			set := set.(map[string]any)
			reactions[id][set["from"].(map[string]any)["user_id"].(string)] = asList(set["emojis"])
		}
	}
	return messages, reactions
}

// copyOf finds A's copy of B's message, by its ext.remote.
func copyOf(messages map[string]map[string]any, remoteID string) map[string]any {
	for _, m := range messages {
		if remote := remoteExtOf(m); remote != nil && remote["message_id"] == remoteID {
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

func text(m map[string]any) string {
	body, _ := m["body"].(map[string]any)
	s, _ := body["text"].(string)
	return s
}

func mentionsOf(m map[string]any) []any {
	body, _ := m["body"].(map[string]any)
	return asList(body["mentions"])
}

func replyOf(m map[string]any) string {
	ref, _ := m["reply_to"].(map[string]any)
	s, _ := ref["message_id"].(string)
	return s
}

func TestFederation(t *testing.T) {
	b := newFakeB(t, selfAddress, "tok")
	b.addUser("bob", "Bob")
	bobOnA := "bob@" + b.authority()
	aliceOnB := "alice@a.test:8081"
	// What B's room held before the bridge first started stays on B.
	before := b.post("bob", "before the bridge", nil, "")
	b.rotateTo = "tok2"

	memory := store.NewMemory()
	link := "https://link.apron.chat/#server=" + b.address() + "&room=r1&token=tok"
	// Without --federation.self, the bridge reads the address B knows A
	// by from the link's ext.remote.
	_, server, stop := startA(t, memory, link, "")
	alice := person(t, server, "alice", "Alice")

	// A → B: Alice's message goes to B as her shadow, which the bridge
	// names first.
	hello, err := alice.Post("ops", "hello from A")
	if err != nil {
		t.Fatal(err)
	}
	var helloOnB map[string]any
	eventually(t, "Alice's message on B", func() string {
		found := b.find(func(m map[string]any) bool { return remoteExtOf(m)["message_id"] == hello })
		if len(found) != 1 {
			return fmt.Sprintf("%d copies", len(found))
		}
		helloOnB = found[0]
		return ""
	})
	if from := helloOnB["from"].(map[string]any); from["user_id"] != aliceOnB || from["name"] != "Alice" {
		t.Fatalf("copy from = %#v", from)
	}
	wantRemote := map[string]any{"server": selfAddress, "user_id": "alice", "room_id": "ops", "message_id": hello}
	if !reflect.DeepEqual(remoteExtOf(helloOnB), wantRemote) {
		t.Fatalf("copy ext.remote = %#v", remoteExtOf(helloOnB))
	}
	helloB := helloOnB["message_id"].(string)

	// B → A: Bob replies to the copy, mentioning Alice's shadow there.
	reply := b.post("bob", "@"+aliceOnB+" hi back, cc @nobody", []string{aliceOnB}, helloB)
	var replyOnA map[string]any
	eventually(t, "Bob's reply on A", func() string {
		messages, _ := opsMessages(t, alice)
		if replyOnA = copyOf(messages, reply); replyOnA == nil {
			return "no copy"
		}
		return ""
	})
	from := replyOnA["from"].(map[string]any)
	if from["user_id"] != bobOnA || from["name"] != "Bob" || remoteExtOf(from)["user_id"] != "bob" {
		t.Fatalf("Bob's copy from = %#v", from)
	}
	if text(replyOnA) != "@alice hi back, cc @nobody" || !reflect.DeepEqual(mentionsOf(replyOnA), []any{"alice"}) || replyOf(replyOnA) != hello {
		t.Fatalf("Bob's copy = %#v", replyOnA)
	}
	if !reflect.DeepEqual(remoteExtOf(replyOnA), map[string]any{"server": b.address(), "user_id": "bob", "room_id": "r1", "message_id": reply}) {
		t.Fatalf("Bob's copy ext.remote = %#v", remoteExtOf(replyOnA))
	}
	replyA := replyOnA["message_id"].(string)

	// A → B: Alice answers Bob's copy, mentioning him by his shadow here.
	answer, err := alice.Call("message", map[string]any{"room_id": "ops", "reply_to": map[string]any{"message_id": replyA},
		"body": map[string]any{"text": "@" + bobOnA + ": thanks!", "format": "markdown", "mentions": []any{bobOnA}}})
	if err != nil {
		t.Fatal(err)
	}
	answerA := answer["message_id"].(string)
	var answerOnB map[string]any
	eventually(t, "Alice's answer on B", func() string {
		found := b.find(func(m map[string]any) bool { return remoteExtOf(m)["message_id"] == answerA })
		if len(found) != 1 {
			return "no copy"
		}
		answerOnB = found[0]
		return ""
	})
	if text(answerOnB) != "@bob: thanks!" || !reflect.DeepEqual(mentionsOf(answerOnB), []any{"bob"}) || replyOf(answerOnB) != reply || answerOnB["body"].(map[string]any)["format"] != "markdown" {
		t.Fatalf("Alice's answer on B = %#v", answerOnB)
	}

	// Edits, both ways.
	if _, err := alice.Call("message", map[string]any{"room_id": "ops", "message_id": hello, "body": map[string]any{"text": "hello from A (edited)"}}); err != nil {
		t.Fatal(err)
	}
	b.edit("bob", reply, "hi back (edited)")
	eventually(t, "the edits", func() string {
		messages, _ := opsMessages(t, alice)
		switch {
		case text(b.snapshot(helloB)) != "hello from A (edited)":
			return "B's copy is " + text(b.snapshot(helloB))
		case text(messages[replyA]) != "hi back (edited)":
			return "A's copy is " + text(messages[replyA])
		}
		return ""
	})
	if !reflect.DeepEqual(remoteExtOf(b.snapshot(helloB)), wantRemote) {
		t.Fatalf("the edited copy's ext.remote = %#v", remoteExtOf(b.snapshot(helloB)))
	}

	// Reactions, both ways, as each user's whole set.
	if _, err := alice.Call("reactions", map[string]any{"message_id": replyA, "emojis": []any{"👍", "🎉"}}); err != nil {
		t.Fatal(err)
	}
	b.react("bob", helloB, "❤️")
	eventually(t, "the reactions", func() string {
		_, reactions := opsMessages(t, alice)
		switch {
		case !slices.Equal(b.reactionsOf(reply, aliceOnB), []string{"👍", "🎉"}):
			return fmt.Sprintf("Alice's on B: %v", b.reactionsOf(reply, aliceOnB))
		case !reflect.DeepEqual(reactions[hello][bobOnA], []any{"❤️"}):
			return fmt.Sprintf("Bob's on A: %v", reactions[hello])
		}
		return ""
	})

	// Profiles, both ways.
	if _, err := alice.Call("me", map[string]any{"name": "Alice L"}); err != nil {
		t.Fatal(err)
	}
	b.rename("bob", "Robert")
	eventually(t, "the names", func() string {
		shadow, _ := b.user(aliceOnB)
		result, err := alice.Call("room_list", map[string]any{"room_id": "ops", "members": true})
		if err != nil {
			return err.Error()
		}
		name := ""
		for _, u := range asList(result["users"]) {
			if u := u.(map[string]any); u["user_id"] == bobOnA {
				name, _ = u["name"].(string)
			}
		}
		switch {
		case shadow.name != "Alice L":
			return "Alice's shadow on B is " + shadow.name
		case name != "Robert":
			return "Bob's shadow on A is " + name
		}
		return ""
	})

	// While B is away, both sides go on; the bridge catches up.
	b.drop()
	away := b.post("bob", "while you were away", nil, "")
	awayA, err := alice.Post("ops", "meanwhile on A")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the catch-up", func() string {
		messages, _ := opsMessages(t, alice)
		switch {
		case copyOf(messages, away) == nil:
			return "Bob's message is not on A"
		case len(b.find(func(m map[string]any) bool { return remoteExtOf(m)["message_id"] == awayA })) != 1:
			return "Alice's message is not on B"
		}
		return ""
	})

	// Deletes, both ways.
	b.remove("bob", reply)
	if _, err := alice.Call("message", map[string]any{"room_id": "ops", "message_id": answerA, "deleted": true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the deletes", func() string {
		messages, _ := opsMessages(t, alice)
		switch {
		case messages[replyA]["deleted"] != true:
			return "Bob's copy on A is not deleted"
		case b.snapshot(answerOnB["message_id"].(string))["deleted"] != true:
			return "Alice's copy on B is not deleted"
		}
		return ""
	})

	// Nothing came back around: B got no copy of a copy, and A has one copy
	// of each of Bob's messages, and none of the one from before.
	for _, request := range b.linkRequests() {
		params, _ := request["params"].(map[string]any)
		if actor := remoteExtOf(params)["user_id"]; actor != "alice" {
			t.Errorf("the link acted as %v: %#v", actor, request)
		}
	}
	messages, _ := opsMessages(t, alice)
	copies := 0
	for _, m := range messages {
		if m["from"].(map[string]any)["user_id"] == bobOnA {
			copies++
		}
		if remoteExtOf(m)["message_id"] == before {
			t.Error("the bridge copied what B had before it started")
		}
	}
	if copies != 2 {
		t.Errorf("A has %d messages by Bob's shadow, want 2", copies)
	}
	if n := len(b.find(func(m map[string]any) bool { return m["from"].(map[string]any)["user_id"] == aliceOnB })); n != 3 {
		t.Errorf("B has %d copies, want 3", n)
	}
	// The bridge kept the token B rotated.
	b.mu.Lock()
	delete(b.tokens, "tok")
	b.mu.Unlock()

	// After a restart, the bridge finds its copies again: an edit saves
	// the copy rather than making another, both ways.
	stop()
	mod, server, _ := startA(t, memory, link, "")
	carol := person(t, server, "carol", "Carol")
	b.edit("bob", away, "while you were away (edited)")
	if _, err := carol.Call("message", map[string]any{"room_id": "ops", "reply_to": map[string]any{"message_id": hello}, "body": map[string]any{"text": "late reply"}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the edit after the restart", func() string {
		messages, _ := opsMessages(t, carol)
		late := b.find(func(m map[string]any) bool { return text(m) == "late reply" })
		switch {
		case text(copyOf(messages, away)) != "while you were away (edited)":
			return "A's copy is " + text(copyOf(messages, away))
		case len(late) != 1 || replyOf(late[0]) != helloB:
			return fmt.Sprintf("Carol's reply on B: %#v", late)
		}
		return ""
	})
	copies = 0
	for _, m := range func() map[string]map[string]any { m, _ := opsMessages(t, carol); return m }() {
		if remoteExtOf(m)["message_id"] == away {
			copies++
		}
	}
	if copies != 1 {
		t.Fatalf("%d copies of Bob's message after the restart", copies)
	}

	// The bridge finds the copies it made before the restart lazily: B's
	// copy of Alice's message, in its ext.remote, for Bob's reply to it,
	// and with a create that B answers as a duplicate, for Carol's
	// reaction to Alice's message.
	historiesBefore := func() int { b.mu.Lock(); defer b.mu.Unlock(); return b.histories }()
	awayB := b.find(func(m map[string]any) bool { return remoteExtOf(m)["message_id"] == awayA })[0]["message_id"].(string)
	reply2 := b.post("bob", "replying after the restart", nil, awayB)
	if _, err := carol.Call("reactions", map[string]any{"message_id": hello, "emojis": []any{"👀"}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the lookups", func() string {
		messages, _ := opsMessages(t, carol)
		switch {
		case copyOf(messages, reply2) == nil || replyOf(copyOf(messages, reply2)) != awayA:
			return fmt.Sprintf("Bob's reply on A: %#v", copyOf(messages, reply2))
		case !slices.Equal(b.reactionsOf(helloB, "carol@a.test:8081"), []string{"👀"}):
			return "Carol's reaction is not on B"
		}
		return ""
	})
	if n := len(b.find(func(m map[string]any) bool { return remoteExtOf(m)["message_id"] == hello })); n != 1 {
		t.Fatalf("%d copies of Alice's message on B after the lookup", n)
	}
	if n := func() int { b.mu.Lock(); defer b.mu.Unlock(); return b.histories }() - historiesBefore; n != 1 {
		t.Fatalf("%d history requests to look up one copy", n)
	}

	// A remote user removed from A's room is not added back: the bridge
	// stops copying their records there.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := mod.Call(ctx, "room_leave", map[string]any{"room_id": "ops", "user_id": bobOnA}); err != nil {
		t.Fatal(err)
	}
	b.addUser("dave", "Dave")
	removed := b.post("bob", "after my removal", nil, "")
	daves := b.post("dave", "dave here", nil, "")
	eventually(t, "Dave's message on A", func() string {
		if messages, _ := opsMessages(t, carol); copyOf(messages, daves) == nil {
			return "no copy"
		}
		return ""
	})
	if messages, _ := opsMessages(t, carol); copyOf(messages, removed) != nil {
		t.Fatal("the bridge copied the removed user's message")
	}

	// A local user's remote user removed from B's room: B refuses the
	// link's writes as them (denied), which the bridge skips.
	b.kick("carol@a.test:8081")
	if _, err := carol.Post("ops", "after my removal on B"); err != nil {
		t.Fatal(err)
	}
	erin := person(t, server, "erin", "Erin")
	if _, err := erin.Post("ops", "erin here"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Erin's message on B", func() string {
		if len(b.find(func(m map[string]any) bool { return text(m) == "erin here" })) != 1 {
			return "no copy"
		}
		return ""
	})
	if len(b.find(func(m map[string]any) bool { return text(m) == "after my removal on B" })) != 0 {
		t.Fatal("B copied the removed user's message")
	}
}

func TestFederationWire(t *testing.T) {
	// Exactly what the link sends B, for the other implementation.
	b := newFakeB(t, selfAddress, "tok")
	b.addUser("bob", "Bob")
	_, server, _ := startA(t, store.NewMemory(), "apron:?server="+b.address()+"&room=r1&token=tok", selfAddress)
	alice := person(t, server, "alice", "Alice")
	hello, err := alice.Post("ops", "hi @bob")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alice.Call("reactions", map[string]any{"message_id": hello, "emojis": []any{"👍"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.Call("message", map[string]any{"room_id": "ops", "message_id": hello, "deleted": true}); err != nil {
		t.Fatal(err)
	}
	var requests []map[string]any
	eventually(t, "the requests", func() string {
		if requests = b.linkRequests(); len(requests) < 4 {
			return fmt.Sprintf("%d requests", len(requests))
		}
		return ""
	})
	copyID := ""
	for _, m := range b.find(func(map[string]any) bool { return true }) {
		copyID = m["message_id"].(string)
	}
	act := map[string]any{"user_id": "alice"}
	want := []map[string]any{
		{"method": "me", "params": map[string]any{"name": "Alice", "ext": map[string]any{"remote": act}}},
		{"method": "message", "params": map[string]any{"room_id": "r1", "body": map[string]any{"text": "hi @bob"},
			"ext": map[string]any{"remote": map[string]any{"user_id": "alice", "room_id": "ops", "message_id": hello}}}},
		{"method": "reactions", "params": map[string]any{"message_id": copyID, "emojis": []any{"👍"}, "ext": map[string]any{"remote": act}}},
		{"method": "message", "params": map[string]any{"message_id": copyID, "room_id": "r1", "deleted": true, "ext": map[string]any{"remote": act}}},
	}
	for i, request := range requests {
		if id, _ := request["id"].(string); id == "" {
			t.Errorf("request %d has no id", i)
		}
		delete(request, "id")
		encoded, _ := json.Marshal(request)
		var got map[string]any
		_ = json.Unmarshal(encoded, &got)
		wantEncoded, _ := json.Marshal(want[i])
		var wanted map[string]any
		_ = json.Unmarshal(wantEncoded, &wanted)
		if !reflect.DeepEqual(got, wanted) {
			t.Errorf("request %d = %s\nwant %s", i, encoded, wantEncoded)
		}
		t.Logf("wire %d: %s", i, encoded)
	}
}

func TestFederationStopsWhenDenied(t *testing.T) {
	b := newFakeB(t, selfAddress, "tok")
	startA(t, store.NewMemory(), "https://link.apron.chat/#server="+b.address()+"&room=r1&token=wrong", selfAddress)
	time.Sleep(2500 * time.Millisecond)
	b.mu.Lock()
	auths := b.auths
	b.mu.Unlock()
	if auths != 1 {
		t.Fatalf("the bridge signed in %d times after denied, want 1", auths)
	}
}

func TestRewriteMentions(t *testing.T) {
	upper := func(id string) string { return strings.ToUpper(id) + "@b.example" }
	for _, tc := range []struct{ in, want string }{
		{"@ada hi", "@ADA@b.example hi"},
		{"hi @ada.", "hi @ADA@b.example."},
		{"mail ada@x.org, @ada: and @adam", "mail ada@x.org, @ADA@b.example: and @adam"},
		{"@&ada role, (@ada)", "@&ada role, (@ADA@b.example)"},
		{"@bob@a.test:8081 hi", "@BOB@A.TEST:8081@b.example hi"},
		{"`@ada` and @ada", "`@ada` and @ADA@b.example"},
		{"```\n@ada\n```\n@ada ``x`` `` @ada ``", "```\n@ada\n```\n@ADA@b.example ``x`` `` @ada ``"},
		{"`unclosed @ada", "`unclosed @ADA@b.example"},
	} {
		got := rewriteMentions(tc.in, true, map[string]bool{"ada": true, "bob@a.test:8081": true}, upper)
		if got != tc.want {
			t.Errorf("rewriteMentions(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
