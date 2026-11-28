package server_test

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

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/aprontest"
	"github.com/apron-chat/apron-server-go/apron/server"
)

// local opens a Local connection to s as ada, closed when the test ends.
func local(t *testing.T, s *server.Server) *server.Local {
	t.Helper()
	l, err := s.Local(server.LocalOptions{User: apron.User{UserID: "ada"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func call(t *testing.T, l *server.Local, method, params string) (string, error) {
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

func (h *commands) Command(_ context.Context, _ *server.Conn, p apron.CommandParams, reply server.Reply[apron.CommandResult]) error {
	switch p.Body.Text {
	case "/twice":
		first := reply.Send(apron.CommandResult{Unknown: jsontext.Value(`{"n":1}`)})
		second := reply.Send(apron.CommandResult{Unknown: jsontext.Value(`{"n":2}`)})
		h.mu.Lock()
		h.second = second
		h.mu.Unlock()
		return first
	case "/forget":
		return nil
	case "/late":
		if err := reply.Send(apron.CommandResult{}); err != nil {
			return err
		}
		return apron.Denied("too late")
	}
	return apron.InvalidParams("unknown")
}

// statuses answers status with nothing, as a Reply[OK] handler may.
type statuses struct{}

func (statuses) Status(context.Context, *server.Conn, apron.StatusParams, server.Reply[apron.OK]) error {
	return nil
}

// A handler sends once: a second Send is ErrReplied, and only the first is
// sent. One that returns nil without sending is answered internal_error and
// logged, but for a Reply[OK]'s, which is sent {}.
func TestSendOnce(t *testing.T) {
	var log logged
	h := &commands{}
	s := server.New(&backend{}, server.Config{Commands: h, Status: statuses{}, Logger: log.logger()})
	l := local(t, s)
	if result, err := call(t, l, "command", `{"body":{"text":"/twice"}}`); err != nil || result != `{"n":1}` {
		t.Fatalf("got %s %v, want the first result", result, err)
	}
	h.mu.Lock()
	second := h.second
	h.mu.Unlock()
	if !errors.Is(second, server.ErrReplied) {
		t.Fatalf("a second Send returned %v, want ErrReplied", second)
	}
	if _, err := call(t, l, "command", `{"body":{"text":"/forget"}}`); apron.CodeOf(err) != apron.CodeInternalError {
		t.Fatalf("a handler that never sent: %v, want internal_error", err)
	}
	if out := log.String(); !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "without replying") {
		t.Fatalf("the missing reply was not logged: %s", out)
	}
	if result, err := call(t, l, "status", `{"idle":false}`); err != nil || result != `{}` {
		t.Fatalf("status: %s %v, want {}", result, err)
	}
	// An error after Send is logged, and the client has the result.
	if result, err := call(t, l, "command", `{"body":{"text":"/late"}}`); err != nil || result != `{}` {
		t.Fatalf("late: %s %v", result, err)
	}
	if out := log.String(); !strings.Contains(out, "too late") {
		t.Fatalf("the error after Send was not logged: %s", out)
	}
}

// rooms lists what it is given, and embeds ReadOnlyRooms for the rest.
type rooms struct {
	server.ReadOnlyRooms
	result apron.RoomListResult
}

func (h *rooms) RoomList(_ context.Context, _ *server.Conn, _ apron.RoomListParams, reply server.Reply[apron.RoomListResult]) error {
	return reply.Send(h.result)
}

// Send makes a room_list result what its filter and members ask for.
func TestRoomListSend(t *testing.T) {
	member := apron.User{UserID: "bob"}
	general := apron.RoomView{RoomID: "general", Members: []apron.CurrentUser{{User: member}}, MemberCount: 9}
	h := &rooms{result: apron.RoomListResult{NotJoined: []apron.RoomView{general}, Users: []apron.CurrentUser{{User: member}}}}
	s := server.New(&backend{}, server.Config{Rooms: h})
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
}

// ReadOnlyRooms refuses what a RoomList-only handler does not take,
// and the server has capability rooms.
func TestReadOnlyRooms(t *testing.T) {
	s := server.New(&backend{}, server.Config{Rooms: &rooms{}})
	l := local(t, s)
	for method, params := range map[string]string{
		"room_join":  `{"room_id":"general"}`,
		"room_leave": `{"room_id":"general"}`,
		"room_set":   `{"title":"x"}`,
	} {
		if _, err := call(t, l, method, params); apron.CodeOf(err) != apron.CodeUnsupported {
			t.Errorf("%s: %v, want unsupported", method, err)
		}
	}
	l.Conn().Announce(apron.ServerInfo{})
	if !slices.Contains(l.Conn().ServerInfo().Capabilities, "rooms") {
		t.Fatalf("capabilities: %v", l.Conn().ServerInfo().Capabilities)
	}
}

// broadcasting signs in as Send queues its auth result, under its own lock,
// and then broadcasts and publishes the user, as a backend that keeps its
// state under a lock does: the connection gets both.
type broadcasting struct {
	backend
	s  *server.Server
	mu sync.Mutex
}

func (b *broadcasting) Auth(_ context.Context, c *server.Conn, p apron.AuthParams, reply server.Reply[apron.AuthResult]) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	you := apron.User{UserID: "ada", Name: "Ada"}
	if p.UserID == "bad id" {
		you.UserID = p.UserID
	}
	if err := reply.Send(apron.AuthResult{You: &apron.CurrentUser{User: you}}); err != nil {
		return err
	}
	if c.UserID() != "ada" {
		return errors.New("not signed in as Send returned")
	}
	frame, _ := server.EncodeRaw("joined", map[string]any{"user_id": "ada"})
	b.s.Broadcast(frame, nil)
	b.s.PublishUser(server.UserChange{Old: you, New: apron.User{UserID: "ada", Name: "Ada L"}}, nil)
	return nil
}

func TestAuthSendSignsIn(t *testing.T) {
	b := &broadcasting{}
	b.s = server.New(b, server.Config{Logger: slog.New(slog.DiscardHandler)})
	c := aprontest.Connect(t, b.s)
	if _, err := c.Auth(wait(t), map[string]any{"scheme": "guest", "user_id": "bad id"}); apron.CodeOf(err) != apron.CodeInternalError {
		t.Fatalf("got %v, want internal_error for an invalid user_id", err)
	}
	result, before, err := c.CallFrames(wait(t), "auth", map[string]any{"scheme": "guest"})
	if err != nil || len(before) != 0 || !strings.Contains(string(result), `"you"`) {
		t.Fatalf("got %s %v, %d frames before, want the auth result first", result, err, len(before))
	}
	if f := next(t, c); f.Method != "joined" {
		t.Fatalf("got %s, want the broadcast after the result", f.Raw)
	}
	var update apron.UserUpdate
	if f := next(t, c); f.Method != "user" || f.Decode(&update) != nil || update.You == nil || update.You.Name != "Ada L" {
		t.Fatalf("got %s, want the published user", f.Raw)
	}
	if u := userOf(onlyConn(t, b.s)); u == nil || u.Name != "Ada L" {
		t.Fatalf("the connection's user is %+v", u)
	}
}

// A method may be registered before the Server serves, and after.
func TestHandleAnyTime(t *testing.T) {
	b := &backend{}
	s := server.New(b, server.Config{Messages: b, Commands: &commands{}})
	s.Handle("echo", server.Handler{Capability: "ext:echo", Serve: func(_ context.Context, _ *server.Conn, r *server.Request, reply server.Reply[any]) error {
		return reply.Send(r.Fields())
	}})
	l := local(t, s)
	if _, err := call(t, l, "message", `{"body":{"text":"hi"}}`); err != nil || b.messages.Load() != 1 {
		t.Fatalf("message: %v", err)
	}
	if result, err := call(t, l, "echo", `{"a":1}`); err != nil || result != `{"a":1}` {
		t.Fatalf("echo: %s %v", result, err)
	}
	// A method registered once the Server serves is served, and listed in
	// the server frames after.
	s.Handle("late", server.Handler{Capability: "ext:late", Serve: func(context.Context, *server.Conn, *server.Request, server.Reply[any]) error { return nil }})
	if result, err := call(t, l, "late", `{}`); err != nil || result != `{}` {
		t.Fatalf("late: %s %v", result, err)
	}
	l.Conn().Announce(apron.ServerInfo{})
	if got := l.Conn().ServerInfo().Capabilities; !slices.Equal(got, []string{"command", "ext:echo", "ext:late"}) {
		t.Fatalf("capabilities: %v", got)
	}
}

// A Server is made with New, with a Backend: a zero one, or one
// without a Backend, panics at once rather than when it first serves.
func TestNewServerPanics(t *testing.T) {
	for name, use := range map[string]func(){
		"no backend": func() { server.New(nil, server.Config{}) },
		"zero Handle": func() {
			new(server.Server).Handle("x", server.Handler{Serve: func(context.Context, *server.Conn, *server.Request, server.Reply[any]) error { return nil }})
		},
		"zero Local": func() { _, _ = new(server.Server).Local(server.LocalOptions{User: apron.User{UserID: "ada"}}) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			use()
		}()
	}
}

// Handle and HandleNotification refuse what they cannot serve.
func TestHandlePanics(t *testing.T) {
	serve := func(context.Context, *server.Conn, *server.Request, server.Reply[any]) error { return nil }
	notify := func(context.Context, *server.Conn, *server.Request) {}
	for name, register := range map[string]func(s *server.Server){
		"auth":                  func(s *server.Server) { s.Handle("auth", server.Handler{Serve: serve}) },
		"ping":                  func(s *server.Server) { s.Handle("ping", server.Handler{Serve: serve}) },
		"ping notification":     func(s *server.Server) { s.HandleNotification("ping", server.NotificationHandler{Serve: notify}) },
		"activity as a request": func(s *server.Server) { s.Handle("activity", server.Handler{Serve: serve}) },
		"no Serve":              func(s *server.Server) { s.Handle("x", server.Handler{}) },
		"no notification Serve": func(s *server.Server) { s.HandleNotification("x", server.NotificationHandler{}) },
		"no name":               func(s *server.Server) { s.Handle("", server.Handler{Serve: serve}) },
		"twice": func(s *server.Server) {
			s.Handle("x", server.Handler{Serve: serve})
			s.HandleNotification("x", server.NotificationHandler{Serve: notify})
		},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			register(server.New(&backend{}, server.Config{}))
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
		s := server.New(b, server.Config{Messages: b, Edits: b})
		s.Handle("message", server.Handler{Capability: capability, Serve: func(_ context.Context, _ *server.Conn, r *server.Request, reply server.Reply[any]) error {
			return reply.Send(map[string]any{"overridden": true})
		}})
		s.HandleNotification("tick", server.NotificationHandler{Serve: func(context.Context, *server.Conn, *server.Request) {}})
		l := local(t, s)
		if result, err := call(t, l, "message", `{"message_id":"1","deleted":true}`); err != nil || result != `{"overridden":true}` || b.edits.Load() != 0 {
			t.Fatalf("got %s %v, %d edits", result, err, b.edits.Load())
		}
		l.Conn().Announce(apron.ServerInfo{})
		if got, want := l.Conn().ServerInfo().Capabilities, []string{capability}; !slices.Equal(got, want) && !(capability == "" && len(got) == 0) {
			t.Errorf("override with capability %q: capabilities %v", capability, got)
		}
		if _, err := call(t, l, "tick", `{}`); !errors.Is(err, server.ErrNotRequest) {
			t.Errorf("Call(tick): %v, want ErrNotRequest", err)
		}
	}
}

func TestOriginPatterns(t *testing.T) {
	check := server.OriginPatterns("*.example.com", "https://app.test")
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
	if !server.AnyOrigin(httptest.NewRequest(http.MethodGet, "/", nil)) {
		t.Error("AnyOrigin refused")
	}
}

// CheckOrigin decides who opens a WebSocket; without it, only the server's
// own origin may.
func TestCheckOrigin(t *testing.T) {
	dial := func(s *server.Server, origin string) error {
		ts := httptest.NewServer(s)
		defer ts.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		header := http.Header{}
		header.Set("Origin", origin)
		ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http"), &websocket.DialOptions{HTTPHeader: header})
		if err == nil {
			ws.CloseNow()
		}
		return err
	}
	if err := dial(wired(&backend{}, server.Config{}), "https://elsewhere.test"); err == nil {
		t.Error("another origin opened a WebSocket by default")
	}
	allowed := wired(&backend{}, server.Config{CheckOrigin: server.OriginPatterns("elsewhere.test")})
	if err := dial(allowed, "https://elsewhere.test"); err != nil {
		t.Errorf("an allowed origin: %v", err)
	}
	if err := dial(allowed, "https://other.test"); err == nil {
		t.Error("an origin CheckOrigin refuses opened a WebSocket")
	}
}

// badLogIDs sends what holds a LogID that is no log_id (§2.1): a
// notification past MaxLogID, and then a result with a negative one.
type badLogIDs struct{ notified chan bool }

func (h badLogIDs) serve(_ context.Context, c *server.Conn, _ *server.Request, reply server.Reply[any]) error {
	h.notified <- c.Notify(apron.Message{LogID: apron.MaxLogID + 1, RoomID: "r", From: apron.User{UserID: "ada"}, Body: &apron.Body{Text: "x"}})
	return reply.Send(map[string]any{"log_id": apron.LogID(-1)})
}

// A LogID that is no log_id is not sent: a result with one is answered
// error/internal_error, and a notification is dropped, both logged.
func TestLogIDOutOfRangeNotSent(t *testing.T) {
	var log logged
	h := badLogIDs{make(chan bool, 1)}
	s := server.New(&backend{}, server.Config{Logger: log.logger()})
	s.Handle("x_bad", server.Handler{Serve: h.serve})
	l := local(t, s)
	if result, err := call(t, l, "x_bad", `{}`); apron.CodeOf(err) != apron.CodeInternalError {
		t.Fatalf("got %s %v, want internal_error", result, err)
	}
	if <-h.notified {
		t.Error("a notification with a log_id past 2^53 was sent")
	}
	out := log.String()
	for _, want := range []string{"encoding a notification", "9007199254740992", "a result is not a JSON object", "not negative: -1"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log does not say %q: %s", want, out)
		}
	}
}
