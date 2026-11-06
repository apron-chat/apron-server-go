package apron

import (
	"context"
	"crypto/rand"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Errors of Sessions, which Session.Err reports and their requests return.
var (
	// ErrSessionClosed is returned by the requests of a Session that Close
	// closed.
	ErrSessionClosed = errors.New("apron: session closed")
	// ErrFellBehind is why the server closed a Session whose OnFrame fell
	// too far behind its frames, as it disconnects a client that does not
	// read.
	ErrFellBehind = errors.New("apron: the session's frame handler fell behind")
	// ErrReentrant is returned by a request a Session is asked to send
	// while the request it is sending waits on the caller, such as from a
	// filter or command that the session's own message runs: the session
	// sends one request at a time. Send it with another Session
	// (Bot.Connect).
	ErrReentrant = errors.New("apron: the session is waiting on this call; use another session")
)

// Bot is an account an extension owns and speaks through: it holds the
// "bot" role (§3.3.4), and is never a person's or another extension's. Its
// requests (Post, Join, History, Call, StreamAppend, StreamTerminal) go
// through a connection of its own, made again if the server closed it, so
// they get the same checks, limits, delivery, and push as a client's. A
// Bot is safe for concurrent use.
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
	u := s.users[userID]
	switch {
	case u != nil && u.botOf == r.name:
	case u != nil && u.botOf != "":
		s.unlock()
		return nil, fmt.Errorf("apron: user_id %q is extension %s's bot", userID, u.botOf)
	case u != nil:
		s.unlock()
		return nil, fmt.Errorf("apron: user_id %q belongs to someone who is not a bot", userID)
	default:
		key := strings.ToLower(userID)
		if s.usedIDs[key] || s.roomNamedLocked(userID) {
			s.unlock()
			return nil, fmt.Errorf("apron: user_id %q was used by someone else", userID)
		}
		s.usedIDs[key] = true
		s.touchUsedID(key)
		u = newUserState(userID, normalizeName(options.Name))
		u.botOf = r.name
		s.users[userID] = u
		s.touchUser(userID)
		s.grantRolesLocked(u)
		s.joinDefaultRoomLocked(u)
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
// would. It sends one request at a time, in order; its methods are safe to
// call from any goroutine.
type Session struct {
	s   *Server
	bot *Bot
	c   *client

	// turn holds a token while a request is processed, as a connection's
	// frames are processed one at a time. Request ids are prefix and a
	// number, unique across the bot's sessions, whose requests share
	// deduplication (§1.2).
	turn    chan struct{}
	prefix  string
	nextID  atomic.Uint64
	pending sync.Map // request id → chan sessionReply
	frames  chan jsontext.Value
	// closed is closed with the transport; done once the server has
	// forgotten the connection, with err set.
	closed    chan struct{}
	closeOnce sync.Once
	done      chan struct{}
	errMu     sync.Mutex
	err       error
}

type sessionReply struct {
	result jsontext.Value
	err    *Error
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

// inFlight keys the context of the request a Session is sending, so that a
// request on the same session from code that request runs is refused.
type inFlight struct{ s *Session }

// Connect opens another connection for the bot, such as one that reads its
// frames with OnFrame, which Close ends. The bot's own requests use a
// connection of their own.
func (b *Bot) Connect(options SessionOptions) (*Session, error) {
	s := b.s
	session := &Session{
		s: s, bot: b, prefix: rand.Text()[:12] + "-",
		turn: make(chan struct{}, 1), closed: make(chan struct{}), done: make(chan struct{}),
	}
	c := &client{
		server: s,
		conn:   sessionTransport{session},
		out:    make(chan outboundBatch, max(s.config.OutgoingQueue, sessionFrameQueue)),
		done:   make(chan struct{}),
	}
	session.c = c
	if options.OnFrame != nil {
		session.frames = make(chan jsontext.Value, sessionFrameQueue)
	}

	s.mu.Lock()
	defer s.unlock()
	u := s.users[b.userID]
	switch {
	case s.closed:
		return nil, ErrServerClosed
	case u == nil || u.botOf != b.host.name:
		return nil, fmt.Errorf("apron: %s is not extension %s's bot", b.userID, b.host.name)
	}
	c.baseURL = s.sessionBaseURLLocked()
	// The session's goroutines are extension work, which Shutdown waits for.
	work := 1
	if options.OnFrame != nil {
		work++
	}
	s.extensionWork.Add(work)
	if options.OnFrame != nil {
		go func() {
			defer s.extensionWork.Done()
			for {
				select {
				case frame := <-session.frames:
					options.OnFrame(frame)
				case <-session.closed:
					return
				}
			}
		}()
	}
	go func() {
		defer s.extensionWork.Done()
		<-c.done
		session.forget()
	}()
	go c.writeLoop()
	s.clients[c] = struct{}{}
	s.sessionCount++
	s.attachLocked(c, u)
	c.enqueue(s.serverFrameLocked(c))
	s.sendAfterAuthLocked(c)
	return session, nil
}

// forget removes the closed session's connection from the server, as a
// WebSocket's is when it closes, and records why it closed.
func (b *Session) forget() {
	s := b.s
	s.mu.Lock()
	if _, ok := s.clients[b.c]; ok {
		delete(s.clients, b.c)
		s.sessionCount--
		s.detachLocked(b.c)
	}
	closed := s.closed
	s.unlock()
	if closed {
		b.setErr(ErrServerClosed)
	}
	b.setErr(ErrSessionClosed)
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

// Done is closed once the session is closed, by Close or by the server.
func (b *Session) Done() <-chan struct{} { return b.done }

// Err reports why the session closed, once Done is closed:
// ErrSessionClosed after Close, ErrServerClosed at shutdown, ErrFellBehind,
// or nil while it is open.
func (b *Session) Err() error {
	b.errMu.Lock()
	defer b.errMu.Unlock()
	return b.err
}

// Close ends the session and waits until the server forgot it. The bot's
// account stays.
func (b *Session) Close() error {
	b.setErr(ErrSessionClosed)
	b.c.stopConnection()
	<-b.done
	return nil
}

// sessionTransport delivers a Session's frames in-process.
type sessionTransport struct{ session *Session }

func (t sessionTransport) write(_ context.Context, payload []byte) error {
	return t.session.receive(payload)
}

func (t sessionTransport) ping(context.Context) error { return nil }

func (t sessionTransport) close() {
	t.session.closeOnce.Do(func() { close(t.session.closed) })
}

// receive routes a frame: a reply to the request that waits for it, and a
// notification to OnFrame.
func (b *Session) receive(payload []byte) error {
	var frame struct {
		ID     *string        `json:"id"`
		Method string         `json:"method"`
		Result jsontext.Value `json:"result"`
		Error  *Error         `json:"error"`
	}
	if err := json.Unmarshal(payload, &frame); err != nil {
		return err
	}
	if frame.ID != nil && frame.Method == "" {
		if waiting, ok := b.pending.LoadAndDelete(*frame.ID); ok {
			waiting.(chan sessionReply) <- sessionReply{frame.Result, frame.Error}
		}
		return nil
	}
	if b.frames == nil {
		return nil
	}
	select {
	case b.frames <- jsontext.Value(payload):
		return nil
	default:
		b.setErr(ErrFellBehind)
		return ErrFellBehind
	}
}

// closedErr is the error of a request on a closed session.
func (b *Session) closedErr() error {
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
	if ctx.Value(inFlight{b}) != nil {
		return nil, ErrReentrant
	}
	id := b.prefix + strconv.FormatUint(b.nextID.Add(1), 10)
	frame := map[string]any{"method": method, "id": id}
	if params != nil {
		frame["params"] = params
	}
	payload, err := json.Marshal(frame, jsonOptions)
	if err != nil {
		return nil, err
	}
	reply := make(chan sessionReply, 1)
	b.pending.Store(id, reply)
	defer b.pending.Delete(id)
	select {
	case b.turn <- struct{}{}:
	case <-b.closed:
		return nil, b.closedErr()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-b.closed:
		<-b.turn
		return nil, b.closedErr()
	default:
	}
	b.c.request = context.WithValue(ctx, inFlight{b}, true)
	b.s.processFrame(b.c, payload)
	b.c.request = nil
	<-b.turn
	select {
	case r := <-reply:
		if r.err != nil {
			return nil, r.err
		}
		return r.result, nil
	case <-b.closed:
		return nil, b.closedErr()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// postRequest is a message a Session posts, as options set it.
type postRequest struct {
	params    map[string]any
	body      map[string]any
	extension string
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
	if r != "" {
		p.params["reply_to"] = map[string]any{"message_id": string(r)}
	}
}

func (r replyTo) applyStream(p *postRequest) { r.applyPost(p) }

// ReplyTo makes the message a reply to messageID (§3.5.4); "" leaves it no
// reply.
func ReplyTo(messageID string) ReplyOption { return replyTo(messageID) }

// Markdown renders the message's text as Markdown (§3.5.2).
func Markdown() PostOption {
	return postFunc(func(p *postRequest) { p.body["format"] = "markdown" })
}

// Mention mentions users (§3.5.5), whom the text names as @user_id.
func Mention(userIDs ...string) PostOption {
	return postFunc(func(p *postRequest) {
		mentioned, _ := p.body["mentions"].([]any)
		for _, id := range userIDs {
			mentioned = append(mentioned, id)
		}
		p.body["mentions"] = mentioned
	})
}

// WithExt writes value under the extension's name in the message's ext
// (§4.12), such as a bridge's ID for the message it relays.
func WithExt(value any) PostOption {
	return postFunc(func(p *postRequest) {
		ext, _ := p.params["ext"].(map[string]any)
		if ext == nil {
			ext = make(map[string]any)
			p.params["ext"] = ext
		}
		ext[p.extension] = value
	})
}

// Post posts a message with text to the room and returns its message_id.
func (b *Session) Post(ctx context.Context, roomID, text string, options ...PostOption) (string, error) {
	p := &postRequest{body: map[string]any{"text": text}, extension: b.bot.host.name}
	p.params = map[string]any{"room_id": roomID, "body": p.body}
	for _, option := range options {
		option.applyPost(p)
	}
	raw, err := b.Call(ctx, "message", p.params)
	if err != nil {
		return "", err
	}
	var result struct {
		MessageID string `json:"message_id"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", err
	}
	return result.MessageID, nil
}

// Join joins the room (§4.2.2).
func (b *Session) Join(ctx context.Context, roomID string) error {
	_, err := b.Call(ctx, "room_join", map[string]any{"room_id": roomID})
	return err
}

// HistoryOptions bound a History page (§4.3): records after After, before
// Before, at most Limit (0 for the server's default page).
type HistoryOptions struct {
	After, Before int64
	Limit         int
}

// History returns the room's message records in a page of its log, oldest
// first, as the bot may read them (§4.3). A message edited within the page
// has a record for each snapshot.
func (b *Session) History(ctx context.Context, roomID string, options HistoryOptions) ([]Message, error) {
	params := map[string]any{"room_id": roomID}
	if options.After > 0 {
		params["after"] = strconv.FormatInt(options.After, 10)
	}
	if options.Before > 0 {
		params["before"] = strconv.FormatInt(options.Before, 10)
	}
	if options.Limit > 0 {
		params["limit"] = options.Limit
	}
	raw, err := b.Call(ctx, "history", params)
	if err != nil {
		return nil, err
	}
	var result struct {
		Messages []jsontext.Value `json:"messages"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	messages := make([]Message, 0, len(result.Messages))
	for _, record := range result.Messages {
		if m, ok := decodeMessageRecord(record); ok {
			messages = append(messages, m)
		}
	}
	return messages, nil
}

// StreamAppend posts a message to the room that starts with opening
// (Markdown) and grows as text is written to the returned LiveWriter
// (§4.8.5), as /embed-append does. The message is posted at the first
// Write; Close ends the text, which then becomes part of the message. Live
// text needs Config.PublicURL, the address people open it at, except on a
// loopback host in development.
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
	p := &postRequest{body: map[string]any{"text": strings.TrimSpace(command + " " + args)}, extension: b.bot.host.name}
	p.params = map[string]any{"room_id": roomID, "body": p.body}
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
	raw, err := b.Call(ctx, "command", p.params)
	if err != nil {
		return nil, err
	}
	var result struct {
		Embeds []struct {
			WriteURL string `json:"write_url"`
		} `json:"embeds"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || len(result.Embeds) != 1 {
		return nil, errors.New("apron: the server returned no write URL")
	}
	_, token, ok := strings.Cut(result.Embeds[0].WriteURL, writePath)
	if !ok {
		return nil, errors.New("apron: the server returned no write URL")
	}
	return &LiveWriter{s: s, token: token}, nil
}

// ErrLiveTextEnded is returned by Write to live text that ended: it reached
// a limit (Config.LiveTextMaxBytes, LiveTextMaxDuration), or its message
// was saved without it. Close it still.
var ErrLiveTextEnded = errors.New("apron: the live text ended")

// LiveWriter writes live text into a message, in-process (§4.8.3). It is
// an io.WriteCloser, which one goroutine writes to at a time; close it when
// the text is done, or it stays live until Config.LiveTextMaxDuration.
type LiveWriter struct {
	s     *Server
	token string

	mu     sync.Mutex
	e      *embedState
	live   *liveText
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
	e := s.writes[w.token]
	if e == nil || e.removed || e.finished || e.live == nil {
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
	return e.messageID
}

// Write adds p to the text.
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
	case !live.write(p):
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
