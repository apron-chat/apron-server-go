package apron

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Extension adds features to a Server without forking it: commands, request
// methods, HTTP routes, capabilities, message filters, bots, and reactions
// to what is logged. Put extensions in Config.Extensions; Open calls Init on
// each, in order, once the stored state is restored and before the server
// serves anything, and fails if one returns an error.
//
// An extension registers everything through the Host it is given, during
// Init. Its code never runs while the server holds its lock, so it may call
// back into the Host and its Sessions from any callback; callbacks that are
// called once per request, such as filters and commands, delay that
// request until they return, so long work belongs in Host.Go.
type Extension interface {
	// Name is the extension's name: lowercase letters, digits, and dashes,
	// starting with a letter, such as "weather". It names the extension's
	// capability (ext:weather), its data in ext (EXTENSIONS.md), its
	// request methods (weather.forecast), and the values it keeps.
	Name() string
	// Init registers the extension's features with h.
	Init(h *Host) error
}

var extensionNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// reservedExtensionNames are the extensions the server implements itself.
var reservedExtensionNames = []string{"emoji", "settings"}

// Host is an extension's handle on its Server.
type Host struct {
	s    *Server
	name string
	// errs collects registration errors during Init, which Open returns.
	errs []error
	// sealed is set once Init returns: later registrations panic.
	sealed bool
	logger *slog.Logger
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
		h := &Host{s: s, name: name, logger: s.logger.With("extension", name)}
		s.mu.Lock()
		s.hosts = append(s.hosts, h)
		s.unlock()
		err := extension.Init(h)
		h.sealed = true
		if err = errors.Join(append(h.errs, err)...); err != nil {
			return fmt.Errorf("extension %s: %w", name, err)
		}
	}
	return nil
}

// register runs a registration under s.mu, refusing one after Init.
func (h *Host) register(what string, add func() error) {
	if h.sealed {
		panic(fmt.Sprintf("apron: extension %s registered %s after Init", h.name, what))
	}
	h.s.mu.Lock()
	err := add()
	h.s.unlock()
	if err != nil {
		h.errs = append(h.errs, err)
	}
}

// Name is the extension's name.
func (h *Host) Name() string { return h.name }

// Logger logs with the extension's name.
func (h *Host) Logger() *slog.Logger { return h.logger }

// Go runs fn on its own goroutine with a context that Shutdown cancels.
// Shutdown waits for fn to return before it closes the store, so fn may
// keep values as it stops.
func (h *Host) Go(fn func(ctx context.Context)) {
	h.s.extensionWork.Add(1)
	go func() {
		defer h.s.extensionWork.Done()
		fn(h.s.extContext)
	}()
}

// Context is canceled when the server shuts down.
func (h *Host) Context() context.Context { return h.s.extContext }

// Command is a command people run by sending /name as a `command` request
// (PROTOCOL.md §4.1).
type Command struct {
	// Name is what follows the slash: lowercase letters, digits, and
	// dashes. It may not be a command the server has.
	Name string
	// Usage and Help describe the command in /help, such as
	// "/weather <city>" and "show the forecast".
	Usage, Help string
	// Available reports whether u may run the command in r, and hides it
	// from /help otherwise. nil makes it available to everyone.
	Available func(u User, r Room) bool
	// Run runs the command. Its replies are notices (CommandCall.Notice,
	// Host.RoomNotice) and the effects of what it does; the result, {} for
	// nil, follows them. An *Error refuses the command with its code;
	// any other error is logged and refused as an internal error.
	Run func(ctx context.Context, call *CommandCall) (map[string]any, error)
}

// CommandCall is one run of a Command.
type CommandCall struct {
	// User ran the command in Room.
	User User
	Room Room
	// Args is the command line after the command's name, trimmed.
	Args string
	// ReplyTo is the message_id the command replied to, if any.
	ReplyTo string
	// Mentions are the user_ids the command line mentions (§3.5.5).
	Mentions []string
	// Body is a copy of the command's message body.
	Body map[string]any

	client *client
}

// Notice sends a ~private notice (Appendix A.1), Markdown, to the connection
// that ran the command, before the command's result.
func (c *CommandCall) Notice(text string) {
	notice := privateNotice(c.Room.ID, text)
	notice["params"].(map[string]any)["body"].(map[string]any)["format"] = "markdown"
	c.client.enqueue(notice)
}

var commandNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

type extensionCommand struct {
	host    *Host
	command Command
}

func (e *extensionCommand) available(u User, r Room) bool {
	return e.command.Available == nil || e.command.Available(u, r)
}

// Command adds a command.
func (h *Host) Command(command Command) {
	h.register("a command", func() error {
		switch {
		case !commandNamePattern.MatchString(command.Name):
			return fmt.Errorf("command %q: a name is lowercase letters, digits, and dashes", command.Name)
		case command.Run == nil:
			return fmt.Errorf("command /%s has no Run", command.Name)
		}
		for _, existing := range h.s.commands {
			if existing.name == command.Name {
				return fmt.Errorf("command /%s exists already", command.Name)
			}
		}
		usage := command.Usage
		if usage == "" {
			usage = "/" + command.Name
		}
		h.s.commands = append(h.s.commands, serverCommand{
			name: command.Name, usage: usage, help: command.Help,
			extension: &extensionCommand{host: h, command: command},
		})
		return nil
	})
}

// runExtensionCommand runs an extension's command. It is called holding
// s.mu, which it releases before the command runs.
func (s *Server) runExtensionCommand(c *client, req request, command *extensionCommand, r *roomState, body map[string]any, args, replyTo string) (any, bool, *Error) {
	call := &CommandCall{
		User: userViewOf(c.user), Room: s.roomViewLocked(r), Args: args, ReplyTo: replyTo,
		Mentions: mentions(body), Body: cloneObject(body), client: c,
	}
	s.unlock()
	if !command.available(call.User, call.Room) {
		return nil, false, &Error{Code: CodeDenied, Message: fmt.Sprintf("You cannot use /%s here", command.command.Name)}
	}
	result, err := command.command.Run(s.extContext, call)
	if err != nil {
		return nil, false, command.host.protocolError(err)
	}
	if result == nil {
		result = map[string]any{}
	}
	c.sendResult(req, result)
	return result, true, nil
}

// protocolError turns an extension's error into a reply: an *Error as it
// is, and anything else, logged, as an internal error.
func (h *Host) protocolError(err error) *Error {
	if err == nil {
		return nil
	}
	if e, ok := errors.AsType[*Error](err); ok {
		return e
	}
	h.logger.Error("extension failed", "error", err)
	return &Error{Code: CodeInternalError, Message: "Something went wrong; try again later"}
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
// encoded as the reply's result; an *Error refuses the request with its
// code, and any other error is logged and refused as an internal error.
type MethodFunc func(ctx context.Context, call *MethodCall) (any, error)

// Method adds a request method for signed-in users. Its name starts with
// the extension's name and a dot, such as "search.query", so that
// extensions never take each other's or the protocol's methods. Clients
// learn of it from the extension's capability (Advertise).
func (h *Host) Method(name string, fn MethodFunc) {
	h.register("a method", func() error {
		suffix, ok := strings.CutPrefix(name, h.name+".")
		switch {
		case !ok || suffix == "" || strings.ContainsFunc(suffix, func(r rune) bool { return r <= ' ' }):
			return fmt.Errorf("method %q: a method's name is %s.<name>", name, h.name)
		case h.s.ops[name] != nil:
			return fmt.Errorf("method %q exists already", name)
		}
		h.s.ops[name] = func(s *Server, c *client, req request) (any, bool, *Error) {
			s.mu.RLock()
			user := userViewOf(c.user)
			s.mu.RUnlock()
			result, err := fn(s.extContext, &MethodCall{User: user, Params: req.params})
			if err != nil {
				return nil, false, h.protocolError(err)
			}
			if result == nil {
				result = map[string]any{}
			}
			return result, false, nil
		}
		return nil
	})
}

// Advertise lists the extension's capability, ext:<name>, in the server
// frame (PROTOCOL.md §3.1), with value under its name in the frame's ext
// unless value is nil. Called after Init, it sends every connection a new
// server frame. value must encode as JSON.
func (h *Host) Advertise(value any) {
	s := h.s
	s.mu.Lock()
	defer s.unlock()
	s.advertised[h.name] = value
	if h.sealed {
		s.announceServerLocked()
	}
}

// route is an HTTP route an extension serves.
type route struct {
	pattern string
	handler http.Handler
}

// Handle serves handler at pattern, as http.ServeMux patterns go, beside the
// server's own routes, such as "/sso/callback" or "POST /hooks/github". A
// pattern that conflicts with another fails Open.
func (h *Host) Handle(pattern string, handler http.Handler) {
	h.register("a route", func() (err error) {
		// http.ServeMux panics on a conflict: try the pattern with the
		// routes so far first.
		mux := http.NewServeMux()
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("route %q: %v", pattern, recovered)
			}
		}()
		for _, core := range []string{"/ws", "/healthz", "/.well-known/webauthn", writePath, filePath, livePath, emojiPath, externalPath} {
			mux.Handle(core, http.NotFoundHandler())
		}
		for _, existing := range h.s.routes {
			mux.Handle(existing.pattern, existing.handler)
		}
		mux.Handle(pattern, handler)
		h.s.routes = append(h.s.routes, route{pattern, handler})
		return nil
	})
}

// Draft is a message as a `message` request saves it: a new message, or an
// edit (MessageID set) or a move (RoomID changed) of one. Deletions are no
// drafts.
type Draft struct {
	// From is the author.
	From User
	// RoomID is the room the message is saved to, MessageID the message an
	// edit saves, and ReplyTo the message it replies to.
	RoomID, MessageID, ReplyTo string
	// Body is the message's body, which a filter may change; the server
	// checks it again after the filters.
	Body map[string]any
	// Ext is what the request writes to the message's ext (§4.12).
	Ext map[string]jsontext.Value
}

// Text is the body's text.
func (d *Draft) Text() string {
	text, _ := d.Body["text"].(string)
	return text
}

type extensionFilter struct {
	host *Host
	fn   func(ctx context.Context, d *Draft) error
}

// FilterMessage checks each message people and bots save, before it is
// saved: fn may change d, or refuse the message by returning an error, an
// *Error with its code, or any other as an internal error. Filters run in
// the order they were added. The sender waits for them, so they need to be
// quick.
func (h *Host) FilterMessage(fn func(ctx context.Context, d *Draft) error) {
	h.register("a filter", func() error {
		h.s.filters = append(h.s.filters, extensionFilter{h, fn})
		return nil
	})
}

// filterMessage runs the filters on a message c saves, outside s.mu, and
// checks what they leave.
func (s *Server) filterMessage(c *client, d *Draft) *Error {
	s.mu.RLock()
	filters := s.filters
	d.From = userViewOf(c.user)
	s.mu.RUnlock()
	for _, filter := range filters {
		if err := filter.fn(s.extContext, d); err != nil {
			return filter.host.protocolError(err)
		}
	}
	if d.Body == nil {
		return invalidParams("body must be an object")
	}
	return validateBody(d.Body)
}

// User is a user as extensions see it: a copy, which does not change.
type User struct {
	ID   string
	Name string
	// Roles are the server roles the user holds (§3.3.4), sorted.
	Roles []string
	// Account is false for a guest.
	Account bool
	// Bot marks an extension's bot (Host.Session).
	Bot bool
	// Email is the account's sign-in address, if any.
	Email string
	// Created is when the user was made, zero if not known, as for
	// accounts made before the server kept it.
	Created time.Time
}

// HasRole reports whether the user holds role.
func (u User) HasRole(role string) bool { return slices.Contains(u.Roles, role) }

func userViewOf(u *userState) User {
	if u == nil {
		return User{}
	}
	return User{
		ID: u.id, Name: u.name, Roles: slices.Clone(u.roles), Account: u.account(), Bot: u.bot,
		Email: u.email, Created: u.created,
	}
}

// Room is a room as extensions see it: a copy, which does not change.
type Room struct {
	ID    string
	Title string
	// ParentID is the room a thread belongs to, if any.
	ParentID string
	// Private reports a private room (§4.2.4), and Public a room everyone
	// may see: neither it nor a room it belongs to is private.
	Private bool
	Public  bool
	// Members counts its members.
	Members int
}

func (s *Server) roomViewLocked(r *roomState) Room {
	if r == nil {
		return Room{}
	}
	everyone, _ := r.audience()
	room := Room{ID: r.id, Title: r.title(), Private: r.private, Public: everyone, Members: len(r.members)}
	if r.parent != nil {
		room.ParentID = r.parent.id
	}
	return room
}

// User returns the user with this user_id, if there is one.
func (h *Host) User(id string) (User, bool) {
	h.s.mu.RLock()
	defer h.s.mu.RUnlock()
	u := h.s.users[id]
	return userViewOf(u), u != nil
}

// Room returns the room with this room_id, if there is one.
func (h *Host) Room(id string) (Room, bool) {
	h.s.mu.RLock()
	defer h.s.mu.RUnlock()
	r := h.s.rooms[id]
	return h.s.roomViewLocked(r), r != nil
}

// Rooms lists every room, threads and private rooms included, oldest
// first.
func (h *Host) Rooms() []Room {
	h.s.mu.RLock()
	defer h.s.mu.RUnlock()
	rooms := slices.SortedFunc(maps.Values(h.s.rooms), func(a, b *roomState) int {
		return int(a.createdID - b.createdID)
	})
	views := make([]Room, len(rooms))
	for i, r := range rooms {
		views[i] = h.s.roomViewLocked(r)
	}
	return views
}

// Visible reports whether the user may see the room and its messages: a
// room that is not private, or whose private rooms all have the user as a
// member. An empty userID is someone not signed in, who sees only public
// rooms. An extension that shows messages to people checks this, so that
// it never shows anyone a private room's.
func (h *Host) Visible(userID, roomID string) bool {
	h.s.mu.RLock()
	defer h.s.mu.RUnlock()
	r := h.s.rooms[roomID]
	if r == nil {
		return false
	}
	if userID == "" {
		everyone, _ := r.audience()
		return everyone
	}
	u := h.s.users[userID]
	return u != nil && r.visibleTo(u)
}

// RoomNotice posts a ~room notice (Appendix A.1), logged, to the room's
// members.
func (h *Host) RoomNotice(roomID, text string) error {
	s := h.s
	s.mu.Lock()
	defer s.unlock()
	r := s.rooms[roomID]
	if r == nil {
		return invalidParams("Unknown room %q", roomID)
	}
	s.postRoomNoticeLocked(r, text)
	return nil
}

// extEntryPrefix starts the store's entry kinds of the values extensions
// keep: ext/<name>.
const extEntryPrefix = "ext/"

// maxKVKeyBytes bounds a key an extension keeps a value under.
const maxKVKeyBytes = 512

// KV keeps an extension's values across restarts, by key, in the server's
// store, written in order with the server's own state. Values are encoded
// as JSON. It suits settings and cursors, not bulk data.
type KV struct{ h *Host }

// Store is the extension's KV.
func (h *Host) Store() KV { return KV{h} }

func (kv KV) kind() string { return extEntryPrefix + kv.h.name }

// Get decodes the value kept under key into into, and reports whether
// there was one.
func (kv KV) Get(key string, into any) (bool, error) {
	s := kv.h.s
	s.mu.RLock()
	raw, ok := s.extData[kv.kind()][key]
	s.mu.RUnlock()
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(raw, into, decodeOptions)
}

// Put keeps value under key.
func (kv KV) Put(key string, value any) error {
	if key == "" || len(key) > maxKVKeyBytes {
		return fmt.Errorf("a key is 1 to %d bytes", maxKVKeyBytes)
	}
	raw, err := json.Marshal(value, jsonOptions)
	if err != nil {
		return err
	}
	s := kv.h.s
	s.mu.Lock()
	defer s.unlock()
	kind := kv.kind()
	if s.extData[kind] == nil {
		s.extData[kind] = make(map[string]jsontext.Value)
	}
	s.extData[kind][key] = raw
	s.dirty.ext[extKey{kind, key}] = true
	return nil
}

// Delete forgets the value under key.
func (kv KV) Delete(key string) {
	s := kv.h.s
	s.mu.Lock()
	defer s.unlock()
	kind := kv.kind()
	if _, ok := s.extData[kind][key]; ok {
		delete(s.extData[kind], key)
		s.dirty.ext[extKey{kind, key}] = true
	}
}

// Keys lists the keys that start with prefix, sorted.
func (kv KV) Keys(prefix string) []string {
	s := kv.h.s
	s.mu.RLock()
	defer s.mu.RUnlock()
	var keys []string
	for key := range s.extData[kv.kind()] {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}
