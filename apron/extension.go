package apron

import (
	"cmp"
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
	"sync/atomic"
	"time"
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
var reservedExtensionNames = []string{"emoji", "settings"}

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
func (r *Registrar) Command(command Command) {
	r.register("a command", func() error {
		switch {
		case !commandNamePattern.MatchString(command.Name):
			return fmt.Errorf("command %q: a name is lowercase letters, digits, and dashes", command.Name)
		case command.Run == nil:
			return fmt.Errorf("command /%s has no Run", command.Name)
		}
		for _, existing := range r.s.commands {
			if existing.name == command.Name {
				return fmt.Errorf("command /%s exists already", command.Name)
			}
		}
		usage := command.Usage
		if usage == "" {
			usage = "/" + command.Name
		}
		r.s.commands = append(r.s.commands, serverCommand{
			name: command.Name, usage: usage, help: command.Help,
			extension: &extensionCommand{host: r.Host, command: command},
		})
		return nil
	})
}

// runExtensionCommand runs an extension's command, outside s.mu.
func (s *Server) runExtensionCommand(c *client, req request, command *extensionCommand, call *CommandCall) (any, bool, *Error) {
	if !command.available(call.User, call.Room) {
		return nil, false, &Error{Code: CodeDenied, Message: fmt.Sprintf("You cannot use /%s here", command.command.Name)}
	}
	result, err := command.command.Run(s.requestContext(c), call)
	if err != nil {
		return nil, false, command.host.protocolError(err)
	}
	if result == nil {
		result = map[string]any{}
	}
	encoded, encodeErr := json.Marshal(result, jsonOptions)
	if encodeErr != nil {
		return nil, false, command.host.protocolError(fmt.Errorf("/%s returned a result that is not JSON: %w", command.command.Name, encodeErr))
	}
	c.enqueue(rawResponse(req.id, encoded))
	return jsontext.Value(encoded), true, nil
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
// encoded as the reply's result, {} for nil; an *Error refuses the request
// with its code, and any other error, or a result that does not encode as
// JSON, is logged and refused as an internal error.
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
		h.s.ops[name] = func(s *Server, c *client, req request) (any, bool, *Error) {
			s.mu.RLock()
			user := userViewOf(c.user)
			s.mu.RUnlock()
			result, err := fn(s.requestContext(c), &MethodCall{User: user, Params: req.params})
			if err != nil {
				return nil, false, h.protocolError(err)
			}
			if result == nil {
				result = map[string]any{}
			}
			// The result is encoded now: one that is not JSON is the
			// extension's error, not the connection's, and the reply that
			// deduplication keeps (§1.2) cannot change afterwards.
			encoded, encodeErr := json.Marshal(result, jsonOptions)
			if encodeErr != nil {
				return nil, false, h.protocolError(fmt.Errorf("method %s returned a result that is not JSON: %w", name, encodeErr))
			}
			return jsontext.Value(encoded), false, nil
		}
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
		if encoded, err = json.Marshal(value, jsonOptions); err != nil {
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
		if encoded, err = json.Marshal(value, jsonOptions); err != nil {
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
// a message that is not theirs, is left to the server to refuse, unfiltered.
func (s *Server) filterMessage(c *client, d *Draft) *Error {
	s.mu.RLock()
	filters := s.filters
	u := c.user
	d.From = userViewOf(u)
	allowed := s.visibleRoomLocked(u, d.RoomID) != nil
	if allowed && d.MessageID != "" {
		m := s.visibleMessageLocked(u, d.MessageID)
		allowed = m != nil && m.owner == u.id
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
	encoded, err := json.Marshal(d.Body, jsonOptions)
	var body map[string]any
	if err != nil || json.Unmarshal(encoded, &body) != nil || body == nil {
		return internalError()
	}
	d.Body = body
	return validateBody(d.Body)
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
		ID: u.id, Name: u.name, Roles: slices.Clone(u.roles), Account: u.account(), Bot: u.botOf != "",
		Email: u.email, Created: u.created,
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
	everyone, _ := r.audience()
	room := Room{ID: r.id, Title: r.title(), Restricted: !everyone, Members: len(r.members)}
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
		return cmp.Compare(a.createdID, b.createdID)
	})
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
	r, user := h.s.rooms[roomID], h.s.users[u.ID]
	return r != nil && user != nil && r.visibleTo(user)
}

// Public reports whether anyone may see the room, without an account: it
// is not restricted, and the server lets guests in (Config.DisableGuests),
// so that whoever is not signed in could see it by signing in as a guest.
// An extension that shows messages to people who are not signed in, such as
// on the web, shows only those of public rooms.
func (h *Host) Public(roomID string) bool {
	h.s.mu.RLock()
	defer h.s.mu.RUnlock()
	r := h.s.rooms[roomID]
	if r == nil || h.s.config.DisableGuests {
		return false
	}
	everyone, _ := r.audience()
	return everyone
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
	raw, ok := s.extData[kv.kind()][key]
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
	raw, err := json.Marshal(value, jsonOptions)
	if err != nil {
		return err
	}
	s := kv.h.s
	s.mu.Lock()
	defer s.unlock()
	if s.storeClosed {
		return ErrServerClosed
	}
	kind := kv.kind()
	if s.extData[kind] == nil {
		s.extData[kind] = make(map[string]jsontext.Value)
	}
	s.extData[kind][key] = raw
	s.dirty.ext[extKey{kind, key}] = true
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
	if s.storeClosed {
		return ErrServerClosed
	}
	kind := kv.kind()
	if _, ok := s.extData[kind][key]; ok {
		delete(s.extData[kind], key)
		s.dirty.ext[extKey{kind, key}] = true
	}
	return nil
}

// Keys lists the extension's keys that start with prefix, sorted.
func (kv KV) Keys(prefix string) []string {
	s := kv.h.s
	s.mu.RLock()
	defer s.mu.RUnlock()
	var keys []string
	for key := range s.extData[kv.kind()] {
		if strings.HasPrefix(key, prefix) && !strings.HasPrefix(key, reservedKeyPrefix) {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}
