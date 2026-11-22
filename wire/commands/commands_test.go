package commands_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/wire"
	"github.com/apron-chat/apron-server-go/wire/commands"
	"github.com/apron-chat/apron-server-go/wire/wiretest"
)

// backend signs guests in, the first of them as the admin, and runs the
// commands of a Router.
type backend struct {
	mu     sync.Mutex
	guests int
}

func (*backend) Hello(context.Context, *wire.Conn) wire.ServerInfo {
	return wire.ServerInfo{Auth: []string{"guest"}}
}

func (b *backend) Auth(_ context.Context, _ *wire.Conn, _ wire.AuthParams, reply wire.Reply[wire.AuthResult]) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.guests++
	return reply.Send(wire.AuthResult{You: &wire.User{UserID: fmt.Sprintf("guest_%d", b.guests)}})
}

// admin lets guest_1 use a form.
func admin(_ context.Context, c *wire.Conn, _ string) bool { return c.UserID() == "guest_1" }

func timeout(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// testRouter has /echo, /topic in rooms, the admin's /ban, /server with
// fixed words, and /help between them.
func testRouter(t *testing.T) *commands.Router {
	var router commands.Router
	echo := func(_ context.Context, call *commands.Call) error {
		call.Notice(call.Name + "|" + call.Args + "|" + call.Rest())
		return call.Done()
	}
	router.MustAdd(
		commands.Command{
			Name:  "echo",
			Forms: []commands.Form{{Usage: "/echo   <text>\n more", Help: "say it\n  back", Group: commands.GroupAll}},
			Run:   echo,
		},
		commands.Command{
			Name: "topic",
			Forms: []commands.Form{{
				Hint: "<text>", Help: "set the topic",
				Uses: []string{commands.FieldRoom}, Requires: []string{commands.FieldRoom},
				May: func(_ context.Context, _ *wire.Conn, roomID string) bool { return roomID != "lobby" },
			}},
			Run: echo,
		},
		commands.Command{Name: "help", Forms: []commands.Form{{Help: "this list"}}},
		commands.Command{
			Name: "ban",
			Forms: []commands.Form{{
				Usage: "/ban @user", Help: "ban someone", Group: commands.GroupAdmin, May: admin,
				Uses: []string{commands.FieldMentions}, Requires: []string{commands.FieldMentions},
			}},
			Run: func(_ context.Context, call *commands.Call) error {
				if len(call.Params.Body.Mentions) != 1 {
					return call.UsageError("mentioning one user")
				}
				return call.Done()
			},
		},
		commands.Command{
			Name: "server",
			Forms: []commands.Form{
				{Name: "server title", Hint: "[<text>]", Help: "set the title"},
				{Name: "server title clear", Help: "clear the title", May: admin},
			},
			Run: echo,
		},
		commands.Command{Name: "quiet", Run: echo},
	)
	return &router
}

func connect(t *testing.T, router *commands.Router) (*wiretest.Client, *wiretest.Client) {
	server := &wire.Server{Backend: &backend{}, Commands: router, Logger: slog.New(slog.DiscardHandler)}
	server.Handle(commands.ListMethod, router.ListHandler())
	ctx := timeout(t)
	var clients []*wiretest.Client
	for range 2 {
		c := wiretest.Connect(server)
		t.Cleanup(func() { c.Close() })
		if _, err := c.Auth(ctx, map[string]any{"scheme": "guest"}); err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
	}
	if !slices.Contains(clients[0].Hello().Capabilities, commands.ListMethod) {
		t.Fatalf("capabilities: %v", clients[0].Hello().Capabilities)
	}
	return clients[0], clients[1]
}

// command runs a command line in a room, and returns the texts of the
// notices before its result.
func command(t *testing.T, c *wiretest.Client, roomID, text string, mentions ...string) ([]string, error) {
	t.Helper()
	body := map[string]any{"text": text}
	if len(mentions) > 0 {
		body["mentions"] = mentions
	}
	params := map[string]any{"body": body}
	if roomID != "" {
		params["room_id"] = roomID
	}
	result, before, err := c.CallFrames(timeout(t), "command", params)
	if err != nil {
		return nil, err
	}
	if string(result) != "{}" {
		t.Fatalf("%s: result %s", text, result)
	}
	var texts []string
	for _, f := range before {
		var m wire.Message
		if f.Method != "message" || f.Decode(&m) != nil || m.From.UserID != wire.Private.UserID || m.RoomID != roomID || m.Body.Format != wire.Markdown {
			t.Fatalf("%s: before its result %s", text, f.Raw)
		}
		texts = append(texts, m.Body.Text)
	}
	return texts, nil
}

func TestRun(t *testing.T) {
	admin, guest := connect(t, testRouter(t))
	for _, test := range []struct{ room, line, want string }{
		{"", "  /echo  a  b ", "echo|a  b|a  b"},
		{"", "/echo\nline", "echo|line|line"},
		{"r", "/topic Ops", "topic|Ops|Ops"},
		{"", "/server title Ops", "server|title Ops|Ops"},
		{"", "/server title", "server|title|"},
		{"", "/server title clear now", "server|title clear now|now"},
	} {
		texts, err := command(t, admin, test.room, test.line)
		if err != nil || !slices.Equal(texts, []string{test.want}) {
			t.Errorf("%q: %q %v, want %q", test.line, texts, err, test.want)
		}
	}
	for _, test := range []struct {
		room, line string
		mentions   []string
		code       int
		message    string
	}{
		{"", "echo", nil, wire.CodeInvalidParams, "A command starts with /; try /help"},
		{"", "/Echo", nil, wire.CodeInvalidParams, "Unknown command /Echo; try /help"},
		{"", "/", nil, wire.CodeInvalidParams, "Unknown command /; try /help"},
		{"", "/topic Ops", nil, wire.CodeInvalidParams, "Send /topic from a room: it needs room_id"},
		{"lobby", "/topic Ops", nil, wire.CodeDenied, "You cannot use /topic here"},
		{"", "/ban", nil, wire.CodeInvalidParams, "Mention someone in /ban: it needs body.mentions"},
		{"", "/ban @a @b", []string{"a", "b"}, wire.CodeInvalidParams, "Usage: /ban @user, mentioning one user"},
		{"", "/server", nil, wire.CodeInvalidParams, "Usage: `/server title [<text>]` or `/server title clear`"},
	} {
		_, err := command(t, admin, test.room, test.line, test.mentions...)
		if e, ok := errors.AsType[*wire.Error](err); !ok || e.Code != test.code || e.Message != test.message {
			t.Errorf("%q: %v, want %d %q", test.line, err, test.code, test.message)
		}
	}
	// The admin's forms are refused to others.
	for _, line := range []string{"/ban @a", "/server title clear"} {
		if _, err := command(t, guest, "", line, "a"); wire.CodeOf(err) != wire.CodeDenied {
			t.Errorf("a guest's %s: %v", line, err)
		}
	}
	if _, err := command(t, admin, "", "/ban @a", "a"); err != nil {
		t.Errorf("the admin's /ban: %v", err)
	}
}

func TestHelpAndList(t *testing.T) {
	admin, guest := connect(t, testRouter(t))
	all := []string{
		"- `/echo <text> more`: say it back",
		"- `/topic <text>`: set the topic",
		"- `/help`: this list",
		"- `/ban @user`: ban someone",
		"- `/server title [<text>]`: set the title",
		"- `/server title clear`: clear the title",
		"- `/quiet`",
	}
	for _, test := range []struct {
		c    *wiretest.Client
		room string
		want []string
	}{
		{admin, "r", all},
		// /topic is not for the lobby.
		{admin, "lobby", slices.Delete(slices.Clone(all), 1, 2)},
		{guest, "r", []string{all[0], all[1], all[2], all[4], all[6]}},
	} {
		texts, err := command(t, test.c, test.room, "/help")
		if err != nil || len(texts) != 1 || texts[0] != strings.Join(test.want, "\n") {
			t.Errorf("/help in %s: %q %v, want %q", test.room, texts, err, test.want)
		}
	}

	raw, err := guest.Call(timeout(t), commands.ListMethod, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"commands":[` +
		`{"description":"say it back","group":"all","hint":"<text> more","name":"echo"},` +
		`{"description":"set the topic","hint":"<text>","name":"topic","requires":["room_id"],"uses":["room_id"]},` +
		`{"description":"this list","name":"help"},` +
		`{"description":"set the title","hint":"[<text>]","name":"server title"},` +
		`{"name":"quiet"}]}`
	if string(raw) != want {
		t.Errorf("list:\n%s\nwant\n%s", raw, want)
	}
	var list struct {
		Commands []commands.Entry `json:"commands"`
	}
	if err := json.Unmarshal(raw, &list); err != nil || len(list.Commands) != 5 {
		t.Fatalf("list: %+v %v", list, err)
	}
}

// A Router without a command named help lists its own /help first.
func TestBuiltInHelp(t *testing.T) {
	var router commands.Router
	c, _ := connect(t, &router)
	texts, err := command(t, c, "", "/help")
	if err != nil || !slices.Equal(texts, []string{"- `/help`: list the commands you can use here"}) {
		t.Fatalf("/help: %q %v", texts, err)
	}
	var other commands.Router
	other.MustAdd(commands.Command{Name: "x", Run: func(_ context.Context, call *commands.Call) error { return call.Done() }})
	if forms := other.Usable(context.Background(), nil, ""); len(forms) != 2 || forms[0].Name != "help" || forms[1].Name != "x" {
		t.Fatalf("Usable: %+v", forms)
	}
}

// Fallback runs the lines of names the Router has no command for.
func TestFallback(t *testing.T) {
	var router commands.Router
	router.Fallback = func(_ context.Context, call *commands.Call) error {
		if call.Form != nil {
			t.Errorf("a Fallback's Form: %+v", call.Form)
		}
		call.Notice("upstream " + call.Name + " " + call.Args)
		return call.Done()
	}
	c, _ := connect(t, &router)
	if texts, err := command(t, c, "", "/nick ada"); err != nil || !slices.Equal(texts, []string{"upstream nick ada"}) {
		t.Fatalf("/nick: %q %v", texts, err)
	}
	if texts, err := command(t, c, "", "/help"); err != nil || len(texts) != 1 || !strings.HasPrefix(texts[0], "- `/help`") {
		t.Fatalf("/help: %q %v", texts, err)
	}
}

// Find picks the form with the most fixed words that start the command
// line's arguments, matched exactly, as names are.
func TestFind(t *testing.T) {
	var router commands.Router
	router.MustAdd(commands.Command{Name: "a", Forms: []commands.Form{{Name: "a b"}, {}, {Name: "a b c"}}})
	for _, test := range []struct{ name, args, want string }{
		{"a", "", "a"},
		{"a", "x", "a"},
		{"a", "b", "a b"},
		{"a", "b  x", "a b"},
		{"a", "b c", "a b c"},
		{"a", "b\nc d", "a b c"},
		{"a", "B c", "a"},
		{"A", "b", ""},
		{"z", "", ""},
	} {
		got := ""
		if _, form := router.Find(test.name, test.args); form != nil {
			got = form.Name
		}
		if got != test.want {
			t.Errorf("Find(%q, %q) = %q, want %q", test.name, test.args, got, test.want)
		}
	}
}

func TestAddErrors(t *testing.T) {
	run := func(context.Context, *commands.Call) error { return nil }
	for name, command := range map[string]commands.Command{
		"empty name":          {Run: run},
		"two words":           {Name: "a b", Run: run},
		"slash":               {Name: "/a", Run: run},
		"taken":               {Name: "taken", Run: run},
		"form of another":     {Name: "a", Forms: []commands.Form{{Name: "b"}}},
		"blank form":          {Name: "a", Forms: []commands.Form{{Name: " "}}},
		"spaced form":         {Name: "a", Forms: []commands.Form{{Name: "a  b"}}},
		"form twice":          {Name: "a", Forms: []commands.Form{{}, {Name: "a"}}},
		"unknown field":       {Name: "a", Forms: []commands.Form{{Uses: []string{"embeds"}}}},
		"field used twice":    {Name: "a", Forms: []commands.Form{{Uses: []string{"reply_to", "reply_to"}}}},
		"required, not used":  {Name: "a", Forms: []commands.Form{{Requires: []string{"reply_to"}}}},
		"required twice":      {Name: "a", Forms: []commands.Form{{Uses: []string{"reply_to"}, Requires: []string{"reply_to", "reply_to"}}}},
		"unknown group":       {Name: "a", Forms: []commands.Form{{Group: "moderator"}}},
		"control in the name": {Name: "a\x00", Run: run},
	} {
		var router commands.Router
		router.MustAdd(commands.Command{Name: "taken", Run: run})
		if err := router.Add(command); err == nil {
			t.Errorf("%s: Add succeeded", name)
		}
		if forms := router.Usable(context.Background(), nil, ""); len(forms) != 2 {
			t.Errorf("%s: added %+v", name, forms)
		}
	}
}

func TestParseUsage(t *testing.T) {
	for _, test := range []struct {
		name, usage, hint string
		ok                bool
	}{
		{"w", "/w <city>", "<city>", true},
		{"w", "/w", "", true},
		{"w", "/w\t <a>\n  [b] ", "<a> [b]", true},
		{"w", "/wx <city>", "", false},
		{"w", "w <city>", "", false},
		{"w", "", "", false},
		{"e a", "/e a :<n>:", ":<n>:", true},
	} {
		if hint, ok := commands.ParseUsage(test.name, test.usage); hint != test.hint || ok != test.ok {
			t.Errorf("ParseUsage(%q, %q) = %q, %v", test.name, test.usage, hint, ok)
		}
	}
	if name, args, ok := commands.Parse(" /a\tb  c "); name != "a" || args != "b  c" || !ok {
		t.Errorf("Parse: %q %q %v", name, args, ok)
	}
}
