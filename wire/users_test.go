package wire_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/wire"
)

// helloBackend is a Backend of Local connections only.
type helloBackend struct{}

func (helloBackend) Hello(context.Context, *wire.Conn) wire.ServerInfo {
	return wire.ServerInfo{Auth: []string{"guest"}}
}

func (helloBackend) Auth(context.Context, *wire.Conn, wire.AuthParams, wire.Reply[wire.AuthResult]) error {
	return wire.Denied("No sign-in")
}

// viewer is a Local connection and the frames sent to it.
type viewer struct {
	l      *wire.Local
	frames chan string
}

func openViewer(t *testing.T, s *wire.Server, u wire.User) viewer {
	t.Helper()
	frames := make(chan string, 16)
	l, err := s.Local(wire.LocalOptions{User: u, OnFrame: func(f wire.Frame) { frames <- string(f) }})
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

// PublishUserChange sends the user's own connections the patch of their
// view, and others the patch of the shown one, each only when it changed.
func TestPublishUserChange(t *testing.T) {
	s := &wire.Server{Backend: helloBackend{}}
	chosen := wire.User{UserID: "ada", Name: "Ada", Status: "online"}
	ada := openViewer(t, s, chosen)
	ada2 := openViewer(t, s, chosen)
	bob := openViewer(t, s, wire.User{UserID: "bob"})
	eve := openViewer(t, s, wire.User{UserID: "eve"})
	notEve := func(c *wire.Conn) bool { return c.UserID() != "eve" }
	// marker is a change everyone is sent, which shows that nothing was
	// sent before it.
	marker := func(name string) {
		t.Helper()
		if !s.PublishUser(wire.User{UserID: "carol"}, wire.User{UserID: "carol", Name: name}, nil) {
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
	if !s.PublishUserChange(wire.UserChange{Old: chosen, New: dnd, OldShown: chosen, NewShown: dnd}, notEve) {
		t.Fatal("dnd: no change")
	}
	ada.expect(t, `{"method":"user","params":{"you":{"user_id":"ada","status":"dnd"}}}`)
	ada2.expect(t, `{"method":"user","params":{"you":{"user_id":"ada","status":"dnd"}}}`)
	bob.expect(t, `{"method":"user","params":{"new":{"user_id":"ada","status":"dnd"}}}`)
	marker("1")

	// A chosen invisible: others see offline, with seen (§4.5.2).
	invisible := chosen
	invisible.Status = "invisible"
	shownOffline := wire.User{UserID: "ada", Name: "Ada", Status: "offline", Seen: "1724803600000"}
	if !s.PublishUserChange(wire.UserChange{Old: dnd, New: invisible, OldShown: dnd, NewShown: shownOffline}, notEve) {
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
	ada.l.Conn().SetUser(wire.User{UserID: "ada", Name: "Stale"})
	idle, online := chosen, chosen
	idle.Status = "idle"
	if !s.PublishUserChange(wire.UserChange{Old: chosen, New: chosen, OldShown: online, NewShown: idle}, notEve) {
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
	if !s.PublishUserChange(wire.UserChange{Old: invisible, New: renamed, OldShown: shownOffline, NewShown: shownRenamed},
		func(*wire.Conn) bool { t.Error("shares called without a change of others' view"); return true }) {
		t.Fatal("own view: no change")
	}
	ada.expect(t, `{"method":"user","params":{"you":{"user_id":"ada","name":"Ada L"}}}`)
	ada2.expect(t, `{"method":"user","params":{"you":{"user_id":"ada","name":"Ada L"}}}`)
	if u := ada.l.Conn().User(); u.Name != "Ada L" {
		t.Fatalf("ada's identity is %+v", u)
	}
	marker("4")

	// Neither: nothing at all.
	if s.PublishUserChange(wire.UserChange{Old: renamed, New: renamed, OldShown: shownOffline, NewShown: shownOffline},
		func(*wire.Conn) bool { t.Error("shares called without a change"); return true }) {
		t.Fatal("reported a change where there was none")
	}
	marker("5")

	// A user_id change: others are sent the shown objects, old too, with
	// Old's and New's user_ids whatever the shown objects hold.
	account := wire.User{UserID: "ada_l", Name: "Ada L", Status: "invisible"}
	if !s.PublishUserChange(wire.UserChange{Old: renamed, New: account, OldShown: shownOffline, NewShown: wire.User{UserID: "ignored", Name: "Ada L", Status: "offline"}}, notEve) {
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
	if !s.PublishUserChange(wire.UserChange{Old: account, New: back, OldShown: wire.User{UserID: "x", Name: "unused"}}, nil) {
		t.Fatal("one view: no change")
	}
	ada.expect(t, `{"method":"user","params":{"you":{"user_id":"ada_l","status":"online"}}}`)
	ada2.expect(t, `{"method":"user","params":{"you":{"user_id":"ada_l","status":"online"}}}`)
	bob.expect(t, `{"method":"user","params":{"new":{"user_id":"ada_l","status":"online"}}}`)
	eve.expect(t, `{"method":"user","params":{"new":{"user_id":"ada_l","status":"online"}}}`)
}
