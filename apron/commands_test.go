package apron

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
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
		if want := "- `" + usage + "`: " + command["description"].(string); lines[i] != want {
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
			// No Help: the description is never empty.
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
		"embed-terminal": {"name": "embed-terminal", "group": "all", "hint": "[<title>]", "description": "get a URL to stream a program's output into this room, shown as an 80×40 terminal", "uses": []any{"room_id", "reply_to"}},
		"emoji add":      {"name": "emoji add", "group": "all", "hint": ":<name>:", "description": "with one image attached, add a custom emoji or replace one you added", "uses": []any{"body.embeds"}, "requires": []any{"body.embeds"}},
		"echo":           {"name": "echo", "group": "member", "hint": "<text>", "description": "say it back", "uses": []any{"room_id", "reply_to"}, "requires": []any{"reply_to"}},
		"quiet":          {"name": "quiet", "description": "run /quiet"},
	} {
		if got := commandNamed(t, commands, name); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s = %#v, want %#v", name, got, want)
		}
	}
	// Params are ignored, and the list is read again on a retry (§1.2).
	if again := guest.result(t, commandListMethod, "same", map[string]any{"room_id": "general"}); len(again["commands"].([]any)) != len(commands) {
		t.Fatalf("with params: %#v", again)
	}
	if again := guest.result(t, commandListMethod, "same", map[string]any{"room_id": "general"}); len(again["commands"].([]any)) != len(commands) {
		t.Fatalf("again: %#v", again)
	}

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
		"uses": []any{"room_id", "body.mentions"}, "requires": []any{"body.mentions"},
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
