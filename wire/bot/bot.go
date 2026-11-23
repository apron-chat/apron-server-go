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
// Package apron's bots make their requests on a Conn.
package bot

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"slices"
	"sync"
	"time"

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

// UserID is the user the connection is signed in as.
func (c *Conn) UserID() string { return c.Conn().UserID() }

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
	// User is who the bot is: each of its connections is signed in as
	// User (wire.LocalOptions.User).
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
	// notices, in order, on a goroutine of the connection's own, with a
	// context that Close cancels. It holds up the frames after it, so
	// one that takes long answers on a goroutine of its own.
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

// UserID is the bot's user_id.
func (b *Bot) UserID() string { return b.options.User.UserID }

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
		options.OnFrame = b.receive
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

// receive hands a frame to the handlers.
func (b *Bot) receive(frame wire.Frame) {
	var head struct {
		Method string         `json:"method"`
		ID     jsontext.Value `json:"id"`
		Params jsontext.Value `json:"params"`
	}
	if json.Unmarshal(frame, &head) == nil && head.Method == "message" && head.ID == nil && b.options.OnMessage != nil {
		var m wire.Message
		if json.Unmarshal(head.Params, &m) == nil {
			if m.From.UserID != b.UserID() {
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
// mentions the bot (§3.5.5): not an edit, a deletion, or a notice.
func (b *Bot) Mentioned(m wire.Message) bool {
	return m.MessageID != 0 && m.PrevLogID == 0 && !m.Deleted && m.From.UserID != b.UserID() &&
		m.Body != nil && slices.Contains(m.Body.Mentions, b.UserID())
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
