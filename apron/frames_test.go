package apron_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"testing"

	"github.com/apron-chat/apron-server-go/apron"
)

func TestExt(t *testing.T) {
	ext := apron.Ext{}.With("irc", map[string]any{"nick": "ada_"})
	if string(ext["irc"]) != `{"nick":"ada_"}` {
		t.Fatalf("With: %s", ext["irc"])
	}
	more := ext.With("tz", "Europe/Oslo").With("raw", jsontext.Value(` [1, 2] `))
	if len(ext) != 1 || len(more) != 3 || string(more["tz"]) != `"Europe/Oslo"` || string(more["raw"]) != `[1,2]` {
		t.Fatalf("With: %v, %v", ext, more)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("With of a value that cannot be encoded did not panic")
		}
	}()
	apron.Ext{}.With("bad", func() {})
}

// The frames keep the members they have no field for, and send them again.
func TestUnknownMembers(t *testing.T) {
	for _, c := range []struct {
		name string
		v    any
		in   string
	}{
		{"body", &apron.Body{}, `{"text":"hi","color":"red","rich":{"a":[1,2]}}`},
		{"user", &apron.User{}, `{"user_id":"ada","name":"Ada","pronouns":"she/her"}`},
		{"room", &apron.Room{}, `{"room_id":"r","title":"R","topic_color":"#fff","ext":{"x":1}}`},
		{"room view", &apron.RoomView{}, `{"room_id":"r","member_count":2,"new_field":true}`},
		{"message", &apron.Message{}, `{"message_id":"5","log_id":"5","from":{"user_id":"ada"},"pinned":true}`},
		{"server", &apron.ServerInfo{}, `{"apron":10,"auth":["guest"],"future":{"x":1}}`},
		{"activity", &apron.Activity{}, `{"room_id":"r","from":{"user_id":"ada"},"typing":0,"ext:seen_at":"now"}`},
		{"embed write", &apron.EmbedWrite{}, `{"embed_id":"e1","kind":"upload","write_url":"https://chat.example/w/1","expires":"soon"}`},
		{"message result", &apron.MessageResult{}, `{"message_id":"5","embeds":[{"embed_id":"e1","kind":"append","write_url":"https://chat.example/w/1","x":1}],"note":true}`},
		{"command result", &apron.CommandResult{}, `{"embeds":[{"embed_id":"e1","kind":"terminal","write_url":"https://chat.example/w/1"}],"note":true}`},
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

// An embed write encodes its fields in order, and what it has no field for
// after them, as it arrived.
func TestEmbedWrite(t *testing.T) {
	built := apron.MessageResult{MessageID: 5, Embeds: []apron.EmbedWrite{{EmbedID: "embed_1", Kind: "upload", WriteURL: "https://chat.example/w/1"}}}
	out, err := json.Marshal(built)
	if want := `{"message_id":"5","embeds":[{"embed_id":"embed_1","kind":"upload","write_url":"https://chat.example/w/1"}]}`; err != nil || string(out) != want {
		t.Fatalf("built: %s %v, want %s", out, err, want)
	}
	in := `{"embed_id":"embed_1","kind":"append","write_url":"https://chat.example/w/1","expires":"soon","limits":{"bytes":1}}`
	var write apron.EmbedWrite
	if err := json.Unmarshal([]byte(in), &write); err != nil || write.Kind != "append" || write.WriteURL != "https://chat.example/w/1" {
		t.Fatalf("decoded %+v %v", write, err)
	}
	if out, err := json.Marshal(write); err != nil || string(out) != in {
		t.Fatalf("decoded and encoded: %s %v, want %s", out, err, in)
	}
}

func TestEncode(t *testing.T) {
	for _, c := range []struct {
		n    apron.Notification
		want string
	}{
		{apron.StatusUpdate{RoomID: "random", Mute: apron.Mute{On: true}}, `{"method":"status","params":{"room_id":"random","mute":true}}`},
		{apron.StatusUpdate{Mute: apron.Mute{On: true, Seconds: 3600}}, `{"method":"status","params":{"mute":3600}}`},
		{apron.Reactions{LogID: 11, MessageID: 3, RoomID: "r", Reactions: []apron.Reaction{{From: apron.User{UserID: "carol"}, Emojis: []string{"👍"}}}},
			`{"method":"reactions","params":{"log_id":"11","message_id":"3","room_id":"r","reactions":[{"from":{"user_id":"carol"},"emojis":["👍"]}]}}`},
		{apron.UserUpdate{New: apron.CurrentUser{UserID: "ada", Cleared: []string{"roles"}}}, `{"method":"user","params":{"new":{"user_id":"ada","roles":[]}}}`},
		{apron.RoomUpdate{Left: []apron.RoomRef{{RoomID: "r"}}}, `{"method":"room_update","params":{"left":[{"room_id":"r"}]}}`},
		{apron.Notice("", "hi"), `{"method":"message","params":{"from":{"user_id":"~private","name":"Only you"},"body":{"text":"hi"}}}`},
		{apron.Activity{RoomID: "general", From: apron.User{UserID: "alice"}, Typing: new(8), ReadMessageID: 1724803312050},
			`{"method":"activity","params":{"from":{"user_id":"alice"},"read_message_id":"1724803312050","room_id":"general","typing":8}}`},
		{apron.Activity{RoomID: "r", From: apron.User{UserID: "bob", Name: "Bob"}, Typing: new(0)}, `{"method":"activity","params":{"from":{"user_id":"bob","name":"Bob"},"room_id":"r","typing":0}}`},
	} {
		frame, err := apron.Encode(c.n)
		if err != nil || string(frame) != c.want {
			t.Errorf("Encode(%T) = %s, %v; want %s", c.n, frame, err, c.want)
		}
	}
	if _, err := apron.Encode(nil); err == nil {
		t.Error("Encode(nil) succeeded")
	}
	// Marshal encodes as frames are: keys sorted, nil as null, HTML
	// characters as they are.
	if got, err := apron.Marshal(map[string]any{"b": []string(nil), "a": "<&>"}); err != nil || string(got) != `{"a":"<&>","b":null}` {
		t.Errorf("Marshal = %s, %v", got, err)
	}
	var mute apron.Mute
	for raw, want := range map[string]apron.Mute{"true": {On: true}, "false": {}, "60": {On: true, Seconds: 60}} {
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
	u := apron.CurrentUser{UserID: "ada", Name: "Ada", Avatar: "https://a", Ext: apron.Ext{}.With("tz", "UTC"), Cleared: []string{"avatar", "roles", "ext"}}
	out, err := json.Marshal(u)
	if err != nil || string(out) != `{"user_id":"ada","name":"Ada","avatar":"","roles":[],"ext":{}}` {
		t.Fatalf("marshal: %s %v", out, err)
	}
	var back apron.CurrentUser
	if err := json.Unmarshal(out, &back); err != nil || back.Name != "Ada" || !slices.Equal(back.Cleared, []string{"avatar", "roles", "ext"}) {
		t.Fatalf("unmarshal: %+v %v", back, err)
	}
	if _, err := json.Marshal(apron.CurrentUser{UserID: "ada", Cleared: []string{"user_id"}}); err == nil {
		t.Fatal("cleared user_id was encoded")
	}
	// Without Cleared, it is the User.
	if out, _ := json.Marshal(apron.CurrentUser{UserID: "ada", Roles: []string{"op"}}); string(out) != `{"user_id":"ada","roles":["op"]}` {
		t.Fatalf("plain: %s", out)
	}
}

func TestPatch(t *testing.T) {
	old := apron.User{UserID: "ada", Name: "Ada", Roles: []string{"op"}, Status: "online", Ext: apron.Ext{"irc": jsontext.Value(`{"nick":"a"}`), "tz": jsontext.Value(`"UTC"`), "keep": jsontext.Value(`1`)}}
	if _, changed := old.Patch(old); changed {
		t.Fatal("an unchanged user patched")
	}
	cur := old
	cur.Roles = nil
	cur.Name = "Ada L"
	cur.Status = ""
	cur.Ext = apron.Ext{"tz": jsontext.Value(`"Europe/Oslo"`), "keep": jsontext.Value(`1`)}
	patch, changed := cur.Patch(old)
	if !changed {
		t.Fatal("no change")
	}
	out, _ := json.Marshal(patch, json.Deterministic(true))
	if want := `{"user_id":"ada","name":"Ada L","ext":{"irc":{},"tz":"Europe/Oslo"},"roles":[],"status":""}`; string(out) != want {
		t.Fatalf("patch = %s, want %s", out, want)
	}
	// Another user, or none: all of it.
	if patch, changed := cur.Patch(apron.User{}); !changed || patch.Name != "Ada L" || len(patch.Ext) != 2 || patch.Cleared != nil {
		t.Fatalf("from nothing: %+v", patch)
	}
}

func TestServerInfoChecks(t *testing.T) {
	info := apron.ServerInfo{Auth: []string{"token"}, Signup: []string{"guest"}, Capabilities: []string{"history", "rooms"}}
	if !info.Has("history") || info.Has("edit") {
		t.Error("Has")
	}
	if !info.Offers("token") || !info.Offers("guest") || info.Offers("email") {
		t.Error("Offers")
	}
}

// The params a client sends encode as the protocol names them.
func TestClientParams(t *testing.T) {
	latest := apron.LogID(42)
	for _, c := range []struct {
		params any
		want   string
	}{
		{apron.RoomListParams{}, `{}`},
		{apron.RoomListParams{Filter: apron.RoomsJoined, Members: true, LatestLogID: &latest}, `{"filter":"joined","members":true,"latest_log_id":"42"}`},
		{apron.RoomListParams{RoomID: "general", ParentRoomID: "p", UserID: "ada"}, `{"parent_room_id":"p","room_id":"general","user_id":"ada"}`},
		{apron.RoomLeaveParams{RoomID: "general"}, `{"room_id":"general"}`},
		{apron.RoomJoinParams{RoomID: "general", UserID: "bob"}, `{"room_id":"general","user_id":"bob"}`},
	} {
		got, err := apron.Marshal(c.params)
		if err != nil || string(got) != c.want {
			t.Errorf("%+v: %s, %v; want %s", c.params, got, err, c.want)
		}
	}
}
