// Package bot runs bots on a wire.Server: users of the server's own, in
// the same program, that read what the server sends them and answer, such
// as a bot that replies when someone mentions it. It works on any backend,
// the reference server and a gateway in front of another chat system
// alike: a bot's connection is in process (wire.Server.Local), signed in
// from the start, and its requests take a network client's path, so the
// backend checks, delivers, and limits them as anyone's. The backend takes
// a bot's connection as it takes a network one, in OnConnect, with the
// value the bot gives it (Options.Data), as it would sign it in.
//
// A [Conn] is one connection, with typed requests: [Conn.Post],
// [Conn.Command], [Conn.Join], [Conn.History], and the raw Call of the
// wire.Local it is. A [Bot] keeps one open, opened again when the server
// ends it, and hands what it receives to its handlers:
//
//	echo, err := bot.Start(server, bot.Options{
//		User: wire.User{UserID: "echo", Name: "Echo"},
//		OnMessage: func(ctx context.Context, b *bot.Bot, m wire.Message) {
//			if b.Mentioned(m) {
//				b.Reply(ctx, m, "You said: "+m.Body.Text)
//			}
//		},
//	})
//
// A bot asks to be Options.User, and the backend may make it another user
// as its connection opens, such as one with a user_id of the backend's
// own, or the name it asked for normalized (wire.Local): [Bot.User] is
// who it is, and what tells its own messages from others' and its
// mentions. A message that names the bot only in its text, as people
// who type @echo or "echo:" on a client that sends no mentions do, is
// [Bot.Addressed].
//
// A handler may make requests, such as a reply, and wait for their
// results, which never wait behind the frames it has yet to handle; see
// Options.OnMessage.
//
// Package apron's bots make their requests on a Conn.
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

	"github.com/apron-chat/apron-server-go/wire"
)

// Conn is a connection of a bot's: a wire.Local one, whose requests it
// makes typed. Its errors are those of wire.Local.Call: a request's error
// reply as a *wire.Error, and wire.ErrClosed or wire.ErrFellBehind once
// the connection has ended.
type Conn struct {
	*wire.Local
}

// Dial opens a connection on s, as s.Local does.
func Dial(s *wire.Server, options wire.LocalOptions) (*Conn, error) {
	l, err := s.Local(options)
	if err != nil {
		return nil, err
	}
	return &Conn{l}, nil
}

// UserID is the user_id of the user the connection is signed in as.
func (c *Conn) UserID() string { return c.Conn().UserID() }

// User is who the connection is signed in as: the user it asked for, or
// the one the backend made it as it opened, or since, such as by a rename
// (wire.Local); the zero User if the backend signed it out.
func (c *Conn) User() wire.User {
	if u := c.Conn().User(); u != nil {
		return *u
	}
	return wire.User{}
}

// call sends a request, and decodes its result.
func call[T any](ctx context.Context, c *Conn, method string, params any) (T, error) {
	var result T
	raw, err := c.Call(ctx, method, params)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return result, err
	}
	return result, nil
}

// Post posts a message (§3.5.1), and returns its result: the new
// message's message_id, unless the body was empty.
func (c *Conn) Post(ctx context.Context, p wire.MessageParams) (wire.MessageResult, error) {
	return call[wire.MessageResult](ctx, c, "message", p)
}

// Command runs a command (§4.1), whose body.text is the command line. Its
// replies come before its result, as notifications.
func (c *Conn) Command(ctx context.Context, p wire.CommandParams) (wire.CommandResult, error) {
	return call[wire.CommandResult](ctx, c, "command", p)
}

// Join joins the room (§4.2.2).
func (c *Conn) Join(ctx context.Context, roomID string) error {
	_, err := c.Call(ctx, "room_join", wire.RoomJoinParams{RoomID: roomID})
	return err
}

// History reads a page of a room's log, or of every room the connection
// receives (§4.3).
func (c *Conn) History(ctx context.Context, p wire.HistoryParams) (wire.HistoryResult, error) {
	return call[wire.HistoryResult](ctx, c, "history", p)
}

// Options configure a Bot.
type Options struct {
	// User is who the bot asks to be: each of its connections is signed
	// in as User (wire.LocalOptions.User), unless the backend makes it
	// another user as it opens (Bot.User).
	User wire.User
	// Data, if set, makes the backend's value for each connection the bot
	// opens (wire.LocalOptions.Data), such as the backend's own record of
	// the connection, which its OnConnect tells from a network one's.
	Data func() any
	// Queue bounds the frames waiting for the handlers
	// (wire.LocalOptions.Queue). A handler that lets more pile up ends
	// the connection, and the Bot opens another.
	Queue int
	// OnMessage, if set, gets each message the bot receives that is not
	// its own (§3.5): new messages, their edits and deletions, and
	// notices, in order, one at a time, on a goroutine of the
	// connection's own, with a context that Close cancels.
	//
	// It may make requests, such as Reply, and wait for their results:
	// a result goes straight to the request that waits for it, never
	// behind the frames OnMessage has yet to handle, so it cannot wait for
	// itself. The frames that arrive meanwhile wait for OnMessage to
	// return, which keeps them in order, and count toward Queue: a handler
	// that waits so long that more than Queue pile up ends the connection
	// (wire.ErrFellBehind), and the Bot opens another, without them. So a
	// handler waits only for what the server answers promptly, such as a
	// reply; one that waits on anything else, such as another service, a
	// timer, or a server that answers later (wire.Reply.Later) after a
	// slow upstream, does it on a goroutine of its own, and gives up the
	// order of what it sends after.
	OnMessage func(ctx context.Context, b *Bot, m wire.Message)
	// OnFrame, if set, gets every other frame the bot receives, as
	// OnMessage does: the server frame first, then notifications, such as
	// room_update, and errors about the connection.
	OnFrame func(ctx context.Context, b *Bot, f wire.Frame)
}

// Bot is a bot: a user of the server's, on a connection the Bot keeps
// open, which it opens again when the server ends it, such as after a
// sign-out or when a handler falls behind, unless the server is shut
// down. Its methods are safe for concurrent use.
type Bot struct {
	s       *wire.Server
	options Options
	ctx     context.Context
	cancel  context.CancelFunc

	mu   sync.Mutex
	conn *Conn
}

// Reopening waits at least this long, and at most that, before the Bot
// opens its connection again, doubling the wait after each failure.
const (
	minReopen = time.Second
	maxReopen = time.Minute
)

// Start starts a bot on s, and returns once its first connection is open,
// or the error of opening it.
func Start(s *wire.Server, options Options) (*Bot, error) {
	b := &Bot{s: s, options: options}
	b.ctx, b.cancel = context.WithCancel(context.Background())
	if _, err := b.Conn(); err != nil {
		b.cancel()
		return nil, err
	}
	return b, nil
}

// User is who the bot is: its connection's user (Conn.User), which the
// backend may have made another than Options.User as it opened, such as
// one with a user_id of its own, or the name normalized, and which a
// rename changes; Options.User before its first connection opens.
func (b *Bot) User() wire.User {
	b.mu.Lock()
	conn := b.conn
	b.mu.Unlock()
	if conn != nil {
		if u := conn.Conn().User(); u != nil {
			return *u
		}
	}
	return b.options.User
}

// UserID is the bot's user_id, its User's.
func (b *Bot) UserID() string { return b.User().UserID }

// Conn returns the bot's connection, opened again if it has ended. Once
// the Bot is closed, it is wire.ErrClosed.
func (b *Bot) Conn() (*Conn, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ctx.Err() != nil {
		return nil, wire.ErrClosed
	}
	if b.conn != nil {
		select {
		case <-b.conn.Done():
		default:
			return b.conn, nil
		}
	}
	options := wire.LocalOptions{User: b.options.User, Queue: b.options.Queue}
	if b.options.Data != nil {
		options.Data = b.options.Data()
	}
	listens := b.options.OnMessage != nil || b.options.OnFrame != nil
	if listens {
		options.OnFrame = (&receiver{b: b, self: b.options.User.UserID}).receive
	}
	conn, err := Dial(b.s, options)
	if err != nil {
		return nil, err
	}
	b.conn = conn
	if listens {
		// A bot that listens is not left without a connection until its
		// next request.
		go b.reopen(conn)
	}
	return conn, nil
}

// reopen opens the bot's connection again once conn ends, unless the Bot
// is closed, or the server shut down.
func (b *Bot) reopen(conn *Conn) {
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
		if _, err := b.Conn(); err == nil || errors.Is(err, wire.ErrClosed) {
			return
		}
	}
}

// receiver hands the frames of one of the bot's connections to its
// handlers. It keeps the connection's user_id as its frames tell it, the
// one it asked for until a user notification says otherwise ({"you": ...},
// §3.3.3), which tells the bot's own messages by the user_id it had when
// it sent them, whatever it has become since.
type receiver struct {
	b *Bot
	// self is used only on the connection's frame goroutine.
	self string
}

// receive hands a frame to the handlers.
func (r *receiver) receive(frame wire.Frame) {
	b := r.b
	var head struct {
		Method string         `json:"method"`
		ID     jsontext.Value `json:"id"`
		Params jsontext.Value `json:"params"`
	}
	if json.Unmarshal(frame, &head) != nil || head.ID != nil {
		head.Method = ""
	}
	switch head.Method {
	case "user":
		var update struct {
			You *struct {
				UserID string `json:"user_id"`
			} `json:"you"`
		}
		if json.Unmarshal(head.Params, &update) == nil && update.You != nil && update.You.UserID != "" {
			r.self = update.You.UserID
		}
	case "message":
		if b.options.OnMessage == nil {
			break
		}
		var m wire.Message
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
func (b *Bot) Mentioned(m wire.Message) bool {
	self := b.UserID()
	return fresh(m, self) && slices.Contains(m.Body.Mentions, self)
}

// Addressed reports whether m is a new message from someone else meant
// for the bot: one that mentions it (Mentioned), or whose text addresses
// its name or its user_id as people do in plain text (Addresses), such as
// one from a client that sends no mentions, as a gateway's SSH or IRC
// users do. Not an edit, a deletion, or a notice.
func (b *Bot) Addressed(m wire.Message) bool {
	self := b.User()
	return fresh(m, self.UserID) && (slices.Contains(m.Body.Mentions, self.UserID) ||
		Addresses(m.Body.Text, self.Name) || Addresses(m.Body.Text, self.UserID))
}

// fresh reports whether m is a new message, with a body, from another
// than self.
func fresh(m wire.Message, self string) bool {
	return m.MessageID != 0 && m.PrevLogID == 0 && !m.Deleted && m.From.UserID != self && m.Body != nil
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

// Post posts a message on the bot's connection (Conn.Post).
func (b *Bot) Post(ctx context.Context, p wire.MessageParams) (wire.MessageResult, error) {
	conn, err := b.Conn()
	if err != nil {
		return wire.MessageResult{}, err
	}
	return conn.Post(ctx, p)
}

// Reply posts text in the room of the message to, as a reply to it
// (§3.5.4), or, for a notice, which has no message_id, after it.
func (b *Bot) Reply(ctx context.Context, to wire.Message, text string) (wire.MessageResult, error) {
	p := wire.MessageParams{RoomID: to.RoomID, Body: &wire.Body{Text: text}}
	if to.MessageID != 0 {
		p.ReplyTo = &wire.Ref{MessageID: to.MessageID}
	}
	return b.Post(ctx, p)
}

// Command runs a command on the bot's connection (Conn.Command).
func (b *Bot) Command(ctx context.Context, p wire.CommandParams) (wire.CommandResult, error) {
	conn, err := b.Conn()
	if err != nil {
		return wire.CommandResult{}, err
	}
	return conn.Command(ctx, p)
}

// Join joins a room on the bot's connection (Conn.Join).
func (b *Bot) Join(ctx context.Context, roomID string) error {
	conn, err := b.Conn()
	if err != nil {
		return err
	}
	return conn.Join(ctx, roomID)
}

// History reads history on the bot's connection (Conn.History).
func (b *Bot) History(ctx context.Context, p wire.HistoryParams) (wire.HistoryResult, error) {
	conn, err := b.Conn()
	if err != nil {
		return wire.HistoryResult{}, err
	}
	return conn.History(ctx, p)
}

// Close ends the bot's connection, and opens no other: the handlers get
// nothing more, and their context is canceled.
func (b *Bot) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cancel()
	if b.conn != nil {
		b.conn.Close()
	}
	return nil
}
