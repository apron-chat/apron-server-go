package apron

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/apron-chat/apron-server-go/wire/commands"
)

// An extension's commands in package commands' terms: forms with fixed
// words, Unlisted, RunChecks, and the run as apron sees it (CallOf).
func TestExtensionCommandsInWireTerms(t *testing.T) {
	staff := May(func(u User, _ Room) bool { return u.HasRole("staff") })
	var calls []*CommandCall
	_, httpServer := newTestServer(t, extensionConfig(testExtension{"poll", func(r *Registrar) error {
		r.Commands(
			commands.Command{
				Name: "poll",
				Forms: []commands.Form{
					{Name: "poll open", Hint: "<question>", Help: "open a poll", Uses: []string{commands.FieldRoom}},
					{Name: "poll close", Help: "close the poll", Group: commands.GroupRole, May: staff},
				},
				Run: func(_ context.Context, call *commands.Call) error {
					cc := CallOf(call)
					calls = append(calls, cc)
					if call.Form.Name == "poll open" && call.Rest() == "" {
						return call.UsageError("with a question")
					}
					cc.Notice(call.Form.Name + ": " + call.Rest() + " by " + cc.User.ID + " in " + cc.Room.ID)
					return call.Done()
				},
			},
			commands.Command{
				Name: "secret", Unlisted: true,
				Run: func(_ context.Context, call *commands.Call) error {
					call.Notice("found it")
					return call.Done()
				},
			},
			commands.Command{
				// It checks the form itself: /loose alone runs, with no form.
				Name: "loose", RunChecks: commands.CheckForm,
				Forms: []commands.Form{{Name: "loose end"}},
				Run: func(_ context.Context, call *commands.Call) error {
					if call.Form != nil {
						return errors.New("the form is not nil")
					}
					return Denied("Only /loose end, please")
				},
			},
			commands.Command{
				Name: "broken",
				Run:  func(context.Context, *commands.Call) error { return errors.New("the database is down") },
			},
		)
		return nil
	}}))
	c := dialTestClient(t, httpServer)

	before, _ := runCommand(t, c, "/help")
	help := noticeText(t, before[0])
	if !strings.Contains(help, "`/poll open <question>`: open a poll") || strings.Contains(help, "/poll close") || strings.Contains(help, "/secret") {
		t.Fatalf("/help = %q, want /poll open, without /poll close or /secret", help)
	}

	before, reply := runCommand(t, c, "/poll open Lunch?")
	if len(before) != 1 || noticeText(t, before[0]) != "poll open: Lunch? by "+c.userID+" in general" || reply["result"] == nil {
		t.Fatalf("/poll open: %#v %#v", before, reply)
	}
	if cc := calls[len(calls)-1]; cc.Args != "open Lunch?" || cc.User.ID != c.userID || cc.Room.ID != "general" {
		t.Fatalf("CallOf = %#v", cc)
	}
	if before, reply = runCommand(t, c, "/secret"); len(before) != 1 || noticeText(t, before[0]) != "found it" || reply["result"] == nil {
		t.Fatalf("/secret: %#v %#v", before, reply)
	}
	for line, code := range map[string]int{
		"/poll open": CodeInvalidParams, "/poll": CodeInvalidParams, "/poll close": CodeDenied,
		"/loose": CodeDenied, "/broken": CodeInternalError,
	} {
		if _, reply = runCommand(t, c, line); errorCode(reply) != code {
			t.Fatalf("%s = %#v, want error %d", line, reply, code)
		}
	}
	if _, reply = runCommand(t, c, "/poll open"); !strings.Contains(reply["error"].(map[string]any)["message"].(string), "Usage: /poll open <question>, with a question") {
		t.Fatalf("/poll open = %#v", reply)
	}
	if CallOf(&commands.Call{}) != nil {
		t.Fatal("CallOf of a call apron did not make")
	}
}

func TestExtensionCommandsRegistrationErrors(t *testing.T) {
	run := func(_ context.Context, call *commands.Call) error { return call.Done() }
	for name, command := range map[string]commands.Command{
		"no Run":       {Name: "x"},
		"core command": {Name: "kick", Run: run},
		"bad name":     {Name: "Bad", Run: run},
		"bad form":     {Name: "x", Forms: []commands.Form{{Name: "y"}}, Run: run},
		"bad field":    {Name: "x", Forms: []commands.Form{{Uses: []string{"embeds"}}}, Run: run},
	} {
		_, err := Open(extensionConfig(testExtension{"demo", func(r *Registrar) error {
			r.Commands(command)
			return nil
		}}))
		if err == nil {
			t.Errorf("%s: Open succeeded", name)
		}
	}
}
