package apron

import (
	"context"
	"crypto/rand"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// ErrSessionClosed is returned by the requests of a Session that is closed,
// or that the server closed, as at shutdown.
var ErrSessionClosed = errors.New("apron: session closed")

// Session is an in-process connection signed in as an extension's bot. It
// speaks the protocol (PROTOCOL.md) as a WebSocket client does, so its
// requests get the same checks, limits, delivery, and push, and it gets
// the frames a client would. Its methods are safe to call from any
// goroutine, and from its frame handler.
type Session struct {
	s      *Server
	host   *Host
	c      *client
	userID string

	// mu sends one request at a time, as a connection's frames are
	// processed in order. Request ids are prefix and a number, unique
	// across the bot's sessions, whose requests share deduplication (§1.2).
	mu      sync.Mutex
	prefix  string
	pending sync.Map // request id → chan sessionReply
	nextID  atomic.Uint64
	frames  chan jsontext.Value
	closed  chan struct{}
	close   sync.Once
}

type sessionReply struct {
	result jsontext.Value
	err    *Error
}

// SessionOptions configure a bot's Session.
type SessionOptions struct {
	// Name is the bot's display name when the account is made.
	Name string
	// OnFrame receives the session's notifications, in order, on a
	// goroutine of the session's own: the frames that a client receives,
	// such as messages in the rooms the bot joined and room_update. nil
	// drops them. A handler that falls far behind closes the session, as a
	// client that does not read is disconnected.
	OnFrame func(frame jsontext.Value)
}

// sessionFrameQueue bounds the notifications waiting for OnFrame.
const sessionFrameQueue = 1024

// Session signs in a bot with this user_id, making its account the first
// time, and returns a connection for it. A bot is an account that holds
// the "bot" role (§3.3.4) and joins the default room when it is made. A
// user_id that a person or another bot has is refused. Each call opens
// another connection, which Close ends.
func (h *Host) Session(ctx context.Context, userID string, options SessionOptions) (*Session, error) {
	s := h.s
	if !validID(userID) || strings.HasPrefix(userID, "~") || !requestableUserID.MatchString(userID) {
		return nil, fmt.Errorf("apron: %q is no user_id a bot can have", userID)
	}
	session := &Session{s: s, host: h, userID: userID, prefix: rand.Text()[:12] + "-", closed: make(chan struct{})}
	c := &client{
		server: s,
		conn:   sessionTransport{session},
		out:    make(chan outboundBatch, max(s.config.OutgoingQueue, sessionFrameQueue)),
		done:   make(chan struct{}),
	}
	session.c = c
	if options.OnFrame != nil {
		session.frames = make(chan jsontext.Value, sessionFrameQueue)
		go func() {
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

	s.mu.Lock()
	defer s.unlock()
	if s.closed {
		return nil, ErrSessionClosed
	}
	u := s.users[userID]
	made := u == nil
	switch {
	case u != nil && !u.bot:
		return nil, fmt.Errorf("apron: user_id %q belongs to someone who is not a bot", userID)
	case u == nil:
		key := strings.ToLower(userID)
		if s.usedIDs[key] || s.roomNamedLocked(userID) {
			return nil, fmt.Errorf("apron: user_id %q is taken", userID)
		}
		s.usedIDs[key] = true
		s.touchUsedID(key)
		u = newUserState(userID, normalizeName(options.Name))
		u.bot = true
		s.users[userID] = u
		s.touchUser(userID)
		s.grantRolesLocked(u)
	}
	c.baseURL = s.sessionBaseURLLocked()
	go c.writeLoop()
	s.clients[c] = struct{}{}
	c.enqueue(map[string]any{"method": "server", "params": s.serverParams(c.baseURL)})
	s.attachLocked(c, u)
	if made {
		s.joinDefaultRoomLocked(u)
	}
	s.sendAfterAuthLocked(c)
	return session, nil
}

// sessionBaseURLLocked is the base of the URLs a Session mints, such as its
// live text's: Config.PublicURL, else the latest WebSocket connection's,
// as no request names the server's address to it.
func (s *Server) sessionBaseURLLocked() string {
	switch {
	case s.config.PublicURL != "":
		return s.config.PublicURL
	case s.seenBaseURL != "":
		return s.seenBaseURL
	}
	return "http://localhost"
}

// UserID is the bot's user_id.
func (b *Session) UserID() string { return b.userID }

// Close signs the session out and ends it. The bot's account stays.
func (b *Session) Close() error {
	s := b.s
	s.mu.Lock()
	if _, ok := s.clients[b.c]; ok {
		delete(s.clients, b.c)
		s.detachLocked(b.c)
	}
	s.unlock()
	b.c.stopConnection()
	return nil
}

// sessionTransport delivers a Session's frames in-process.
type sessionTransport struct{ session *Session }

func (t sessionTransport) write(_ context.Context, payload []byte) error {
	return t.session.receive(payload)
}

func (t sessionTransport) ping(context.Context) error { return nil }

func (t sessionTransport) close() {
	t.session.close.Do(func() { close(t.session.closed) })
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
		return errors.New("the session's frame handler fell behind")
	}
}

// Call sends a request (PROTOCOL.md §1) and returns its result, or its
// error as an *Error. params is any value that encodes as a JSON object,
// or nil. auth is refused: a session stays signed in as its bot.
func (b *Session) Call(ctx context.Context, method string, params any) (jsontext.Value, error) {
	if method == "auth" {
		return nil, errors.New("apron: a session is signed in as its bot")
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
	b.mu.Lock()
	select {
	case <-b.closed:
		b.mu.Unlock()
		return nil, ErrSessionClosed
	default:
	}
	b.s.processFrame(b.c, payload)
	b.mu.Unlock()
	select {
	case r := <-reply:
		if r.err != nil {
			return nil, r.err
		}
		return r.result, nil
	case <-b.closed:
		return nil, ErrSessionClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// PostOption sets a field of a message a Session posts.
type PostOption func(params map[string]any)

// ReplyTo makes the message a reply to messageID (§3.5.4).
func ReplyTo(messageID string) PostOption {
	return func(params map[string]any) { params["reply_to"] = messageID }
}

// Markdown renders the message's text as Markdown (§3.5.2).
func Markdown() PostOption {
	return func(params map[string]any) { params["body"].(map[string]any)["format"] = "markdown" }
}

// Mention mentions users (§3.5.5), who the text names as @user_id.
func Mention(userIDs ...string) PostOption {
	return func(params map[string]any) {
		body := params["body"].(map[string]any)
		mentioned, _ := body["mentions"].([]any)
		for _, id := range userIDs {
			mentioned = append(mentioned, id)
		}
		body["mentions"] = mentioned
	}
}

// WithExt writes value under key in the message's ext (§4.12), such as a
// bridge's ID for the message it relays.
func WithExt(key string, value any) PostOption {
	return func(params map[string]any) {
		ext, _ := params["ext"].(map[string]any)
		if ext == nil {
			ext = make(map[string]any)
			params["ext"] = ext
		}
		ext[key] = value
	}
}

// Post posts a message with text to the room and returns its message_id.
func (b *Session) Post(ctx context.Context, roomID, text string, options ...PostOption) (string, error) {
	params := map[string]any{"room_id": roomID, "body": map[string]any{"text": text}}
	for _, option := range options {
		option(params)
	}
	raw, err := b.Call(ctx, "message", params)
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
// Write; Close ends the text, which then becomes part of the message.
// Options other than ReplyTo are ignored.
func (b *Session) StreamAppend(ctx context.Context, roomID, opening string, options ...PostOption) (*LiveWriter, error) {
	return b.stream(ctx, roomID, "/embed-append", opening, options)
}

// StreamTerminal posts a terminal with this title to the room, an 80×40
// view of what is written to the returned LiveWriter (§4.8.6), as
// /embed-terminal does. Write a program's output to it, and Close it when
// the program ends.
func (b *Session) StreamTerminal(ctx context.Context, roomID, title string, options ...PostOption) (*LiveWriter, error) {
	return b.stream(ctx, roomID, "/embed-terminal", title, options)
}

func (b *Session) stream(ctx context.Context, roomID, command, args string, options []PostOption) (*LiveWriter, error) {
	params := map[string]any{"room_id": roomID, "body": map[string]any{"text": strings.TrimSpace(command + " " + args)}}
	for _, option := range options {
		option(params)
	}
	for key := range params {
		if key != "room_id" && key != "body" && key != "reply_to" {
			delete(params, key)
		}
	}
	params["body"] = map[string]any{"text": params["body"].(map[string]any)["text"]}
	s := b.s
	s.mu.Lock()
	b.c.baseURL = s.sessionBaseURLLocked()
	s.unlock()
	raw, err := b.Call(ctx, "command", params)
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
// an io.WriteCloser; one goroutine writes to it at a time.
type LiveWriter struct {
	s     *Server
	token string
	e     *embedState
	live  *liveText
	err   error
	close sync.Once
}

// start admits the writer at its first Write, which posts its message
// (§4.8.5), within Config.UploadStartTimeout of the stream's start.
func (w *LiveWriter) start() error {
	if w.e != nil || w.err != nil {
		return w.err
	}
	s := w.s
	s.mu.Lock()
	defer s.unlock()
	e := s.writes[w.token]
	if e == nil || e.removed || e.finished || e.live == nil {
		w.err = errors.New("apron: the write URL is unknown, used, or expired")
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
	if w.e == nil {
		return ""
	}
	w.s.mu.RLock()
	defer w.s.mu.RUnlock()
	return w.e.messageID
}

// Write adds p to the text.
func (w *LiveWriter) Write(p []byte) (int, error) {
	if err := w.start(); err != nil {
		return 0, err
	}
	if !w.live.write(p) {
		return 0, ErrLiveTextEnded
	}
	return len(p), nil
}

// Close ends the text and publishes its message's final snapshot. Live
// text that was never written to posts nothing.
func (w *LiveWriter) Close() error {
	if w.e == nil {
		return w.err
	}
	w.close.Do(func() {
		s := w.s
		s.mu.Lock()
		defer s.unlock()
		w.e.writing = false
		if !w.e.removed {
			s.finishLiveLocked(w.e)
		}
	})
	return nil
}
