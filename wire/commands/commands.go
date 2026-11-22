// Package commands gives a wire backend the command capability (§4.1)
// without writing its parts: commands registered by name, each with the
// forms /help shows, such as /kick @user [<reason>]; a [Router] that is
// the Server's [wire.CommandHandler], which parses the command line, picks
// the command and its form, refuses one sent without a field it requires
// or by someone it is not for, runs it, and answers /help with a ~private
// notice that lists what the sender can use (§4.1); and the same list as
// data for a command listing (apron's ext:command_list, [ListMethod]).
// It keeps no chat state: who may use what is the backend's, asked
// through each form's May.
//
//	var router commands.Router
//	router.MustAdd(commands.Command{
//		Name:  "roll",
//		Forms: []commands.Form{{Usage: "/roll [<sides>]", Help: "roll a die"}},
//		Run: func(ctx context.Context, call *commands.Call) error {
//			call.Notice("You rolled a 4")
//			return call.Done()
//		},
//	})
//	server.Commands = &router
//	server.Handle(commands.ListMethod, router.ListHandler()) // optional
//
// # Command lines
//
// A command line is body.text, trimmed: a slash, the command's name, which
// ends at the first whitespace, a line break too, and its arguments, the
// rest, trimmed ([Parse]). Names are case-sensitive: /Kick is not /kick. A
// command has one form or more, each of them its name and any fixed words
// that follow it, such as /emoji add and /emoji remove; the form a line
// selects is the one with the most fixed words that start its arguments
// ([Router.Find]). The command's other fields, mentions, reply_to, embeds,
// and ext, are its arguments (§4.1), which a form says it uses or
// requires (Form.Uses, Form.Requires).
//
// # Replies
//
// A command replies as any typed handler does: its notices first, such as
// with [Call.Notice], which the sender sees before the result, then the
// result with Call.Reply ([Call.Done] for {}), or an error. A backend that
// keeps its state under a lock replies while it holds it; one whose
// command takes long answers later (wire.Reply.Later). A failure is an
// ordinary *wire.Error, whose message the client shows: the Router
// answers a line it cannot run invalid_params with what to do instead,
// and a form the sender may not use denied.
//
// # Backends with their own dispatch
//
// Router.Command checks in one order, and holds no lock of the backend's.
// A backend whose commands are checked in another order, or under its own
// lock, such as package apron's, which checks the room before the command
// and runs its own commands while it holds its lock, keeps its commands
// in a Router all the same, for their forms, and dispatches them itself,
// with the parts Router.Command is made of: [Parse], [Router.Find],
// [Router.Usable], [Help], and [Entries].
package commands

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/apron-chat/apron-server-go/wire"
)

// The fields of a command request, besides body.text, that a form may use
// or require (Form.Uses): room_id, when it acts on the room it is sent in,
// and the arguments of §4.1. A command's ext is an argument too, but one
// no listing names.
const (
	FieldRoom     = "room_id"
	FieldReplyTo  = "reply_to"
	FieldMentions = "body.mentions"
	FieldEmbeds   = "body.embeds"
)

var fields = []string{FieldRoom, FieldReplyTo, FieldMentions, FieldEmbeds}

// The groups a form may name (Form.Group), from the widest to the
// narrowest: who may use it, for display only.
const (
	GroupAll    = "all"
	GroupGuest  = "guest"
	GroupMember = "member"
	GroupRoom   = "room"
	GroupRole   = "role"
	GroupAdmin  = "admin"
)

var groups = []string{GroupAll, GroupGuest, GroupMember, GroupRoom, GroupRole, GroupAdmin}

// ListMethod is apron's command listing, ext:command_list, as a request
// and its capability: it answers {"commands": [...]}, an [Entry] for each
// form the sender may use in at least one room (Router.ListHandler).
const ListMethod = "ext:command_list"

// Command is a command people run by sending /Name as a command request.
type Command struct {
	// Name is what follows the slash: one word, without whitespace or a
	// leading slash.
	Name string
	// Forms are what /help and a listing show of the command, in order:
	// one for each fixed word that may follow the name, such as
	// /emoji add and /emoji remove, or one for the command alone. None is
	// one form, Name alone.
	Forms []Form
	// Run runs the command for Router.Command, with the form the command
	// line selects, once the Router has checked what the form requires
	// and that the sender may use it (Form.May). It replies with
	// call.Reply, or returns an error, as a typed handler does (see the
	// package documentation).
	//
	// A command without Run is one Router.Command does not run: a command
	// named help, which places /help, or changes how it is shown, while
	// the Router answers it; or one that a backend with its own dispatch
	// runs itself.
	Run func(ctx context.Context, call *Call) error
}

// Form is one form of a command, as /help and a listing show it.
type Form struct {
	// Name is the words the user types after the slash: the command's
	// name, then any fixed word, such as "emoji add", each separated from
	// the next by one space. "" is the command's name alone.
	Name string
	// Usage is the form as people type it, such as
	// "/kick @user [<reason>]". When it starts with the slash and Name,
	// then whitespace or nothing, what follows is the form's Hint, unless
	// one is set; any other Usage is shown in /help as written, and gives
	// no hint. "" is the slash, Name, and Hint.
	Usage string
	// Hint describes the text after Name, such as "@user [<reason>]",
	// following the suggested convention: <value>, [optional], a|b,
	// repeats..., and @user for a mention. Its whitespace is collapsed.
	Hint string
	// Help says what the form does, plain text on one line: its
	// whitespace is collapsed. "" is none, and /help shows the usage
	// alone.
	Help string
	// Uses are the fields of the command request, besides body.text, that
	// the form reads: FieldRoom when it acts on the room it is sent in,
	// and the arguments it takes. Requires are those of them it fails
	// without, which Router.Command refuses it without, as
	// invalid_params: with FieldRoom, the form acts only on a room the
	// client names, not on the default room.
	Uses, Requires []string
	// Group says who may use the form, for display: the narrowest group
	// that holds everyone May lets use it, or "".
	Group string
	// May reports whether the connection may use the form in the room
	// roomID, or, for "", in a room it can see, as a listing asks. nil
	// lets everyone. /help lists the forms the sender may use in the
	// command's room, and Router.Command refuses the others, as denied.
	// It may be called for every form on each /help and listing: keep it
	// cheap, and free of side effects.
	May func(ctx context.Context, c *wire.Conn, roomID string) bool
}

// Synopsis is the form as /help shows it, such as "/kick @user [<reason>]".
func (f Form) Synopsis() string {
	switch {
	case f.Usage != "":
		return f.Usage
	case f.Hint == "":
		return "/" + f.Name
	}
	return "/" + f.Name + " " + f.Hint
}

// HelpLine is the form's line in /help, such as
// "- `/kick @user [<reason>]`: remove someone from this room", or
// "- `/quiet`" without Help.
func (f Form) HelpLine() string {
	if f.Help == "" {
		return fmt.Sprintf("- `%s`", f.Synopsis())
	}
	return fmt.Sprintf("- `%s`: %s", f.Synopsis(), f.Help)
}

// Entry is the form as a listing lists it.
func (f Form) Entry() Entry {
	return Entry{
		Name: f.Name, Description: f.Help, Group: f.Group, Hint: f.Hint,
		Uses: slices.Clone(f.Uses), Requires: slices.Clone(f.Requires),
	}
}

// fixed returns the fixed words of the form, after the command's name.
func (f *Form) fixed() []string { return strings.Fields(f.Name)[1:] }

// Entry is a form in a command listing: a Command object of apron's
// ext:command_list. Its fields are in the order of their JSON names, as
// wire encodes an object's keys.
type Entry struct {
	Description string   `json:"description,omitempty"`
	Group       string   `json:"group,omitempty"`
	Hint        string   `json:"hint,omitempty"`
	Name        string   `json:"name"`
	Requires    []string `json:"requires,omitempty"`
	Uses        []string `json:"uses,omitempty"`
}

// Help is the text of /help that lists forms: Markdown, a HelpLine for
// each, one to a line.
func Help(forms []Form) string {
	lines := make([]string, len(forms))
	for i, f := range forms {
		lines[i] = f.HelpLine()
	}
	return strings.Join(lines, "\n")
}

// Entries are the forms as a listing lists them, [] for none.
func Entries(forms []Form) []Entry {
	entries := make([]Entry, len(forms))
	for i, f := range forms {
		entries[i] = f.Entry()
	}
	return entries
}

// Parse reads a command line: body.text, trimmed, a slash, then the
// command's name and its arguments ([Cut]). ok is false for text that
// does not start with a slash.
func Parse(text string) (name, args string, ok bool) {
	line, ok := strings.CutPrefix(strings.TrimSpace(text), "/")
	if !ok {
		return "", "", false
	}
	name, args = Cut(line)
	return name, args, true
}

// Cut splits text, trimmed, at its first whitespace, of any kind, into the
// word before it and the rest, trimmed: how a command's name, and a fixed
// word, are told from what follows them.
func Cut(text string) (word, rest string) {
	text = strings.TrimSpace(text)
	i := strings.IndexFunc(text, unicode.IsSpace)
	if i < 0 {
		return text, ""
	}
	return text[:i], strings.TrimSpace(text[i:])
}

// ParseUsage returns the hint of a form's usage: what follows the slash
// and the form's name, then whitespace, its whitespace collapsed, such as
// "<city>" of "/weather <city>". ok is false for a usage that does not
// start so, such as "" or "weather <city>".
func ParseUsage(name, usage string) (hint string, ok bool) {
	rest, ok := strings.CutPrefix(usage, "/"+name)
	trimmed := strings.TrimLeftFunc(rest, unicode.IsSpace)
	if !ok || rest != "" && trimmed == rest {
		return "", false
	}
	return collapse(trimmed), true
}

// collapse collapses text's whitespace, line breaks too, into single
// spaces, and trims it.
func collapse(text string) string { return strings.Join(strings.Fields(text), " ") }

// helpForm is how a Router lists /help when no command named help was
// added.
var helpForm = Form{Name: "help", Group: GroupAll, Help: "list the commands you can use here"}

// Router is a set of commands, in the order they were added, and the
// Server's CommandHandler (Server.Commands) that runs them. Its zero
// value is ready, with /help and no other command. Add every command
// before the Router serves: Add is not safe for use with its other
// methods at the same time, which are safe for concurrent use.
type Router struct {
	// Fallback runs a command line whose name the Router has no command
	// for, with a Call whose Form is nil, such as a gateway's that hands
	// it to the system behind it. nil answers invalid_params, Unknown
	// command.
	Fallback func(ctx context.Context, call *Call) error

	commands []Command
	// hasHelp is whether commands has one named help.
	hasHelp bool
}

// Add adds a command, after those added before it: its forms are listed
// in that order. /help is listed first unless a command named help is
// added, which takes its place. It returns an error, and adds nothing,
// for a command whose name is taken or not one word, or a form that is
// not of the command, names a field that is none of the Field constants,
// or more than once, requires a field it does not use, or names a group
// that is none of the Group constants, so that a typo does not reach
// clients.
func (r *Router) Add(command Command) error {
	name := command.Name
	switch {
	case name == "" || strings.ContainsFunc(name, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }):
		return fmt.Errorf("commands: command %q: a name is one word", name)
	case strings.HasPrefix(name, "/"):
		return fmt.Errorf("commands: command %q: a name does not start with the slash", name)
	case slices.ContainsFunc(r.commands, func(c Command) bool { return c.Name == name }):
		return fmt.Errorf("commands: command /%s exists already", name)
	}
	forms := slices.Clone(command.Forms)
	if len(forms) == 0 {
		forms = []Form{{}}
	}
	for i := range forms {
		f := &forms[i]
		if f.Name == "" {
			f.Name = name
		}
		if words := strings.Fields(f.Name); len(words) == 0 || words[0] != name || strings.Join(words, " ") != f.Name {
			return fmt.Errorf("commands: command /%s: form %q is not /%s and fixed words, one space apart", name, f.Name, name)
		}
		for j := range i {
			if forms[j].Name == f.Name {
				return fmt.Errorf("commands: command /%s: form %q is there twice", name, f.Name)
			}
		}
		if err := checkFields(f.Uses, f.Requires); err != nil {
			return fmt.Errorf("commands: command /%s: %w", name, err)
		}
		if f.Group != "" && !slices.Contains(groups, f.Group) {
			return fmt.Errorf("commands: command /%s: group %q is none of %s", name, f.Group, strings.Join(groups, ", "))
		}
		if f.Hint == "" && f.Usage != "" {
			if hint, ok := ParseUsage(f.Name, f.Usage); ok {
				f.Hint, f.Usage = hint, ""
			}
		}
		f.Hint, f.Help = collapse(f.Hint), collapse(f.Help)
		f.Uses, f.Requires = slices.Clone(f.Uses), slices.Clone(f.Requires)
	}
	command.Forms = forms
	r.hasHelp = r.hasHelp || name == "help"
	r.commands = append(r.commands, command)
	return nil
}

// MustAdd adds commands, as Add does, and panics on the error of any.
func (r *Router) MustAdd(commands ...Command) {
	for _, c := range commands {
		if err := r.Add(c); err != nil {
			panic(err)
		}
	}
}

// checkFields checks a form's Uses and Requires: Field constants, each
// once, and every field it requires among those it uses.
func checkFields(uses, requires []string) error {
	for i, field := range uses {
		switch {
		case !slices.Contains(fields, field):
			return fmt.Errorf("field %q is none of %s", field, strings.Join(fields, ", "))
		case slices.Contains(uses[:i], field):
			return fmt.Errorf("field %q is used twice", field)
		}
	}
	for i, field := range requires {
		switch {
		case !slices.Contains(uses, field):
			return fmt.Errorf("field %q is required but not in Uses", field)
		case slices.Contains(requires[:i], field):
			return fmt.Errorf("field %q is required twice", field)
		}
	}
	return nil
}

// Find returns the command named name, and its form that args, a command
// line's arguments, select: the one with the most fixed words that start
// args, matched exactly, as names are. The form is nil when none does,
// such as for /emoji alone, of /emoji add and /emoji remove, and both
// are nil for a name the Router has no command for. What Find returns is
// the Router's, not to be changed.
func (r *Router) Find(name, args string) (*Command, *Form) {
	i := slices.IndexFunc(r.commands, func(c Command) bool { return c.Name == name })
	if i < 0 {
		return nil, nil
	}
	command := &r.commands[i]
	words := strings.Fields(args)
	var best *Form
	most := -1
	for j := range command.Forms {
		f := &command.Forms[j]
		fixed := f.fixed()
		if len(fixed) > most && len(fixed) <= len(words) && slices.Equal(fixed, words[:len(fixed)]) {
			best, most = f, len(fixed)
		}
	}
	return command, best
}

// Usable returns the forms c may use in the room roomID (Form.May), as
// /help lists them, or, for "", in a room it can see, as a listing does,
// in the order of their commands: /help first, unless a command named
// help was added.
func (r *Router) Usable(ctx context.Context, c *wire.Conn, roomID string) []Form {
	var forms []Form
	if !r.hasHelp {
		forms = append(forms, helpForm)
	}
	for _, command := range r.commands {
		for _, f := range command.Forms {
			if f.May == nil || f.May(ctx, c, roomID) {
				forms = append(forms, f)
			}
		}
	}
	return forms
}

// List is what a listing lists for c: an Entry for each form c may use in
// a room it can see.
func (r *Router) List(ctx context.Context, c *wire.Conn) []Entry {
	return Entries(r.Usable(ctx, c, ""))
}

// ListHandler answers ListMethod, {"commands": [...]}, with List, and
// lists its capability: registered with Server.Handle under ListMethod,
// it gives clients that know apron's ext:command_list, such as to
// complete what people type, the commands of any backend. It takes no
// params, and ignores any.
func (r *Router) ListHandler() wire.Handler {
	return wire.Handler{
		Capability: ListMethod,
		NoDedup:    true,
		Serve: func(ctx context.Context, c *wire.Conn, req *wire.Request) error {
			req.Reply(map[string]any{"commands": r.List(ctx, c)})
			return nil
		},
	}
}

// Command runs a command request (§4.1): it parses the command line,
// finds its command and form, checks the fields the form requires and
// whether c may use it, and runs it; it answers /help itself. A line it
// cannot run is invalid_params, saying what to do instead, and a form c
// may not use is denied. A backend that checks more than the Router can,
// such as that the room exists, checks it first, and then calls Command.
func (r *Router) Command(ctx context.Context, c *wire.Conn, p wire.CommandParams, reply wire.Reply[wire.CommandResult]) error {
	name, args, ok := Parse(p.Body.Text)
	if !ok {
		return wire.InvalidParams("A command starts with /; try /help")
	}
	command, form := r.Find(name, args)
	call := &Call{Conn: c, Params: p, Reply: reply, Name: name, Args: args}
	switch {
	case command == nil && name == "help":
		return r.runHelp(ctx, call)
	case command == nil && r.Fallback != nil:
		return r.Fallback(ctx, call)
	case command == nil:
		return wire.InvalidParams(fmt.Sprintf("Unknown command /%s; try /help", name))
	case form == nil:
		synopses := make([]string, len(command.Forms))
		for i, f := range command.Forms {
			synopses[i] = "`" + f.Synopsis() + "`"
		}
		return wire.InvalidParams("Usage: " + strings.Join(synopses, " or "))
	}
	if err := missing(form, p); err != nil {
		return err
	}
	if form.May != nil && !form.May(ctx, c, p.RoomID) {
		return wire.Denied(fmt.Sprintf("You cannot use /%s here", form.Name))
	}
	f := *form
	call.Form = &f
	switch {
	case command.Run != nil:
		return command.Run(ctx, call)
	case name == "help":
		return r.runHelp(ctx, call)
	}
	return errors.New("commands: command /" + name + " has no Run")
}

// missing returns the error of a command without a field its form
// requires, or nil.
func missing(f *Form, p wire.CommandParams) error {
	for _, field := range f.Requires {
		var absent bool
		var message string
		switch field {
		case FieldRoom:
			absent, message = p.RoomID == "", "Send /%s from a room: it needs room_id"
		case FieldReplyTo:
			absent, message = p.ReplyTo == nil, "Send /%s as a reply to a message: it needs reply_to"
		case FieldMentions:
			absent, message = len(p.Body.Mentions) == 0, "Mention someone in /%s: it needs body.mentions"
		case FieldEmbeds:
			absent, message = len(p.Body.Embeds) == 0, "Attach something to /%s: it needs body.embeds"
		}
		if absent {
			return wire.InvalidParams(fmt.Sprintf(message, f.Name))
		}
	}
	return nil
}

// runHelp answers /help: a ~private notice in the command's room that
// lists the forms the sender may use there, then {}.
func (r *Router) runHelp(ctx context.Context, call *Call) error {
	call.Notice(Help(r.Usable(ctx, call.Conn, call.Params.RoomID)))
	return call.Done()
}

// Call is one run of a command.
type Call struct {
	// Conn sent the command.
	Conn *wire.Conn
	// Params are the command's: its room, "" for the default room, its
	// body, whose text is the command line, and its other arguments.
	Params wire.CommandParams
	// Reply answers the command, once its notices are sent.
	Reply wire.Reply[wire.CommandResult]
	// Name is the command's name, and Args the command line after it,
	// trimmed.
	Name, Args string
	// Form is the form the command line selects, a copy; nil for a
	// Fallback's.
	Form *Form
}

// Rest is the command line after the form's fixed words, trimmed, such as
// "Ops" of "/server title Ops".
func (c *Call) Rest() string {
	rest := c.Args
	if c.Form != nil {
		for range c.Form.fixed() {
			_, rest = Cut(rest)
		}
	}
	return rest
}

// Notice sends the sender a ~private notice in the command's room, as
// Markdown (Appendix A.1), before the result. It reports whether the
// notice was queued.
func (c *Call) Notice(text string) bool {
	notice := wire.Notice(c.Params.RoomID, text)
	notice.Body.Format = wire.Markdown
	return c.Conn.Notify(notice)
}

// Done answers the command {}, followed by then, as Reply.Send does.
func (c *Call) Done(then ...wire.Frame) error {
	return c.Reply.Send(wire.CommandResult{}, then...)
}

// UsageError is the error of a command line the form cannot run:
// invalid_params, with the form's usage, and detail, if any, after it,
// such as "mentioning exactly one user".
func (c *Call) UsageError(detail string) *wire.Error {
	usage := "/" + c.Name
	if c.Form != nil {
		usage = c.Form.Synopsis()
	}
	message := "Usage: " + usage
	if detail != "" {
		message += ", " + detail
	}
	return wire.InvalidParams(message)
}
