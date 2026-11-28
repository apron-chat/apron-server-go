package aprond

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/server"
	"github.com/apron-chat/apron-server-go/apron/server/commands"
	"github.com/apron-chat/apron-server-go/aprond/internal/messages"
	"github.com/apron-chat/apron-server-go/internal/auth/email"
)

// Extension adds features to a Server without forking it: commands, request
// methods, HTTP routes, capabilities, message filters, bots, sign-in, and
// reactions to what is logged. Put extensions in Config.Extensions; Open
// calls Init on each, in order, once the stored state is restored and before
// the server serves anything, and fails if one returns an error.
//
// Init registers the extension's features with its Registrar, which works
// only during Init. The Registrar's Host is the extension's handle on the
// server for as long as it runs: keep it, not the Registrar.
//
// Extension code never runs while the server holds its lock, so a callback
// may call any Host, Bot, or Session method. A callback that runs for a
// request, such as a command, method, or filter, delays that request's
// connection until it returns, and gets a context that is canceled when the
// connection closes; long work belongs in Host.Go.
//
// An extension with settings has a method Flags() any that returns a
// pointer to them: a struct of options tagged as aproncmd reads them
// (aproncmd.Configurable), such as `long:"days" default:"3"`. aproncmd sets
// each from the command line or the config file, zero too, or else to its
// default; when a program or a test sets them in Go, Open gives each that
// is zero its default. So a setting whose zero is a choice of its own,
// such as a limit where 0 means none, is a pointer, *int, which only nil
// leaves to the default.
type Extension interface {
	// Name is the extension's name: lowercase letters, digits, and dashes,
	// starting with a letter, such as "weather". It names the extension's
	// capability (ext:weather), its data in ext (EXTENSIONS.md), its
	// request methods (weather.forecast), and the values it keeps.
	Name() string
	// Init registers the extension's features.
	Init(r *Registrar) error
}

var extensionNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// reservedExtensionNames are the extensions the server implements itself.
var reservedExtensionNames = []string{"emoji", "settings", accountExt}

// Roles the server gives meaning to (§3.3.4). Config.Roles grants them, and
// so may external sign-in; extensions may define others.
const (
	// RoleAdmin may set the server's profile with /server, replace and
	// remove any custom emoji, and remove people from rooms.
	RoleAdmin = "admin"
	// RoleModerator may remove people from rooms.
	RoleModerator = "moderator"
	// RoleBot marks an extension's bot (Registrar.Bot).
	RoleBot = "bot"
)

// Host is an extension's handle on its server, valid for as long as the
// server runs.
type Host struct {
	s      *Server
	name   string
	logger *slog.Logger
}

// Registrar registers an extension's features during its Init. Its methods
// panic once Init has returned; keep its Host instead, which it embeds.
type Registrar struct {
	*Host
	// errs collects registration errors, which Open returns.
	errs      []error
	done      atomic.Bool
	following bool
}

// initExtensions runs each extension's Init, in order.
func (s *Server) initExtensions(extensions []Extension) error {
	names := make(map[string]bool)
	for _, extension := range extensions {
		name := extension.Name()
		switch {
		case !extensionNamePattern.MatchString(name):
			return fmt.Errorf("extension %q: a name is lowercase letters, digits, and dashes, starting with a letter", name)
		case slices.Contains(reservedExtensionNames, name):
			return fmt.Errorf("extension %q: the server implements ext:%s itself", name, name)
		case names[name]:
			return fmt.Errorf("extension %q is added twice", name)
		}
		names[name] = true
		r := &Registrar{Host: &Host{s: s, name: name, logger: s.logger.With("extension", name)}}
		err := extension.Init(r)
		r.done.Store(true)
		if err = errors.Join(append(r.errs, err)...); err != nil {
			return fmt.Errorf("extension %s: %w", name, err)
		}
	}
	return nil
}

// register runs a registration under s.mu, refusing one after Init.
func (r *Registrar) register(what string, add func() error) {
	if r.done.Load() {
		panic(fmt.Sprintf("apron: extension %s registered %s after Init", r.name, what))
	}
	s := r.s
	s.mu.Lock()
	err := add()
	s.unlock()
	if err != nil {
		r.errs = append(r.errs, err)
	}
}

// Name is the extension's name.
func (h *Host) Name() string { return h.name }

// Logger logs with the extension's name.
func (h *Host) Logger() *slog.Logger { return h.logger }

// Context is canceled when the server shuts down.
func (h *Host) Context() context.Context { return h.s.extContext }

// Go runs fn on its own goroutine with a context that Shutdown cancels.
// Shutdown waits for fn to return before it closes the store, so fn may
// keep values as it stops. Once the server is shutting down, Go runs
// nothing and reports false.
func (h *Host) Go(fn func(ctx context.Context)) bool {
	return h.s.goExtension(func() { fn(h.s.extContext) })
}

// goExtension runs fn as extension work, which Shutdown waits for, unless
// the server is closed.
func (s *Server) goExtension(fn func()) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.extensionWork.Go(fn)
	return true
}

// requestContext is the context of the request c is processing, which
// callbacks run for it get: canceled when the connection closes.
func (s *Server) requestContext(c *client) context.Context {
	if c.request != nil {
		return c.request
	}
	return s.extContext
}

// protocolError turns an extension's error into a reply: an *Error as it
// is, and anything else, logged, as an internal error.
func (h *Host) protocolError(err error) *Error {
	if err == nil {
		return nil
	}
	if e, ok := errors.AsType[*Error](err); ok {
		if e != nil {
			return e
		}
		err = errors.New("returned a nil *aprond.Error as its error")
	}
	h.logger.Error("extension failed", "error", err)
	return internalError()
}

func internalError() *Error {
	return &Error{Code: CodeInternalError, Message: "Something went wrong; try again later"}
}

// Command is a command, which people run by sending /Name as a `command`
// request (PROTOCOL.md §4.1), such as /weather <city>: the forms that /help
// and ext:command_list (apron's extensions/ext_command_list.md) show of it,
// each with who may use it, and how each runs. Most commands have one form,
// which the Command's own fields describe. A command of more, such as
// /slowmode <seconds> and /slowmode off, lists them in Forms, each with a
// Run of its own, so that adding a form changes nothing about the others.
type Command struct {
	// Name is what follows the slash: lowercase letters, digits, and
	// dashes. It may not be a command the server has. Names are
	// case-sensitive (ext:command_list): only /name exactly runs it, and
	// /Name is an unknown command.
	Name string
	// Usage, Help, Uses, Requires, Group, Available, and Run describe
	// the command's one form, as Form's do. A command with Forms leaves
	// Usage, Help, and Run to its forms, and its Uses and Requires, Group,
	// and Available are those of each form that sets none of its own:
	// what all its forms share, such as that moderators may use them in
	// the room they run in.
	Usage, Help    string
	Uses, Requires []string
	Group          string
	Available      func(u User, r Room) bool
	Run            func(ctx context.Context, call *CommandCall) (map[string]any, error)
	// Forms are the command's forms, in the order /help shows them, for a
	// command of more than one: one for each fixed word that may follow
	// the name, such as "slowmode off", and one for the name alone, if
	// it runs alone or with arguments, such as /slowmode <seconds>. A
	// command line runs the form with the most fixed words that start its
	// arguments; one that selects no form, such as /poll alone of
	// /poll open and /poll close, is refused as invalid_params, with the
	// forms' usage.
	Forms []Form
	// Unlisted keeps the command out of /help and ext:command_list, while
	// it runs as any other: an alias, or a command people are not meant to
	// find.
	Unlisted bool
}

// Form is one form of a command (Command.Forms): the command's name and
// any fixed words after it, what /help and ext:command_list show of it, who
// may use it, and how it runs.
type Form struct {
	// Name is the words people type after the slash: the command's name,
	// then any fixed word, such as "slowmode off", each one space from the
	// next. "" is the command's name alone.
	Name string
	// Usage and Help describe the form in /help, such as
	// "/weather <city>" and "show the forecast". When Usage starts with
	// the slash and the form's name, then whitespace or nothing, what
	// follows is the form's hint in ext:command_list, such as "<city>",
	// which follows its suggested convention: <value>, [optional], a|b,
	// repeats..., and @user for a mention, its whitespace collapsed. ""
	// is the slash and the name alone. Any other Usage gives the form no
	// hint, /help shows it as written, and Open logs it. Help is its
	// description: plain text on one line, its whitespace collapsed;
	// without it, the form has none.
	Usage, Help string
	// Uses are the fields of the `command` request, besides body.text,
	// that change what the form does, as ext:command_list lists them:
	// FieldRoom when it acts on the room it runs in (CommandCall.Room),
	// and the arguments it reads, FieldReplyTo, FieldMentions, and
	// FieldEmbeds. Requires are those of Uses that the form fails
	// without; Run checks them itself, but FieldRoom: a form that requires
	// it acts only on a room the client names, and one sent without
	// room_id is refused as invalid_params rather than run in the default
	// room.
	Uses, Requires []string
	// Group says who may use the form, for display in ext:command_list:
	// one of the Group constants, or "" to leave it out. It changes
	// nothing about who may use it, which is Available's. Open refuses any
	// other value, on purpose, so that a typo does not reach clients.
	Group string
	// Available reports whether u may use the form in r. The server asks
	// it about the room a command runs in, to refuse the command as denied
	// where it is not, and to list the form in /help; and
	// ext:command_list about each room the user can see, the default room
	// first, until it reports one. So it may be called once for each room
	// the user can see on every listing: keep it cheap and free of side
	// effects. A user who can see no room may use no command; nil makes
	// the form available to everyone else.
	Available func(u User, r Room) bool
	// Run runs the form. Its replies are notices (CommandCall.Notice,
	// Host.RoomNotice) and the effects of what it does; the result, {} for
	// nil, with nil slices and maps in it as [] and {}, follows them. An
	// *Error, such as CommandCall.UsageError's, refuses the command with
	// its code; any other error is logged and refused as an internal
	// error.
	Run func(ctx context.Context, call *CommandCall) (map[string]any, error)
}

// The fields of a `command` request, besides body.text, that a command
// form may use or require (Form.Uses): room_id, when it acts on the room it
// runs in, and the arguments of §4.1.
const (
	FieldRoom     = "room_id"
	FieldReplyTo  = "reply_to"
	FieldMentions = "body.mentions"
	FieldEmbeds   = "body.embeds"
)

// The groups a command form may name (Form.Group), from the widest to the
// narrowest: who may use it, for display in ext:command_list only.
const (
	GroupAll    = "all"
	GroupGuest  = "guest"
	GroupMember = "member"
	GroupRoom   = "room"
	GroupRole   = "role"
	GroupAdmin  = "admin"
)

// CommandCall is one run of an extension's command: who ran it, where, and
// with what.
type CommandCall struct {
	// User ran the command in Room: the room the request names, or the
	// default room for one without room_id, which the user can see.
	User User
	Room Room
	// Args is the command line after the form's name, trimmed, such as
	// "30" of "/slowmode 30", and "" of "/slowmode off".
	Args string
	// ReplyTo is the message_id the command replied to, if any.
	ReplyTo string
	// Mentions are the user_ids the command line mentions (§3.5.5).
	Mentions []string
	// Body is a copy of the command's message body.
	Body map[string]any

	call *commands.Call
}

// Notice sends a ~private notice (Appendix A.1), Markdown, to the connection
// that ran the command, before the command's result.
func (c *CommandCall) Notice(text string) {
	c.call.Notice(text)
}

// UsageError is the error of a command line the form cannot run, for Run
// to return: invalid_params, "Usage: " and the form as /help shows it, then
// detail, if any, such as "Usage: /slowmode <seconds>, at most 3600
// seconds".
func (c *CommandCall) UsageError(detail string) *Error {
	return c.call.UsageError(detail)
}

var commandNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Command adds a command.
func (r *Registrar) Command(command Command) {
	r.register("a command", func() error {
		if !commandNamePattern.MatchString(command.Name) {
			return fmt.Errorf("command %q: a name is lowercase letters, digits, and dashes", command.Name)
		}
		forms, err := command.forms()
		if err != nil {
			return err
		}
		// Each form takes its hint from Usage, and its Help is collapsed
		// into one line of plain text in /help's Markdown list; the Router
		// checks the fields and the group, as package commands does for
		// any backend. A form's May asks Available, outside s.mu, and the
		// Router refuses the command where it is not available, and a
		// command line that selects no form; Run checks the other fields
		// it requires itself, but room_id, which the Router does. The
		// Router's command runs the Run of the form the line selects.
		runs := make(map[string]func(context.Context, *CommandCall) (map[string]any, error), len(forms))
		listed := make([]commands.Form, len(forms))
		for i, f := range forms {
			name := cmp.Or(f.Name, command.Name)
			if f.Run == nil {
				if len(command.Forms) == 0 {
					return fmt.Errorf("command /%s has no Run", command.Name)
				}
				return fmt.Errorf("command /%s: form %q has no Run", command.Name, name)
			}
			runs[name] = f.Run
			listed[i] = commands.Form{
				Name: f.Name, Usage: f.Usage, Help: f.Help, Group: f.Group,
				Uses: slices.Clone(f.Uses), Requires: slices.Clone(f.Requires), May: available(f.Available),
			}
		}
		h := r.Host
		err = r.s.commands.router.Add(commands.Command{
			Name: command.Name, Forms: listed, Unlisted: command.Unlisted,
			Run: func(ctx context.Context, call *commands.Call) error {
				return h.runCommand(ctx, call, runs[call.Form.Name])
			},
			SkipChecks: commands.CheckRequires,
		})
		if err != nil {
			return err
		}
		// /help shows any Usage that does not start with "/name" and
		// whitespace as written.
		for _, f := range forms {
			name := cmp.Or(f.Name, command.Name)
			if _, ok := commands.ParseUsage(name, f.Usage); !ok && f.Usage != "" {
				r.logger.Warn("a command's Usage does not start with its name, so ext:command_list gives it no hint", "command", name, "usage", f.Usage)
			}
		}
		return nil
	})
}

// forms are the command's forms: its Forms, each with the command's Uses
// and Requires, Group, and Available where it sets none of its own, or the
// one form that the command's own fields describe.
func (c Command) forms() ([]Form, error) {
	if len(c.Forms) == 0 {
		return []Form{{
			Usage: c.Usage, Help: c.Help, Uses: c.Uses, Requires: c.Requires,
			Group: c.Group, Available: c.Available, Run: c.Run,
		}}, nil
	}
	if c.Usage != "" || c.Help != "" || c.Run != nil {
		return nil, fmt.Errorf("command /%s has Forms, which have its Usage, Help, and Run", c.Name)
	}
	forms := slices.Clone(c.Forms)
	for i := range forms {
		f := &forms[i]
		if f.Uses == nil && f.Requires == nil {
			f.Uses, f.Requires = c.Uses, c.Requires
		}
		f.Group = cmp.Or(f.Group, c.Group)
		if f.Available == nil {
			f.Available = c.Available
		}
	}
	return forms, nil
}

// available makes a form's Available a commands.Form's May, which reports
// whether the connection's user may use the form in the room roomID, or,
// for "", in some room it can see: fn runs outside s.mu, with copies of the
// user and the rooms made under it.
func available(fn func(u User, r Room) bool) func(ctx context.Context, c *server.Conn, roomID string) bool {
	return func(_ context.Context, wc *server.Conn, roomID string) bool {
		c, ok := wc.Data().(*client)
		if !ok {
			return false
		}
		s := c.server
		s.mu.RLock()
		user := userViewOf(c.user)
		var rooms []Room
		switch {
		case roomID != "":
			rooms = []Room{s.roomViewLocked(s.rooms.Get(roomID))}
		case fn != nil:
			rooms = s.visibleRoomsLocked(c.user)
		case !s.seesARoomLocked(c.user):
			// A user who sees no room can run no command: each runs in a
			// room.
			s.mu.RUnlock()
			return false
		}
		s.mu.RUnlock()
		if fn == nil {
			return true
		}
		return slices.ContainsFunc(rooms, func(r Room) bool { return fn(user, r) })
	}
}

// runCommand runs a form of an extension's command, after the core
// commands' checks (beforeCommand), in the room they checked, unless it has
// gone since: its result, which it renders as an object, goes out as a
// CommandResult, whose Unknown keeps any field of the extension's own, and
// its error as the extension's.
func (h *Host) runCommand(ctx context.Context, call *commands.Call, run func(context.Context, *CommandCall) (map[string]any, error)) error {
	c, err := clientOf(call.Conn)
	if err != nil {
		return err
	}
	cc, callErr := c.server.commandCall(c, call)
	if callErr != nil {
		return callErr
	}
	var typed apron.CommandResult
	result, runErr := run(ctx, cc)
	if runErr == nil {
		typed, runErr = commandResult(call.Form.Name, result)
	}
	if runErr != nil {
		return replyError(h.protocolError(runErr))
	}
	_ = call.Reply.Send(typed)
	return nil
}

// commandResult is the result of the form name's Run as a CommandResult,
// or the error of one that is none.
func commandResult(name string, result map[string]any) (apron.CommandResult, error) {
	var typed apron.CommandResult
	if result == nil {
		result = map[string]any{}
	}
	encoded, err := encodeResult(result)
	if err != nil {
		return typed, fmt.Errorf("/%s returned a result that is not JSON: %w", name, err)
	}
	if err := json.Unmarshal(encoded, &typed); err != nil {
		return typed, fmt.Errorf("/%s returned a result that is not an object, or embeds that are not embed writes (§4.8.3): %w", name, err)
	}
	return typed, nil
}

// commandCall is the run of an extension's command as aprond sees it, in
// the room the Router's Before checked, unless it has gone since.
func (s *Server) commandCall(c *client, call *commands.Call) (*CommandCall, *Error) {
	body := bodyObject(&call.Params.Body)
	s.mu.RLock()
	defer s.mu.RUnlock()
	r := s.visibleRoomLocked(c.user, call.Params.RoomID)
	if r == nil {
		return nil, invalidParams("Unknown room %q", call.Params.RoomID)
	}
	return &CommandCall{
		User: userViewOf(c.user), Room: s.roomViewLocked(r), Args: call.Rest(), ReplyTo: replyIDOf(call.Params),
		Mentions: mentions(body), Body: cloneObject(body), call: call,
	}, nil
}

// MethodCall is one request to an extension's method.
type MethodCall struct {
	// User sent the request.
	User User
	// Params are the request's params object, raw, as it was sent ({}
	// when it had none): the call's own copy, which the extension may
	// keep.
	Params jsontext.Value
}

// decodeOptions match JSON names to Go fields ignoring case, as
// encoding/json does, for the values extensions decode.
var decodeOptions = json.MatchCaseInsensitiveNames(true)

// Decode decodes the params into a struct, as with encoding/json, or
// returns a CodeInvalidParams error.
func (m *MethodCall) Decode(into any) error {
	if err := json.Unmarshal(m.Params, into, decodeOptions); err != nil {
		return InvalidParams("Invalid params: " + err.Error())
	}
	return nil
}

// MethodFunc answers a request to an extension's method. Its result is
// encoded as the reply's result, {} for nil, with nil slices and maps in it
// as [] and {}, not null; an *Error refuses the request with its code, and
// any other error, or a result that does not encode as JSON, is logged and
// refused as an internal error.
type MethodFunc func(ctx context.Context, call *MethodCall) (any, error)

// Method adds a request method for signed-in users. Its name starts with
// the extension's name and a dot, such as "search.query", so that
// extensions never take each other's or the protocol's methods. Clients
// learn of it from the extension's capability (Advertise).
func (r *Registrar) Method(name string, fn MethodFunc) {
	h := r.Host
	r.register("a method", func() error {
		suffix, ok := strings.CutPrefix(name, h.name+".")
		switch {
		case !ok || suffix == "" || strings.ContainsFunc(suffix, func(r rune) bool { return r <= ' ' }):
			return fmt.Errorf("method %q: a method's name is %s.<name>", name, h.name)
		case fn == nil:
			return fmt.Errorf("method %q has no function", name)
		case h.s.ops[name] != nil:
			return fmt.Errorf("method %q exists already", name)
		}
		h.s.ops[name] = func(s *Server, c *client, r *server.Request, reply server.Reply[any]) *Error {
			s.mu.RLock()
			user := userViewOf(c.user)
			s.mu.RUnlock()
			result, err := fn(s.requestContext(c), &MethodCall{User: user, Params: slices.Clone(r.Params())})
			if err != nil {
				return h.protocolError(err)
			}
			if result == nil {
				result = map[string]any{}
			}
			// The result is encoded now: one that is not JSON is the
			// extension's error, not the connection's, and the reply that
			// deduplication keeps (§1.2) cannot change afterwards.
			encoded, encodeErr := encodeResult(result)
			if encodeErr != nil {
				return h.protocolError(fmt.Errorf("method %s returned a result that is not JSON: %w", name, encodeErr))
			}
			_ = reply.Send(jsontext.Value(encoded))
			return nil
		}
		h.s.registerOp(name, h.s.ops[name])
		return nil
	})
}

// Advertise lists the extension's capability, ext:<name>, in the server
// frame (PROTOCOL.md §3.1), with value, encoded now, under its name in the
// frame's ext unless value is nil. Called after Init, it sends every
// connection a new server frame. A value that does not encode as JSON is
// an error, and changes nothing.
func (h *Host) Advertise(value any) error {
	var encoded jsontext.Value
	if value != nil {
		var err error
		if encoded, err = apron.Marshal(value); err != nil {
			return fmt.Errorf("apron: advertised value: %w", err)
		}
	}
	s := h.s
	s.mu.Lock()
	defer s.unlock()
	s.advertised[h.name] = encoded
	s.announceServerLocked()
	return nil
}

// route is an HTTP route an extension serves.
type route struct {
	pattern string
	handler http.Handler
}

// Handle serves handler at pattern, as http.ServeMux patterns go, beside the
// server's own routes, such as "/sso/callback" or "POST /hooks/github". A
// pattern that conflicts with another, the server's included, fails Open.
func (r *Registrar) Handle(pattern string, handler http.Handler) {
	r.register("a route", func() error {
		if handler == nil {
			return fmt.Errorf("route %q has no handler", pattern)
		}
		routes := append(slices.Clone(r.s.routes), route{pattern, handler})
		if _, err := r.s.buildHandler(routes); err != nil {
			return err
		}
		r.s.routes = routes
		return nil
	})
}

// Draft is a message as a `message` request saves it: a new message, or an
// edit (MessageID set) or a move (RoomID changed) of one, or the opening
// text of an /embed-append. Deletions are no drafts.
type Draft struct {
	// From is the author.
	From User
	// RoomID is the room the message is saved to, MessageID the message an
	// edit saves, and ReplyTo the message it replies to.
	RoomID, MessageID, ReplyTo string
	// Body is the message's body (§3.5), which a filter may change. The
	// server checks it again after the filters: one that is no longer a
	// valid body refuses the message.
	Body map[string]any

	// ext is what the request writes to the message's ext (§4.12), and
	// filter the extension whose filter runs.
	ext    extObject
	filter string
	// shared is set when the filters ran on a save of the text that the
	// sender shares with its saver (ext:editors).
	shared bool
}

// Text is the body's text.
func (d *Draft) Text() string {
	text, _ := d.Body["text"].(string)
	return text
}

// SetText replaces the body's text.
func (d *Draft) SetText(text string) {
	if d.Body == nil {
		d.Body = make(map[string]any)
	}
	d.Body["text"] = text
}

// Ext is what the request writes to the message's ext under name (§4.12),
// nil for nothing.
func (d *Draft) Ext(name string) jsontext.Value {
	return slices.Clone(d.ext[name])
}

// SetExt writes value under the filtering extension's name in the
// message's ext, encoded now; nil clears it. A value that does not encode as
// JSON is an error, and changes nothing.
func (d *Draft) SetExt(value any) error {
	encoded := jsontext.Value(`{}`) // an empty value clears the key (§4.12)
	if value != nil {
		var err error
		if encoded, err = apron.Marshal(value); err != nil {
			return fmt.Errorf("apron: ext value: %w", err)
		}
	}
	if d.ext == nil {
		d.ext = make(extObject)
	}
	d.ext[d.filter] = encoded
	return nil
}

type extensionFilter struct {
	host *Host
	fn   func(ctx context.Context, d *Draft) error
}

// FilterMessage checks each message people and bots save before it is
// saved, and the opening text of each /embed-append: fn may change d, or
// refuse the message by returning an error, an *Error with its code, or any
// other as an internal error. Filters run in the order they were added, on
// messages to rooms their authors may see. The server checks the message
// after them, so a message a filter passes may still be refused: a filter
// that counts messages counts them from OnEvent instead. The sender waits
// for filters, so they need to be quick.
//
// What filters do not see: the text that live text (§4.8.3) appends, a
// terminal's output and title, uploads, reactions, room titles, and
// profiles.
func (r *Registrar) FilterMessage(fn func(ctx context.Context, d *Draft) error) {
	r.register("a filter", func() error {
		if fn == nil {
			return errors.New("a filter needs a function")
		}
		r.s.filters = append(r.s.filters, extensionFilter{r.Host, fn})
		return nil
	})
}

// filterMessage runs the filters on a message c saves, outside s.mu, and
// checks what they leave: the body must still be one, and is normalized to
// decoded JSON. A message to a room its author may not see, or an edit of
// a message that is not theirs and does not share its text with them
// (ext:editors), is left to the server to refuse, unfiltered. A save of
// shared text is filtered as it will be saved: in the message's room, with
// its reply and the rest of its body, and only the request's text.
func (s *Server) filterMessage(c *client, d *Draft) *Error {
	s.mu.RLock()
	filters := s.filters
	u := c.user
	d.From = userViewOf(u)
	allowed := s.visibleRoomLocked(u, d.RoomID) != nil
	if allowed && d.MessageID != "" {
		m := s.visibleMessageLocked(u, d.MessageID)
		switch {
		case m == nil:
			allowed = false
		case m.Owner() == u.id:
		case sharesText(m.Current(), u.id):
			snapshot := m.Current()
			d.RoomID, d.ReplyTo, d.ext = m.RoomID(), snapshot.ReplyID(), nil
			d.Body = sharedTextBody(snapshot, d.Body)
			d.shared = len(filters) > 0
		default:
			allowed = false
		}
	}
	s.mu.RUnlock()
	if len(filters) == 0 || !allowed {
		return nil
	}
	ctx := s.requestContext(c)
	for _, filter := range filters {
		d.filter = filter.host.name
		if err := filter.fn(ctx, d); err != nil {
			return filter.host.protocolError(err)
		}
	}
	encoded, err := apron.Marshal(d.Body)
	var body map[string]any
	if err != nil || json.Unmarshal(encoded, &body) != nil || body == nil {
		return internalError()
	}
	d.Body = body
	return messages.CheckRaw(encoded)
}

// User is a user as extensions see it: a copy, which does not change.
type User struct {
	// ID is the user_id, and Name the display name.
	ID   string
	Name string
	// Roles are the server roles the user holds (§3.3.4), sorted.
	Roles []string
	// Account is false for a guest.
	Account bool
	// Bot marks an extension's bot (Registrar.Bot).
	Bot bool
	// Email is the account's sign-in address, if any.
	Email string
	// Created is when the user was made, zero if not known, as for
	// accounts made before the server kept it.
	Created time.Time
}

// HasRole reports whether the user holds role, such as RoleModerator.
func (u User) HasRole(role string) bool { return slices.Contains(u.Roles, role) }

func userViewOf(u *userState) User {
	if u == nil {
		return User{}
	}
	return User{
		ID: u.id, Name: u.Name, Roles: slices.Clone(u.roles), Account: u.Account(), Bot: u.Bot != "",
		Email: u.Identities[email.Name], Created: u.Created,
	}
}

// Room is a room as extensions see it: a copy, which does not change.
type Room struct {
	// ID is the room_id, and Title the title clients show.
	ID    string
	Title string
	// ParentID is the room a thread belongs to, if any.
	ParentID string
	// Restricted reports a room that only some users may see: a private
	// room (§4.2.4), or a thread of one. Whether a given user may see it,
	// or anyone may, is Host.Visible's and Host.Public's to say.
	Restricted bool
	// Members counts its members.
	Members int
}

func (s *Server) roomViewLocked(r *roomState) Room {
	if r == nil {
		return Room{}
	}
	// Restricted as audience() would say, without copying its members:
	// a private room on the way up makes the room one some users see.
	restricted := false
	for room := r; room != nil && !restricted; room = room.Parent() {
		restricted = room.Private()
	}
	room := Room{ID: r.ID(), Title: r.Title(), Restricted: restricted, Members: r.MemberCount()}
	if r.Parent() != nil {
		room.ParentID = r.Parent().ID()
	}
	return room
}

// User returns the user with this user_id, if there is one.
func (h *Host) User(id string) (User, bool) {
	h.s.mu.RLock()
	defer h.s.mu.RUnlock()
	u := h.s.users.Get(id)
	return userViewOf(u), u != nil
}

// Room returns the room with this room_id, if there is one.
func (h *Host) Room(id string) (Room, bool) {
	h.s.mu.RLock()
	defer h.s.mu.RUnlock()
	r := h.s.rooms.Get(id)
	return h.s.roomViewLocked(r), r != nil
}

// Rooms lists every room, threads and private rooms included, oldest
// first.
func (h *Host) Rooms() []Room {
	h.s.mu.RLock()
	defer h.s.mu.RUnlock()
	rooms := h.s.rooms.All()
	views := make([]Room, len(rooms))
	for i, r := range rooms {
		views[i] = h.s.roomViewLocked(r)
	}
	return views
}

// Visible reports whether u may see the room and its messages: a room that
// is not restricted, or whose private rooms all have u as a member. An
// extension that shows messages to people checks it, so that it never
// shows anyone a private room's.
func (h *Host) Visible(u User, roomID string) bool {
	h.s.mu.RLock()
	defer h.s.mu.RUnlock()
	r, user := h.s.rooms.Get(roomID), h.s.users.Get(u.ID)
	return r != nil && user != nil && r.VisibleTo(user)
}

// HistoryLogID is the room's history_log_id (§4.3.3): where its retained
// history starts, its creation unless history retention discarded the
// start of it (Config.HistoryRecords, HistoryMaxAge), and false when there
// is no such room. An extension that keeps messages of its own, such as an
// index of them, lets go of those of a room before it, which history no
// longer has: a message whose LogID, its latest snapshot's, is before it.
func (h *Host) HistoryLogID(roomID string) (int64, bool) {
	h.s.mu.RLock()
	defer h.s.mu.RUnlock()
	r := h.s.rooms.Get(roomID)
	if r == nil {
		return 0, false
	}
	return r.Log().HistoryID(), true
}

// Public reports whether anyone may see the room, without an account: it
// is not restricted, and the server lets guests in (Config.DisableGuests,
// Registrar.RestrictSignIn), so that whoever is not signed in could see it
// by signing in as a guest.
// An extension that shows messages to people who are not signed in, such as
// on the web, shows only those of public rooms.
func (h *Host) Public(roomID string) bool {
	h.s.mu.RLock()
	defer h.s.mu.RUnlock()
	r := h.s.rooms.Get(roomID)
	if r == nil || !h.s.guestsAllowedLocked() {
		return false
	}
	everyone, _ := r.Audience()
	return everyone
}

// RoomNotice posts a ~room notice (Appendix A.1), logged, to the room's
// members.
func (h *Host) RoomNotice(roomID, text string) error {
	s := h.s
	s.mu.Lock()
	defer s.unlock()
	r := s.rooms.Get(roomID)
	if r == nil {
		return invalidParams("Unknown room %q", roomID)
	}
	s.postRoomNoticeLocked(r, text)
	return nil
}

// ErrServerClosed is returned by what an extension asks of a server that
// shut down.
var ErrServerClosed = errors.New("apron: the server is shut down")

// extEntryPrefix starts the store's entry kinds of the values extensions
// keep: ext/<name>.
const extEntryPrefix = "ext/"

// reservedKeyPrefix starts the KV keys the server keeps for an extension,
// such as Follow's cursor.
const reservedKeyPrefix = "apron:"

// maxKVKeyBytes bounds a key an extension keeps a value under.
const maxKVKeyBytes = 512

// KV keeps an extension's values across restarts, by key, in the server's
// store, written in order with the server's own state. Values are encoded
// as JSON. It suits settings and cursors, not bulk data. Keys starting with
// "apron:" are the server's.
type KV struct{ h *Host }

// Store is the extension's KV.
func (h *Host) Store() KV { return KV{h} }

func (kv KV) kind() string { return extEntryPrefix + kv.h.name }

// Get decodes the value kept under key into into, and reports whether
// there was one.
func (kv KV) Get(key string, into any) (bool, error) {
	s := kv.h.s
	s.mu.RLock()
	raw, ok := s.extData.Get(kv.kind(), key)
	s.mu.RUnlock()
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(raw, into, decodeOptions)
}

// Put keeps value under key. Once the server's store is closed, it is
// ErrServerClosed.
func (kv KV) Put(key string, value any) error {
	if err := checkKey(key); err != nil {
		return err
	}
	return kv.put(key, value)
}

func checkKey(key string) error {
	switch {
	case key == "" || len(key) > maxKVKeyBytes:
		return fmt.Errorf("apron: a key is 1 to %d bytes", maxKVKeyBytes)
	case strings.HasPrefix(key, reservedKeyPrefix):
		return fmt.Errorf("apron: keys starting with %q are the server's", reservedKeyPrefix)
	}
	return nil
}

func (kv KV) put(key string, value any) error {
	raw, err := apron.Marshal(value)
	if err != nil {
		return err
	}
	s := kv.h.s
	s.mu.Lock()
	defer s.unlock()
	if s.store.Stopped() {
		return ErrServerClosed
	}
	s.extData.Put(kv.kind(), key, raw)
	return nil
}

// Delete forgets the value under key. Once the server's store is closed, it
// is ErrServerClosed.
func (kv KV) Delete(key string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	s := kv.h.s
	s.mu.Lock()
	defer s.unlock()
	if s.store.Stopped() {
		return ErrServerClosed
	}
	s.extData.Delete(kv.kind(), key)
	return nil
}

// Keys lists the extension's keys that start with prefix, sorted.
func (kv KV) Keys(prefix string) []string {
	s := kv.h.s
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.DeleteFunc(s.extData.Keys(kv.kind(), prefix), func(key string) bool {
		return strings.HasPrefix(key, reservedKeyPrefix)
	})
}
