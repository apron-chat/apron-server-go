package apron

import (
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

	"github.com/apron-chat/apron-server-go/apron/internal/messages"
	"github.com/apron-chat/apron-server-go/internal/auth/email"
	"github.com/apron-chat/apron-server-go/wire"
	"github.com/apron-chat/apron-server-go/wire/commands"
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
	closed := s.closed
	if !closed {
		s.extensionWork.Add(1)
	}
	s.mu.Unlock()
	if closed {
		return false
	}
	go func() {
		defer s.extensionWork.Done()
		fn()
	}()
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
		err = errors.New("returned a nil *apron.Error as its error")
	}
	h.logger.Error("extension failed", "error", err)
	return internalError()
}

func internalError() *Error {
	return &Error{Code: CodeInternalError, Message: "Something went wrong; try again later"}
}

// Command is a command of one form, which people run by sending /name as a
// `command` request (PROTOCOL.md §4.1): the shorthand for the
// commands.Command that Registrar.Command makes of it, for a command that
// is its name and arguments, and answers with notices and a result. A
// command of more forms, such as /poll open and /poll close, one that is
// Unlisted, or one whose Run replies itself, is a commands.Command, which
// Registrar.Commands adds.
type Command struct {
	// Name is what follows the slash: lowercase letters, digits, and
	// dashes. It may not be a command the server has. Names are
	// case-sensitive (ext:command_list): only /name exactly runs it, and
	// /Name is an unknown command.
	Name string
	// Usage and Help describe the command in /help, such as
	// "/weather <city>" and "show the forecast": its form's Usage and Help
	// (commands.Form). When Usage starts with the slash and Name, then
	// whitespace or nothing, what follows is the command's hint in
	// ext:command_list (apron's extensions/ext_command_list.md), such as
	// "<city>", which follows its suggested convention: <value>,
	// [optional], a|b, repeats..., and @user for a mention, its whitespace
	// collapsed. Any other Usage gives the command no hint, /help shows it
	// as written, and Open logs it. Help is its description: plain text on
	// one line, its whitespace collapsed; without it, the command has none.
	Usage, Help string
	// Uses are the fields of the `command` request, besides body.text,
	// that change what the command does, as ext:command_list lists them:
	// commands.FieldRoom ("room_id") when it acts on the room it runs in
	// (CommandCall.Room), and the arguments it reads, FieldReplyTo,
	// FieldMentions, and FieldEmbeds. Requires are those of Uses that the
	// command fails without; Run checks them itself, but FieldRoom: a
	// command that requires it acts only on a room the client names, and
	// one sent without room_id is refused as invalid_params rather than
	// run in the default room.
	Uses, Requires []string
	// Group says who may use the command, for display in ext:command_list:
	// one of package commands' Group constants, "all", "guest", "member",
	// "room", "role", or "admin", from the widest to the narrowest; ""
	// leaves it out. It changes nothing about who may use it, which is
	// Available's. Open refuses any other value, on purpose, so that a
	// typo does not reach clients.
	Group string
	// Available reports whether u may run the command in r: the form's May
	// (see May). nil makes it available to everyone who can see a room.
	Available func(u User, r Room) bool
	// Run runs the command. Its replies are notices (CommandCall.Notice,
	// Host.RoomNotice) and the effects of what it does; the result, {} for
	// nil, with nil slices and maps in it as [] and {}, follows them. An
	// *Error refuses the command with its code; any other error is logged
	// and refused as an internal error.
	Run func(ctx context.Context, call *CommandCall) (map[string]any, error)
}

// CommandCall is one run of an extension's command, as apron sees it: who
// ran it, where, and with what. A Command's Run is given it; a
// commands.Command's Run gets it from its commands.Call with CallOf.
type CommandCall struct {
	// User ran the command in Room: the room the request names, or the
	// default room for one without room_id, which the user can see.
	User User
	Room Room
	// Args is the command line after the command's name, trimmed: its
	// fixed words too, for a form that has some (commands.Call.Rest is
	// what follows them).
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
// that ran the command, before the command's result, as
// commands.Call.Notice does.
func (c *CommandCall) Notice(text string) {
	c.call.Notice(text)
}

// CallOf is the run of an extension's command that call is, as apron sees
// it, for the Run of a commands.Command that Registrar.Commands added; nil
// for a call apron did not make, such as one a test makes.
func CallOf(call *commands.Call) *CommandCall {
	cc, _ := call.Data.(*CommandCall)
	return cc
}

var commandNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// May makes a check of who may use a command form a commands.Form's May,
// from fn, which reports whether u may use it in r, as Command.Available
// does, with copies of the user and the room. The Router asks it about the
// room a command runs in, to refuse it as denied and to list it in /help;
// and ext:command_list about each room the user can see, the default room
// first, until fn reports one. So fn may be called once for each room the
// user can see on every listing: keep it cheap and free of side effects.
// A user who can see no room may use no command, whatever fn says, and a
// nil fn lets everyone else.
func May(fn func(u User, r Room) bool) func(ctx context.Context, c *wire.Conn, roomID string) bool {
	return func(_ context.Context, wc *wire.Conn, roomID string) bool {
		c, ok := wire.DataOf[*client](wc)
		if !ok {
			return false
		}
		// fn runs outside s.mu, with views of the user and rooms made
		// under it.
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

// Command adds a command of one form.
func (r *Registrar) Command(command Command) {
	r.register("a command", func() error {
		if command.Run == nil {
			return fmt.Errorf("command /%s has no Run", command.Name)
		}
		// The command's one form takes the hint from Usage, collapses Help
		// into one line of plain text in /help's Markdown list, and checks
		// the fields and the group, as package commands does for any
		// backend. Its May asks Available, outside s.mu, and the Router
		// refuses it where it is not available; Run checks the other
		// fields it requires itself, but room_id, which the Router does.
		uses, requires := slices.Clone(command.Uses), slices.Clone(command.Requires)
		err := r.addCommand(commands.Command{
			Name: command.Name,
			Forms: []commands.Form{{
				Usage: command.Usage, Help: command.Help, Group: command.Group,
				Uses: uses, Requires: requires, May: May(command.Available),
			}},
			Run:       func(ctx context.Context, call *commands.Call) error { return r.runCommand(ctx, call, command) },
			RunChecks: commands.CheckForm | commands.CheckRequires,
		})
		if err != nil {
			return err
		}
		// /help shows any Usage that does not start with "/name" and
		// whitespace as written.
		if _, ok := commands.ParseUsage(command.Name, command.Usage); !ok && command.Usage != "" {
			r.logger.Warn("a command's Usage does not start with its name, so ext:command_list gives it no hint", "command", command.Name, "usage", command.Usage)
		}
		return nil
	})
}

// runCommand is a Command's Run: its result, which it renders as an
// object, goes out as a CommandResult, whose Unknown keeps any field of the
// extension's own.
func (h *Host) runCommand(ctx context.Context, call *commands.Call, command Command) error {
	result, err := command.Run(ctx, CallOf(call))
	if err != nil {
		return err
	}
	if result == nil {
		result = map[string]any{}
	}
	encoded, err := encodeResult(result)
	if err != nil {
		return fmt.Errorf("/%s returned a result that is not JSON: %w", command.Name, err)
	}
	var typed wire.CommandResult
	if err := json.Unmarshal(encoded, &typed); err != nil {
		return fmt.Errorf("/%s returned a result that is not an object, or embeds that are not embed writes (§4.8.3): %w", command.Name, err)
	}
	_ = call.Reply.Send(typed)
	return nil
}

// Commands adds commands in package commands' terms, which any wire
// backend's commands are in: forms with fixed words, such as /poll open
// and /poll close, each with what it uses and requires, its group, and who
// may use it (Form.May, which May makes from a check on a User and a
// Room); commands that are Unlisted; and commands whose Run checks some of
// what the Router would itself, with errors of its own (RunChecks).
//
// Names follow Command.Name's. A form without May is for everyone who can
// see a room. Before any command runs, the server checks that its room,
// the default room for a command without room_id, is one the user can see,
// and that its reply_to is a message the user can see; then the Router
// checks the command line as package commands says.
//
// Run replies as package commands says, with call.Notice and call.Done or
// call.Reply, or returns an error: an *Error refuses the command with its
// code, and any other is logged and refused as an internal error. It runs
// outside the server's lock, with call.Params.RoomID set to the command's
// room; CallOf(call) is the run as apron sees it, such as the user and the
// room as extensions see them.
func (r *Registrar) Commands(list ...commands.Command) {
	for _, command := range list {
		r.register("a command", func() error {
			if command.Run == nil {
				return fmt.Errorf("command /%s has no Run", command.Name)
			}
			return r.addCommand(command)
		})
	}
}

// addCommand adds an extension's command to the server's Router, which
// runs it after the core commands' checks (beforeCommand): its Run, with
// the run as apron sees it in call.Data, and its error as the extension's.
// It is called holding s.mu, which the Router's Add does not need.
func (r *Registrar) addCommand(command commands.Command) error {
	if !commandNamePattern.MatchString(command.Name) {
		return fmt.Errorf("command %q: a name is lowercase letters, digits, and dashes", command.Name)
	}
	command.Forms = slices.Clone(command.Forms)
	if len(command.Forms) == 0 {
		command.Forms = []commands.Form{{}}
	}
	for i := range command.Forms {
		if command.Forms[i].May == nil {
			command.Forms[i].May = May(nil)
		}
	}
	run, h := command.Run, r.Host
	command.Run = func(ctx context.Context, call *commands.Call) error {
		c, err := clientOf(call.Conn)
		if err != nil {
			return err
		}
		cc, callErr := c.server.commandCall(c, call)
		if callErr != nil {
			return callErr
		}
		call.Data = cc
		return replyError(h.protocolError(run(ctx, call)))
	}
	return r.s.commands.router.Add(command)
}

// commandCall is the run of an extension's command as apron sees it, in
// the room the Router's Before checked, unless it has gone since.
func (s *Server) commandCall(c *client, call *commands.Call) (*CommandCall, *Error) {
	body := bodyObject(call.Params.Body)
	s.mu.RLock()
	defer s.mu.RUnlock()
	r := s.visibleRoomLocked(c.user, call.Params.RoomID)
	if r == nil {
		return nil, invalidParams("Unknown room %q", call.Params.RoomID)
	}
	return &CommandCall{
		User: userViewOf(c.user), Room: s.roomViewLocked(r), Args: call.Args, ReplyTo: replyIDOf(call.Params),
		Mentions: mentions(body), Body: cloneObject(body), call: call,
	}, nil
}

// MethodCall is one request to an extension's method.
type MethodCall struct {
	// User sent the request.
	User User
	// Params are the request's params, raw.
	Params map[string]jsontext.Value
}

// decodeOptions match JSON names to Go fields ignoring case, as
// encoding/json does, for the values extensions decode.
var decodeOptions = json.MatchCaseInsensitiveNames(true)

// Decode decodes the params into a struct, as with encoding/json, or
// returns a CodeInvalidParams error.
func (m *MethodCall) Decode(into any) error {
	if err := json.Unmarshal(encodeJSON(m.Params), into, decodeOptions); err != nil {
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
		h.s.ops[name] = func(s *Server, c *client, r *wire.Request) *Error {
			s.mu.RLock()
			user := userViewOf(c.user)
			s.mu.RUnlock()
			result, err := fn(s.requestContext(c), &MethodCall{User: user, Params: r.Fields()})
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
			r.Reply(jsontext.Value(encoded))
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
		if encoded, err = wire.Marshal(value); err != nil {
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
		if encoded, err = wire.Marshal(value); err != nil {
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
	encoded, err := wire.Marshal(d.Body)
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
	raw, err := wire.Marshal(value)
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
