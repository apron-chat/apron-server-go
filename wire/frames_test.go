package wire_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/wire"
)

func TestExt(t *testing.T) {
	ext := wire.MustExt("irc", map[string]any{"nick": "ada_"})
	if string(ext["irc"]) != `{"nick":"ada_"}` {
		t.Fatalf("MustExt: %s", ext["irc"])
	}
	more := ext.With("tz", "Europe/Oslo").With("raw", jsontext.Value(` [1, 2] `))
	if len(ext) != 1 || len(more) != 3 || string(more["tz"]) != `"Europe/Oslo"` || string(more["raw"]) != `[1,2]` {
		t.Fatalf("With: %v, %v", ext, more)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("MustExt of a value that cannot be encoded did not panic")
		}
	}()
	wire.MustExt("bad", func() {})
}

// The frames keep the members they have no field for, and send them again.
func TestUnknownMembers(t *testing.T) {
	for _, c := range []struct {
		name string
		v    any
		in   string
	}{
		{"body", &wire.Body{}, `{"text":"hi","color":"red","rich":{"a":[1,2]}}`},
		{"user", &wire.User{}, `{"user_id":"ada","name":"Ada","pronouns":"she/her"}`},
		{"room", &wire.Room{}, `{"room_id":"r","title":"R","topic_color":"#fff","ext":{"x":1}}`},
		{"room view", &wire.RoomView{}, `{"room_id":"r","member_count":2,"new_field":true}`},
		{"message", &wire.Message{}, `{"message_id":"5","log_id":"5","from":{"user_id":"ada"},"pinned":true}`},
		{"server", &wire.ServerInfo{}, `{"apron":10,"auth":["guest"],"future":{"x":1}}`},
	} {
		if err := json.Unmarshal([]byte(c.in), c.v); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		out, err := json.Marshal(c.v)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var want, got map[string]any
		_ = json.Unmarshal([]byte(c.in), &want)
		_ = json.Unmarshal(out, &got)
		w, _ := json.Marshal(want, json.Deterministic(true))
		g, _ := json.Marshal(got, json.Deterministic(true))
		if string(w) != string(g) {
			t.Errorf("%s: %s, want %s", c.name, out, c.in)
		}
	}
}

func TestEncode(t *testing.T) {
	for _, c := range []struct {
		n    wire.Notification
		want string
	}{
		{wire.StatusUpdate{RoomID: "random", Mute: wire.Mute{On: true}}, `{"method":"status","params":{"room_id":"random","mute":true}}`},
		{wire.StatusUpdate{Mute: wire.Mute{On: true, Seconds: 3600}}, `{"method":"status","params":{"mute":3600}}`},
		{wire.Reactions{LogID: 11, MessageID: 3, RoomID: "r", Reactions: []wire.Reaction{{From: wire.User{UserID: "carol"}, Emojis: []string{"👍"}}}},
			`{"method":"reactions","params":{"log_id":"11","message_id":"3","room_id":"r","reactions":[{"from":{"user_id":"carol"},"emojis":["👍"]}]}}`},
		{wire.UserUpdate{New: &wire.CurrentUser{User: wire.User{UserID: "ada"}, Cleared: []string{"roles"}}}, `{"method":"user","params":{"new":{"user_id":"ada","roles":[]}}}`},
		{wire.RoomUpdate{Left: []wire.RoomRef{{RoomID: "r"}}}, `{"method":"room_update","params":{"left":[{"room_id":"r"}]}}`},
		{wire.Notice("", "hi"), `{"method":"message","params":{"from":{"user_id":"~private","name":"Only you"},"body":{"text":"hi"}}}`},
	} {
		frame, err := wire.Encode(c.n)
		if err != nil || string(frame) != c.want {
			t.Errorf("Encode(%T) = %s, %v; want %s", c.n, frame, err, c.want)
		}
	}
	if _, err := wire.Encode(nil); err == nil {
		t.Error("Encode(nil) succeeded")
	}
	var mute wire.Mute
	for raw, want := range map[string]wire.Mute{"true": {On: true}, "false": {}, "60": {On: true, Seconds: 60}} {
		if err := json.Unmarshal([]byte(raw), &mute); err != nil || mute != want {
			t.Errorf("mute %s: %+v %v", raw, mute, err)
		}
	}
	for _, raw := range []string{"0", "null", `"x"`, "1.5"} {
		if json.Unmarshal([]byte(raw), &mute) == nil {
			t.Errorf("mute %s was accepted", raw)
		}
	}
}

func TestCurrentUser(t *testing.T) {
	u := wire.CurrentUser{User: wire.User{UserID: "ada", Name: "Ada", Avatar: "https://a", Ext: wire.MustExt("tz", "UTC")}, Cleared: []string{"avatar", "roles", "ext"}}
	out, err := json.Marshal(u)
	if err != nil || string(out) != `{"user_id":"ada","name":"Ada","avatar":"","roles":[],"ext":{}}` {
		t.Fatalf("marshal: %s %v", out, err)
	}
	var back wire.CurrentUser
	if err := json.Unmarshal(out, &back); err != nil || back.Name != "Ada" || !slices.Equal(back.Cleared, []string{"avatar", "roles", "ext"}) {
		t.Fatalf("unmarshal: %+v %v", back, err)
	}
	if _, err := json.Marshal(wire.CurrentUser{User: wire.User{UserID: "ada"}, Cleared: []string{"user_id"}}); err == nil {
		t.Fatal("cleared user_id was encoded")
	}
	// Without Cleared, it is the User.
	if out, _ := json.Marshal(wire.CurrentUser{User: wire.User{UserID: "ada", Roles: []string{"op"}}}); string(out) != `{"user_id":"ada","roles":["op"]}` {
		t.Fatalf("plain: %s", out)
	}
}

func TestPatch(t *testing.T) {
	old := wire.User{UserID: "ada", Name: "Ada", Roles: []string{"op"}, Status: "online", Ext: wire.Ext{"irc": jsontext.Value(`{"nick":"a"}`), "tz": jsontext.Value(`"UTC"`), "keep": jsontext.Value(`1`)}}
	if _, changed := old.Patch(old); changed {
		t.Fatal("an unchanged user patched")
	}
	cur := old
	cur.Roles = nil
	cur.Name = "Ada L"
	cur.Status = ""
	cur.Ext = wire.Ext{"tz": jsontext.Value(`"Europe/Oslo"`), "keep": jsontext.Value(`1`)}
	patch, changed := cur.Patch(old)
	if !changed {
		t.Fatal("no change")
	}
	out, _ := json.Marshal(patch, json.Deterministic(true))
	if want := `{"user_id":"ada","name":"Ada L","ext":{"irc":{},"tz":"Europe/Oslo"},"roles":[],"status":""}`; string(out) != want {
		t.Fatalf("patch = %s, want %s", out, want)
	}
	// Another user, or none: all of it.
	if patch, changed := cur.Patch(wire.User{}); !changed || patch.Name != "Ada L" || len(patch.Ext) != 2 || patch.Cleared != nil {
		t.Fatalf("from nothing: %+v", patch)
	}
}

// PublishUser makes the change the user's own connections' identity, and
// sends them you and the others who share a room new.
func TestPublishUser(t *testing.T) {
	s := wired(&wire.Server{Backend: &backend{}})
	type local struct {
		l      *wire.Local
		frames chan string
	}
	open := func(id string) local {
		frames := make(chan string, 8)
		l, err := s.Local(wire.LocalOptions{User: wire.User{UserID: id, Roles: []string{"op"}}, OnFrame: func(f wire.Frame) { frames <- string(f) }})
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
	old := wire.User{UserID: "ada", Roles: []string{"op"}}
	if !s.PublishUser(old, wire.User{UserID: "ada"}, func(c *wire.Conn) bool { return c.UserID() != "eve" }) {
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
	if u := ada2.l.Conn().User(); len(u.Roles) != 0 {
		t.Fatalf("the connection's user is %+v", u)
	}
	// Unchanged: nothing is done at all, so bob's next frame is eve's
	// change, of which eve was not told before; shares is not called, and
	// no connection's identity is set, even one that is not cur, which is
	// the backend's to bring up to date.
	ada.l.Conn().SetUser(wire.User{UserID: "ada", Name: "Stale"})
	same := wire.User{UserID: "ada", Roles: []string{}, Ext: wire.Ext{}}
	if s.PublishUser(wire.User{UserID: "ada"}, same, func(*wire.Conn) bool { t.Error("shares called with no change"); return true }) {
		t.Fatal("PublishUser reported a change where there was none")
	}
	if u := ada.l.Conn().User(); u.Name != "Stale" {
		t.Fatalf("an unchanged PublishUser set the connection's user: %+v", u)
	}
	if !s.PublishUser(wire.User{UserID: "eve"}, wire.User{UserID: "eve", Name: "Eve"}, nil) {
		t.Fatal("PublishUser reported no change")
	}
	expect(eve, `{"method":"user","params":{"you":{"user_id":"eve","name":"Eve"}}}`)
	expect(bob, `{"method":"user","params":{"new":{"user_id":"eve","name":"Eve"}}}`)
}

// typed is a backend with every typed handler this file tests.
type typed struct {
	backend
	mu       sync.Mutex
	edits    []wire.EditParams
	commands []wire.CommandParams
	joins    []wire.RoomJoinParams
	leaves   []wire.RoomLeaveParams
	bodies   []wire.Body
	extra    map[string]any
}

func (b *typed) Auth(_ context.Context, c *wire.Conn, p wire.AuthParams, reply wire.Reply[wire.AuthResult]) error {
	you, err := b.guest(c, p)
	if err != nil {
		return err
	}
	return reply.Send(wire.AuthResult{You: you, Extra: b.extra})
}

func (b *typed) Message(_ context.Context, _ *wire.Conn, p wire.MessageParams, reply wire.Reply[wire.MessageResult]) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bodies = append(b.bodies, *p.Body)
	return reply.Send(wire.MessageResult{MessageID: 1})
}

func (b *typed) Edit(_ context.Context, _ *wire.Conn, p wire.EditParams, reply wire.Reply[wire.MessageResult]) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.edits = append(b.edits, p)
	return reply.Send(wire.MessageResult{MessageID: p.MessageID})
}

func (b *typed) Command(_ context.Context, _ *wire.Conn, p wire.CommandParams, reply wire.Reply[wire.CommandResult]) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.commands = append(b.commands, p)
	return reply.Send(wire.CommandResult{Embeds: []jsontext.Value{jsontext.Value(`{"embed_id":"e1","kind":"upload"}`)}, Unknown: jsontext.Value(`{"note":1}`)})
}

func (b *typed) RoomList(_ context.Context, _ *wire.Conn, _ wire.RoomListParams, reply wire.Reply[wire.RoomListResult]) error {
	return reply.Send(wire.RoomListResult{})
}

func (b *typed) RoomJoin(_ context.Context, _ *wire.Conn, p wire.RoomJoinParams, _ wire.Reply[wire.Empty]) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.joins = append(b.joins, p)
	return nil
}

func (b *typed) RoomLeave(_ context.Context, _ *wire.Conn, p wire.RoomLeaveParams, reply wire.Reply[wire.Empty]) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.leaves = append(b.leaves, p)
	return reply.Send(wire.Empty{})
}

func (b *typed) RoomSet(_ context.Context, _ *wire.Conn, p wire.RoomSetParams, reply wire.Reply[wire.RoomSetResult]) error {
	return reply.Send(wire.RoomSetResult{RoomID: "new_" + *p.Title})
}

func TestTypedParams(t *testing.T) {
	b := &typed{}
	s := wired(&wire.Server{Backend: b})
	l, err := s.Local(wire.LocalOptions{User: wire.User{UserID: "ada"}})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	call := func(method, params string) (string, error) {
		t.Helper()
		result, err := l.Call(context.Background(), method, jsontext.Value(params))
		return string(result), err
	}
	// A new message's body keeps the fields wire does not know, and
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
	if _, err := call("message", `{"deleted":true}`); wire.CodeOf(err) != wire.CodeInvalidParams || !strings.Contains(err.Error(), "deleted") {
		t.Fatalf("a deletion without message_id: %v", err)
	}
	result, err := call("command", `{"room_id":"r","body":{"text":"/x"}}`)
	if err != nil || result != `{"embeds":[{"embed_id":"e1","kind":"upload"}],"note":1}` {
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
	if b.joins[0] != (wire.RoomJoinParams{RoomID: "r", UserID: "bob"}) || b.leaves[0] != (wire.RoomLeaveParams{RoomID: "r"}) {
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
	c, _ := serve(t, wired(&wire.Server{Backend: b}))
	c.send(`{"method":"auth","id":"a","params":{"scheme":"guest"}}`)
	f := c.read()
	if f.result()["options"] == nil || f.result()["you"] == nil {
		t.Fatalf("got %v", f)
	}
	c.read() // The welcome notice.

	b2 := &typed{extra: map[string]any{"you": 1}}
	c2, _ := serve(t, wired(&wire.Server{Backend: b2, Logger: slog.New(slog.DiscardHandler)}))
	c2.send(`{"method":"auth","id":"a","params":{"scheme":"guest"}}`)
	if f := c2.read(); f.errorCode() != wire.CodeInternalError {
		t.Fatalf("got %v", f)
	}
}
