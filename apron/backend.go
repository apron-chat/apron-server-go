package apron

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"maps"
	"net/http"

	"github.com/apron-chat/apron-server-go/wire"
)

// The server speaks the protocol through package wire, as any server built
// on it does: wire carries the connections, frames, sign-in order, and
// deduplication, and backend answers for the Server, as its Backend and
// each of its handlers. The Server's other operations are wire methods
// (registerOp). Each replies while it holds s.mu where its result describes
// state (request.reply), or, for me, with its typed reply.

// backend is the Server as wire's Backend and handlers, kept unexported so
// that wire's interfaces are not part of the Server's API.
type backend struct{ s *Server }

// newWire makes the Server's wire server, and registers its operations.
func (s *Server) newWire() {
	config := s.config
	b := backend{s}
	patterns := wire.OriginPatterns(config.OriginPatterns...)
	s.wire = &wire.Server{
		Backend:   b,
		Messages:  b,
		Edits:     b,
		Commands:  b,
		Profiles:  b,
		Rooms:     b,
		History:   b,
		Status:    b,
		Reactions: b,
		Activity:  b,

		OnConnect:        b.connected,
		OnDisconnect:     b.disconnected,
		RefreshDuplicate: b.refresh,
		// Every scheme goes through to Auth, which knows the server's own
		// rules, such as a service token's sign-in where no scheme offers
		// tokens.
		CheckScheme: func(context.Context, *wire.Conn, wire.AuthParams) error { return nil },
		CheckOrigin: func(r *http.Request) bool {
			// Native apps' passkey origins (§4.10) open WebSockets too.
			return config.AllowAnyOrigin || patterns(r) || s.nativeOrigin(r.Header.Get("Origin"))
		},

		ReadLimit:      config.ReadLimit,
		OutgoingQueue:  config.OutgoingQueue,
		WriteTimeout:   config.WriteTimeout,
		PingInterval:   config.PingInterval,
		PingTimeout:    config.PingTimeout,
		MaxConnections: config.MaxConnections,
		DedupSize:      maxDedupEntries,
		Logger:         s.logger,
		FenceFailed:    internalError(),
		ShutdownError:  &Error{Code: CodeRetryAfter, Message: "Server is shutting down; reconnect shortly", Data: map[string]any{"retry_after": 5}},
	}
	for name, op := range s.ops {
		s.registerOp(name, op)
	}
}

// registerOp makes an operation a wire method.
func (s *Server) registerOp(name string, op operation) {
	s.wire.Handle(name, wire.Handler{
		NoDedup: readOnlyOps[name],
		Serve: func(ctx context.Context, wc *wire.Conn, r *wire.Request) error {
			return serveOp(ctx, wc, r, op, s)
		},
	})
}

// serveOp runs an operation for a request: its result goes out as it
// replies (request.reply), or once it returns.
func serveOp(ctx context.Context, wc *wire.Conn, r *wire.Request, op operation, s *Server) error {
	c, ok := wire.DataOf[*client](wc)
	if !ok {
		return internalError()
	}
	// Extension code the request runs gets its context (Host).
	previous := c.request
	c.request = ctx
	defer func() { c.request = previous }()
	req := newRequest(r)
	result, replied, err := op(s, c, req)
	switch {
	case err != nil:
		return err
	case !replied:
		req.reply(result)
	}
	return nil
}

// run runs an operation for one of wire's typed handlers, raw, as
// registerOp does for a method: it replies through reply's request, which
// the Server's operations do while they hold s.mu.
func run[T any](b backend, ctx context.Context, wc *wire.Conn, reply wire.Reply[T], op func(c *client, req request) (any, bool, *Error)) error {
	return serveOp(ctx, wc, reply.Request(), func(_ *Server, c *client, req request) (any, bool, *Error) {
		return op(c, req)
	}, b.s)
}

// connected keeps a new WebSocket's client.
func (b backend) connected(_ context.Context, wc *wire.Conn) error {
	s := b.s
	r := wc.HTTPRequest()
	c := &client{server: s, conn: wc}
	if r != nil {
		c.origin = r.Header.Get("Origin")
		c.clientKey = clientKey(s.clientIP(r))
		c.baseURL = s.baseURL(r)
	}
	c.request = wc.Context()
	wc.SetData(c)
	s.mu.Lock()
	defer s.unlock()
	if s.closed {
		return ErrServerClosed
	}
	s.clients[c] = struct{}{}
	s.seenBaseURL = c.baseURL
	return nil
}

// disconnected forgets a closed connection, network or session.
func (b backend) disconnected(wc *wire.Conn) {
	c, ok := wire.DataOf[*client](wc)
	if !ok {
		return
	}
	s := b.s
	s.mu.Lock()
	_, known := s.clients[c]
	if known {
		delete(s.clients, c)
		s.detachLocked(c)
		if c.session != nil {
			s.sessionCount--
		}
	}
	closed := s.closed
	s.unlock()
	// A session's Close waits for this, so it is told without s.mu.
	if known && c.session != nil {
		c.session.forgotten(closed)
	}
}

// Hello is the server frame of a new connection.
func (b backend) Hello(_ context.Context, wc *wire.Conn) wire.ServerInfo {
	s := b.s
	c, ok := wire.DataOf[*client](wc)
	s.mu.Lock()
	defer s.unlock()
	if !ok {
		return s.serverParams("", false)
	}
	return s.serverFrameLocked(c)
}

// Auth signs a connection in. authenticate replies raw when it succeeds,
// while it holds s.mu, and signs the connection in itself in the same hold
// (attachLocked), so that every notification it sends after the result
// reaches the connection. Its results are objects of the server's own,
// such as a ceremony's options, or the token of a session, as it stores
// them, so they are not AuthResults.
func (b backend) Auth(ctx context.Context, wc *wire.Conn, _ wire.AuthParams, reply wire.Reply[wire.AuthResult]) error {
	return run(b, ctx, wc, reply, func(c *client, req request) (any, bool, *Error) {
		_, err := b.s.authenticate(c, req)
		return nil, req.w.Replied(), err
	})
}

// refresh brings a duplicate's result up to date (§1.2): `me` answers with
// the current profile, and a message result lists only the write URLs that
// are still unused. Other results name what the request made, which does
// not change.
func (b backend) refresh(_ context.Context, wc *wire.Conn, method string, result jsontext.Value) any {
	s := b.s
	c, ok := wire.DataOf[*client](wc)
	if !ok {
		return result
	}
	s.mu.Lock()
	defer s.unlock()
	u := c.user
	if u == nil {
		return result
	}
	switch method {
	case "me":
		return map[string]any{"you": u.you()}
	case "message", "command":
		var original map[string]any
		if json.Unmarshal(result, &original) != nil || original["embeds"] == nil {
			return result
		}
		current := maps.Clone(original)
		var writes []any
		for _, value := range asList(original["embeds"]) {
			written, _ := value.(map[string]any)
			id, _ := written["embed_id"].(string)
			if e := s.embeds[id]; e != nil && !e.started && !e.removed && s.writes[e.token] == e {
				writes = append(writes, written)
			}
		}
		if len(writes) == 0 {
			delete(current, "embeds")
		} else {
			current["embeds"] = writes
		}
		return current
	}
	return result
}

// Rooms (§4.2). A room_list result is the rooms as the server keeps
// them, objects of their stored fields, so it is sent raw, followed by the
// read cursors of the rooms listed.

func (b backend) RoomJoin(ctx context.Context, wc *wire.Conn, p wire.RoomJoinParams, reply wire.Reply[wire.Empty]) error {
	return run(b, ctx, wc, reply, func(c *client, req request) (any, bool, *Error) {
		return b.s.joinRoom(c, req, p.RoomID, p.UserID)
	})
}

func (b backend) RoomLeave(ctx context.Context, wc *wire.Conn, p wire.RoomLeaveParams, reply wire.Reply[wire.Empty]) error {
	return run(b, ctx, wc, reply, func(c *client, req request) (any, bool, *Error) {
		return b.s.leaveRoom(c, req, p.RoomID, p.UserID)
	})
}

func (b backend) RoomList(ctx context.Context, wc *wire.Conn, p wire.RoomListParams, reply wire.Reply[wire.RoomListResult]) error {
	return run(b, ctx, wc, reply, func(c *client, req request) (any, bool, *Error) {
		return b.s.listRooms(c, req, p)
	})
}

func (b backend) RoomSet(ctx context.Context, wc *wire.Conn, p wire.RoomSetParams, reply wire.Reply[wire.RoomSetResult]) error {
	return run(b, ctx, wc, reply, func(c *client, req request) (any, bool, *Error) {
		return b.s.setRoom(c, req, p)
	})
}

// History (§4.3).

func (b backend) History(ctx context.Context, wc *wire.Conn, p wire.HistoryParams, reply wire.Reply[wire.HistoryResult]) error {
	return run(b, ctx, wc, reply, func(c *client, req request) (any, bool, *Error) {
		return b.s.history(c, req, p)
	})
}

// Profiles (§3.3.2).

// Me changes the user's profile. updateProfile sends the user's complete
// object while it holds s.mu, which makes it the connection's user.
func (b backend) Me(ctx context.Context, wc *wire.Conn, p wire.MeParams, reply wire.Reply[wire.CurrentUser]) error {
	return run(b, ctx, wc, reply, func(c *client, _ request) (any, bool, *Error) {
		return nil, true, b.s.updateProfile(c, p, reply)
	})
}

// Status (§4.5).

func (b backend) Status(ctx context.Context, wc *wire.Conn, p wire.StatusParams, reply wire.Reply[wire.Empty]) error {
	return run(b, ctx, wc, reply, func(c *client, req request) (any, bool, *Error) {
		return b.s.status(c, req, p)
	})
}

// Activity (§4.6): a notification, which gets no reply.

func (b backend) Activity(ctx context.Context, wc *wire.Conn, p wire.ActivityParams) {
	c, ok := wire.DataOf[*client](wc)
	if !ok {
		return
	}
	previous := c.request
	c.request = ctx
	defer func() { c.request = previous }()
	_ = b.s.activity(c, p)
}

// Reactions (§4.7).

func (b backend) React(ctx context.Context, wc *wire.Conn, p wire.ReactParams, reply wire.Reply[wire.Empty]) error {
	return run(b, ctx, wc, reply, func(c *client, req request) (any, bool, *Error) {
		return b.s.react(c, req, p)
	})
}

// Messages (§3.5, §4.4) and commands (§4.1).

// Message saves a new message, as a save without a message_id.
func (b backend) Message(ctx context.Context, wc *wire.Conn, p wire.MessageParams, reply wire.Reply[wire.MessageResult]) error {
	return b.Edit(ctx, wc, wire.EditParams{RoomID: p.RoomID, Body: p.Body, ReplyTo: p.ReplyTo, Ext: p.Ext}, reply)
}

// Edit saves an existing message: an edit, a move, or a deletion, as
// saveMessage handles new ones.
func (b backend) Edit(ctx context.Context, wc *wire.Conn, p wire.EditParams, reply wire.Reply[wire.MessageResult]) error {
	return run(b, ctx, wc, reply, func(c *client, req request) (any, bool, *Error) {
		return b.s.saveMessage(c, req, p)
	})
}

func (b backend) Command(ctx context.Context, wc *wire.Conn, p wire.CommandParams, reply wire.Reply[wire.CommandResult]) error {
	return run(b, ctx, wc, reply, func(c *client, req request) (any, bool, *Error) {
		return b.s.command(c, req, p)
	})
}
