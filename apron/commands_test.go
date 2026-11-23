package apron

import (
	"context"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/wire/commands"
)

// commandList sends ext:command_list and returns its commands.
func commandList(t *testing.T, c *testClient) []map[string]any {
	t.Helper()
	result := c.result(t, commandListMethod, c.nextID("commands"), map[string]any{})
	if len(result) != 1 {
		t.Fatalf("ext:command_list result: %#v", result)
	}
	var commands []map[string]any
	for _, value := range result["commands"].([]any) {
		commands = append(commands, value.(map[string]any))
	}
	return commands
}

func asMaps(value any) []map[string]any {
	var list []map[string]any
	for _, item := range value.([]any) {
		list = append(list, item.(map[string]any))
	}
	return list
}

func commandNames(commands []map[string]any) []string {
	names := make([]string, len(commands))
	for i, command := range commands {
		names[i] = command["name"].(string)
	}
	return names
}

// helpLines runs /help in a room and returns its lines.
func helpLines(t *testing.T, c *testClient, roomID string) []string {
	t.Helper()
	before, result := c.request(t, "command", c.nextID("help"), map[string]any{"room_id": roomID, "body": map[string]any{"text": "/help"}})
	if len(result) != 0 || len(before) != 1 {
		t.Fatalf("/help: %#v after %#v", result, before)
	}
	return strings.Split(privateNoticeText(t, before[0]), "\n")
}

// expectCommands requires ext:command_list to list the commands named, in
// order, and returns them.
func expectCommands(t *testing.T, c *testClient, who string, names ...string) []map[string]any {
	t.Helper()
	commands := commandList(t, c)
	if got := commandNames(commands); !slices.Equal(got, names) {
		t.Fatalf("%s's commands = %q, want %q", who, got, names)
	}
	return commands
}

// expectHelp requires /help in a room to list the commands named, in
// order, one line each, from the values ext:command_list lists for them.
func expectHelp(t *testing.T, c *testClient, roomID, who string, names ...string) {
	t.Helper()
	commands := commandList(t, c)
	lines := helpLines(t, c, roomID)
	if len(lines) != len(names) {
		t.Fatalf("%s's /help in %s = %q, want %q", who, roomID, lines, names)
	}
	for i, name := range names {
		command := commandNamed(t, commands, name)
		usage := "/" + name
		if hint, has := command["hint"]; has {
			if hint == "" {
				t.Fatalf("%s: an empty hint: %#v", who, command)
			}
			usage += " " + hint.(string)
		}
		want := "- `" + usage + "`"
		if description, has := command["description"]; has {
			if description == "" {
				t.Fatalf("%s: an empty description: %#v", who, command)
			}
			want += ": " + description.(string)
		}
		if lines[i] != want {
			t.Fatalf("%s's /help line %d in %s = %q, want %q", who, i, roomID, lines[i], want)
		}
	}
}

func commandNamed(t *testing.T, commands []map[string]any, name string) map[string]any {
	t.Helper()
	for _, command := range commands {
		if command["name"] == name {
			return command
		}
	}
	t.Fatalf("no command %q in %#v", name, commands)
	return nil
}

// ext:command_list (apron's extensions/ext_command_list.md) lists, as
// data, each form of a command that the user may use on the connection in
// at least one room, with its hint and the fields it uses and requires.
// /help lists the same values, for the room it is sent in.
func TestCommandList(t *testing.T) {
	_, mailbox, httpServer := emailTestServer(t, func(config *Config) {
		config.Roles = map[string][]string{"admin": {"ada@example.com"}}
		config.Extensions = []Extension{testExtension{"demo", func(r *Registrar) error {
			run := func(context.Context, *CommandCall) (map[string]any, error) { return nil, nil }
			r.Command(Command{Name: "echo", Usage: "/echo <text>", Help: "say it back", Group: "member", Uses: []string{"room_id", "reply_to"}, Requires: []string{"reply_to"}, Run: run})
			// No Help: no description, and no colon in /help.
			r.Command(Command{Name: "quiet", Run: run})
			r.Command(Command{
				Name: "staff", Help: "for staff",
				Available: func(u User, _ Room) bool { return u.HasRole("staff") },
				Run:       run,
			})
			// Available in one room only: listed for whoever can see it.
			r.Command(Command{
				Name: "ops-only", Usage: "/ops-only", Help: "only in Ops",
				Available: func(_ User, r Room) bool { return r.Title == "Ops" },
				Run:       run,
			})
			return nil
		}}}
	})
	_, frame := dialRaw(t, httpServer)
	if !slices.Contains(asList(frame["params"].(map[string]any)["capabilities"]), any(commandListMethod)) {
		t.Fatalf("capabilities without ext:command_list: %#v", frame["params"])
	}

	guest := dialTestClient(t, httpServer)
	guestCommands := []string{"help", "avatar", "embed-terminal", "embed-append", "emoji add", "echo", "quiet"}
	commands := expectCommands(t, guest, "a guest", guestCommands...)
	expectHelp(t, guest, "general", "a guest", guestCommands...)
	for name, want := range map[string]map[string]any{
		"help":           {"name": "help", "group": "all", "description": "list the commands you can use here"},
		"avatar":         {"name": "avatar", "group": "all", "description": "with one image attached: set your avatar", "uses": []any{"body.embeds"}, "requires": []any{"body.embeds"}},
		"embed-terminal": {"name": "embed-terminal", "group": "all", "hint": "[<title>]", "description": "get a URL to stream a program's output into this room, shown as an 80×40 terminal", "uses": []any{"room_id", "reply_to"}, "requires": []any{"room_id"}},
		"emoji add":      {"name": "emoji add", "group": "all", "hint": ":<name>:", "description": "with one image attached, add a custom emoji or replace one you added", "uses": []any{"body.embeds"}, "requires": []any{"body.embeds"}},
		"echo":           {"name": "echo", "group": "member", "hint": "<text>", "description": "say it back", "uses": []any{"room_id", "reply_to"}, "requires": []any{"reply_to"}},
		"quiet":          {"name": "quiet"},
	} {
		if got := commandNamed(t, commands, name); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s = %#v, want %#v", name, got, want)
		}
	}
	if lines := helpLines(t, guest, "general"); lines[len(lines)-1] != "- `/quiet`" {
		t.Fatalf("/help without a description: %q", lines)
	}

	// Params are ignored, and the list is read again on a retry (§1.2):
	// a room made in between lets the retry's sender /kick.
	retrier := dialTestClient(t, httpServer)
	params := map[string]any{"room_id": "general"}
	first := commandNames(asMaps(retrier.result(t, commandListMethod, "same", params)["commands"]))
	saveRoom(t, retrier, "own", map[string]any{"title": "Own"})
	retrier.drain(t)
	again := commandNames(asMaps(retrier.result(t, commandListMethod, "same", params)["commands"]))
	if slices.Contains(first, "kick") || !slices.Contains(again, "kick") {
		t.Fatalf("before a room of one's own: %q; after: %q", first, again)
	}
	guest.drain(t)

	// A room's creator may /kick in it, and an extension's command
	// available in one room is listed for whoever can see that room: the
	// list has both, and /help only in that room.
	creator := dialTestClient(t, httpServer)
	opsID, _ := saveRoom(t, creator, "ops", map[string]any{"title": "Ops", "private": true})
	creator.drain(t)
	expectCommands(t, creator, "a room's creator", "help", "avatar", "embed-terminal", "embed-append", "emoji add", "kick", "echo", "quiet", "ops-only")
	expectHelp(t, creator, "general", "a room's creator", guestCommands...)
	expectHelp(t, creator, opsID, "a room's creator", "help", "avatar", "embed-terminal", "embed-append", "emoji add", "kick", "echo", "quiet", "ops-only")
	if kick := commandNamed(t, commandList(t, creator), "kick"); !reflect.DeepEqual(kick, map[string]any{
		"name": "kick", "group": "room", "hint": "@user [<reason>]", "description": "remove someone from this room (its creator or a moderator)",
		"uses": []any{"room_id", "body.mentions"}, "requires": []any{"room_id", "body.mentions"},
	}) {
		t.Fatalf("kick = %#v", kick)
	}
	guest.drain(t)
	expectCommands(t, guest, "a guest who cannot see Ops", guestCommands...)

	// An admin's account has each form of /emoji and /server, and may
	// /kick anywhere.
	admin, _ := dialRaw(t, httpServer)
	signInAdmin(t, admin, mailbox)
	adminCommands := []string{"help", "avatar", "embed-terminal", "embed-append", "invite-device", "emoji add", "emoji remove",
		"server", "server title", "server description", "server avatar", "kick", "echo", "quiet"}
	commands = expectCommands(t, admin, "an admin", adminCommands...)
	expectHelp(t, admin, "general", "an admin", adminCommands...)
	for name, want := range map[string]map[string]any{
		"server":        {"name": "server", "group": "admin", "description": "show the server's title, description, and avatar"},
		"server title":  {"name": "server title", "group": "admin", "hint": "[<text>]", "description": "set the server's title, or without the text clear it back to its flag's"},
		"server avatar": {"name": "server avatar", "group": "admin", "description": "with one image attached, set the server's avatar, or without one clear it back to its flag's", "uses": []any{"body.embeds"}},
		"emoji remove":  {"name": "emoji remove", "group": "admin", "hint": ":<name>:", "description": "remove a custom emoji"},
	} {
		if got := commandNamed(t, commands, name); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s = %#v, want %#v", name, got, want)
		}
	}

	// A connection not offered ext:emoji may not add one, so neither lists
	// /emoji add.
	proxied, _ := dialHost(t, httpServer, "chat.example")
	guestAuth(t, proxied)
	proxiedCommands := []string{"help", "avatar", "embed-terminal", "embed-append", "echo", "quiet"}
	expectCommands(t, proxied, "a guest over plain HTTP", proxiedCommands...)
	expectHelp(t, proxied, "general", "a guest over plain HTTP", proxiedCommands...)

	// Before sign-in it is refused, as every request is.
	anonymous, _ := dialRaw(t, httpServer)
	anonymous.expectError(t, commandListMethod, "early", map[string]any{}, CodeDenied)
}

// A command's Usage gives its hint, on one line, when it starts with /name
// and then whitespace or nothing; any other Usage gives none, /help shows
// it as written, and Open still succeeds.
func TestCommandListUsage(t *testing.T) {
	run := func(context.Context, *CommandCall) (map[string]any, error) { return nil, nil }
	_, httpServer := newTestServer(t, extensionConfig(testExtension{"demo", func(r *Registrar) error {
		r.Command(Command{Name: "bare", Usage: "/bare", Help: "no arguments", Run: run})
		r.Command(Command{Name: "tabbed", Usage: "/tabbed\t<arg>\n  [<more>]", Help: "one\nline", Run: run})
		r.Command(Command{Name: "odd", Usage: "odd <arg>", Help: "no hint", Run: run})
		r.Command(Command{Name: "other", Usage: "/otherwise <arg>", Help: "no hint either", Run: run})
		return nil
	}}))
	c := dialTestClient(t, httpServer)
	commands := commandList(t, c)
	for name, want := range map[string]map[string]any{
		"bare":   {"name": "bare", "description": "no arguments"},
		"tabbed": {"name": "tabbed", "hint": "<arg> [<more>]", "description": "one line"},
		"odd":    {"name": "odd", "description": "no hint"},
		"other":  {"name": "other", "description": "no hint either"},
	} {
		if got := commandNamed(t, commands, name); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s = %#v, want %#v", name, got, want)
		}
	}
	lines := helpLines(t, c, "general")
	if want := []string{
		"- `/bare`: no arguments",
		"- `/tabbed <arg> [<more>]`: one line",
		"- `odd <arg>`: no hint",
		"- `/otherwise <arg>`: no hint either",
	}; !slices.Equal(lines[len(lines)-4:], want) {
		t.Fatalf("/help = %q, ending with %q", lines, want)
	}
}

// A command line selects the form with the most fixed words that start its
// arguments, matched exactly.
func TestFormOf(t *testing.T) {
	sc := coreCommands()
	if err := sc.router.Add(commands.Command{Name: "a", Forms: []commands.Form{{Name: "a"}, {Name: "a b"}, {Name: "a b c"}}}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, args, want string }{
		{"emoji", "add :x:", "emoji add"},
		{"emoji", "remove\t:x:", "emoji remove"},
		{"emoji", "", ""},
		{"emoji", "ADD :x:", ""},
		{"server", "", "server"},
		{"server", "title My server", "server title"},
		{"server", "titles", "server"},
		{"server", "Title x", "server"},
		{"a", "b c d", "a b c"},
		{"a", "b\nx", "a b"},
		{"a", "c", "a"},
		{"kick", "@bob spam", "kick"},
		{"nope", "", ""},
	} {
		got := ""
		if _, form := sc.router.Find(test.name, test.args); form != nil {
			got = form.Name
		}
		if got != test.want {
			t.Errorf("Find(%q, %q) = %q, want %q", test.name, test.args, got, test.want)
		}
	}
}

// The list asks an extension's Available about the rooms the user can see,
// the default room first and then by room_id, until one says yes, and
// never about a room the user cannot see. A user who sees no room is
// listed no command.
func TestCommandListAsksAvailable(t *testing.T) {
	var mu sync.Mutex
	asked := map[string][]string{}
	ask := func(name string, yes func(Room) bool) func(User, Room) bool {
		return func(u User, r Room) bool {
			mu.Lock()
			asked[name] = append(asked[name], r.ID)
			mu.Unlock()
			return yes(r)
		}
	}
	var target string
	run := func(context.Context, *CommandCall) (map[string]any, error) { return nil, nil }
	app, httpServer := newTestServer(t, extensionConfig(testExtension{"demo", func(r *Registrar) error {
		r.Command(Command{Name: "never", Help: "never", Available: ask("never", func(Room) bool { return false }), Run: run})
		r.Command(Command{Name: "first", Help: "first", Available: ask("first", func(Room) bool { return true }), Run: run})
		r.Command(Command{Name: "target", Help: "target", Available: ask("target", func(r Room) bool { return r.ID == target }), Run: run})
		r.Command(Command{Name: "anywhere", Help: "anywhere", Run: run})
		return nil
	}}))
	owner := dialTestClient(t, httpServer)
	var visible []string
	for _, title := range []string{"Zed", "Alpha", "Mid"} {
		id, _ := saveRoom(t, owner, owner.nextID("room"), map[string]any{"title": title})
		visible = append(visible, id)
	}
	hidden, _ := saveRoom(t, owner, "hidden", map[string]any{"title": "Hidden", "private": true})
	slices.Sort(visible)
	target = visible[1]

	user := dialTestClient(t, httpServer)
	mu.Lock()
	clear(asked)
	mu.Unlock()
	expectCommands(t, user, "a guest", "help", "avatar", "embed-terminal", "embed-append", "emoji add", "first", "target", "anywhere")
	mu.Lock()
	got := maps.Clone(asked)
	mu.Unlock()
	all := append([]string{defaultRoomID}, visible...)
	if !slices.Equal(got["never"], all) || !slices.Equal(got["first"], all[:1]) || !slices.Equal(got["target"], all[:3]) {
		t.Fatalf("asked %#v, want %q in that order, stopping at the first yes", got, all)
	}
	for name, rooms := range got {
		if slices.Contains(rooms, hidden) {
			t.Fatalf("%s asked about %s, which the user cannot see", name, hidden)
		}
	}

	// A user who sees no room can run no command, so none is listed.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.SetScope(ctx, user.userID, &RoomScope{Rooms: []string{"nowhere"}}); err != nil {
		t.Fatal(err)
	}
	user.drain(t)
	if result := user.result(t, commandListMethod, user.nextID("commands"), map[string]any{}); !reflect.DeepEqual(result, map[string]any{"commands": []any{}}) {
		t.Fatalf("a user who sees no room: %#v", result)
	}
}

// A moderator may /kick in any room, without one of their own; a
// connection on a service token that may not manage its account lists no
// /invite-device, and one that may lists it.
func TestCommandListModeratorsAndServiceTokens(t *testing.T) {
	_, mailbox, httpServer := emailTestServer(t, func(config *Config) {
		config.Roles = map[string][]string{"moderator": {"bob@example.com"}}
	})
	moderator, _ := dialRaw(t, httpServer)
	propose(t, moderator, "bob@example.com")
	signInByEmail(t, moderator, mailbox.receive(t).Code, nil)
	moderator.drain(t)
	commands := []string{"help", "avatar", "embed-terminal", "embed-append", "invite-device", "emoji add", "kick"}
	expectCommands(t, moderator, "a moderator", commands...)
	expectHelp(t, moderator, "general", "a moderator", commands...)

	_, botServer, _, bot := scopedBotServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, manage := range []bool{false, true} {
		issued, err := bot.IssueToken(ctx, ServiceTokenOptions{Lifetime: time.Hour, ManageAccount: manage})
		if err != nil {
			t.Fatal(err)
		}
		agent, _ := dialRaw(t, botServer)
		agent.result(t, "auth", "auth", map[string]any{"scheme": "token", "token": issued.Token})
		agent.drain(t)
		if got := slices.Contains(commandNames(commandList(t, agent)), "invite-device"); got != manage {
			t.Fatalf("a service token that may manage its account (%v) lists /invite-device: %v", manage, got)
		}
	}
}

// A command form that requires room_id acts only on a room the client
// names (ext:command_list): without room_id it is refused, rather than run
// in the default room, and with it it runs. Commands that do not require
// it still run in the default room.
func TestCommandsThatRequireRoomID(t *testing.T) {
	var ran atomic.Int32
	_, httpServer := newTestServer(t, extensionConfig(testExtension{"demo", func(r *Registrar) error {
		r.Command(Command{
			Name: "here", Usage: "/here", Help: "act on this room", Uses: []string{"room_id"}, Requires: []string{"room_id"},
			Run: func(_ context.Context, call *CommandCall) (map[string]any, error) {
				ran.Add(1)
				return map[string]any{"room": call.Room.ID}, nil
			},
		})
		return nil
	}}))
	c := dialTestClient(t, httpServer)
	other := dialTestClient(t, httpServer)
	c.drain(t)
	if here := commandNamed(t, commandList(t, c), "here"); !reflect.DeepEqual(here["requires"], []any{"room_id"}) {
		t.Fatalf("here = %#v", here)
	}

	for name, body := range map[string]map[string]any{
		"kick":           {"text": "/kick @" + other.userID, "mentions": []any{other.userID}},
		"embed-terminal": {"text": "/embed-terminal build"},
		"embed-append":   {"text": "/embed-append The plan:"},
		"here":           {"text": "/here"},
	} {
		frame := c.call(t, "command", c.nextID(name), map[string]any{"body": body})
		failure, _ := frame["error"].(map[string]any)
		if failure["code"] != float64(CodeInvalidParams) || failure["message"] != "Send /"+name+" from a room: it needs room_id" {
			t.Fatalf("/%s without room_id = %#v", name, frame)
		}
	}
	if ran.Load() != 0 {
		t.Fatal("/here ran without room_id")
	}
	c.expectQuiet(t)

	// With room_id they run.
	if result := c.result(t, "command", "here", map[string]any{"room_id": "general", "body": map[string]any{"text": "/here"}}); result["room"] != "general" {
		t.Fatalf("/here in general = %#v", result)
	}
	liveCommand(t, c, "terminal", map[string]any{"room_id": "general", "body": map[string]any{"text": "/embed-terminal build"}})
	room, _ := saveRoom(t, c, "mine", map[string]any{"title": "Mine"})
	joinRoom(t, other, room)
	c.drain(t)
	if _, result := c.request(t, "command", "kick", map[string]any{"room_id": room, "body": map[string]any{"text": "/kick @" + other.userID, "mentions": []any{other.userID}}}); len(result) != 0 {
		t.Fatalf("/kick in %s = %#v", room, result)
	}

	// /help requires no room: without room_id it runs in general.
	if lines := helpLines(t, c, "general"); len(lines) == 0 {
		t.Fatal("no /help")
	}
	if before, _ := c.request(t, "command", "help", map[string]any{"body": map[string]any{"text": "/help"}}); len(before) != 1 || privateNoticeText(t, before[0]) == "" {
		t.Fatalf("/help without room_id: %#v", before)
	}
}

// Command names are case-sensitive (ext:command_list): /Kick is not
// /kick, for the server's commands, their forms' words, and extensions'.
func TestCommandNamesAreCaseSensitive(t *testing.T) {
	_, httpServer := newTestServer(t, extensionConfig(testExtension{"demo", func(r *Registrar) error {
		r.Command(Command{Name: "echo", Usage: "/echo <text>", Run: func(_ context.Context, call *CommandCall) (map[string]any, error) {
			return map[string]any{"echoed": call.Args}, nil
		}})
		return nil
	}}))
	c := dialTestClient(t, httpServer)
	other := dialTestClient(t, httpServer)
	c.drain(t)
	room, _ := saveRoom(t, c, "mine", map[string]any{"title": "Mine"})
	joinRoom(t, other, room)
	c.drain(t)

	for _, line := range []string{"/KICK @" + other.userID, "/Kick @" + other.userID, "/Help", "/HELP", "/Echo hi", "/EMOJI add :x:"} {
		word, _, _ := strings.Cut(line[1:], " ")
		frame := c.call(t, "command", c.nextID("unknown"), map[string]any{"room_id": room, "body": map[string]any{"text": line, "mentions": []any{other.userID}}})
		failure, _ := frame["error"].(map[string]any)
		if failure["code"] != float64(CodeInvalidParams) || failure["message"] != "Unknown command /"+word+"; try /help" {
			t.Fatalf("%s = %#v", line, frame)
		}
	}
	// A form's word matches exactly too: /emoji ADD is no form of /emoji.
	frame := c.call(t, "command", "emoji", map[string]any{"body": map[string]any{"text": "/emoji ADD :x:", "embeds": []any{map[string]any{"kind": "upload"}}}})
	if failure, _ := frame["error"].(map[string]any); failure["code"] != float64(CodeInvalidParams) || !strings.HasPrefix(failure["message"].(string), "Usage: /emoji add") {
		t.Fatalf("/emoji ADD = %#v", frame)
	}
	c.expectQuiet(t)

	if result := c.result(t, "command", "echo", map[string]any{"body": map[string]any{"text": "/echo hi"}}); result["echoed"] != "hi" {
		t.Fatalf("/echo = %#v", result)
	}
	if _, result := c.request(t, "command", "kick", map[string]any{"room_id": room, "body": map[string]any{"text": "/kick @" + other.userID, "mentions": []any{other.userID}}}); len(result) != 0 {
		t.Fatalf("/kick = %#v", result)
	}
}

// A command's name ends at the first whitespace of any kind, such as the
// line break of a multi-line /embed-append.
func TestCommandNameEndsAtAnyWhitespace(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	c := dialTestClient(t, httpServer)
	if lines := helpLines(t, c, "general"); len(lines) == 0 {
		t.Fatal("no /help")
	}
	before, result := c.request(t, "command", "help", map[string]any{"room_id": "general", "body": map[string]any{"text": "/help\nplease"}})
	if len(result) != 0 || len(before) != 1 || !strings.Contains(privateNoticeText(t, before[0]), "`/help`") {
		t.Fatalf("/help\\nplease: %#v after %#v", result, before)
	}
	write, _, _ := liveCommand(t, c, "append", map[string]any{"room_id": "general", "body": map[string]any{"text": "/embed-append\n# Plan\n\n- one"}})
	if write["kind"] != "append" {
		t.Fatalf("/embed-append\\n# Plan: %#v", write)
	}
	// A tab parts a form's word too: /emoji remove, which only admins use.
	frame := c.call(t, "command", "emoji", map[string]any{"body": map[string]any{"text": "/emoji\tremove\t:x:"}})
	if failure, _ := frame["error"].(map[string]any); failure["message"] != "Only an admin can remove custom emoji" {
		t.Fatalf("/emoji\\tremove: %#v", frame)
	}
}
