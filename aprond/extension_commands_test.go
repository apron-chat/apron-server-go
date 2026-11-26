package aprond

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/apron-chat/apron-server-go/apron/server/commands"
)

// A command of more forms: each runs its own Run, with the arguments after
// its fixed words, and takes the command's Uses, Requires, Group, and
// Available where it sets none of its own; and an Unlisted command.
func TestExtensionCommandForms(t *testing.T) {
	staff := func(u User, _ Room) bool { return u.HasRole("staff") }
	var calls []*CommandCall
	_, httpServer := newTestServer(t, extensionConfig(testExtension{"poll", func(r *Registrar) error {
		r.Command(Command{
			Name: "poll",
			Uses: []string{FieldRoom}, Group: GroupMember,
			Forms: []Form{
				{
					Name: "poll open", Usage: "/poll open <question>", Help: "open a poll",
					Run: func(_ context.Context, call *CommandCall) (map[string]any, error) {
						calls = append(calls, call)
						if call.Args == "" {
							return nil, call.UsageError("with a question")
						}
						call.Notice("open: " + call.Args + " by " + call.User.ID + " in " + call.Room.ID)
						return map[string]any{"question": call.Args}, nil
					},
				},
				{
					Name: "poll close", Help: "close the poll", Group: GroupRole, Available: staff,
					Run: func(context.Context, *CommandCall) (map[string]any, error) { return nil, errors.New("not for you") },
				},
			},
		})
		r.Command(Command{
			Name: "secret", Unlisted: true,
			Run: func(_ context.Context, call *CommandCall) (map[string]any, error) {
				call.Notice("found it")
				return nil, nil
			},
		})
		r.Command(Command{
			Name: "broken",
			Run: func(context.Context, *CommandCall) (map[string]any, error) {
				return nil, errors.New("the database is down")
			},
		})
		return nil
	}}))
	c := dialTestClient(t, httpServer)

	before, _ := runCommand(t, c, "/help")
	help := noticeText(t, before[0])
	if !strings.Contains(help, "`/poll open <question>`: open a poll") || strings.Contains(help, "/poll close") || strings.Contains(help, "/secret") {
		t.Fatalf("/help = %q, want /poll open, without /poll close or /secret", help)
	}
	open := commandNamed(t, commandList(t, c), "poll open")
	if open["group"] != GroupMember || open["hint"] != "<question>" || len(open["uses"].([]any)) != 1 {
		t.Fatalf("poll open listed as %#v, with the command's group and uses", open)
	}

	before, reply := runCommand(t, c, "/poll open Lunch?")
	if len(before) != 1 || noticeText(t, before[0]) != "open: Lunch? by "+c.userID+" in general" {
		t.Fatalf("/poll open: %#v %#v", before, reply)
	}
	if result, _ := reply["result"].(map[string]any); result["question"] != "Lunch?" {
		t.Fatalf("/poll open = %#v", reply)
	}
	if call := calls[len(calls)-1]; call.Args != "Lunch?" || call.User.ID != c.userID || call.Room.ID != "general" {
		t.Fatalf("call = %#v", call)
	}
	if before, reply = runCommand(t, c, "/secret"); len(before) != 1 || noticeText(t, before[0]) != "found it" || reply["result"] == nil {
		t.Fatalf("/secret: %#v %#v", before, reply)
	}
	for line, code := range map[string]int{
		"/poll open": CodeInvalidParams, "/poll": CodeInvalidParams, "/poll close": CodeDenied,
		"/broken": CodeInternalError,
	} {
		if _, reply = runCommand(t, c, line); errorCode(reply) != code {
			t.Fatalf("%s = %#v, want error %d", line, reply, code)
		}
	}
	if _, reply = runCommand(t, c, "/poll open"); !strings.Contains(reply["error"].(map[string]any)["message"].(string), "Usage: /poll open <question>, with a question") {
		t.Fatalf("/poll open = %#v", reply)
	}
}

func TestExtensionCommandsRegistrationErrors(t *testing.T) {
	run := func(context.Context, *CommandCall) (map[string]any, error) { return nil, nil }
	for name, command := range map[string]Command{
		"no Run":           {Name: "x"},
		"form without Run": {Name: "x", Forms: []Form{{}}},
		"Run and Forms":    {Name: "x", Run: run, Forms: []Form{{Run: run}}},
		"core command":     {Name: "kick", Run: run},
		"bad name":         {Name: "Bad", Run: run},
		"bad form":         {Name: "x", Forms: []Form{{Name: "y", Run: run}}},
		"form twice":       {Name: "x", Forms: []Form{{Run: run}, {Name: "x", Run: run}}},
		"bad field":        {Name: "x", Uses: []string{"embeds"}, Run: run},
		"bad group":        {Name: "x", Group: "admins", Run: run},
	} {
		_, err := Open(extensionConfig(testExtension{"demo", func(r *Registrar) error {
			r.Command(command)
			return nil
		}}))
		if err == nil {
			t.Errorf("%s: Open succeeded", name)
		}
	}
}

// aprond's Field and Group constants are package commands', which checks
// them.
func TestCommandConstants(t *testing.T) {
	for ours, theirs := range map[string]string{
		FieldRoom: commands.FieldRoom, FieldReplyTo: commands.FieldReplyTo,
		FieldMentions: commands.FieldMentions, FieldEmbeds: commands.FieldEmbeds,
		GroupAll: commands.GroupAll, GroupGuest: commands.GroupGuest, GroupMember: commands.GroupMember,
		GroupRoom: commands.GroupRoom, GroupRole: commands.GroupRole, GroupAdmin: commands.GroupAdmin,
	} {
		if ours != theirs {
			t.Errorf("%q is %q in package commands", ours, theirs)
		}
	}
}
