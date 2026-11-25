// Package commands gives a wire backend the command capability (§4.1)
// without writing its parts: commands registered by name, each with the
// forms /help shows, such as /kick @user [<reason>]; a [Router] that is
// the Server's [server.CommandHandler], which parses the command line, picks
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
//	srv := server.New(chat, server.Config{Messages: chat, Profiles: chat, Commands: &router})
//	srv.Handle(commands.ListMethod, router.ListHandler()) // optional
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
// command takes long answers later (server.Reply.Later). A failure is an
// ordinary *apron.Error, whose message the client shows: the Router
// answers a line it cannot run invalid_params with what to do instead,
// and a form the sender may not use denied.
//
// # Checks
//
// Router.Command checks a command line in this order, each check's error
// the reply: the line starts with a slash; a form that requires room_id
// has one; the backend's own checks of every command line
// (Router.Before), such as that the room exists and the sender may see
// it, or that reply_to names a message; the name is a command's, or
// /help; the line selects one of its forms; the form's other required
// fields are there; and the sender may use it (Form.May). Before may also
// settle what the command line leaves to the server, such as the default
// room of a command sent without room_id, which the checks after it,
// /help, and Run then see: the Router knows no rooms, so a backend with a
// default room sets it there (see Router.Before), or May and Run get "".
// What Before works out that Run needs too, such as the sender's record,
// or the command line as the backend rewrote it, it passes on in
// [Call].Data.
//
// A command whose Run makes some of the Router's checks itself, with
// errors that say more, such as who may use it, names them in its
// SkipChecks: the Router skips them, and its forms' Requires and May then
// only say what /help and a listing show. A backend whose commands all
// check something themselves, such as a gateway whose host checks every
// command, names it once, in Router.SkipChecks. Package apron's own
// commands check everything themselves; its extensions' leave who may use
// them to the Router.
//
// A command that runs but is not shown, such as an alias, or one people
// are not meant to find, is Unlisted: /help and a listing leave it out,
// and the Router runs it as any other.
//
// The Router holds no lock of the backend's, and calls a form's May
// without one: a backend that keeps its state under a lock takes it in
// May, Before, and Run, for as long as each needs it. So May may run code
// that must not run under the lock, such as package apron's extensions',
// once it has let go of it.
//
// A backend that dispatches commands itself still keeps them in a Router,
// for their forms, and has the parts Router.Command is made of: [Parse],
// [Router.Find], [Router.Usable], [Help], and [Entries].
package commands

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/server"
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
// and its capability: it answers a [ListResult], {"commands": [...]}, an
// [Entry] for each form the sender may use in at least one room
// (Router.ListHandler).
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
	// SkipChecks are the checks of a command line that Router.Command
	// skips, with those of Router.SkipChecks, as Run makes them itself,
	// with errors of its own (see Checks in the package documentation). With
	// CheckForm, a line that selects none of the forms runs with a nil
	// call.Form.
	SkipChecks Checks
	// Unlisted keeps the command out of /help and listings (Router.Usable,
	// Router.List), while the Router finds and runs it as any other: an
	// alias, or a command people are not meant to find. Its forms still
	// say what it uses and requires, which the Router checks.
	Unlisted bool
}

// Checks are checks Router.Command makes of a command line before it runs
// the command, unless the command makes them itself (Command.SkipChecks).
type Checks uint8

const (
	// CheckForm refuses a line that selects none of the command's forms,
	// as invalid_params with their usage.
	CheckForm Checks = 1 << iota
	// CheckRequires refuses a command without a field its form requires
	// (Form.Requires), as invalid_params, but for room_id, which is
	// checked for every command, before Router.Before.
	CheckRequires
	// CheckMay refuses a form the sender may not use (Form.May), as
	// denied.
	CheckMay

	// CheckAll is every check.
	CheckAll = CheckForm | CheckRequires | CheckMay
)

// Form is one form of a command, as /help and a listing show it.
type Form struct {
	// Name is the words the user types after the slash: the command's
	// name, then any fixed word, such as "emoji add", each separated from
	// the next by one space. "" is the command's name alone.
	Name string
	// Usage and Hint are two ways to say how the form is typed: set
	// one. Usage is the whole line, such as "/kick @user [<reason>]",
	// and Hint the text after Name, "@user [<reason>]"; Add turns a Usage
	// that starts with the slash and Name, then whitespace or nothing,
	// into the Hint, so the two are the same form. Hint is the shorter
	// for a form with fixed words, such as "emoji add" with ":<name>:".
	//
	// A Usage that does not start with the form, such as one with words
	// of its own, is shown in /help as written, and gives a listing no
	// hint; so is one set beside a Hint, which the listing gives. With
	// neither, the form is the slash and Name alone.
	Usage string
	// Hint describes the text after Name, following the suggested
	// convention: <value>, [optional], a|b, repeats..., and @user for a
	// mention. /help shows it after the slash and Name, and a listing as
	// the form's hint. Its whitespace is collapsed.
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
	May func(ctx context.Context, c *server.Conn, roomID string) bool
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

// ListResult is the result of ListMethod: an Entry for each form the
// sender may use, [] for none (Router.ListHandler). A client decodes a
// listing into one.
type ListResult struct {
	Commands []Entry `json:"commands"`
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
// Server's CommandHandler (Config.Commands) that runs them. Its zero
// value is ready, with /help and no other command. Add every command
// before the Router serves: Add is not safe for use with its other
// methods at the same time, which are safe for concurrent use.
type Router struct {
	// Before, if set, checks each command line that starts with a slash,
	// once a form that requires room_id has it, before the Router looks
	// at the name: the backend's checks of what every command needs, such
	// as that its room exists and the sender may see it, or that reply_to
	// names a message. Its error is the reply. It may change call.Params
	// and call.Args, such as to set the room a command without room_id
	// runs in, or the arguments as a filter rewrites them, and the checks
	// after it, /help, and Run see the change. call.Form is the form the
	// line selects, or nil.
	//
	// The Router knows no rooms: a backend with a default room, which a
	// command without room_id runs in (§3.6.3), sets it here, so that May
	// and Run are asked about that room, not "":
	//
	//	router.Before = func(ctx context.Context, call *commands.Call) error {
	//		if call.Params.RoomID == "" {
	//			call.Params.RoomID = chat.defaultRoom
	//		}
	//		if !chat.exists(call.Params.RoomID) {
	//			return apron.InvalidParams("No such room")
	//		}
	//		return nil
	//	}
	Before func(ctx context.Context, call *Call) error
	// Fallback runs a command line whose name the Router has no command
	// for, but /help, which it answers itself, with a Call whose Form is
	// nil, such as a gateway's that hands it to the system behind it. nil
	// answers invalid_params, Unknown command.
	Fallback func(ctx context.Context, call *Call) error
	// SkipChecks are checks the Router skips for every command, as each
	// command's Run makes them itself, as if each named them in its own
	// Command.SkipChecks, which add to them: such as
	// CheckAll for a gateway whose host checks every command line, and
	// says what is wrong with it. A command cannot take one back.
	SkipChecks Checks
	// NoticeFrom is who the Router's ~private notices are from: those of
	// Call.Notice, and /help's. The zero User is apron.Private; a backend
	// that names its own system identities otherwise sets it, with the
	// user_id "~private" (Appendix A.1).
	NoticeFrom apron.User

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
// help was added. Unlisted commands are left out.
func (r *Router) Usable(ctx context.Context, c *server.Conn, roomID string) []Form {
	var forms []Form
	if !r.hasHelp {
		forms = append(forms, helpForm)
	}
	for _, command := range r.commands {
		if command.Unlisted {
			continue
		}
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
func (r *Router) List(ctx context.Context, c *server.Conn) []Entry {
	return Entries(r.Usable(ctx, c, ""))
}

// ListHandler answers ListMethod, a ListResult, with List, and
// lists its capability: registered with Server.Handle under ListMethod,
// it gives clients that know apron's ext:command_list, such as to
// complete what people type, the commands of any backend. It takes no
// params, and ignores any.
func (r *Router) ListHandler() server.Handler {
	return server.Handler{
		Capability: ListMethod,
		NoDedup:    true,
		Serve: func(ctx context.Context, c *server.Conn, req *server.Request, reply server.Reply[any]) error {
			return reply.Send(ListResult{Commands: r.List(ctx, c)})
		},
	}
}

// Command runs a command request (§4.1): it parses the command line,
// finds its command and form, checks the fields the form requires, the
// backend's own checks (Before), and whether c may use the form, and runs
// it; it answers /help itself. A line it cannot run is invalid_params,
// saying what to do instead, and a form c may not use is denied. The
// package documentation lists the checks in their order (Checks).
func (r *Router) Command(ctx context.Context, c *server.Conn, p apron.CommandParams, reply server.Reply[apron.CommandResult]) error {
	name, args, ok := Parse(p.Body.Text)
	if !ok {
		return apron.InvalidParams("A command starts with /; try /help")
	}
	command, form := r.Find(name, args)
	call := &Call{Conn: c, Params: p, Reply: reply, Name: name, Args: args, from: r.NoticeFrom}
	if form != nil {
		// A form that requires room_id acts only on a room the client
		// names, not on one Before settles for it.
		if slices.Contains(form.Requires, FieldRoom) && p.RoomID == "" {
			return missingField(form, FieldRoom)
		}
		f := *form
		call.Form = &f
	}
	if r.Before != nil {
		if err := r.Before(ctx, call); err != nil {
			return err
		}
	}
	var checks Checks = CheckAll
	if command != nil {
		checks &^= r.SkipChecks | command.SkipChecks
	}
	switch {
	case command == nil && name == "help":
		return r.runHelp(ctx, call)
	case command == nil && r.Fallback != nil:
		return r.Fallback(ctx, call)
	case command == nil:
		return apron.InvalidParams(fmt.Sprintf("Unknown command /%s; try /help", name))
	case form == nil && checks&CheckForm != 0:
		synopses := make([]string, len(command.Forms))
		for i, f := range command.Forms {
			synopses[i] = "`" + f.Synopsis() + "`"
		}
		return apron.InvalidParams("Usage: " + strings.Join(synopses, " or "))
	}
	if form != nil && checks&CheckRequires != 0 {
		if err := missing(form, call.Params); err != nil {
			return err
		}
	}
	if form != nil && checks&CheckMay != 0 && form.May != nil && !form.May(ctx, c, call.Params.RoomID) {
		return apron.Denied(fmt.Sprintf("You cannot use /%s here", form.Name))
	}
	switch {
	case command.Run != nil:
		return command.Run(ctx, call)
	case name == "help":
		return r.runHelp(ctx, call)
	}
	return errors.New("commands: command /" + name + " has no Run")
}

// missing returns the error of a command without a field its form
// requires, or nil. room_id was checked before Router.Before.
func missing(f *Form, p apron.CommandParams) error {
	for _, field := range f.Requires {
		var absent bool
		switch field {
		case FieldReplyTo:
			absent = p.ReplyTo == nil
		case FieldMentions:
			absent = len(p.Body.Mentions) == 0
		case FieldEmbeds:
			absent = len(p.Body.Embeds) == 0
		}
		if absent {
			return missingField(f, field)
		}
	}
	return nil
}

// missingMessages say what to do about a field a form requires.
var missingMessages = map[string]string{
	FieldRoom:     "Send /%s from a room: it needs room_id",
	FieldReplyTo:  "Send /%s as a reply to a message: it needs reply_to",
	FieldMentions: "Mention someone in /%s: it needs body.mentions",
	FieldEmbeds:   "Attach something to /%s: it needs body.embeds",
}

// missingField is the error of a command without a field its form
// requires.
func missingField(f *Form, field string) *apron.Error {
	return apron.InvalidParams(fmt.Sprintf(missingMessages[field], f.Name))
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
	Conn *server.Conn
	// Params are the command's: its room, "" for the default room, its
	// body, whose text is the command line, and its other arguments. The
	// Router knows no rooms, so "" stays "" for May, /help, and Run,
	// unless Router.Before sets the backend's default room (§3.6.3).
	Params apron.CommandParams
	// Reply answers the command, once its notices are sent.
	Reply server.Reply[apron.CommandResult]
	// Name is the command's name, and Args the command line after it,
	// trimmed.
	Name, Args string
	// Form is the form the command line selects, a copy; nil for a
	// Fallback's, and for a line that selects none, as Before, and the
	// Run of a command that makes CheckForm itself, may see.
	Form *Form
	// Data is the backend's, for this run alone: nil until Before sets
	// it, then passed on as it is to Run, or to Fallback. It carries what
	// Before works out that Run needs too, such as the sender's record or
	// the command line as the backend rewrote it, so that Before need not
	// fold it into Params, and Run need not work it out again.
	Data any

	// from is Router.NoticeFrom, the sender of Notice's notices.
	from apron.User
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
// Markdown (Appendix A.1), from Router.NoticeFrom, before the result. It reports
// whether the notice was queued.
func (c *Call) Notice(text string) bool {
	notice := apron.Notice(c.Params.RoomID, text)
	notice.Body.Format = apron.Markdown
	if c.from.UserID != "" {
		notice.From = c.from
	}
	return c.Conn.Notify(notice)
}

// Done answers the command {}, followed by then, as Reply.Send does.
func (c *Call) Done(then ...apron.Frame) error {
	return c.Reply.Send(apron.CommandResult{}, then...)
}

// UsageError is the error of a command line the form cannot run:
// invalid_params, with the form's usage, and detail, if any, after it,
// such as "mentioning exactly one user".
func (c *Call) UsageError(detail string) *apron.Error {
	usage := "/" + c.Name
	if c.Form != nil {
		usage = c.Form.Synopsis()
	}
	message := "Usage: " + usage
	if detail != "" {
		message += ", " + detail
	}
	return apron.InvalidParams(message)
}
