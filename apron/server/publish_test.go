package server_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/aprontest"
	"github.com/apron-chat/apron-server-go/apron/server"
)

// PublishUser makes the change the user's own connections' identity, and
// sends them you and the others who share a room new.
func TestPublishUser(t *testing.T) {
	s := wired(&backend{}, server.Config{})
	type local struct {
		l      *server.Local
		frames chan string
	}
	open := func(id string) local {
		frames := make(chan string, 8)
		l, err := s.Local(server.LocalOptions{User: apron.User{UserID: id, Roles: []string{"op"}}, OnFrame: func(f apron.Frame) { frames <- string(f) }})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
		if f := <-frames; !strings.HasPrefix(f, `{"method":"server"`) {
			t.Fatalf("first frame %s, want the server frame", f)
		}
		return local{l, frames}
	}
	ada, ada2, bob, eve := open("ada"), open("ada"), open("bob"), open("eve")
	old := apron.User{UserID: "ada", Roles: []string{"op"}}
	if !s.PublishUser(server.UserChange{Old: old, New: apron.User{UserID: "ada"}}, func(c *server.Conn) bool { return c.UserID() != "eve" }) {
		t.Fatal("PublishUser reported no change")
	}
	expect := func(who local, want string) {
		t.Helper()
		select {
		case f := <-who.frames:
			if f != want {
				t.Fatalf("got %s, want %s", f, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no frame, want %s", want)
		}
	}
	expect(ada, `{"method":"user","params":{"you":{"user_id":"ada","roles":[]}}}`)
	expect(ada2, `{"method":"user","params":{"you":{"user_id":"ada","roles":[]}}}`)
	expect(bob, `{"method":"user","params":{"new":{"user_id":"ada","roles":[]}}}`)
	if u := userOf(ada2.l.Conn()); len(u.Roles) != 0 {
		t.Fatalf("the connection's user is %+v", u)
	}
	// Unchanged: nothing is done at all, so bob's next frame is eve's
	// change, of which eve was not told before; shares is not called, and
	// no connection's identity is set, even one that is not cur, which is
	// the backend's to bring up to date.
	ada.l.Conn().SetUser(apron.User{UserID: "ada", Name: "Stale"})
	same := apron.User{UserID: "ada", Roles: []string{}, Ext: apron.Ext{}}
	if s.PublishUser(server.UserChange{Old: apron.User{UserID: "ada"}, New: same}, func(*server.Conn) bool { t.Error("shares called with no change"); return true }) {
		t.Fatal("PublishUser reported a change where there was none")
	}
	if u := userOf(ada.l.Conn()); u.Name != "Stale" {
		t.Fatalf("an unchanged PublishUser set the connection's user: %+v", u)
	}
	if !s.PublishUser(server.UserChange{Old: apron.User{UserID: "eve"}, New: apron.User{UserID: "eve", Name: "Eve"}}, nil) {
		t.Fatal("PublishUser reported no change")
	}
	expect(eve, `{"method":"user","params":{"you":{"user_id":"eve","name":"Eve"}}}`)
	expect(bob, `{"method":"user","params":{"new":{"user_id":"eve","name":"Eve"}}}`)
}

// typed is a backend with every typed handler this file tests.
type typed struct {
	backend
	mu       sync.Mutex
	edits    []apron.EditParams
	commands []apron.CommandParams
	joins    []apron.RoomJoinParams
	leaves   []apron.RoomLeaveParams
	bodies   []apron.Body
	extra    map[string]any
}

func (b *typed) Auth(_ context.Context, c *server.Conn, p apron.AuthParams, reply server.Reply[apron.AuthResult]) error {
	you, err := b.guest(c, p)
	if err != nil {
		return err
	}
	return reply.Send(apron.AuthResult{You: you, Extra: b.extra})
}

func (b *typed) Message(_ context.Context, _ *server.Conn, p apron.MessageParams, reply server.Reply[apron.MessageResult]) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bodies = append(b.bodies, *p.Body)
	return reply.Send(apron.MessageResult{MessageID: 1})
}

func (b *typed) Edit(_ context.Context, _ *server.Conn, p apron.EditParams, reply server.Reply[apron.MessageResult]) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.edits = append(b.edits, p)
	return reply.Send(apron.MessageResult{MessageID: p.MessageID})
}

func (b *typed) Command(_ context.Context, _ *server.Conn, p apron.CommandParams, reply server.Reply[apron.CommandResult]) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.commands = append(b.commands, p)
	return reply.Send(apron.CommandResult{Embeds: []apron.EmbedWrite{{EmbedID: "e1", Kind: "upload", WriteURL: "https://chat.example/w/1"}}, Unknown: jsontext.Value(`{"note":1}`)})
}

func (b *typed) RoomList(_ context.Context, _ *server.Conn, _ apron.RoomListParams, reply server.Reply[apron.RoomListResult]) error {
	return reply.Send(apron.RoomListResult{})
}

func (b *typed) RoomJoin(_ context.Context, _ *server.Conn, p apron.RoomJoinParams, _ server.Reply[apron.OK]) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.joins = append(b.joins, p)
	return nil
}

func (b *typed) RoomLeave(_ context.Context, _ *server.Conn, p apron.RoomLeaveParams, reply server.Reply[apron.OK]) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.leaves = append(b.leaves, p)
	return reply.Send(apron.OK{})
}

func (b *typed) RoomSet(_ context.Context, _ *server.Conn, p apron.RoomSetParams, reply server.Reply[apron.RoomSetResult]) error {
	return reply.Send(apron.RoomSetResult{RoomID: "new_" + *p.Title})
}

func TestTypedParams(t *testing.T) {
	b := &typed{}
	s := wired(b, server.Config{})
	l, err := s.Local(server.LocalOptions{User: apron.User{UserID: "ada"}})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	call := func(method, params string) (string, error) {
		t.Helper()
		result, err := l.Call(context.Background(), method, jsontext.Value(params))
		return string(result), err
	}
	// A new message's body keeps the fields apron does not know, and
	// encodes as it was sent.
	if _, err := call("message", `{"body":{"text":"hi","color":"red","mentions":[]}}`); err != nil {
		t.Fatal(err)
	}
	if body := b.bodies[0]; body.Text != "hi" || string(body.Unknown) != `{"color":"red"}` {
		t.Fatalf("body: %+v", body)
	}
	if out, _ := json.Marshal(b.bodies[0]); string(out) != `{"text":"hi","mentions":[],"color":"red"}` {
		t.Fatalf("body sent as %s", out)
	}
	if _, err := call("message", `{"message_id":"7","room_id":"r","body":{"text":"x"},"ext":{"a":1}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call("message", `{"message_id":"8","deleted":true,"body":{"text":"ignored"}}`); err != nil {
		t.Fatal(err)
	}
	if e := b.edits; len(e) != 2 || e[0].MessageID != 7 || e[0].RoomID != "r" || e[0].Body.Text != "x" || string(e[0].Ext["a"]) != "1" || !e[1].Deleted || e[1].Body != nil {
		t.Fatalf("edits: %+v", e)
	}
	if _, err := call("message", `{"deleted":true}`); apron.CodeOf(err) != apron.CodeInvalidParams || !strings.Contains(err.Error(), "deleted") {
		t.Fatalf("a deletion without message_id: %v", err)
	}
	result, err := call("command", `{"room_id":"r","body":{"text":"/x"}}`)
	if err != nil || result != `{"embeds":[{"embed_id":"e1","kind":"upload","write_url":"https://chat.example/w/1"}],"note":1}` {
		t.Fatalf("command: %s %v", result, err)
	}
	if c := b.commands; len(c) != 1 || c[0].RoomID != "r" || c[0].Body.Text != "/x" {
		t.Fatalf("commands: %+v", c)
	}
	if _, err := call("room_join", `{"room_id":"r","user_id":"bob"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call("room_leave", `{"room_id":"r"}`); err != nil {
		t.Fatal(err)
	}
	if b.joins[0] != (apron.RoomJoinParams{RoomID: "r", UserID: "bob"}) || b.leaves[0] != (apron.RoomLeaveParams{RoomID: "r"}) {
		t.Fatalf("joins %+v leaves %+v", b.joins, b.leaves)
	}
	if result, err := call("room_set", `{"title":"t"}`); err != nil || result != `{"room_id":"new_t"}` {
		t.Fatalf("room_set: %s %v", result, err)
	}
}

// An auth result's Extra is sent beside you and token, and may not name
// either.
func TestAuthExtra(t *testing.T) {
	b := &typed{extra: map[string]any{"options": map[string]any{"challenge": "c"}}}
	c := aprontest.Connect(t, wired(b, server.Config{}))
	raw, err := c.Call(wait(t), "auth", map[string]any{"scheme": "guest"})
	var result map[string]any
	if err != nil || json.Unmarshal(raw, &result) != nil || result["options"] == nil || result["you"] == nil {
		t.Fatalf("got %s %v", raw, err)
	}
	if f := next(t, c); f.Method != "message" {
		t.Fatalf("after the auth result got %s, want the welcome notice", f.Raw)
	}

	b2 := &typed{extra: map[string]any{"you": 1}}
	c2 := aprontest.Connect(t, wired(b2, server.Config{Logger: slog.New(slog.DiscardHandler)}))
	if _, err := c2.Auth(wait(t), map[string]any{"scheme": "guest"}); apron.CodeOf(err) != apron.CodeInternalError {
		t.Fatalf("got %v", err)
	}
}
