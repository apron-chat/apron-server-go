package server_test

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/aprontest"
	"github.com/apron-chat/apron-server-go/apron/server"
)

// helloBackend is a Backend of Local connections only.
type helloBackend struct{}

func (helloBackend) Hello(context.Context, *server.Conn) apron.ServerInfo {
	return apron.ServerInfo{Auth: []string{"guest"}}
}

func (helloBackend) Auth(context.Context, *server.Conn, apron.AuthParams, server.Reply[apron.AuthResult]) error {
	return apron.Denied("No sign-in")
}

// viewer is a Local connection and the frames sent to it.
type viewer struct {
	l      *server.Local
	frames chan string
}

func openViewer(t *testing.T, s *server.Server, u apron.User) viewer {
	t.Helper()
	frames := make(chan string, 16)
	l, err := s.Local(server.LocalOptions{User: u, OnFrame: func(f apron.Frame) { frames <- string(f) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	if f := <-frames; !strings.HasPrefix(f, `{"method":"server"`) {
		t.Fatalf("first frame %s, want the server frame", f)
	}
	return viewer{l, frames}
}

func (v viewer) expect(t *testing.T, want string) {
	t.Helper()
	select {
	case f := <-v.frames:
		if f != want {
			t.Fatalf("%s got %s, want %s", v.l.Conn().UserID(), f, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s got no frame, want %s", v.l.Conn().UserID(), want)
	}
}

// PublishUser sends the user's own connections the patch of their
// view, and others the patch of the shown one, each only when it changed.
func TestPublishUserViews(t *testing.T) {
	s := server.New(helloBackend{}, server.Config{})
	chosen := apron.User{UserID: "ada", Name: "Ada", Status: "online"}
	ada := openViewer(t, s, chosen)
	ada2 := openViewer(t, s, chosen)
	bob := openViewer(t, s, apron.User{UserID: "bob"})
	eve := openViewer(t, s, apron.User{UserID: "eve"})
	notEve := func(c *server.Conn) bool { return c.UserID() != "eve" }
	// marker is a change everyone is sent, which shows that nothing was
	// sent before it.
	marker := func(name string) {
		t.Helper()
		if !s.PublishUser(server.UserChange{Old: apron.User{UserID: "carol"}, New: apron.User{UserID: "carol", Name: name}}, nil) {
			t.Fatal("marker: no change")
		}
		for _, v := range []viewer{ada, ada2, bob, eve} {
			v.expect(t, `{"method":"user","params":{"new":{"user_id":"carol","name":"`+name+`"}}}`)
		}
	}

	// A chosen dnd: ada's own connections see it, and others too, as it
	// is shown while she is online (§4.5.1).
	dnd := chosen
	dnd.Status = "dnd"
	if !s.PublishUser(server.UserChange{Old: chosen, New: dnd, OldShown: chosen, NewShown: dnd}, notEve) {
		t.Fatal("dnd: no change")
	}
	ada.expect(t, `{"method":"user","params":{"you":{"user_id":"ada","status":"dnd"}}}`)
	ada2.expect(t, `{"method":"user","params":{"you":{"user_id":"ada","status":"dnd"}}}`)
	bob.expect(t, `{"method":"user","params":{"new":{"user_id":"ada","status":"dnd"}}}`)
	marker("1")

	// A chosen invisible: others see offline, with seen (§4.5.2).
	invisible := chosen
	invisible.Status = "invisible"
	shownOffline := apron.User{UserID: "ada", Name: "Ada", Status: "offline", Seen: "1724803600000"}
	if !s.PublishUser(server.UserChange{Old: dnd, New: invisible, OldShown: dnd, NewShown: shownOffline}, notEve) {
		t.Fatal("invisible: no change")
	}
	ada.expect(t, `{"method":"user","params":{"you":{"user_id":"ada","status":"invisible"}}}`)
	ada2.expect(t, `{"method":"user","params":{"you":{"user_id":"ada","status":"invisible"}}}`)
	bob.expect(t, `{"method":"user","params":{"new":{"user_id":"ada","status":"offline","seen":"1724803600000"}}}`)
	if u := ada2.l.Conn().User(); u.Status != "invisible" || u.Seen != "" {
		t.Fatalf("ada's identity is %+v, want the chosen object", u)
	}
	marker("2")

	// Others' view alone: the shown status follows her connections while
	// the chosen one stays, so her own connections are sent nothing, and
	// their identity is not set.
	ada.l.Conn().SetUser(apron.User{UserID: "ada", Name: "Stale"})
	idle, online := chosen, chosen
	idle.Status = "idle"
	if !s.PublishUser(server.UserChange{Old: chosen, New: chosen, OldShown: online, NewShown: idle}, notEve) {
		t.Fatal("idle: no change")
	}
	bob.expect(t, `{"method":"user","params":{"new":{"user_id":"ada","status":"idle"}}}`)
	if u := ada.l.Conn().User(); u.Name != "Stale" {
		t.Fatalf("a change of others' view set ada's identity: %+v", u)
	}
	marker("3")

	// Her own view alone: others are sent nothing, and shares is not
	// called.
	renamed := invisible
	renamed.Name = "Ada L"
	shownRenamed := shownOffline
	if !s.PublishUser(server.UserChange{Old: invisible, New: renamed, OldShown: shownOffline, NewShown: shownRenamed},
		func(*server.Conn) bool { t.Error("shares called without a change of others' view"); return true }) {
		t.Fatal("own view: no change")
	}
	ada.expect(t, `{"method":"user","params":{"you":{"user_id":"ada","name":"Ada L"}}}`)
	ada2.expect(t, `{"method":"user","params":{"you":{"user_id":"ada","name":"Ada L"}}}`)
	if u := ada.l.Conn().User(); u.Name != "Ada L" {
		t.Fatalf("ada's identity is %+v", u)
	}
	marker("4")

	// Neither: nothing at all.
	if s.PublishUser(server.UserChange{Old: renamed, New: renamed, OldShown: shownOffline, NewShown: shownOffline},
		func(*server.Conn) bool { t.Error("shares called without a change"); return true }) {
		t.Fatal("reported a change where there was none")
	}
	marker("5")

	// A user_id change: others are sent the shown objects, old too, with
	// Old's and New's user_ids whatever the shown objects hold.
	account := apron.User{UserID: "ada_l", Name: "Ada L", Status: "invisible"}
	if !s.PublishUser(server.UserChange{Old: renamed, New: account, OldShown: shownOffline, NewShown: apron.User{UserID: "ignored", Name: "Ada L", Status: "offline"}}, notEve) {
		t.Fatal("user_id: no change")
	}
	ada.expect(t, `{"method":"user","params":{"you":{"user_id":"ada_l","name":"Ada L","status":"invisible"}}}`)
	ada2.expect(t, `{"method":"user","params":{"you":{"user_id":"ada_l","name":"Ada L","status":"invisible"}}}`)
	bob.expect(t, `{"method":"user","params":{"new":{"user_id":"ada_l","name":"Ada L","status":"offline"},"old":{"user_id":"ada","name":"Ada","status":"offline","seen":"1724803600000"}}}`)
	if u := ada2.l.Conn().User(); u.UserID != "ada_l" {
		t.Fatalf("ada's identity is %+v", u)
	}
	marker("6")

	// Without NewShown, everyone sees Old and New, as with PublishUser.
	back := account
	back.Status = "online"
	if !s.PublishUser(server.UserChange{Old: account, New: back, OldShown: apron.User{UserID: "x", Name: "unused"}}, nil) {
		t.Fatal("one view: no change")
	}
	ada.expect(t, `{"method":"user","params":{"you":{"user_id":"ada_l","status":"online"}}}`)
	ada2.expect(t, `{"method":"user","params":{"you":{"user_id":"ada_l","status":"online"}}}`)
	bob.expect(t, `{"method":"user","params":{"new":{"user_id":"ada_l","status":"online"}}}`)
	eve.expect(t, `{"method":"user","params":{"new":{"user_id":"ada_l","status":"online"}}}`)
	marker("7")

	// Except, a connection whose own result carries the change, is sent
	// nothing, and keeps its identity.
	named := back
	named.Name = "Ada"
	if !s.PublishUser(server.UserChange{Old: back, New: named, Except: ada.l.Conn()}, nil) {
		t.Fatal("except: no change")
	}
	ada2.expect(t, `{"method":"user","params":{"you":{"user_id":"ada_l","name":"Ada"}}}`)
	bob.expect(t, `{"method":"user","params":{"new":{"user_id":"ada_l","name":"Ada"}}}`)
	eve.expect(t, `{"method":"user","params":{"new":{"user_id":"ada_l","name":"Ada"}}}`)
	if u := ada.l.Conn().User(); u.Name != "Ada L" {
		t.Fatalf("the excepted connection's identity is %+v", u)
	}
	marker("8")
}

// ApplyMe merges a me request as the protocol does, and leaves the status
// and what old holds alone; Complete is the whole user, with what the
// change cleared.
func TestApplyMeAndComplete(t *testing.T) {
	old := apron.User{UserID: "ada", Name: "Ada", Avatar: "https://a/1", Roles: []string{"admin"}, Status: "online", Ext: apron.Ext{"tz": []byte(`"UTC"`), "irc": []byte(`{"nick":"ada_"}`)}}
	name, empty, status := "Ada L", "", "dnd"
	cur := server.ApplyMe(old, apron.MeParams{Name: &name, Avatar: &empty, Status: &status, Ext: apron.Ext{"tz": []byte(`""`), "lang": []byte(`"en"`)}})
	if cur.Name != "Ada L" || cur.Avatar != "" || cur.Status != "online" || !slices.Equal(cur.Roles, old.Roles) ||
		len(cur.Ext) != 2 || string(cur.Ext["irc"]) != `{"nick":"ada_"}` || string(cur.Ext["lang"]) != `"en"` {
		t.Fatalf("ApplyMe: %+v", cur)
	}
	if old.Name != "Ada" || old.Avatar == "" || len(old.Ext) != 2 || string(old.Ext["tz"]) != `"UTC"` {
		t.Fatalf("ApplyMe changed old: %+v", old)
	}
	cur.Roles[0] = "changed"
	if old.Roles[0] != "admin" {
		t.Fatal("ApplyMe's result shares old's roles")
	}
	if same := server.ApplyMe(old, apron.MeParams{}); !reflect.DeepEqual(same, old) {
		t.Fatalf("an empty me: %+v", same)
	}

	cur.Roles = nil
	complete := cur.Complete(old)
	if !reflect.DeepEqual(complete.User, cur) || !slices.Equal(complete.Cleared, []string{"avatar", "roles"}) {
		t.Fatalf("Complete: %+v", complete)
	}
	raw, err := apron.Marshal(complete)
	if err != nil || string(raw) != `{"user_id":"ada","name":"Ada L","status":"online","ext":{"irc":{"nick":"ada_"},"lang":"en"},"avatar":"","roles":[]}` {
		t.Fatalf("Complete encodes as %s %v", raw, err)
	}
	if other := cur.Complete(apron.User{UserID: "bob", Avatar: "x"}); len(other.Cleared) != 0 {
		t.Fatalf("Complete for another user clears %v", other.Cleared)
	}
}

// A Server that takes messages but answers no me says so as it starts
// serving.
func TestNoMeWarning(t *testing.T) {
	for _, test := range []struct {
		name     string
		profiles bool
		want     string
	}{
		{"without me", false, "answers no me"},
		{"with me", true, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			var log logged
			config := server.Config{Messages: &backend{}, Logger: log.logger()}
			if test.profiles {
				config.Profiles = profiles{}
			}
			s := server.New(&backend{}, config)
			c := aprontest.Connect(t, s)
			if err := c.Sync(wait(t)); err != nil {
				t.Fatal(err)
			}
			out := log.String()
			if !strings.Contains(out, "level=DEBUG msg=\"wire: serving\"") {
				t.Errorf("no debug line: %s", out)
			}
			if got := strings.Contains(out, "answers no me"); got != (test.want != "") || strings.Contains(out, "takes no messages") {
				t.Errorf("logged %s", out)
			}
		})
	}
}

type profiles struct{}

func (profiles) Me(_ context.Context, c *server.Conn, p apron.MeParams, reply server.Reply[apron.CurrentUser]) error {
	return reply.Send(apron.CurrentUser{User: *c.User()})
}
