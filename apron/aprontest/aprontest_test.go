package aprontest_test

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/aprontest"
	"github.com/apron-chat/apron-server-go/wire"
	"github.com/apron-chat/apron-server-go/wire/wiretest/conformance"
)

// echo is an extension with a command, a method, and a bot, for the tests.
type echo struct{ bot *apron.Bot }

func (*echo) Name() string { return "echo" }

func (e *echo) Init(r *apron.Registrar) (err error) {
	r.Command(apron.Command{
		Name: "echo", Usage: "/echo <text>", Help: "say it back",
		Run: func(_ context.Context, call *apron.CommandCall) (map[string]any, error) {
			if call.Args == "" {
				return nil, apron.InvalidParams("Usage: /echo <text>")
			}
			call.Notice(call.Args)
			call.Notice("again: " + call.Args)
			return nil, nil
		},
	})
	r.Method("echo.words", func(_ context.Context, call *apron.MethodCall) (any, error) {
		var params struct{ Text string }
		if err := call.Decode(&params); err != nil {
			return nil, err
		}
		return map[string]any{"words": []string(nil), "text": params.Text}, nil
	})
	e.bot, err = r.Bot("echo", apron.BotOptions{})
	return err
}

func TestClients(t *testing.T) {
	e, mail := &echo{}, &aprontest.Mailbox{}
	config := apron.DefaultConfig()
	config.EmailSender = mail
	server := aprontest.Serve(t, config, e)
	ada, bob := server.Guest("Ada"), server.Account("bob@example.com")
	if ada.UserID == "" || bob.UserID == "" || bob.Token == "" {
		t.Fatalf("signed in as %q and %q, token %q", ada.UserID, bob.UserID, bob.Token)
	}
	if hello := server.Dial().Hello(); hello.Apron == 0 {
		t.Fatalf("server frame = %#v", hello)
	}

	// The bot's message arrives while Ada runs a command, which takes only
	// its notices and leaves the message for Message.
	posted, err := e.bot.Post(t.Context(), "general", "from the bot")
	if err != nil {
		t.Fatal(err)
	}
	notices, err := ada.Command("general", "/echo hi")
	if err != nil || len(notices) != 2 || notices[0] != "hi" || notices[1] != "again: hi" {
		t.Fatalf("/echo = %q, %v", notices, err)
	}
	if got := ada.Message(func(m apron.Message) bool { return m.From.UserID == "echo" }); got.ID != posted || got.Text != "from the bot" {
		t.Fatalf("message = %+v, want %s", got, posted)
	}
	if _, err := ada.Command("general", "/echo"); wire.CodeOf(err) != apron.CodeInvalidParams {
		t.Fatalf("/echo with no text = %v", err)
	}

	id, err := bob.Post("general", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if got := ada.Message(func(m apron.Message) bool { return m.ID == id }); got.From.UserID != bob.UserID || got.Text != "hello" {
		t.Fatalf("message = %+v", got)
	}

	var result struct {
		Words []string
		Text  string
	}
	if err := ada.Call("echo.words", map[string]any{"text": "a b"}, &result); err != nil || result.Text != "a b" || result.Words == nil {
		t.Fatalf("echo.words = %+v, %v", result, err)
	}
	if err := ada.Call("echo.words", map[string]any{"text": 1}, nil); wire.CodeOf(err) != apron.CodeInvalidParams {
		t.Fatalf("echo.words with a number = %v", err)
	}

	// A session token signs in again; signed out, its connections close.
	again := server.SignIn(map[string]any{"scheme": "token", "token": bob.Token})
	if again.UserID != bob.UserID {
		t.Fatalf("token signed in as %q, want %q", again.UserID, bob.UserID)
	}
	if err := server.SignOut(t.Context(), bob.UserID); err != nil {
		t.Fatal(err)
	}
	if !bob.Closed() || !again.Closed() {
		t.Fatal("a signed-out connection stayed open")
	}
	if code := mail.Code("nobody@example.com", 0); code != "" {
		t.Fatalf("mailed nobody %q", code)
	}
}

// counter is an extension that counts each user's new messages as it
// follows the log, slowly, and tells them with a command, which only
// counters may run.
type counter struct {
	mu     sync.Mutex
	counts map[string]int
}

func (*counter) Name() string { return "counter" }

func (c *counter) Init(r *apron.Registrar) error {
	c.counts = make(map[string]int)
	r.OnEvent(func(ctx context.Context, e apron.Event) {
		time.Sleep(5 * time.Millisecond)
		if m, ok := apron.DecodeMessage(e); ok && m.New() {
			c.mu.Lock()
			c.counts[m.From.UserID]++
			c.mu.Unlock()
		}
	})
	r.Command(apron.Command{
		Name: "count", Usage: "/count <user_id>", Help: "how many messages someone posted",
		Available: func(u apron.User, _ apron.Room) bool { return u.HasRole("counter") },
		Run: func(_ context.Context, call *apron.CommandCall) (map[string]any, error) {
			c.mu.Lock()
			defer c.mu.Unlock()
			call.Notice(strconv.Itoa(c.counts[call.Args]))
			return nil, nil
		},
	})
	return nil
}

func TestMessages(t *testing.T) {
	server := aprontest.Serve(t, apron.DefaultConfig(), &counter{})
	ada, bob := server.Guest("Ada"), server.Guest("Bob")
	room, err := ada.CreateRoom("Plans", true)
	if err != nil || room == "" {
		t.Fatalf("room = %q, %v", room, err)
	}
	asked, err := ada.Post("general", "@"+bob.UserID+" *hi*", apron.Mention(bob.UserID), apron.Markdown(), apron.WithExt(map[string]int{"n": 1}))
	if err != nil {
		t.Fatal(err)
	}
	got := bob.Message(func(m apron.Message) bool { return m.ID == asked })
	if !got.MentionsUser(bob.UserID) || got.Format != "markdown" || string(got.Ext["counter"]) != `{"n":1}` {
		t.Fatalf("message = %+v", got)
	}
	reply, err := bob.Post("general", "hello", apron.ReplyTo(asked))
	if err != nil {
		t.Fatal(err)
	}
	if got := ada.Message(func(m apron.Message) bool { return m.ID == reply }); got.ReplyTo != asked {
		t.Fatalf("reply = %+v", got)
	}
	if _, err := ada.Post("general", "x", apron.ReplyTo("nope")); wire.CodeOf(err) != apron.CodeInvalidParams {
		t.Fatalf("a reply to no message_id = %v", err)
	}

	if err := ada.Edit("general", asked, "edited"); err != nil {
		t.Fatal(err)
	}
	if got := bob.Message(func(m apron.Message) bool { return m.ID == asked }); got.Text != "edited" || got.New() {
		t.Fatalf("edit = %+v", got)
	}
	if err := bob.Edit("general", asked, "mine now"); wire.CodeOf(err) == 0 {
		t.Fatal("Bob edited Ada's message")
	}
	if err := ada.Delete("general", asked); err != nil {
		t.Fatal(err)
	}
	if got := bob.Message(func(m apron.Message) bool { return m.ID == asked }); !got.Deleted {
		t.Fatalf("deletion = %+v", got)
	}
}

// A member holds its roles; the follower's count is settled before the
// command reads it.
func TestMembersAndSettle(t *testing.T) {
	server := aprontest.Serve(t, apron.DefaultConfig(), &counter{})
	ada, mo := server.Guest("Ada"), server.Member("mo", "counter", apron.RoleModerator)
	if mo.UserID != "mo" || mo.Token == "" {
		t.Fatalf("member = %q, token %q", mo.UserID, mo.Token)
	}
	if user, ok := server.Enrollment("mo"); !ok || !user.Admitted {
		t.Fatalf("enrollment = %+v, %v", user, ok)
	}
	if _, err := ada.Command("general", "/count mo"); err == nil {
		t.Fatal("a guest counted")
	}
	for i := range 20 {
		if _, err := ada.Post("general", strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	server.Settle()
	if notices, err := mo.Command("general", "/count "+ada.UserID); err != nil || len(notices) != 1 || notices[0] != "20" {
		t.Fatalf("/count = %q, %v", notices, err)
	}
	// A member signs in again with its token, and its user_id is its own.
	if again := server.SignIn(wire.AuthParams{Scheme: "token", Token: mo.Token}); again.UserID != "mo" {
		t.Fatalf("token signed in as %q", again.UserID)
	}
}

// external is an extension whose external sign-in signs everyone in as
// octocat at once.
type external struct{ signIn *apron.ExternalSignIn }

func (*external) Name() string { return "external" }

func (x *external) Init(r *apron.Registrar) error {
	x.signIn = r.ExternalSignIn(func(w http.ResponseWriter, r *http.Request, req apron.ExternalRequest) {
		_ = x.signIn.Complete(w, r, req.ID, apron.Identity{Subject: "test:1", Name: "The Octocat", UserID: "octocat"})
	})
	return nil
}

func TestApp(t *testing.T) {
	server := aprontest.Serve(t, apron.DefaultConfig(), &external{})
	app := server.App()
	page := app.Open()
	if page.Status != http.StatusOK || page.Return != nil {
		t.Fatalf("sign-in page = %d %s", page.Status, page.Body)
	}
	result, err := app.Redeem(app.Press(page, "allow"))
	if err != nil || result.You == nil || result.You.UserID != "octocat" || app.Conn.UserID != "octocat" {
		t.Fatalf("redeemed: %+v, %v", result, err)
	}
	declined := server.App()
	back := declined.Press(declined.Open(), "cancel")
	if back.Return == nil || back.Return.Query().Get("error") != "access_denied" || back.Return.Query().Get("state") != declined.State {
		t.Fatalf("cancel = %d %v", back.Status, back.Return)
	}
}

// The extensions' server keeps the protocol's core, over its WebSocket.
func TestConformance(t *testing.T) {
	server := aprontest.Serve(t, apron.DefaultConfig(), &echo{})
	conformance.Run(t, conformance.Config{URL: server.WebSocketURL()})
}
