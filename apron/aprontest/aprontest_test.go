package aprontest_test

import (
	"context"
	"net/http"
	"testing"

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
