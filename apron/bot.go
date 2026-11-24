package apron

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/apron-chat/apron-server-go/apron/internal/livetext"
	"github.com/apron-chat/apron-server-go/wire"
	"github.com/apron-chat/apron-server-go/wire/bot"
)

// Errors of Sessions, which Session.Err reports and their requests return.
var (
	// ErrSessionClosed is returned by the requests of a Session that Close
	// closed.
	ErrSessionClosed = errors.New("apron: session closed")
	// ErrFellBehind is why the server closed a Session whose OnFrame fell
	// too far behind its frames, as it disconnects a client that does not
	// read.
	ErrFellBehind = wire.ErrFellBehind
	// ErrReentrant is returned by a request a Session is asked to send
	// while the request it is sending waits on the caller, such as from a
	// filter or command that the session's own message runs: the session
	// sends one request at a time. Send it with another Session
	// (Bot.Connect).
	ErrReentrant = wire.ErrReentrant
)

// Bot is an account an extension owns and speaks through: it holds the
// "bot" role (§3.3.4), and is never a person's or another extension's. Its
// requests (Post, Join, History, Call, StreamAppend, StreamTerminal, and
// Conn's, in package wire/bot's terms) go through a connection of its own,
// made again if the server closed it, so they get the same checks,
// limits, delivery, and push as a client's. A Bot is safe for concurrent
// use.
type Bot struct {
	s      *Server
	host   *Host
	userID string

	mu      sync.Mutex
	session *Session
}

// BotOptions configure a Bot.
type BotOptions struct {
	// Name is the bot's display name when its account is made.
	Name string
	// Scope, if set, limits the bot to some rooms (Server.SetScope), from
	// this start on: it sees no other, and leaves those it was in. A new
	// bot joins the default room only if the scope lists it. Nil keeps the
	// scope the bot had, if any; lift it with Host.SetScope.
	Scope *RoomScope
}

// Bot makes the extension's bot with this user_id, or signs in the one it
// made at an earlier start, and connects it. A new bot joins the default
// room. The user_id follows the rules for requested ones (§3.3), and is
// refused if a person, or another extension's bot, ever had it; a user_id
// that Config.Roles grants roles to is the bot's to take, with those roles.
func (r *Registrar) Bot(userID string, options BotOptions) (*Bot, error) {
	if r.done.Load() {
		panic(fmt.Sprintf("apron: extension %s made a bot after Init", r.name))
	}
	s := r.s
	if !requestable(userID) {
		return nil, fmt.Errorf("apron: %q is no user_id a bot can have", userID)
	}
	s.mu.Lock()
	u := s.users.Get(userID)
	switch {
	case u != nil && u.Bot == r.name:
	case u != nil && u.Bot != "":
		s.unlock()
		return nil, fmt.Errorf("apron: user_id %q is extension %s's bot", userID, u.Bot)
	case u != nil:
		s.unlock()
		return nil, fmt.Errorf("apron: user_id %q belongs to someone who is not a bot", userID)
	default:
		if !s.ids.ClaimGranted(userID, s.roomNamedLocked) {
			s.unlock()
			return nil, fmt.Errorf("apron: user_id %q was used by someone else", userID)
		}
		u = newUserState(userID, normalizeName(options.Name))
		u.Bot = r.name
		s.users.Add(u)
		s.grantRolesLocked(u)
		u.Scope = newRoomScope(options.Scope)
		s.joinDefaultRoomLocked(u)
	}
	if options.Scope != nil {
		s.setScopeLocked(u, newRoomScope(options.Scope))
	}
	s.unlock()
	b := &Bot{s: s, host: r.Host, userID: userID}
	if _, err := b.conn(); err != nil {
		return nil, err
	}
	return b, nil
}

// UserID is the bot's user_id.
func (b *Bot) UserID() string { return b.userID }

// conn returns the bot's own connection, made again if it was closed.
func (b *Bot) conn() (*Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.session != nil {
		select {
		case <-b.session.Done():
		default:
			return b.session, nil
		}
	}
	session, err := b.Connect(SessionOptions{})
	if err != nil {
		return nil, err
	}
	b.session = session
	return session, nil
}

// Conn is the bot's own connection in package wire/bot's terms
// (Session.Conn), made again if the server closed it: typed requests with
// wire's params and results, such as Post with a wire.MessageParams, or
// History, whose page has every kind of record.
func (b *Bot) Conn() (*bot.Conn, error) {
	session, err := b.conn()
	if err != nil {
		return nil, err
	}
	return session.conn, nil
}

// Call sends a request on the bot's connection (Session.Call).
func (b *Bot) Call(ctx context.Context, method string, params any) (jsontext.Value, error) {
	session, err := b.conn()
	if err != nil {
		return nil, err
	}
	return session.Call(ctx, method, params)
}

// Post posts a message on the bot's connection (Session.Post).
func (b *Bot) Post(ctx context.Context, roomID, text string, options ...PostOption) (string, error) {
	session, err := b.conn()
	if err != nil {
		return "", err
	}
	return session.Post(ctx, roomID, text, options...)
}

// Join joins a room on the bot's connection (Session.Join).
func (b *Bot) Join(ctx context.Context, roomID string) error {
	session, err := b.conn()
	if err != nil {
		return err
	}
	return session.Join(ctx, roomID)
}

// History reads a room's messages on the bot's connection (Session.History).
func (b *Bot) History(ctx context.Context, roomID string, options HistoryOptions) ([]Message, error) {
	session, err := b.conn()
	if err != nil {
		return nil, err
	}
	return session.History(ctx, roomID, options)
}

// StreamAppend streams text into a message on the bot's connection
// (Session.StreamAppend).
func (b *Bot) StreamAppend(ctx context.Context, roomID, opening string, options ...StreamOption) (*LiveWriter, error) {
	session, err := b.conn()
	if err != nil {
		return nil, err
	}
	return session.StreamAppend(ctx, roomID, opening, options...)
}

// StreamTerminal streams a terminal on the bot's connection
// (Session.StreamTerminal).
func (b *Bot) StreamTerminal(ctx context.Context, roomID, title string, options ...StreamOption) (*LiveWriter, error) {
	session, err := b.conn()
	if err != nil {
		return nil, err
	}
	return session.StreamTerminal(ctx, roomID, title, options...)
}

// Session is one in-process connection of a Bot. It speaks the protocol
// (PROTOCOL.md) as a WebSocket client does, and gets the frames a client
// would: it is a wire.Local connection, and Conn is the same connection in
// package wire/bot's terms, whose requests are typed with wire's params and
// results. Its own methods, Post, Join, History, and the streams, are in
// apron's: message_ids as strings, and Messages. It sends one request at a
// time, in order; its methods are safe to call from any goroutine.
type Session struct {
	s    *Server
	bot  *Bot
	c    *client
	conn *bot.Conn
	// done is closed once the server has forgotten the connection, with
	// err set.
	done  chan struct{}
	errMu sync.Mutex
	err   error
}

// SessionOptions configure a Session.
type SessionOptions struct {
	// OnFrame receives the session's notifications, in order, on a
	// goroutine of the session's own: the frames that a client receives,
	// such as messages in the rooms the bot joined and room_update. nil
	// drops them. A handler that falls far behind closes the session with
	// ErrFellBehind, as a client that does not read is disconnected.
	OnFrame func(frame jsontext.Value)
}

// sessionFrameQueue bounds the notifications waiting for OnFrame.
const sessionFrameQueue = 1024

// Connect opens another connection for the bot, such as one that reads its
// frames with OnFrame, which Close ends. The bot's own requests use a
// connection of their own.
func (b *Bot) Connect(options SessionOptions) (*Session, error) {
	s := b.s
	c := &client{server: s}
	session := &Session{s: s, bot: b, c: c, done: make(chan struct{})}
	c.session = session
	local := wire.LocalOptions{
		User:  wire.User{UserID: b.userID},
		Data:  c,
		Queue: max(s.config.OutgoingQueue, sessionFrameQueue),
	}
	if options.OnFrame != nil {
		local.OnFrame = func(frame wire.Frame) { options.OnFrame(jsontext.Value(frame)) }
	}
	// wire opens the connection as a network one, calling backend.connected
	// (connectSession) and Hello, which take s.mu, so it is opened without
	// it.
	conn, err := bot.Dial(s.wire, local)
	switch {
	case errors.Is(err, wire.ErrClosed):
		return nil, ErrServerClosed
	case err != nil:
		return nil, err
	}
	session.conn = conn
	return session, nil
}

// connectSession signs a Session's connection in as its bot, as a network
// connection is signed in by auth, while wire opens it: frames sent to it
// meanwhile, such as those after a sign-in, go after its server frame.
func (s *Server) connectSession(c *client, wc *wire.Conn) error {
	b := c.session.bot
	s.mu.Lock()
	defer s.unlock()
	u := s.users.Get(b.userID)
	switch {
	case s.closed:
		return ErrServerClosed
	case u == nil || u.Bot != b.host.name:
		return fmt.Errorf("apron: %s is not extension %s's bot", b.userID, b.host.name)
	}
	c.conn = wc
	c.baseURL = s.sessionBaseURLLocked()
	c.request = s.extContext
	s.clients[c] = struct{}{}
	s.sessionCount++
	s.attachLocked(c, u)
	s.sendAfterAuthLocked(c)
	return nil
}

// forgotten records why the session closed, once the server has forgotten
// its connection (backend.disconnected). Whether it fell behind is read
// from its wire.Local when asked (Err), as wire may forget a connection
// that fails to open before Connect has it.
func (b *Session) forgotten(serverClosed bool) {
	if serverClosed {
		b.setErr(ErrServerClosed)
	}
	close(b.done)
}

// setErr records why the session closed, unless it was recorded already.
func (b *Session) setErr(err error) {
	b.errMu.Lock()
	if b.err == nil {
		b.err = err
	}
	b.errMu.Unlock()
}

// sessionBaseURLLocked is the base of the URLs a Session's connection mints:
// Config.PublicURL, else the latest WebSocket connection's when it is on a
// loopback host, as in development.
func (s *Server) sessionBaseURLLocked() string {
	if base, err := s.streamBaseURLLocked(); err == nil {
		return base
	}
	return "http://localhost"
}

// streamBaseURLLocked is the base of a Session's live text URLs, which
// people open. Without Config.PublicURL a session has no request to take
// the server's address from: the latest WebSocket connection's is used
// only on a loopback host, as one from elsewhere names whatever host its
// client said.
func (s *Server) streamBaseURLLocked() (string, error) {
	if s.config.PublicURL != "" {
		return s.config.PublicURL, nil
	}
	if parsed, err := url.Parse(s.seenBaseURL); err == nil && parsed.Host != "" {
		host := parsed.Hostname()
		if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
			return s.seenBaseURL, nil
		}
	}
	return "", errors.New("apron: a bot's live text needs Config.PublicURL (aprond --public-url), the address people open it at")
}

// UserID is the bot's user_id.
func (b *Session) UserID() string { return b.bot.userID }

// Conn is the session's connection in package wire/bot's terms, for
// requests typed with wire's params and results (bot.Conn.Post, Command,
// Join, History, and the raw Call). Its errors are wire's: a refusal as a
// *wire.Error, which is an *Error, and wire.ErrClosed once the session is
// closed, where the session's own methods return Err's. auth is refused:
// a session stays signed in as its bot.
func (b *Session) Conn() *bot.Conn { return b.conn }

// Done is closed once the session is closed, by Close or by the server.
func (b *Session) Done() <-chan struct{} { return b.done }

// Err reports why the session closed, once Done is closed:
// ErrSessionClosed after Close, ErrServerClosed at shutdown, ErrFellBehind,
// or nil while it is open.
func (b *Session) Err() error {
	select {
	case <-b.done:
	default:
		b.errMu.Lock()
		defer b.errMu.Unlock()
		return b.err
	}
	b.closing()
	b.errMu.Lock()
	defer b.errMu.Unlock()
	return b.err
}

// closing records why the session is closing, unless that was recorded
// already: it fell behind, or else it was closed.
func (b *Session) closing() {
	if errors.Is(b.conn.Err(), wire.ErrFellBehind) {
		b.setErr(ErrFellBehind)
	}
	b.setErr(ErrSessionClosed)
}

// Close ends the session and waits until the server forgot it. The bot's
// account stays.
func (b *Session) Close() error {
	b.closing()
	_ = b.conn.Close()
	<-b.done
	return nil
}

func (b *Session) closedErr() error {
	if errors.Is(b.conn.Err(), wire.ErrFellBehind) {
		return ErrFellBehind
	}
	if err := b.Err(); err != nil {
		return err
	}
	return ErrSessionClosed
}

// Call sends a request (PROTOCOL.md §1) and returns its result, or its
// error as an *Error. params is any value that encodes as a JSON object,
// or nil. auth is refused: a session stays signed in as its bot.
func (b *Session) Call(ctx context.Context, method string, params any) (jsontext.Value, error) {
	if method == "auth" {
		return nil, errors.New("apron: a session is signed in as its bot")
	}
	result, err := b.conn.Call(ctx, method, params)
	return result, b.requestErr(err)
}

// requestErr is the error of a request: why the session closed, once it
// has.
func (b *Session) requestErr(err error) error {
	if errors.Is(err, wire.ErrClosed) || errors.Is(err, wire.ErrFellBehind) {
		return b.closedErr()
	}
	return err
}

// postRequest is a message a Session posts, as options set it: the params
// of a new message, or of a command that streams live text.
type postRequest struct {
	params    wire.MessageParams
	extension string
	// extErr is the error of an ext value that is not JSON, and replyErr
	// of a reply_to that is no message_id.
	extErr, replyErr error
}

// check returns the error of a request that is not sent: one whose ext
// does not encode, or that the server refuses for its room_id, which it
// reads first, or its reply_to, with the server's errors. The room_id ""
// is refused, rather than left out for the default room.
func (p *postRequest) check() error {
	switch {
	case p.extErr != nil:
		return p.extErr
	case !wire.ValidID(p.params.RoomID):
		return InvalidParams("room_id must be an ID: not empty, without whitespace or control characters")
	}
	return p.replyErr
}

// PostOption sets a field of a message a Session posts.
type PostOption interface{ applyPost(p *postRequest) }

// StreamOption sets a field of the message live text is posted in.
type StreamOption interface{ applyStream(p *postRequest) }

// ReplyOption is an option of both posts and streams.
type ReplyOption interface {
	PostOption
	StreamOption
}

type postFunc func(p *postRequest)

func (f postFunc) applyPost(p *postRequest) { f(p) }

type replyTo string

func (r replyTo) applyPost(p *postRequest) {
	if r == "" {
		return
	}
	// The server's own error for a reply_to that names no message_id.
	id, err := wire.ParseLogID(string(r))
	if err != nil || id == 0 {
		p.replyErr = InvalidParams("reply_to.message_id must be a positive decimal string")
		return
	}
	p.params.ReplyTo = &wire.Ref{MessageID: id}
}

func (r replyTo) applyStream(p *postRequest) { r.applyPost(p) }

// ReplyTo makes the message a reply to messageID (§3.5.4); "" leaves it no
// reply.
func ReplyTo(messageID string) ReplyOption { return replyTo(messageID) }

// Markdown renders the message's text as Markdown (§3.5.2).
func Markdown() PostOption {
	return postFunc(func(p *postRequest) { p.params.Body.Format = wire.Markdown })
}

// Mention mentions users (§3.5.5), whom the text names as @user_id.
func Mention(userIDs ...string) PostOption {
	return postFunc(func(p *postRequest) {
		p.params.Body.Mentions = append(p.params.Body.Mentions, userIDs...)
	})
}

// WithExt writes value under the extension's name in the message's ext
// (§4.12), such as a bridge's ID for the message it relays.
func WithExt(value any) PostOption {
	return postFunc(func(p *postRequest) {
		encoded, err := wire.Marshal(value)
		if err != nil {
			p.extErr = err
			return
		}
		if p.params.Ext == nil {
			p.params.Ext = make(wire.Ext)
		}
		p.params.Ext[p.extension] = encoded
	})
}

// Post posts a message with text to the room and returns its message_id.
func (b *Session) Post(ctx context.Context, roomID, text string, options ...PostOption) (string, error) {
	p := b.post(roomID, text)
	for _, option := range options {
		option.applyPost(p)
	}
	if err := p.check(); err != nil {
		return "", err
	}
	result, err := b.conn.Post(ctx, p.params)
	if err != nil || result.MessageID == 0 {
		return "", b.requestErr(err)
	}
	return result.MessageID.String(), nil
}

// post starts a message with text to the room.
func (b *Session) post(roomID, text string) *postRequest {
	return &postRequest{params: wire.MessageParams{RoomID: roomID, Body: &wire.Body{Text: text}}, extension: b.bot.host.name}
}

// Join joins the room (§4.2.2).
func (b *Session) Join(ctx context.Context, roomID string) error {
	return b.requestErr(b.conn.Join(ctx, roomID))
}

// HistoryOptions bound a History page (§4.3): records after After, before
// Before, at most Limit (0 for the server's default page).
type HistoryOptions struct {
	After, Before int64
	Limit         int
}

// History returns the room's message records in a page of its log, oldest
// first, as the bot may read them (§4.3). A message edited within the page
// has a record for each snapshot. Conn's History reads the page in wire's
// terms, with its other records and bounds.
func (b *Session) History(ctx context.Context, roomID string, options HistoryOptions) ([]Message, error) {
	params := wire.HistoryParams{RoomID: roomID, Limit: max(options.Limit, 0)}
	if options.After > 0 {
		after := wire.LogID(options.After)
		params.After = &after
	}
	if options.Before > 0 {
		before := wire.LogID(options.Before)
		params.Before = &before
	}
	page, err := b.conn.History(ctx, params)
	if err != nil {
		return nil, b.requestErr(err)
	}
	messages := make([]Message, len(page.Messages))
	for i, m := range page.Messages {
		messages[i] = MessageOf(m)
	}
	return messages, nil
}

// MessageOf is a message as package wire has it, such as a record of a
// history page that Conn reads, or a message a wire/bot handler gets, as
// extensions see messages (Message): the same snapshot, with its IDs as
// Message has them, its body's text, format, and mentions in their own
// fields, and the whole body, those of its members wire has no field for
// too, decoded into Body.
func MessageOf(m wire.Message) Message {
	message := Message{
		LogID: int64(m.LogID), RoomID: m.RoomID, PrevLogID: int64(m.PrevLogID), PrevRoomID: m.PrevRoomID,
		From: Sender{UserID: m.From.UserID, Name: m.From.Name}, Deleted: m.Deleted, Ext: maps.Clone(m.Ext),
	}
	if m.MessageID != 0 {
		message.ID = m.MessageID.String()
	}
	if m.ReplyTo != nil {
		message.ReplyTo = m.ReplyTo.MessageID.String()
	}
	if body := m.Body; body != nil {
		message.Body = bodyObject(body)
		message.Text, message.Format = body.Text, cmp.Or(body.Format, wire.Plain)
		message.Mentions = slices.Clone(body.Mentions)
	}
	return message
}

// Wire is the message as package wire has it, as MessageOf reads it: its
// body is Body, or, for a Message made without one, Text, Format, and
// Mentions, unless it is deleted. An ID that is not a log_id is left out.
func (m Message) Wire() wire.Message {
	message := wire.Message{
		LogID: wire.LogID(m.LogID), RoomID: m.RoomID, PrevLogID: wire.LogID(m.PrevLogID), PrevRoomID: m.PrevRoomID,
		From: wire.User{UserID: m.From.UserID, Name: m.From.Name}, Deleted: m.Deleted, Ext: maps.Clone(m.Ext),
	}
	message.MessageID, _ = wire.ParseLogID(m.ID)
	if id, err := wire.ParseLogID(m.ReplyTo); err == nil && id != 0 {
		message.ReplyTo = &wire.Ref{MessageID: id}
	}
	switch {
	case m.Body != nil:
		var body wire.Body
		if json.Unmarshal(encodeJSON(m.Body), &body) == nil {
			message.Body = &body
		}
	case !m.Deleted:
		message.Body = &wire.Body{Text: m.Text, Format: m.Format, Mentions: slices.Clone(m.Mentions)}
	}
	return message
}

// StreamAppend posts a message to the room that starts with opening
// (Markdown) and grows as text is written to the returned LiveWriter
// (§4.8.5), as /embed-append does. The message is posted at the first
// Write; Close ends the text, which then becomes part of the message. Once
// the text ended otherwise, such as at Config.LiveTextMaxBytes, Write
// returns ErrLiveTextEnded. Live text needs Config.PublicURL, the address
// people open it at, except on a loopback host in development.
func (b *Session) StreamAppend(ctx context.Context, roomID, opening string, options ...StreamOption) (*LiveWriter, error) {
	return b.stream(ctx, roomID, "/embed-append", opening, options)
}

// StreamTerminal posts a terminal with this title to the room, an 80×40
// view of what is written to the returned LiveWriter (§4.8.6), as
// /embed-terminal does. Write a program's output to it, and Close it when
// the program ends. It needs Config.PublicURL as StreamAppend does.
func (b *Session) StreamTerminal(ctx context.Context, roomID, title string, options ...StreamOption) (*LiveWriter, error) {
	return b.stream(ctx, roomID, "/embed-terminal", title, options)
}

func (b *Session) stream(ctx context.Context, roomID, command, args string, options []StreamOption) (*LiveWriter, error) {
	p := b.post(roomID, strings.TrimSpace(command+" "+args))
	for _, option := range options {
		option.applyStream(p)
	}
	s := b.s
	s.mu.Lock()
	base, err := s.streamBaseURLLocked()
	if err == nil {
		b.c.baseURL = base
	}
	s.unlock()
	if err != nil {
		return nil, err
	}
	if err := p.check(); err != nil {
		return nil, err
	}
	result, err := b.conn.Command(ctx, wire.CommandParams(p.params))
	if err != nil {
		return nil, b.requestErr(err)
	}
	if len(result.Embeds) != 1 {
		return nil, errors.New("apron: the server returned no write URL")
	}
	_, token, ok := strings.Cut(result.Embeds[0].WriteURL, writePath)
	if !ok {
		return nil, errors.New("apron: the server returned no write URL")
	}
	return &LiveWriter{s: s, token: token}, nil
}

// ErrLiveTextEnded is returned by Write to live text that ended: it reached
// a limit (Config.LiveTextMaxBytes, LiveTextMaxDuration), its message was
// saved without it, or its bot lost access to the room or was signed out,
// which keeps what it wrote. Close it still.
var ErrLiveTextEnded = errors.New("apron: the live text ended")

// LiveWriter writes live text into a message, in-process (§4.8.3). It is
// an io.WriteCloser, which one goroutine writes to at a time; close it when
// the text is done, or it stays live until Config.LiveTextMaxDuration.
type LiveWriter struct {
	s     *Server
	token string

	mu     sync.Mutex
	e      *embedState
	live   *livetext.Text
	err    error
	closed bool
}

// startLocked admits the writer at its first Write, which posts its
// message (§4.8.5), within Config.UploadStartTimeout of the stream's start.
// It is called holding w.mu.
func (w *LiveWriter) startLocked() error {
	if w.e != nil || w.err != nil {
		return w.err
	}
	s := w.s
	s.mu.Lock()
	defer s.unlock()
	e := s.embeds.Write(w.token)
	if e == nil || e.removed || e.Finished || e.live == nil {
		w.err = errors.New("apron: the live text's write expired; write within Config.UploadStartTimeout of starting it")
		return w.err
	}
	if status, message := s.startWriterLocked(e); status != 0 {
		w.err = errors.New("apron: " + message)
		return w.err
	}
	w.e, w.live = e, e.live
	return nil
}

// MessageID is the message the text is in, once the first Write posted it.
func (w *LiveWriter) MessageID() string {
	w.mu.Lock()
	e := w.e
	w.mu.Unlock()
	if e == nil {
		return ""
	}
	w.s.mu.RLock()
	defer w.s.mu.RUnlock()
	return e.MessageID
}

// Write adds p to the text. Once the text ended, by Close or otherwise,
// such as at a limit, it returns ErrLiveTextEnded, and writing again does
// not change that: stop writing, and Close the writer still.
func (w *LiveWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	err := w.startLocked()
	live, closed := w.live, w.closed
	w.mu.Unlock()
	switch {
	case err != nil:
		return 0, err
	case closed:
		return 0, ErrLiveTextEnded
	case !live.Write(p):
		return 0, ErrLiveTextEnded
	}
	return len(p), nil
}

// Close ends the text and publishes its message's final snapshot. Live
// text that was never written to posts nothing.
func (w *LiveWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.e == nil || w.closed {
		return w.err
	}
	w.closed = true
	s := w.s
	s.mu.Lock()
	defer s.unlock()
	w.e.writing = false
	if !w.e.removed {
		s.finishLiveLocked(w.e)
	}
	return nil
}
