package apron

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/store"
)

// testExtension is an Extension made of a name and an Init function.
type testExtension struct {
	name string
	init func(h *Host) error
}

func (e testExtension) Name() string       { return e.name }
func (e testExtension) Init(h *Host) error { return e.init(h) }

func extensionConfig(extensions ...Extension) Config {
	config := DefaultConfig()
	config.Extensions = extensions
	return config
}

// runCommand sends a command line in general and returns the frames before
// its reply, and the reply.
func runCommand(t *testing.T, c *testClient, line string) ([]map[string]any, map[string]any) {
	t.Helper()
	id := c.nextID("command")
	c.write(t, map[string]any{"method": "command", "id": id, "params": map[string]any{"room_id": "general", "body": map[string]any{"text": line}}})
	var before []map[string]any
	for {
		frame := c.read(t)
		if frame["id"] == id {
			return before, frame
		}
		before = append(before, frame)
	}
}

func noticeText(t *testing.T, frame map[string]any) string {
	t.Helper()
	params := notificationParams(t, frame, "message")
	if from := params["from"].(map[string]any); from["user_id"] != privateNoticeID {
		t.Fatalf("notice from %#v, want %s", from, privateNoticeID)
	}
	return params["body"].(map[string]any)["text"].(string)
}

func TestExtensionCommands(t *testing.T) {
	_, httpServer := newTestServer(t, extensionConfig(testExtension{"demo", func(h *Host) error {
		h.Command(Command{
			Name: "echo", Usage: "/echo <text>", Help: "say it back",
			Run: func(_ context.Context, call *CommandCall) (map[string]any, error) {
				if call.Args == "" {
					return nil, InvalidParams("Usage: /echo <text>")
				}
				call.Notice(call.User.ID + " said " + call.Args + " in " + call.Room.ID)
				return map[string]any{"echoed": call.Args}, nil
			},
		})
		h.Command(Command{
			Name: "staff", Help: "for staff",
			Available: func(u User, _ Room) bool { return u.HasRole("staff") },
			Run:       func(context.Context, *CommandCall) (map[string]any, error) { return nil, nil },
		})
		h.Command(Command{
			Name: "fail",
			Run: func(context.Context, *CommandCall) (map[string]any, error) {
				return nil, errors.New("the database is down")
			},
		})
		return nil
	}}))
	c := dialTestClient(t, httpServer)

	before, reply := runCommand(t, c, "/help")
	help := noticeText(t, before[0])
	if !strings.Contains(help, "`/echo <text>`: say it back") || strings.Contains(help, "/staff") || !strings.Contains(help, "`/emoji") {
		t.Fatalf("/help = %q, want /echo and the core commands, without /staff", help)
	}
	if _, ok := reply["result"]; !ok {
		t.Fatalf("/help reply = %#v", reply)
	}

	before, reply = runCommand(t, c, "/echo hello there")
	if len(before) != 1 || noticeText(t, before[0]) != c.userID+" said hello there in general" {
		t.Fatalf("/echo frames = %#v", before)
	}
	if result := reply["result"].(map[string]any); result["echoed"] != "hello there" {
		t.Fatalf("/echo result = %#v", reply)
	}
	for line, code := range map[string]int{"/echo": CodeInvalidParams, "/staff": CodeDenied, "/fail": CodeInternalError} {
		_, reply = runCommand(t, c, line)
		if failure, _ := reply["error"].(map[string]any); failure["code"] != float64(code) {
			t.Fatalf("%s = %#v, want error %d", line, reply, code)
		}
	}
}

func TestExtensionRegistrationErrors(t *testing.T) {
	for name, init := range map[string]func(h *Host) error{
		"core command": func(h *Host) error {
			h.Command(Command{Name: "kick", Run: func(context.Context, *CommandCall) (map[string]any, error) { return nil, nil }})
			return nil
		},
		"foreign method": func(h *Host) error { h.Method("message", nil); return nil },
		"core route":     func(h *Host) error { h.Handle("/ws", http.NotFoundHandler()); return nil },
		"two sign-ins":   func(h *Host) error { h.ExternalSignIn(nil); h.ExternalSignIn(nil); return nil },
		"init fails":     func(*Host) error { return errors.New("no config") },
		"command no Run": func(h *Host) error { h.Command(Command{Name: "x"}); return nil },
		"bad command name": func(h *Host) error {
			h.Command(Command{Name: "Bad Name", Run: func(context.Context, *CommandCall) (map[string]any, error) { return nil, nil }})
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			if app, err := Open(extensionConfig(testExtension{"demo", init})); err == nil {
				_ = app.Shutdown(context.Background())
				t.Fatal("Open succeeded")
			}
		})
	}
	for _, name := range []string{"emoji", "Bad", ""} {
		if app, err := Open(extensionConfig(testExtension{name, func(*Host) error { return nil }})); err == nil {
			_ = app.Shutdown(context.Background())
			t.Fatalf("Open with extension %q succeeded", name)
		}
	}
}

func TestExtensionMethodAdvertiseAndRoute(t *testing.T) {
	_, httpServer := newTestServer(t, extensionConfig(testExtension{"calc", func(h *Host) error {
		h.Advertise(map[string]any{"version": 1})
		h.Method("calc.add", func(_ context.Context, call *MethodCall) (any, error) {
			var params struct{ A, B int }
			if err := call.Decode(&params); err != nil {
				return nil, err
			}
			return map[string]any{"sum": params.A + params.B, "by": call.User.ID}, nil
		})
		h.Handle("GET /calc/hello", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "hi")
		}))
		return nil
	}}))
	c, frame := dialRaw(t, httpServer)
	params := frame["params"].(map[string]any)
	if !slices.Contains(asList(params["capabilities"]), any("ext:calc")) {
		t.Fatalf("capabilities = %#v, want ext:calc", params["capabilities"])
	}
	if ext := params["ext"].(map[string]any); ext["calc"].(map[string]any)["version"] != float64(1) {
		t.Fatalf("ext = %#v", ext)
	}
	c.expectError(t, "calc.add", "early", map[string]any{"a": 1}, CodeDenied)
	guestAuth(t, c)
	result := c.result(t, "calc.add", "add", map[string]any{"a": 2, "b": 3})
	if result["sum"] != float64(5) || result["by"] != c.userID {
		t.Fatalf("calc.add = %#v", result)
	}
	c.expectError(t, "calc.add", "bad", map[string]any{"a": "two"}, CodeInvalidParams)
	if status, _, body := httpDo(t, http.MethodGet, httpServer.URL+"/calc/hello", nil, ""); status != http.StatusOK || string(body) != "hi" {
		t.Fatalf("GET /calc/hello = %d %q", status, body)
	}
}

func TestExtensionFilter(t *testing.T) {
	_, httpServer := newTestServer(t, extensionConfig(testExtension{"clean", func(h *Host) error {
		h.FilterMessage(func(_ context.Context, d *Draft) error {
			if strings.Contains(d.Text(), "spam") {
				return Denied("No spam, " + d.From.ID)
			}
			d.Body["text"] = strings.ReplaceAll(d.Text(), "darn", "d**n")
			return nil
		})
		return nil
	}}))
	c := dialTestClient(t, httpServer)
	_, snapshot := save(t, c, "m1", map[string]any{"body": map[string]any{"text": "darn it"}})
	if text := snapshot["body"].(map[string]any)["text"]; text != "d**n it" {
		t.Fatalf("filtered text = %q", text)
	}
	frame := c.call(t, "message", "m2", map[string]any{"room_id": "general", "body": map[string]any{"text": "buy spam"}})
	if failure, _ := frame["error"].(map[string]any); failure["code"] != float64(CodeDenied) || failure["message"] != "No spam, "+c.userID {
		t.Fatalf("spam = %#v", frame)
	}
}

func TestExtensionSessionEventsAndViews(t *testing.T) {
	events := make(chan Event, 16)
	frames := make(chan jsontext.Value, 64)
	var host *Host
	var bot *Session
	_, httpServer := newTestServer(t, extensionConfig(testExtension{"helper", func(h *Host) error {
		host = h
		h.OnEvent(func(_ context.Context, e Event) { events <- e })
		var err error
		bot, err = h.Session(context.Background(), "helper", SessionOptions{Name: "Helper", OnFrame: func(frame jsontext.Value) { frames <- frame }})
		return err
	}}))
	user, ok := host.User("helper")
	if !ok || !user.Bot || !user.Account || !user.HasRole("bot") || user.Name != "Helper" || user.Created.IsZero() {
		t.Fatalf("bot user = %#v", user)
	}
	if _, err := host.Session(context.Background(), "helper", SessionOptions{}); err != nil {
		t.Fatalf("second session: %v", err)
	}

	c := dialTestClient(t, httpServer)
	if _, err := host.Session(context.Background(), c.userID, SessionOptions{}); err == nil {
		t.Fatal("a bot took a guest's user_id")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := bot.Post(ctx, "general", "hello @"+c.userID, Mention(c.userID), Markdown(), WithExt("helper", map[string]any{"n": 1}))
	if err != nil {
		t.Fatal(err)
	}
	got := notificationParams(t, c.read(t), "message")
	if got["message_id"] != id || got["from"].(map[string]any)["user_id"] != "helper" {
		t.Fatalf("guest got %#v", got)
	}
	m := waitMessageEvent(t, events, id)
	if m.From.UserID != "helper" || !m.MentionsUser(c.userID) || m.Format != "markdown" || string(m.Ext["helper"]) != `{"n":1}` || !m.New() {
		t.Fatalf("event message = %#v", m)
	}

	// The guest's message reaches the bot as a frame, and extensions as an
	// event.
	guestID, _ := save(t, c, "m1", map[string]any{"body": map[string]any{"text": "hi helper"}})
	waitMessageEvent(t, events, guestID)
	for frame := range frames {
		if strings.Contains(string(frame), `"message_id":"`+guestID+`"`) {
			break
		}
	}

	replyID, err := bot.Post(ctx, "general", "hi yourself", ReplyTo(guestID))
	if err != nil {
		t.Fatal(err)
	}
	if got := notificationParams(t, c.read(t), "message"); got["message_id"] != replyID {
		t.Fatalf("guest got %#v, want the reply", got)
	}
	if reply := waitMessageEvent(t, events, replyID); reply.ReplyTo != guestID {
		t.Fatalf("reply = %#v", reply)
	}

	history, err := bot.History(ctx, "general", HistoryOptions{Limit: 10})
	if err != nil || len(history) != 3 || history[0].ID != id || history[1].Text != "hi helper" || history[2].ReplyTo != guestID {
		t.Fatalf("history = %#v, %v", history, err)
	}

	// A private room is visible to its members only.
	roomID, _ := saveRoom(t, c, "r1", map[string]any{"title": "Secret", "private": true})
	if host.Visible("helper", roomID) || !host.Visible(c.userID, roomID) || host.Visible("", roomID) || !host.Visible("", "general") {
		t.Fatal("Visible disagrees with the private room")
	}
	if room, _ := host.Room(roomID); !room.Private || room.Public || room.Title != "Secret" {
		t.Fatalf("room = %#v", room)
	}
	if _, err := bot.Post(ctx, roomID, "let me in"); err == nil {
		t.Fatal("the bot posted to a private room it is not in")
	}
	if err := host.RoomNotice("general", "maintenance soon"); err != nil {
		t.Fatal(err)
	}
	if err := bot.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := bot.Post(ctx, "general", "gone"); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("post after close: %v", err)
	}
}

func waitMessageEvent(t *testing.T, events chan Event, messageID string) Message {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case e := <-events:
			if m, ok := DecodeMessage(e); ok && m.ID == messageID {
				return m
			}
		case <-timeout:
			t.Fatalf("no event for message %s", messageID)
		}
	}
}

func TestExtensionStreamAppend(t *testing.T) {
	var bot *Session
	_, httpServer := newTestServer(t, extensionConfig(testExtension{"streamer", func(h *Host) error {
		var err error
		bot, err = h.Session(context.Background(), "streamer", SessionOptions{})
		return err
	}}))
	c := dialTestClient(t, httpServer)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	w, err := bot.StreamAppend(ctx, "general", "**Answer:** ")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "hello "); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "world"); err != nil {
		t.Fatal(err)
	}
	posted := notificationParams(t, c.read(t), "message")
	if posted["message_id"] != w.MessageID() || len(embedsOf(t, posted)) != 1 {
		t.Fatalf("posted = %#v", posted)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	final := notificationParams(t, c.read(t), "message")
	body := final["body"].(map[string]any)
	if text, _ := body["text"].(string); !strings.HasPrefix(text, "**Answer:**") || !strings.HasSuffix(text, "hello world") || body["embeds"] != nil {
		t.Fatalf("final = %#v", final)
	}
}

func TestExtensionEventsReplayAndKV(t *testing.T) {
	memory := store.NewMemory()
	config := extensionConfig(testExtension{"keeper", func(h *Host) error {
		return h.Store().Put("greeting", map[string]any{"text": "hi"})
	}})
	config.Store = memory
	app, httpServer := newTestServer(t, config)
	c := dialTestClient(t, httpServer)
	id, _ := save(t, c, "m1", map[string]any{"body": map[string]any{"text": "remember me"}})
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	events := make(chan Event, 64)
	var kept struct{ Text string }
	var found bool
	config = extensionConfig(testExtension{"keeper", func(h *Host) error {
		var err error
		found, err = h.Store().Get("greeting", &kept)
		if keys := h.Store().Keys("gree"); !slices.Equal(keys, []string{"greeting"}) {
			t.Errorf("keys = %v", keys)
		}
		h.OnEventFrom(0, func(_ context.Context, e Event) error {
			events <- e
			return nil
		})
		return err
	}})
	config.Store = memory
	newTestServer(t, config)
	if !found || kept.Text != "hi" {
		t.Fatalf("kept = %#v, %v", kept, found)
	}
	first := <-events
	if first.Kind != EventRoom || !slices.Equal(first.Rooms, []string{"general"}) {
		t.Fatalf("first event = %#v, want general's creation", first)
	}
	waitMessageEvent(t, events, id)
}

func TestDisableGuests(t *testing.T) {
	config := DefaultConfig()
	config.DisableGuests = true
	_, httpServer := newTestServer(t, config)
	c, frame := dialRaw(t, httpServer)
	if auth := asList(frame["params"].(map[string]any)["auth"]); slices.Contains(auth, any("guest")) {
		t.Fatalf("auth = %#v, want no guest", auth)
	}
	c.expectError(t, "auth", "a1", map[string]any{"scheme": "guest"}, CodeUnsupported)
}

func TestExternalSignIn(t *testing.T) {
	var host *Host
	_, httpServer := newTestServer(t, extensionConfig(testExtension{"sso", func(h *Host) error {
		host = h
		h.ExternalSignIn(func(w http.ResponseWriter, r *http.Request, p ExternalProposal) {
			http.Redirect(w, r, "/sso/callback?p="+p.ID, http.StatusFound)
		})
		h.Handle("/sso/callback", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = h.CompleteExternal(w, r, r.URL.Query().Get("p"), Identity{Subject: "idp:1", Name: "Ada", UserID: "ada", Roles: []string{"Staff"}})
		}))
		return nil
	}}))
	origin := httpServer.URL

	// With return_url, the page links back to the client.
	c, frame := dialOrigin(t, httpServer, origin)
	if auth := asList(frame["params"].(map[string]any)["auth"]); !slices.Contains(auth, any("external")) || !slices.Contains(auth, any("token")) {
		t.Fatalf("auth = %#v", auth)
	}
	c.expectError(t, "auth", "p0", map[string]any{"scheme": "external", "return_url": "https://evil.example/"}, CodeDenied)
	result := c.result(t, "auth", "p1", map[string]any{"scheme": "external", "return_url": origin + "/app/", "state": "st1"})
	pageURL := result["url"].(string)
	status, _, body := httpDo(t, http.MethodGet, pageURL, nil, "")
	link := regexp.MustCompile(`href="([^"]+)"`).FindStringSubmatch(string(body))
	if status != http.StatusOK || link == nil || !strings.Contains(link[1], "state=st1") {
		t.Fatalf("page = %d %s", status, body)
	}
	token := regexp.MustCompile(`#token=([^&]+)`).FindStringSubmatch(strings.ReplaceAll(link[1], "&amp;", "&"))[1]
	if status, _, _ := httpDo(t, http.MethodGet, pageURL, nil, ""); status != http.StatusNotFound {
		t.Fatalf("second visit = %d, want 404", status)
	}

	// The link signs in elsewhere on the client's origin.
	other, _ := dialRaw(t, httpServer)
	other.expectError(t, "auth", "a0", map[string]any{"scheme": "external", "token": token}, CodeDenied)
	signIn, _ := dialOrigin(t, httpServer, origin)
	_, result = signIn.request(t, "auth", "a1", map[string]any{"scheme": "external", "token": token})
	you := result["you"].(map[string]any)
	if you["user_id"] != "ada" || you["name"] != "Ada" || result["token"] == nil || !slices.Equal(asList(you["roles"]), []any{"staff"}) {
		t.Fatalf("sign-in = %#v", result)
	}
	if user, _ := host.User("ada"); !user.Account || user.Bot {
		t.Fatalf("user = %#v", user)
	}

	// Without return_url, the page shows a code for the proposing
	// connection, which signs in to the same account.
	again, _ := dialRaw(t, httpServer)
	result = again.result(t, "auth", "p2", map[string]any{"scheme": "external"})
	_, _, body = httpDo(t, http.MethodGet, result["url"].(string), nil, "")
	code := regexp.MustCompile(`class="code">(\d{6})<`).FindStringSubmatch(string(body))
	if code == nil {
		t.Fatalf("page = %s", body)
	}
	_, result = again.request(t, "auth", "a2", map[string]any{"scheme": "external", "token": code[1]})
	if result["you"].(map[string]any)["user_id"] != "ada" {
		t.Fatalf("code sign-in = %#v", result)
	}
}
