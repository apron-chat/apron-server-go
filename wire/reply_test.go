package wire_test

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/apron-chat/apron-server-go/wire"
)

// local opens a Local connection to s as ada, closed when the test ends.
func local(t *testing.T, s *wire.Server) *wire.Local {
	t.Helper()
	l, err := s.Local(wire.LocalOptions{User: wire.User{UserID: "ada"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func call(t *testing.T, l *wire.Local, method, params string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := l.Call(ctx, method, jsontext.Value(params))
	return string(result), err
}

// logged is a Server's log, for tests that look at what it says.
type logged struct {
	mu  sync.Mutex
	out bytes.Buffer
}

func (l *logged) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(lockedWriter{&l.mu, &l.out}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func (l *logged) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.out.String()
}

// commands answers each command by its text, to test Send's contract.
type commands struct {
	mu     sync.Mutex
	second error
}

func (h *commands) Command(_ context.Context, _ *wire.Conn, p wire.CommandParams, reply wire.Reply[wire.CommandResult]) error {
	switch p.Body.Text {
	case "/twice":
		first := reply.Send(wire.CommandResult{Unknown: jsontext.Value(`{"n":1}`)})
		second := reply.Send(wire.CommandResult{Unknown: jsontext.Value(`{"n":2}`)})
		h.mu.Lock()
		h.second = second
		h.mu.Unlock()
		return first
	case "/forget":
		return nil
	case "/raw":
		reply.Request().Reply(map[string]any{"raw": true})
		return nil
	case "/late":
		if err := reply.Send(wire.CommandResult{}); err != nil {
			return err
		}
		return wire.Denied("too late")
	}
	return wire.InvalidParams("unknown")
}

// statuses answers status with nothing, as a Reply[Empty] handler may.
type statuses struct{}

func (statuses) Status(context.Context, *wire.Conn, wire.StatusParams, wire.Reply[wire.Empty]) error {
	return nil
}

// A handler sends once: a second Send is ErrReplied, and only the first is
// sent. One that returns nil without sending is answered internal_error and
// logged, but for a Reply[Empty]'s, which is sent {}.
func TestSendOnce(t *testing.T) {
	var log logged
	h := &commands{}
	s := &wire.Server{Backend: &backend{}, Commands: h, Status: statuses{}, Logger: log.logger()}
	l := local(t, s)
	if result, err := call(t, l, "command", `{"body":{"text":"/twice"}}`); err != nil || result != `{"n":1}` {
		t.Fatalf("got %s %v, want the first result", result, err)
	}
	h.mu.Lock()
	second := h.second
	h.mu.Unlock()
	if !errors.Is(second, wire.ErrReplied) {
		t.Fatalf("a second Send returned %v, want ErrReplied", second)
	}
	if _, err := call(t, l, "command", `{"body":{"text":"/forget"}}`); wire.CodeOf(err) != wire.CodeInternalError {
		t.Fatalf("a handler that never sent: %v, want internal_error", err)
	}
	if out := log.String(); !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "without replying") {
		t.Fatalf("the missing reply was not logged: %s", out)
	}
	if result, err := call(t, l, "status", `{"idle":false}`); err != nil || result != `{}` {
		t.Fatalf("status: %s %v, want {}", result, err)
	}
	// The raw request takes any result.
	if result, err := call(t, l, "command", `{"body":{"text":"/raw"}}`); err != nil || result != `{"raw":true}` {
		t.Fatalf("raw: %s %v", result, err)
	}
	// An error after Send is logged, and the client has the result.
	if result, err := call(t, l, "command", `{"body":{"text":"/late"}}`); err != nil || result != `{}` {
		t.Fatalf("late: %s %v", result, err)
	}
	if out := log.String(); !strings.Contains(out, "too late") {
		t.Fatalf("the error after Send was not logged: %s", out)
	}
}

// rooms lists what it is given, and embeds UnimplementedRooms for the rest.
type rooms struct {
	wire.UnimplementedRooms
	result wire.RoomListResult
	raw    bool
}

func (h *rooms) RoomList(_ context.Context, _ *wire.Conn, _ wire.RoomListParams, reply wire.Reply[wire.RoomListResult]) error {
	if h.raw {
		reply.Request().Reply(h.result)
		return nil
	}
	return reply.Send(h.result)
}

// Send makes a room_list result what its filter and members ask for; the
// raw reply sends it as it is.
func TestRoomListSend(t *testing.T) {
	member := wire.User{UserID: "bob"}
	general := wire.RoomView{Room: wire.Room{RoomID: "general"}, Members: []wire.User{member}, MemberCount: 9}
	h := &rooms{result: wire.RoomListResult{NotJoined: []wire.RoomView{general}, Users: []wire.User{member}}}
	s := &wire.Server{Backend: &backend{}, Rooms: h}
	l := local(t, s)
	for _, tc := range []struct{ params, want string }{
		{`{}`, `{"joined":[],"not_joined":[{"room_id":"general"}]}`},
		{`{"filter":"joined"}`, `{"joined":[]}`},
		{`{"filter":"not_joined","members":true}`, `{"not_joined":[{"room_id":"general","members":[{"user_id":"bob"}],"member_count":9}],"users":[{"user_id":"bob"}]}`},
	} {
		if result, err := call(t, l, "room_list", tc.params); err != nil || result != tc.want {
			t.Errorf("%s: got %s %v, want %s", tc.params, result, err, tc.want)
		}
	}
	if len(h.result.NotJoined[0].Members) != 1 {
		t.Fatal("Send changed the backend's own rooms")
	}
	h.raw = true
	if result, err := call(t, l, "room_list", `{"filter":"joined"}`); err != nil || !strings.Contains(result, `"members"`) || strings.Contains(result, `"joined"`) {
		t.Fatalf("raw: %s %v, want the result as it was", result, err)
	}
}

// UnimplementedRooms refuses what a RoomList-only handler does not take,
// and the server has capability rooms.
func TestUnimplementedRooms(t *testing.T) {
	s := &wire.Server{Backend: &backend{}, Rooms: &rooms{}}
	l := local(t, s)
	for method, params := range map[string]string{
		"room_join":  `{"room_id":"general"}`,
		"room_leave": `{"room_id":"general"}`,
		"room_set":   `{"title":"x"}`,
	} {
		if _, err := call(t, l, method, params); wire.CodeOf(err) != wire.CodeUnsupported {
			t.Errorf("%s: %v, want unsupported", method, err)
		}
	}
	l.Conn().Announce(wire.ServerInfo{})
	if !slices.Contains(l.Conn().Info().Capabilities, "rooms") {
		t.Fatalf("capabilities: %v", l.Conn().Info().Capabilities)
	}
}

// broadcasting signs in as Send queues its auth result, under its own lock,
// and then broadcasts and publishes the user, as a backend that keeps its
// state under a lock does: the connection gets both.
type broadcasting struct {
	backend
	s  *wire.Server
	mu sync.Mutex
}

func (b *broadcasting) Auth(_ context.Context, c *wire.Conn, p wire.AuthParams, reply wire.Reply[wire.AuthResult]) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	you := wire.User{UserID: "ada", Name: "Ada"}
	if p.UserID == "bad id" {
		you.UserID = p.UserID
	}
	if err := reply.Send(wire.AuthResult{You: &you}); err != nil {
		return err
	}
	if c.UserID() != "ada" {
		return errors.New("not signed in as Send returned")
	}
	frame, _ := wire.EncodeRaw("joined", map[string]any{"user_id": "ada"})
	b.s.Broadcast(frame, nil)
	b.s.PublishUser(you, wire.User{UserID: "ada", Name: "Ada L"}, nil)
	return nil
}

func TestAuthSendSignsIn(t *testing.T) {
	b := &broadcasting{}
	b.s = &wire.Server{Backend: b, Logger: slog.New(slog.DiscardHandler)}
	c := connect(t, b.s)
	if _, err := c.Auth(wait(t), map[string]any{"scheme": "guest", "user_id": "bad id"}); wire.CodeOf(err) != wire.CodeInternalError {
		t.Fatalf("got %v, want internal_error for an invalid user_id", err)
	}
	result, before, err := c.CallFrames(wait(t), "auth", map[string]any{"scheme": "guest"})
	if err != nil || len(before) != 0 || !strings.Contains(string(result), `"you"`) {
		t.Fatalf("got %s %v, %d frames before, want the auth result first", result, err, len(before))
	}
	if f := next(t, c); f.Method != "joined" {
		t.Fatalf("got %s, want the broadcast after the result", f.Raw)
	}
	var update wire.UserUpdate
	if f := next(t, c); f.Method != "user" || f.Decode(&update) != nil || update.You == nil || update.You.Name != "Ada L" {
		t.Fatalf("got %s, want the published user", f.Raw)
	}
	if u := onlyConn(t, b.s).User(); u == nil || u.Name != "Ada L" {
		t.Fatalf("the connection's user is %+v", u)
	}
}

// A Server's fields may be set in any order before it serves, and a
// method registered before the Backend is.
func TestConfigAnyOrder(t *testing.T) {
	s := &wire.Server{}
	s.Handle("echo", wire.Handler{Capability: "ext:echo", Serve: func(_ context.Context, _ *wire.Conn, r *wire.Request) error {
		r.Reply(r.Params)
		return nil
	}})
	b := &backend{}
	s.Backend = b
	s.Messages = b
	s.Commands = &commands{}
	l := local(t, s)
	if _, err := call(t, l, "message", `{"body":{"text":"hi"}}`); err != nil || b.messages.Load() != 1 {
		t.Fatalf("message: %v", err)
	}
	if result, err := call(t, l, "echo", `{"a":1}`); err != nil || result != `{"a":1}` {
		t.Fatalf("echo: %s %v", result, err)
	}
	// A method registered once the Server serves is served, and listed in
	// the server frames after.
	s.Handle("late", wire.Handler{Capability: "ext:late", Serve: func(context.Context, *wire.Conn, *wire.Request) error { return nil }})
	if result, err := call(t, l, "late", `{}`); err != nil || result != `{}` {
		t.Fatalf("late: %s %v", result, err)
	}
	l.Conn().Announce(wire.ServerInfo{})
	if got := l.Conn().Info().Capabilities; !slices.Equal(got, []string{"command", "ext:echo", "ext:late"}) {
		t.Fatalf("capabilities: %v", got)
	}
}

// Handle and HandleNotification refuse what they cannot serve.
func TestHandlePanics(t *testing.T) {
	serve := func(context.Context, *wire.Conn, *wire.Request) error { return nil }
	notify := func(context.Context, *wire.Conn, *wire.Request) {}
	for name, register := range map[string]func(s *wire.Server){
		"auth":                  func(s *wire.Server) { s.Handle("auth", wire.Handler{Serve: serve}) },
		"ping":                  func(s *wire.Server) { s.Handle("ping", wire.Handler{Serve: serve}) },
		"ping notification":     func(s *wire.Server) { s.HandleNotification("ping", wire.NotificationHandler{Serve: notify}) },
		"activity as a request": func(s *wire.Server) { s.Handle("activity", wire.Handler{Serve: serve}) },
		"no Serve":              func(s *wire.Server) { s.Handle("x", wire.Handler{}) },
		"no notification Serve": func(s *wire.Server) { s.HandleNotification("x", wire.NotificationHandler{}) },
		"no name":               func(s *wire.Server) { s.Handle("", wire.Handler{Serve: serve}) },
		"twice": func(s *wire.Server) {
			s.Handle("x", wire.Handler{Serve: serve})
			s.HandleNotification("x", wire.NotificationHandler{Serve: notify})
		},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			register(&wire.Server{Backend: &backend{}})
		}()
	}
}

// A method registered for a field's name takes its place, and the
// capabilities follow what is served: an override of message lists edit
// only when it says so. A notification registered with HandleNotification
// is no request for Local.Call.
func TestOverride(t *testing.T) {
	for _, capability := range []string{"edit", ""} {
		b := &editor{}
		s := &wire.Server{Backend: b, Messages: b, Edits: b}
		s.Handle("message", wire.Handler{Capability: capability, Serve: func(_ context.Context, _ *wire.Conn, r *wire.Request) error {
			r.Reply(map[string]any{"overridden": true})
			return nil
		}})
		s.HandleNotification("tick", wire.NotificationHandler{Serve: func(context.Context, *wire.Conn, *wire.Request) {}})
		l := local(t, s)
		if result, err := call(t, l, "message", `{"message_id":"1","deleted":true}`); err != nil || result != `{"overridden":true}` || b.edits.Load() != 0 {
			t.Fatalf("got %s %v, %d edits", result, err, b.edits.Load())
		}
		l.Conn().Announce(wire.ServerInfo{})
		if got, want := l.Conn().Info().Capabilities, []string{capability}; !slices.Equal(got, want) && !(capability == "" && len(got) == 0) {
			t.Errorf("override with capability %q: capabilities %v", capability, got)
		}
		if _, err := call(t, l, "tick", `{}`); !errors.Is(err, wire.ErrNotRequest) {
			t.Errorf("Call(tick): %v, want ErrNotRequest", err)
		}
	}
}

func TestOriginPatterns(t *testing.T) {
	check := wire.OriginPatterns("*.example.com", "https://app.test")
	for _, tc := range []struct {
		origin string
		want   bool
	}{
		{"", true},
		{"http://chat.local", true}, // The server's own.
		{"https://a.example.com", true},
		{"http://a.example.com", true},
		{"https://app.test", true},
		{"http://app.test", false},
		{"https://evil.test", false},
		{"%zz", false},
	} {
		r := httptest.NewRequest(http.MethodGet, "http://chat.local/ws", nil)
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if got := check(r); got != tc.want {
			t.Errorf("Origin %q: %v, want %v", tc.origin, got, tc.want)
		}
	}
	if !wire.AnyOrigin(httptest.NewRequest(http.MethodGet, "/", nil)) {
		t.Error("AnyOrigin refused")
	}
}

// CheckOrigin decides who opens a WebSocket; without it, only the server's
// own origin may.
func TestCheckOrigin(t *testing.T) {
	dial := func(s *wire.Server, origin string) error {
		server := httptest.NewServer(s)
		defer server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		header := http.Header{}
		header.Set("Origin", origin)
		ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), &websocket.DialOptions{HTTPHeader: header})
		if err == nil {
			ws.CloseNow()
		}
		return err
	}
	if err := dial(wired(&wire.Server{Backend: &backend{}}), "https://elsewhere.test"); err == nil {
		t.Error("another origin opened a WebSocket by default")
	}
	allowed := wired(&wire.Server{Backend: &backend{}, CheckOrigin: wire.OriginPatterns("elsewhere.test")})
	if err := dial(allowed, "https://elsewhere.test"); err != nil {
		t.Errorf("an allowed origin: %v", err)
	}
	if err := dial(allowed, "https://other.test"); err == nil {
		t.Error("an origin CheckOrigin refuses opened a WebSocket")
	}
}
