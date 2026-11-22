package wire

import (
	"encoding/json/jsontext"
	"strings"
	"sync"
	"testing"
)

// A Decoder keeps its first error, in the order of the reads, and reads
// zero values after it.
func TestDecoder(t *testing.T) {
	d := req(`{"kind":"web","n":3,"on":true,"ext":{"a":{"b": 1}},"after":"7"}`).Decoder()
	kind := d.String("kind", Required)
	n := d.Count("n", Required)
	on := Opt(d, "on", d.Bool)
	off := Opt(d, "off", d.Bool)
	after := Opt(d, "after", d.LogID)
	ext := d.Ext("ext", Optional)
	if err := d.Err(); err != nil || kind != "web" || n != 3 || on == nil || !*on || off != nil || after == nil || *after != 7 || string(ext["a"]) != `{"b":1}` {
		t.Fatalf("got %q %d %v %v %v %s, %v", kind, n, on, off, after, ext["a"], d.Err())
	}

	d = req(`{"kind":5,"n":0}`).Decoder()
	if kind := d.String("kind", Required); kind != "" {
		t.Fatalf("kind = %q", kind)
	}
	if n := d.Count("n", Required); n != 0 {
		t.Fatalf("n = %d after an error", n)
	}
	var none *Error
	d.Fail(none)
	d.Fail(InvalidParams("later"))
	if err := d.Err(); CodeOf(err) != CodeInvalidParams || !strings.Contains(err.Error(), "kind must be a string") {
		t.Fatalf("Err = %v, want the first", err)
	}
	if got := Opt(d, "n", d.Count); got != nil {
		t.Fatalf("Opt after an error = %v", got)
	}

	d = req(`{}`).Decoder()
	d.Fail(none)
	d.ID("room_id", Optional)
	if d.Err() != nil {
		t.Fatalf("an optional param: %v", d.Err())
	}
	d.ID("room_id", Required)
	if err := d.Err(); err == nil || !strings.Contains(err.Error(), "Missing room_id") {
		t.Fatalf("a required param: %v", err)
	}
}

// Each parser reports the first param that is not well formed, with what it
// is meant to be.
func TestParsers(t *testing.T) {
	for _, tc := range []struct {
		name, params, want string
		parse              func(*Request) error
	}{
		{"message", `{"message_id":"3","body":{"text":"x"}}`, "A new message has no message_id", errOf(ParseMessage)},
		{"message", `{"room_id":"general"}`, "Missing body", errOf(ParseMessage)},
		{"message", `{"room_id":"a b","body":5}`, "room_id must be an ID", errOf(ParseMessage)},
		{"edit", `{"body":{"text":"x"}}`, "Missing message_id", errOf(ParseEdit)},
		{"edit", `{"deleted":true}`, "Cannot create a deleted message", errOf(ParseEdit)},
		{"edit", `{"message_id":"3","deleted":true,"body":5}`, "", errOf(ParseEdit)},
		{"command", `{"deleted":false,"body":{"text":"/x"}}`, "A command has no deleted", errOf(ParseCommand)},
		{"auth", `{}`, "Missing scheme", errOf(ParseAuth)},
		{"auth", `{"scheme":""}`, "Missing scheme", errOf(ParseAuth)},
		{"auth", `{"scheme":5}`, "scheme must be a string", errOf(ParseAuth)},
		{"auth", `{"scheme":"guest","name":5}`, "name must be a string", errOf(ParseAuth)},
		{"auth", `{"scheme":"guest","ext":[]}`, "ext must be an object", errOf(ParseAuth)},
		{"auth", `{"scheme":"guest","user_id":"a b","credential":{}}`, "", errOf(ParseAuth)},
		{"me", `{"avatar":null}`, "avatar must be a string", errOf(ParseMe)},
		{"room_list", `{"filter":"some"}`, "filter must be joined", errOf(ParseRoomList)},
		{"room_join", `{"user_id":"ada"}`, "Missing room_id", errOf(ParseRoomJoin)},
		{"room_leave", `{"room_id":"general","user_id":""}`, "user_id must be an ID", errOf(ParseRoomLeave)},
		{"room_set", `{"private":"yes"}`, "private must be a boolean", errOf(ParseRoomSet)},
		{"history", `{"limit":-1}`, "limit must be a positive integer", errOf(ParseHistory)},
		{"status", `{"room_id":"general","idle":5}`, "room_id scopes a mute", errOf(ParseStatus)},
		{"reactions", `{"message_id":"0","emojis":[]}`, "message_id must be a positive", errOf(ParseReact)},
		{"reactions", `{"message_id":"1"}`, "Missing emojis", errOf(ParseReact)},
		{"activity", `{"typing":null}`, "typing must be a non-negative integer", errOf(ParseActivity)},
		{"activity", `{"read_message_id":"0"}`, "read_message_id must be a positive", errOf(ParseActivity)},
	} {
		err := tc.parse(req(tc.params))
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s %s: %v", tc.name, tc.params, err)
		case tc.want != "" && (CodeOf(err) != CodeInvalidParams || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s %s: %v, want %q", tc.name, tc.params, err, tc.want)
		}
	}

	p, err := ParseAuth(req(`{"scheme":"token","token":"t","ext":{"x": [1, 2]}}`))
	if err != nil || p.Scheme != "token" || p.Token != "t" || string(p.Ext["x"]) != "[1,2]" || !strings.Contains(string(p.Raw), `"token"`) {
		t.Fatalf("ParseAuth: %+v %v", p, err)
	}
	if p, err := ParseReact(req(`{"message_id":"4","emojis":[]}`)); err != nil || p.Emojis == nil {
		t.Fatalf("ParseReact: %+v %v", p, err)
	}
}

func errOf[P any](parse func(*Request) (P, error)) func(*Request) error {
	return func(r *Request) error {
		_, err := parse(r)
		return err
	}
}

// recording records a detached request's reply.
type recording struct {
	sent any
	raw  jsontext.Value
	err  error
}

func (r *recording) Record(sent any, raw jsontext.Value, err error, _ [][]byte) {
	r.sent, r.raw, r.err = sent, raw, err
}

// NewReply does what the Server does with the result of the request's
// method, for the method's result type.
func TestNewReply(t *testing.T) {
	send := func(method, params string, f func(*Request)) *recording {
		rec := &recording{}
		r, err := detachedRequest(method, jsontext.Value(params), rec)
		if err != nil {
			t.Fatal(err)
		}
		f(r)
		return rec
	}
	rec := send("me", `{}`, func(r *Request) {
		_ = NewReply[CurrentUser](r).Send(CurrentUser{User: User{UserID: "ada"}, Cleared: []string{"avatar"}})
	})
	if string(rec.raw) != `{"you":{"user_id":"ada","avatar":""}}` {
		t.Fatalf("me: %s", rec.raw)
	}
	if you, ok := rec.sent.(CurrentUser); !ok || you.UserID != "ada" {
		t.Fatalf("me sent %#v", rec.sent)
	}
	rec = send("room_list", `{"filter":"joined"}`, func(r *Request) { _ = NewReply[RoomListResult](r).Send(RoomListResult{}) })
	if string(rec.raw) != `{"joined":[]}` {
		t.Fatalf("room_list: %s", rec.raw)
	}
	rec = send("x", `{}`, func(r *Request) { _ = NewReply[CurrentUser](r).Send(CurrentUser{User: User{UserID: "ada"}}) })
	if string(rec.raw) != `{"user_id":"ada"}` {
		t.Fatalf("another method: %s", rec.raw)
	}
	rec = send("auth", `{"scheme":"guest"}`, func(r *Request) {
		if err := NewReply[AuthResult](r).Send(AuthResult{You: &CurrentUser{User: User{UserID: "a b"}}}); CodeOf(err) != CodeInternalError {
			t.Errorf("an invalid user_id: %v", err)
		}
	})
	if rec.err == nil {
		t.Fatal("an invalid auth result was not recorded as an error")
	}
	send("x", `{}`, func(r *Request) {
		if err := NewReply[Empty](r).Send(Empty{}); err != nil {
			t.Fatal(err)
		}
		if err := NewReply[Empty](r).Send(Empty{}); err != ErrReplied {
			t.Fatalf("a second reply: %v", err)
		}
	})
}

// A request's params are decoded once, and safe to read from goroutines of
// the handler's own.
func TestFieldsShared(t *testing.T) {
	r := req(`{"a":"x","b":true}`)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if v, _, err := r.String("a"); v != "x" || err != nil {
				t.Errorf("String: %q %v", v, err)
			}
			if !r.Has("b") {
				t.Error("Has")
			}
		})
	}
	wg.Wait()
}
