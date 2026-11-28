// Package bot runs bots: users of a server's that read what it sends them
// and answer, such as a bot that replies when someone mentions it. A bot
// works on any connection it is given (Conn), which it opens with a
// Dialer, and imports only package apron: in the same program as a
// server, on an in-process connection (server.Server.Local), signed in
// from the start, whose requests take a network client's path, so the
// backend checks, delivers, and limits them as anyone's; or on a network
// connection to any Apron server.
//
// A [Bot] keeps one connection open, opened again when the server ends
// it, makes typed requests on it ([Bot.Post], [Bot.Reply],
// [Bot.Command], [Bot.Join], [Bot.History], and the raw [Bot.Call]), and
// hands what it receives to its handlers. On a server in the same
// program:
//
//	echo, err := bot.Start(func(_ context.Context, onFrame func(apron.Frame)) (bot.Conn, error) {
//		return srv.Local(server.LocalOptions{User: apron.User{UserID: "echo", Name: "Echo"}, OnFrame: onFrame})
//	}, bot.Options{
//		OnMessage: func(ctx context.Context, b *bot.Bot, m apron.Message) {
//			if b.Mentioned(m) {
//				b.Reply(ctx, m, "You said: "+m.Body.Text)
//			}
//		},
//	})
//
// The Dialer says who the bot asks to be, and the server may make it
// another user as its connection opens, such as one with a user_id of the
// backend's own, or the name it asked for normalized (server.Local):
// [Bot.User] is who it is, and what tells its own messages from others'
// and its mentions. A message that names the bot only in its text, as
// people who type @echo or "echo:" on a client that sends no mentions do,
// is [Bot.Addressed].
//
// A handler may make requests, such as a reply, and wait for their
// results, which never wait behind the frames it has yet to handle; see
// Options.OnMessage.
package bot

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/apron-chat/apron-server-go/apron"
)

// Conn is a connection a bot makes its requests on, signed in as the
// bot: an in-process one (*server.Local), or a network one. A Dialer opens
// one.
type Conn interface {
	// Call sends a request with params, any value that encodes as a JSON
	// object, and returns its result, or its error reply as an
	// *apron.Error. Once the connection has ended, its error wraps
	// apron.ErrClosed, or tells why it ended.
	Call(ctx context.Context, method string, params any) (jsontext.Value, error)
	// Identity is who the connection is signed in as: the user it asked
	// for, or the one the server made it, such as by a rename since, as
	// the connection keeps it, without a copy, and whether it is signed
	// in. Its Roles, Ext, and Unknown are shared, and not changed.
	Identity() (apron.User, bool)
	// Done is closed once the connection has ended.
	Done() <-chan struct{}
	// Close ends the connection.
	Close() error
}

// A Dialer opens a connection for a bot, signed in as the user the bot is
// to be, and hands each frame the server sends it but the replies to Call,
// which go to Call, to onFrame: in order, on a goroutine of the
// connection's own, from the server frame on. onFrame is nil when the bot
// has no handler for them, and may be called before the Dialer returns:
// it waits until it has, so a reply to Call must never wait behind the
// frames onFrame has yet to take, as it does not on a server.Local. Its
// error wraps apron.ErrClosed when the connection will never open again,
// such as on a server that shut down: the bot then stops opening it.
//
// On a server.Server in the same program, a Dialer opens a Local
// connection:
//
//	func(_ context.Context, onFrame func(apron.Frame)) (bot.Conn, error) {
//		return srv.Local(server.LocalOptions{User: echo, OnFrame: onFrame})
//	}
//
// On any Apron server over the network, it dials one with package
// [github.com/apron-chat/apron-server-go/apron/client]:
//
//	func(ctx context.Context, onFrame func(apron.Frame)) (bot.Conn, error) {
//		return client.Dial(ctx, url, auth, client.OnFrame(onFrame))
//	}
type Dialer func(ctx context.Context, onFrame func(apron.Frame)) (Conn, error)

// call sends a request on conn, and decodes its result.
func call[T any](ctx context.Context, conn Conn, method string, params any) (T, error) {
	var result T
	raw, err := conn.Call(ctx, method, params)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return result, err
	}
	return result, nil
}

// Options configure a Bot.
type Options struct {
	// OnMessage, if set, gets each message the bot receives that is not
	// its own (§3.5): new messages, their edits and deletions, and
	// notices, in order, one at a time, on a goroutine of the
	// connection's own, with a context that Close cancels.
	//
	// It may make requests, such as Reply, and wait for their results:
	// a result goes straight to the request that waits for it, never
	// behind the frames OnMessage has yet to handle, so it cannot wait for
	// itself. The frames that arrive meanwhile wait for OnMessage to
	// return, which keeps them in order, and count toward the connection's
	// queue (server.LocalOptions.Queue): a handler that waits so long that
	// more pile up ends the connection (server.ErrFellBehind), and the Bot
	// opens another, without them. So a
	// handler waits only for what the server answers promptly, such as a
	// reply; one that waits on anything else, such as another service, a
	// timer, or a server that answers later (server.Reply.Later) after a
	// slow upstream, does it on a goroutine of its own, and gives up the
	// order of what it sends after.
	OnMessage func(ctx context.Context, b *Bot, m apron.Message)
	// OnFrame, if set, gets every other frame the bot receives, as
	// OnMessage does: the server frame first, then notifications, such as
	// room_update, and errors about the connection.
	OnFrame func(ctx context.Context, b *Bot, f apron.Frame)
}

// Bot is a bot: a user of the server's, on a connection the Bot keeps
// open, which it opens again when the server ends it, such as after a
// sign-out or when a handler falls behind, unless the server is shut
// down. Its methods are safe for concurrent use.
type Bot struct {
	dial    Dialer
	options Options
	ctx     context.Context
	cancel  context.CancelFunc

	// dialing is held while the connection opens, so that one opens at a
	// time; mu guards conn, and is never held while one opens, so that
	// Close and User do not wait for a Dialer.
	dialing sync.Mutex
	mu      sync.Mutex
	conn    Conn
}

// Reopening waits at least this long, and at most that, before the Bot
// opens its connection again, doubling the wait after each failure.
const (
	minReopen = time.Second
	maxReopen = time.Minute
)

// Start starts a bot on the connections dial opens, and returns once its
// first connection is open, or the error of opening it.
func Start(dial Dialer, options Options) (*Bot, error) {
	b := &Bot{dial: dial, options: options}
	b.ctx, b.cancel = context.WithCancel(context.Background())
	if _, err := b.Conn(); err != nil {
		b.cancel()
		return nil, err
	}
	return b, nil
}

// User is who the bot is: its connection's user (Conn.Identity), which
// the server may have made another than the Dialer asked for as it
// opened, such as one with a user_id of its own, or the name normalized,
// and which a rename changes. It is not copied: its Roles, Ext, and
// Unknown are the connection's, and are not changed.
func (b *Bot) User() apron.User {
	b.mu.Lock()
	conn := b.conn
	b.mu.Unlock()
	if conn == nil {
		return apron.User{}
	}
	u, _ := conn.Identity()
	return u
}

// UserID is the bot's user_id, its User's.
func (b *Bot) UserID() string { return b.User().UserID }

// Conn returns the bot's connection, opened again if it has ended. Once
// the Bot is closed, its error is apron.ErrClosed.
func (b *Bot) Conn() (Conn, error) {
	b.dialing.Lock()
	defer b.dialing.Unlock()
	if b.ctx.Err() != nil {
		return nil, apron.ErrClosed
	}
	b.mu.Lock()
	current := b.conn
	b.mu.Unlock()
	if current != nil {
		select {
		case <-current.Done():
		default:
			return current, nil
		}
	}
	var onFrame func(apron.Frame)
	var r *receiver
	listens := b.options.OnMessage != nil || b.options.OnFrame != nil
	if listens {
		r = &receiver{b: b, opened: make(chan struct{})}
		onFrame = r.receive
	}
	conn, err := b.dial(b.ctx, onFrame)
	if r != nil {
		if err == nil {
			u, _ := conn.Identity()
			r.self, r.open = u.UserID, true
		}
		close(r.opened)
	}
	if err != nil {
		return nil, err
	}
	// Close cancels b.ctx before it closes b.conn, under mu: a connection
	// that opened as it did is closed here, or by Close.
	b.mu.Lock()
	if b.ctx.Err() != nil {
		b.mu.Unlock()
		conn.Close()
		return nil, apron.ErrClosed
	}
	b.conn = conn
	b.mu.Unlock()
	if listens {
		// A bot that listens is not left without a connection until its
		// next request.
		go b.reopen(conn)
	}
	return conn, nil
}

// reopen opens the bot's connection again once conn ends, unless the Bot
// is closed, or the server shut down.
func (b *Bot) reopen(conn Conn) {
	select {
	case <-conn.Done():
	case <-b.ctx.Done():
		return
	}
	for wait := minReopen; ; wait = min(2*wait, maxReopen) {
		select {
		case <-time.After(wait):
		case <-b.ctx.Done():
			return
		}
		if _, err := b.Conn(); err == nil || errors.Is(err, apron.ErrClosed) {
			return
		}
	}
}

// receiver hands the frames of one of the bot's connections to its
// handlers. It keeps the connection's user_id as its frames tell it, the
// one it had as it opened until a user notification says otherwise
// ({"you": ...}, §3.3.3), which tells the bot's own messages by the
// user_id it had when it sent them, whatever it has become since.
type receiver struct {
	b *Bot
	// opened is closed once the Dialer returned, with open set if it
	// opened the connection, and self its user_id.
	opened chan struct{}
	open   bool
	// self is used only on the connection's frame goroutine, once opened
	// is closed.
	self string
}

// receive hands a frame to the handlers, once the connection is open.
func (r *receiver) receive(frame apron.Frame) {
	<-r.opened
	b := r.b
	if !r.open {
		// The Dialer failed: the connection is not the bot's.
		return
	}
	// Only the head is read of every frame, and the params only of those
	// the bot handles, in place.
	head, err := frame.Head()
	if err != nil || head.ID != nil {
		head.Method = ""
	}
	switch head.Method {
	case "user":
		var update struct {
			You struct {
				UserID string `json:"user_id"`
			} `json:"you"`
		}
		if json.Unmarshal(head.Params, &update) == nil && update.You.UserID != "" {
			r.self = update.You.UserID
		}
	case "message":
		if b.options.OnMessage == nil {
			break
		}
		var m apron.Message
		if json.Unmarshal(head.Params, &m) == nil {
			if m.From.UserID != r.self {
				b.options.OnMessage(b.ctx, b, m)
			}
			return
		}
	}
	if b.options.OnFrame != nil {
		b.options.OnFrame(b.ctx, b, frame)
	}
}

// Mentioned reports whether m is a new message from someone else that
// mentions the bot (§3.5.5): not an edit, a deletion, or a notice. Only
// body.mentions count; Addressed also takes the bot's name in the text.
func (b *Bot) Mentioned(m apron.Message) bool {
	self := b.UserID()
	return fresh(m, self) && slices.Contains(m.Body.Mentions, self)
}

// Addressed reports whether m is a new message from someone else meant
// for the bot: one that mentions it (Mentioned), or whose text addresses
// its name or its user_id as people do in plain text (Addresses), such as
// one from a client that sends no mentions, as a gateway's SSH or IRC
// users do. Not an edit, a deletion, or a notice.
func (b *Bot) Addressed(m apron.Message) bool {
	self := b.User()
	return fresh(m, self.UserID) && (slices.Contains(m.Body.Mentions, self.UserID) ||
		Addresses(m.Body.Text, self.Name) || Addresses(m.Body.Text, self.UserID))
}

// fresh reports whether m is a new message, with a body, from another
// than self.
func fresh(m apron.Message, self string) bool {
	return m.MessageID != 0 && m.PrevLogID == 0 && !m.Deleted && m.From.UserID != self && !m.Body.Empty()
}

// Addresses reports whether text addresses name as people do in plain
// text: @name anywhere, not inside a word, as in "thanks @echo!" but not
// "me@echo"; or name followed by a colon or a comma at the start of the
// text, its leading whitespace aside, as in "echo: hi" or "Echo, hi". The
// name matches whatever its case, and whole: "@echoes" and "@echo-1" do
// not address echo, and neither does "@echo.org", though "ask @echo." does:
// a name followed by a period is whole when the period is not followed by
// what may be part of a name. An empty name is addressed by nothing.
func Addresses(text, name string) bool {
	if name == "" {
		return false
	}
	if rest, ok := cutPrefixFold(strings.TrimLeftFunc(text, unicode.IsSpace), name); ok && (strings.HasPrefix(rest, ":") || strings.HasPrefix(rest, ",")) {
		return true
	}
	for i := 0; i < len(text); i++ {
		if text[i] != '@' {
			continue
		}
		if before, _ := utf8.DecodeLastRuneInString(text[:i]); i > 0 && (inName(before) || before == '.' || before == '@') {
			continue
		}
		if rest, ok := cutPrefixFold(text[i+1:], name); ok && whole(rest) {
			return true
		}
	}
	return false
}

// inName reports whether r may be part of a name, so that a name followed
// by it goes on: a letter, a digit, '_', or '-'.
func inName(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-'
}

// whole reports whether a name followed by rest is whole: rest is empty,
// or starts with a rune that is not part of a name, a period too unless
// one is after it.
func whole(rest string) bool {
	if rest == "" {
		return true
	}
	next, size := utf8.DecodeRuneInString(rest)
	if next == '.' {
		after, _ := utf8.DecodeRuneInString(rest[size:])
		return rest[size:] == "" || !inName(after)
	}
	return !inName(next)
}

// cutPrefixFold returns s without prefix, matched whatever its case, and
// whether s starts with it.
func cutPrefixFold(s, prefix string) (string, bool) {
	for _, want := range prefix {
		got, size := utf8.DecodeRuneInString(s)
		if s == "" || !strings.EqualFold(string(got), string(want)) {
			return "", false
		}
		s = s[size:]
	}
	return s, true
}

// Call sends a request on the bot's connection (Conn.Call).
func (b *Bot) Call(ctx context.Context, method string, params any) (jsontext.Value, error) {
	conn, err := b.Conn()
	if err != nil {
		return nil, err
	}
	return conn.Call(ctx, method, params)
}

// Post posts a message (§3.5.1), and returns its result: the new
// message's message_id, unless the body was empty.
func (b *Bot) Post(ctx context.Context, p apron.MessageParams) (apron.MessageResult, error) {
	conn, err := b.Conn()
	if err != nil {
		return apron.MessageResult{}, err
	}
	return call[apron.MessageResult](ctx, conn, "message", p)
}

// Reply posts text in the room of the message to, as a reply to it
// (§3.5.4), or, for a notice, which has no message_id, after it.
func (b *Bot) Reply(ctx context.Context, to apron.Message, text string) (apron.MessageResult, error) {
	p := apron.MessageParams{RoomID: to.RoomID, Body: apron.Body{Text: text}}
	if to.MessageID != 0 {
		p.ReplyTo = apron.MessageRef{MessageID: to.MessageID}
	}
	return b.Post(ctx, p)
}

// Command runs a command (§4.1), whose body.text is the command line. Its
// replies come before its result, as notifications.
func (b *Bot) Command(ctx context.Context, p apron.CommandParams) (apron.CommandResult, error) {
	conn, err := b.Conn()
	if err != nil {
		return apron.CommandResult{}, err
	}
	return call[apron.CommandResult](ctx, conn, "command", p)
}

// Join joins the room (§4.2.2).
func (b *Bot) Join(ctx context.Context, roomID string) error {
	_, err := b.Call(ctx, "room_join", apron.RoomJoinParams{RoomID: roomID})
	return err
}

// History reads a page of a room's log, or of every room the connection
// receives (§4.3).
func (b *Bot) History(ctx context.Context, p apron.HistoryParams) (apron.HistoryResult, error) {
	conn, err := b.Conn()
	if err != nil {
		return apron.HistoryResult{}, err
	}
	return call[apron.HistoryResult](ctx, conn, "history", p)
}

// Close ends the bot's connection, and opens no other: the handlers get
// nothing more, and their context is canceled.
func (b *Bot) Close() error {
	// Canceled first, which ends a Dialer still opening a connection.
	b.cancel()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil {
		b.conn.Close()
	}
	return nil
}
