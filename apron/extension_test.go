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
	"sync/atomic"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/store"
)

// testExtension is an Extension made of a name and an Init function.
type testExtension struct {
	name string
	init func(r *Registrar) error
}

func (e testExtension) Name() string            { return e.name }
func (e testExtension) Init(r *Registrar) error { return e.init(r) }

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
	_, httpServer := newTestServer(t, extensionConfig(testExtension{"demo", func(r *Registrar) error {
		r.Command(Command{
			Name: "echo", Usage: "/echo <text>", Help: "say it back",
			Run: func(ctx context.Context, call *CommandCall) (map[string]any, error) {
				if call.Args == "" {
					return nil, InvalidParams("Usage: /echo <text>")
				}
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				call.Notice(call.User.ID + " said " + call.Args + " in " + call.Room.ID)
				return map[string]any{"echoed": call.Args}, nil
			},
		})
		r.Command(Command{
			Name: "staff", Help: "for staff",
			Available: func(u User, _ Room) bool { return u.HasRole("staff") },
			Run:       func(context.Context, *CommandCall) (map[string]any, error) { return nil, nil },
		})
		r.Command(Command{
			Name: "fail",
			Run: func(context.Context, *CommandCall) (map[string]any, error) {
				return nil, errors.New("the database is down")
			},
		})
		r.Command(Command{
			// A typed nil *Error is no error to refuse with: it must not
			// make a reply without a result or an error.
			Name: "typed-nil",
			Run: func(context.Context, *CommandCall) (map[string]any, error) {
				var e *Error
				return nil, e
			},
		})
		r.Command(Command{
			Name: "unencodable",
			Run: func(context.Context, *CommandCall) (map[string]any, error) {
				return map[string]any{"f": func() {}}, nil
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
	for line, code := range map[string]int{
		"/echo": CodeInvalidParams, "/staff": CodeDenied, "/fail": CodeInternalError,
		"/typed-nil": CodeInternalError, "/unencodable": CodeInternalError,
	} {
		if _, reply = runCommand(t, c, line); errorCode(reply) != code {
			t.Fatalf("%s = %#v, want error %d", line, reply, code)
		}
	}
	// The connection survives the replies above.
	if _, reply = runCommand(t, c, "/echo still here"); reply["result"] == nil {
		t.Fatalf("after the errors: %#v", reply)
	}
}

func TestExtensionRegistrationErrors(t *testing.T) {
	run := func(context.Context, *CommandCall) (map[string]any, error) { return nil, nil }
	for name, init := range map[string]func(r *Registrar) error{
		"core command": func(r *Registrar) error { r.Command(Command{Name: "kick", Run: run}); return nil },
		"foreign method": func(r *Registrar) error {
			r.Method("message", func(context.Context, *MethodCall) (any, error) { return nil, nil })
			return nil
		},
		"nil method":       func(r *Registrar) error { r.Method("demo.x", nil); return nil },
		"nil filter":       func(r *Registrar) error { r.FilterMessage(nil); return nil },
		"core route":       func(r *Registrar) error { r.Handle("/ws", http.NotFoundHandler()); return nil },
		"two sign-ins":     func(r *Registrar) error { r.ExternalSignIn(nil); return nil },
		"init fails":       func(*Registrar) error { return errors.New("no config") },
		"command no Run":   func(r *Registrar) error { r.Command(Command{Name: "x"}); return nil },
		"bad command name": func(r *Registrar) error { r.Command(Command{Name: "Bad Name", Run: run}); return nil },
		"follow twice": func(r *Registrar) error {
			follow := func(context.Context, Event) error { return nil }
			r.Follow(follow)
			r.Follow(follow)
			return nil
		},
		"bad advertise": func(r *Registrar) error { return r.Advertise(map[string]any{"f": func() {}}) },
	} {
		t.Run(name, func(t *testing.T) {
			if app, err := Open(extensionConfig(testExtension{"demo", init})); err == nil {
				_ = app.Shutdown(context.Background())
				t.Fatal("Open succeeded")
			}
		})
	}
	for _, name := range []string{"emoji", "Bad", ""} {
		if app, err := Open(extensionConfig(testExtension{name, func(*Registrar) error { return nil }})); err == nil {
			_ = app.Shutdown(context.Background())
			t.Fatalf("Open with extension %q succeeded", name)
		}
	}

	// A route that takes StaticDir's root fails Open rather than panicking.
	config := extensionConfig(testExtension{"demo", func(r *Registrar) error {
		r.Handle("/", http.NotFoundHandler())
		return nil
	}})
	config.StaticDir = t.TempDir()
	if app, err := Open(config); err == nil {
		_ = app.Shutdown(context.Background())
		t.Fatal("a route on / beside StaticDir was accepted")
	}

	// Registering after Init panics.
	var kept *Registrar
	app, err := Open(extensionConfig(testExtension{"demo", func(r *Registrar) error { kept = r; return nil }}))
	if err != nil {
		t.Fatal(err)
	}
	defer app.Shutdown(context.Background())
	defer func() {
		if recover() == nil {
			t.Fatal("registering after Init did not panic")
		}
	}()
	kept.Command(Command{Name: "late", Run: run})
}

// closeCounter is a store that counts its closes.
type closeCounter struct {
	store.Store
	closes atomic.Int32
}

func (c *closeCounter) Close() error {
	c.closes.Add(1)
	return c.Store.Close()
}

func TestOpenClosesTheStoreWhenItFails(t *testing.T) {
	counted := &closeCounter{Store: store.NewMemory()}
	config := extensionConfig(testExtension{"demo", func(*Registrar) error { return errors.New("broken") }})
	config.Store = counted
	if _, err := Open(config); err == nil {
		t.Fatal("Open succeeded")
	}
	if n := counted.closes.Load(); n != 1 {
		t.Fatalf("the store closed %d times, want once", n)
	}
}

func TestExtensionMethodAdvertiseAndRoute(t *testing.T) {
	var host *Host
	_, httpServer := newTestServer(t, extensionConfig(testExtension{"calc", func(r *Registrar) error {
		host = r.Host
		r.Method("calc.add", func(_ context.Context, call *MethodCall) (any, error) {
			var params struct{ A, B int }
			if err := call.Decode(&params); err != nil {
				return nil, err
			}
			return map[string]any{"sum": params.A + params.B, "by": call.User.ID}, nil
		})
		r.Method("calc.broken", func(context.Context, *MethodCall) (any, error) {
			return map[string]any{"ch": make(chan int)}, nil
		})
		r.Handle("GET /calc/hello", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "hi")
		}))
		return r.Advertise(map[string]any{"version": 1})
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
	c.expectError(t, "calc.broken", "broken", nil, CodeInternalError)
	if status, _, body := httpDo(t, http.MethodGet, httpServer.URL+"/calc/hello", nil, ""); status != http.StatusOK || string(body) != "hi" {
		t.Fatalf("GET /calc/hello = %d %q", status, body)
	}

	// A value that is not JSON changes nothing; a new value reaches every
	// connection.
	if err := host.Advertise(map[string]any{"f": func() {}}); err == nil {
		t.Fatal("an unencodable value was advertised")
	}
	if err := host.Advertise(map[string]any{"version": 2}); err != nil {
		t.Fatal(err)
	}
	params = notificationParams(t, c.read(t), "server")
	if ext := params["ext"].(map[string]any); ext["calc"].(map[string]any)["version"] != float64(2) {
		t.Fatalf("new ext = %#v", ext)
	}
}

func TestExtensionFilter(t *testing.T) {
	var filtered atomic.Int32
	_, httpServer := newTestServer(t, extensionConfig(testExtension{"clean", func(r *Registrar) error {
		r.FilterMessage(func(_ context.Context, d *Draft) error {
			filtered.Add(1)
			switch {
			case strings.Contains(d.Text(), "spam"):
				return Denied("No spam, " + d.From.ID)
			case strings.Contains(d.Text(), "break the body"):
				d.Body["text"] = func() {}
				return nil
			case strings.Contains(d.Text(), "tag"):
				if err := d.SetExt(map[string]any{"checked": true}); err != nil {
					return err
				}
			}
			d.SetText(strings.ReplaceAll(d.Text(), "darn", "d**n"))
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
	if errorCode(frame) != CodeDenied || frame["error"].(map[string]any)["message"] != "No spam, "+c.userID {
		t.Fatalf("spam = %#v", frame)
	}
	if frame := c.call(t, "message", "m3", map[string]any{"room_id": "general", "body": map[string]any{"text": "break the body"}}); errorCode(frame) != CodeInternalError {
		t.Fatalf("broken body = %#v", frame)
	}
	_, snapshot = save(t, c, "m4", map[string]any{"body": map[string]any{"text": "tag me"}})
	if ext := snapshot["ext"].(map[string]any); ext["clean"].(map[string]any)["checked"] != true {
		t.Fatalf("ext = %#v", snapshot["ext"])
	}

	// The opening text of /embed-append is filtered as a message is.
	_, reply := runCommand(t, c, "/embed-append buy spam")
	if errorCode(reply) != CodeDenied {
		t.Fatalf("/embed-append spam = %#v", reply)
	}

	// A message to a room its author may not see is refused by the server,
	// unfiltered.
	before := filtered.Load()
	if frame := c.call(t, "message", "m5", map[string]any{"room_id": "nowhere", "body": map[string]any{"text": "hi"}}); errorCode(frame) != CodeInvalidParams {
		t.Fatalf("unknown room = %#v", frame)
	}
	if filtered.Load() != before {
		t.Fatal("a message to an unknown room was filtered")
	}
}

func TestExtensionBotsEventsAndViews(t *testing.T) {
	events := make(chan Event, 64)
	frames := make(chan jsontext.Value, 256)
	var host *Host
	var bot *Bot
	var watcher *Session
	_, httpServer := newTestServer(t, extensionConfig(testExtension{"helper", func(r *Registrar) error {
		host = r.Host
		r.OnEvent(func(_ context.Context, e Event) { events <- e })
		var err error
		if bot, err = r.Bot("helper", BotOptions{Name: "Helper"}); err != nil {
			return err
		}
		watcher, err = bot.Connect(SessionOptions{OnFrame: func(frame jsontext.Value) { frames <- frame }})
		return err
	}}, testExtension{"other", func(r *Registrar) error {
		// Another extension's bot id is never this one's.
		if _, err := r.Bot("helper", BotOptions{}); err == nil {
			return errors.New("took another extension's bot")
		}
		return nil
	}}))
	user, ok := host.User("helper")
	if !ok || !user.Bot || !user.Account || !user.HasRole(RoleBot) || user.Name != "Helper" || user.Created.IsZero() {
		t.Fatalf("bot user = %#v", user)
	}

	c := dialTestClient(t, httpServer)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := bot.Post(ctx, "general", "hello @"+c.userID, Mention(c.userID), Markdown(), WithExt(map[string]any{"n": 1}))
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

	// The guest's message reaches the watching session as a frame, and
	// extensions as an event.
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

	// A private room is visible to its members only, and public to nobody.
	roomID, _ := saveRoom(t, c, "r1", map[string]any{"title": "Secret", "private": true})
	guest, _ := host.User(c.userID)
	if host.Visible(user, roomID) || !host.Visible(guest, roomID) || host.Public(roomID) || !host.Public("general") {
		t.Fatal("Visible and Public disagree with the private room")
	}
	if room, _ := host.Room(roomID); !room.Restricted || room.Title != "Secret" {
		t.Fatalf("room = %#v", room)
	}
	if _, err := bot.Post(ctx, roomID, "let me in"); err == nil {
		t.Fatal("the bot posted to a private room it is not in")
	}
	if err := host.RoomNotice("general", "maintenance soon"); err != nil {
		t.Fatal(err)
	}

	// A closed session says why; the bot's own requests reconnect.
	if err := watcher.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := watcher.Post(ctx, "general", "gone"); !errors.Is(err, ErrSessionClosed) || !errors.Is(watcher.Err(), ErrSessionClosed) {
		t.Fatalf("post after close: %v, %v", err, watcher.Err())
	}
	session, _ := bot.conn()
	_ = session.Close()
	if _, err := bot.Post(ctx, "general", "back again"); err != nil {
		t.Fatalf("the bot did not reconnect: %v", err)
	}
}

func TestBotIDs(t *testing.T) {
	memory := store.NewMemory()
	config := DefaultConfig()
	config.Store = memory
	app, httpServer := newTestServer(t, config)
	c, _ := dialRaw(t, httpServer)
	c.result(t, "auth", "a1", map[string]any{"scheme": "guest", "user_id": "assistant"})
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A user_id a guest ever had is never a bot's.
	config = extensionConfig(testExtension{"assistant", func(r *Registrar) error {
		_, err := r.Bot("assistant", BotOptions{})
		return err
	}})
	config.Store = memory
	if app, err := Open(config); err == nil {
		_ = app.Shutdown(context.Background())
		t.Fatal("a bot took a guest's user_id")
	}
}

func TestSessionThatFallsBehindIsForgotten(t *testing.T) {
	block := make(chan struct{})
	var host *Host
	var session *Session
	app, httpServer := newTestServer(t, extensionConfig(testExtension{"slow", func(r *Registrar) error {
		host = r.Host
		bot, err := r.Bot("slow", BotOptions{})
		if err != nil {
			return err
		}
		session, err = bot.Connect(SessionOptions{OnFrame: func(jsontext.Value) { <-block }})
		return err
	}}))
	defer close(block)
	c := dialTestClient(t, httpServer)
	for i := 0; i < sessionFrameQueue+16; i++ {
		select {
		case <-session.Done():
		default:
			c.write(t, map[string]any{"method": "message", "id": c.nextID("m"), "params": map[string]any{"room_id": "general", "body": map[string]any{"text": "flood"}}})
			c.read(t)
			c.read(t)
			continue
		}
		break
	}
	select {
	case <-session.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the session that fell behind was not closed")
	}
	if !errors.Is(session.Err(), ErrFellBehind) {
		t.Fatalf("Err = %v", session.Err())
	}
	// Only the bot's own connection is left.
	app.mu.RLock()
	_, kept := app.clients[session.c]
	sessions, connected := app.sessionCount, len(app.users["slow"].clients)
	app.mu.RUnlock()
	if kept || sessions != 1 || connected != 1 {
		t.Fatalf("kept %v, sessions %d, bot connections %d after the session closed", kept, sessions, connected)
	}
	_ = host
}

func TestSessionRefusesReentrantCalls(t *testing.T) {
	var bot *Bot
	reentrant := make(chan error, 1)
	_, _ = newTestServer(t, extensionConfig(testExtension{"loop", func(r *Registrar) error {
		var err error
		if bot, err = r.Bot("loop", BotOptions{}); err != nil {
			return err
		}
		// The bot's own message runs this filter, which posts with the
		// same session: that waits on itself, so it is refused.
		r.FilterMessage(func(ctx context.Context, d *Draft) error {
			if d.From.ID == "loop" && d.Text() == "first" {
				_, err := bot.Post(ctx, "general", "second")
				reentrant <- err
			}
			return nil
		})
		return nil
	}}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := bot.Post(ctx, "general", "first"); err != nil {
		t.Fatal(err)
	}
	if err := <-reentrant; !errors.Is(err, ErrReentrant) {
		t.Fatalf("re-entrant post = %v, want ErrReentrant", err)
	}
}

func TestSlowCommandKeepsItsConnection(t *testing.T) {
	config := extensionConfig(testExtension{"slow", func(r *Registrar) error {
		r.Command(Command{Name: "slow", Run: func(ctx context.Context, call *CommandCall) (map[string]any, error) {
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return map[string]any{"done": true}, nil
		}})
		return nil
	}})
	config.PingInterval, config.PingTimeout = 100*time.Millisecond, 100*time.Millisecond
	_, httpServer := newTestServer(t, config)
	c := dialTestClient(t, httpServer)
	id := c.nextID("command")
	c.write(t, map[string]any{"method": "command", "id": id, "params": map[string]any{"body": map[string]any{"text": "/slow"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		// Reading answers the server's pings while the command runs.
		_, payload, err := c.ws.Read(ctx)
		if err != nil {
			t.Fatalf("the connection closed while the command ran: %v", err)
		}
		if strings.Contains(string(payload), `"id":"`+id+`"`) {
			if !strings.Contains(string(payload), `"done":true`) {
				t.Fatalf("reply = %s", payload)
			}
			return
		}
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
	var bot *Bot
	app, httpServer := newTestServer(t, extensionConfig(testExtension{"streamer", func(r *Registrar) error {
		var err error
		bot, err = r.Bot("streamer", BotOptions{})
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
	if embed := embedsOf(t, posted)[0]; !strings.HasPrefix(embed["url"].(string), httpServer.URL+livePath) {
		t.Fatalf("live url = %v, want this server's", embed["url"])
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	final := notificationParams(t, c.read(t), "message")
	body := final["body"].(map[string]any)
	if text, _ := body["text"].(string); !strings.HasPrefix(text, "**Answer:**") || !strings.HasSuffix(text, "hello world") || body["embeds"] != nil {
		t.Fatalf("final = %#v", final)
	}

	// Without PublicURL, a live URL never takes the host a client from
	// elsewhere named.
	app.mu.Lock()
	app.seenBaseURL = "http://evil.example"
	app.unlock()
	if _, err := bot.StreamAppend(ctx, "general", "x"); err == nil || !strings.Contains(err.Error(), "PublicURL") {
		t.Fatalf("stream with a foreign host = %v", err)
	}
}

func TestExtensionEventsReplayAndKV(t *testing.T) {
	memory := store.NewMemory()
	config := extensionConfig(testExtension{"keeper", func(r *Registrar) error {
		if err := r.Store().Put("apron:mine", 1); err == nil {
			return errors.New("an extension took a key of the server's")
		}
		r.Follow(func(context.Context, Event) error { return nil })
		return r.Store().Put("greeting", map[string]any{"text": "hi"})
	}})
	config.Store = memory
	app, httpServer := newTestServer(t, config)
	c := dialTestClient(t, httpServer)
	id, _ := save(t, c, "m1", map[string]any{"body": map[string]any{"text": "remember me"}})
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	replayed := make(chan Event, 64)
	followed := make(chan Event, 64)
	attempts := 0
	var kept struct{ Text string }
	var found bool
	config = extensionConfig(testExtension{"keeper", func(r *Registrar) error {
		var err error
		found, err = r.Store().Get("greeting", &kept)
		if keys := r.Store().Keys(""); !slices.Equal(keys, []string{"greeting"}) {
			t.Errorf("keys = %v, want the server's hidden", keys)
		}
		r.OnEventFrom(0, func(_ context.Context, e Event) error {
			replayed <- e
			return nil
		})
		// Follow takes up after the last event handled before the
		// restart; an event it can never handle is skipped.
		r.Follow(func(_ context.Context, e Event) error {
			if m, ok := DecodeMessage(e); ok && m.Text == "poison" {
				attempts++
				return SkipEvent(errors.New("cannot use it"))
			}
			followed <- e
			return nil
		})
		return err
	}})
	config.Store = memory
	_, httpServer = newTestServer(t, config)
	if !found || kept.Text != "hi" {
		t.Fatalf("kept = %#v, %v", kept, found)
	}
	first := <-replayed
	if first.Kind != EventRoom || !slices.Equal(first.Rooms, []string{"general"}) {
		t.Fatalf("first event = %#v, want general's creation", first)
	}
	waitMessageEvent(t, replayed, id)
	c = dialTestClient(t, httpServer)
	save(t, c, "m2", map[string]any{"body": map[string]any{"text": "poison"}})
	after, _ := save(t, c, "m3", map[string]any{"body": map[string]any{"text": "after"}})
	for e := range followed {
		if m, ok := DecodeMessage(e); ok {
			if m.ID == id {
				t.Fatal("Follow handled an event from before the restart again")
			}
			if m.ID == after {
				break
			}
		}
	}
	if attempts != 1 {
		t.Fatalf("the skipped event was tried %d times", attempts)
	}
}

func TestDisableGuests(t *testing.T) {
	var host *Host
	config := extensionConfig(testExtension{"view", func(r *Registrar) error { host = r.Host; return nil }})
	config.DisableGuests = true
	config.EmailSender = LogEmailSender{}
	_, httpServer := newTestServer(t, config)
	c, frame := dialRaw(t, httpServer)
	if auth := asList(frame["params"].(map[string]any)["auth"]); slices.Contains(auth, any("guest")) {
		t.Fatalf("auth = %#v, want no guest", auth)
	}
	c.expectError(t, "auth", "a1", map[string]any{"scheme": "guest"}, CodeUnsupported)
	if host.Public("general") {
		t.Fatal("a room is public on a server without guests")
	}
}

func TestExternalSignIn(t *testing.T) {
	var host *Host
	_, httpServer := newTestServer(t, extensionConfig(testExtension{"sso", func(r *Registrar) error {
		host = r.Host
		signIn := r.ExternalSignIn(func(w http.ResponseWriter, req *http.Request, p ExternalProposal) {
			http.Redirect(w, req, "/sso/callback?p="+p.ID, http.StatusFound)
		})
		r.Handle("/sso/callback", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			_ = signIn.Complete(w, req, req.URL.Query().Get("p"), Identity{Subject: "idp:1", Name: "Ada", UserID: "ada", Roles: []string{"Staff"}})
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
