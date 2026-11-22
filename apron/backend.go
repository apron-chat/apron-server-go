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
// each of its handlers. Each handler renders its result as wire's type for
// it, and sends it with its Reply while it holds s.mu where the result
// describes state, so that wire's types make the bytes of every result.
// The Server's own methods beyond the protocol's, such as ext:account's,
// are wire methods (registerOp), which reply raw.

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

// registerOp makes an operation a wire method, which replies raw.
func (s *Server) registerOp(name string, op operation) {
	s.wire.Handle(name, wire.Handler{
		Capability: methodCapabilities[name],
		NoDedup:    readOnlyOps[name],
		Serve: func(ctx context.Context, wc *wire.Conn, r *wire.Request) error {
			return backend{s}.serve(ctx, wc, func(c *client) *Error {
				req := request{params: r.Fields(), send: func(result any, frames []wire.Frame) bool {
					return r.Reply(result, frames...)
				}}
				result, replied, err := op(s, c, req)
				if err == nil && !replied {
					req.reply(result)
				}
				return err
			})
		},
	})
}

// serve runs a handler for the connection's client, with the request's
// context as the one extension code it runs gets (Host).
func (b backend) serve(ctx context.Context, wc *wire.Conn, handle func(c *client) *Error) error {
	c, ok := wire.DataOf[*client](wc)
	if !ok {
		return internalError()
	}
	previous := c.request
	c.request = ctx
	defer func() { c.request = previous }()
	if err := handle(c); err != nil {
		return err
	}
	return nil
}

// connected keeps a new WebSocket's client, or signs a Session's in, whose
// client its Bot made (Bot.Connect).
func (b backend) connected(_ context.Context, wc *wire.Conn) error {
	s := b.s
	if c, ok := wire.DataOf[*client](wc); ok && c.session != nil {
		return s.connectSession(c, wc)
	}
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

// Auth signs a connection in. authenticate sends its result with reply
// while it holds s.mu, which signs the connection in, and makes it the
// identity's in the same hold (attachLocked), so that every notification
// it sends after the result reaches the connection. The schemes read
// params of their own, such as a ceremony's, from the request's.
func (b backend) Auth(ctx context.Context, wc *wire.Conn, p wire.AuthParams, reply wire.Reply[wire.AuthResult]) error {
	var params wire.Fields
	if err := json.Unmarshal(p.Raw, &params); err != nil {
		return internalError()
	}
	return b.serve(ctx, wc, func(c *client) *Error {
		_, err := b.s.authenticate(c, request{params: params, auth: reply})
		return err
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

// Rooms (§4.2).

func (b backend) RoomJoin(ctx context.Context, wc *wire.Conn, p wire.RoomJoinParams, reply wire.Reply[wire.Empty]) error {
	return b.serve(ctx, wc, func(c *client) *Error { return b.s.joinRoom(c, p, reply) })
}

func (b backend) RoomLeave(ctx context.Context, wc *wire.Conn, p wire.RoomLeaveParams, reply wire.Reply[wire.Empty]) error {
	return b.serve(ctx, wc, func(c *client) *Error { return b.s.leaveRoom(c, p, reply) })
}

func (b backend) RoomList(ctx context.Context, wc *wire.Conn, p wire.RoomListParams, reply wire.Reply[wire.RoomListResult]) error {
	return b.serve(ctx, wc, func(c *client) *Error { return b.s.listRooms(c, p, reply) })
}

func (b backend) RoomSet(ctx context.Context, wc *wire.Conn, p wire.RoomSetParams, reply wire.Reply[wire.RoomSetResult]) error {
	return b.serve(ctx, wc, func(c *client) *Error { return b.s.setRoom(c, p, reply) })
}

// History (§4.3).

func (b backend) History(ctx context.Context, wc *wire.Conn, p wire.HistoryParams, reply wire.Reply[wire.HistoryResult]) error {
	return b.serve(ctx, wc, func(c *client) *Error { return b.s.history(c, p, reply) })
}

// Profiles (§3.3.2).

// Me changes the user's profile. updateProfile sends the user's complete
// object while it holds s.mu, which makes it the connection's user.
func (b backend) Me(ctx context.Context, wc *wire.Conn, p wire.MeParams, reply wire.Reply[wire.CurrentUser]) error {
	return b.serve(ctx, wc, func(c *client) *Error { return b.s.updateProfile(c, p, reply) })
}

// Status (§4.5).

func (b backend) Status(ctx context.Context, wc *wire.Conn, p wire.StatusParams, reply wire.Reply[wire.Empty]) error {
	return b.serve(ctx, wc, func(c *client) *Error { return b.s.status(c, p, reply) })
}

// Activity (§4.6): a notification, which gets no reply.

func (b backend) Activity(ctx context.Context, wc *wire.Conn, p wire.ActivityParams) {
	_ = b.serve(ctx, wc, func(c *client) *Error { return b.s.activity(c, p) })
}

// Reactions (§4.7).

func (b backend) React(ctx context.Context, wc *wire.Conn, p wire.ReactParams, reply wire.Reply[wire.Empty]) error {
	return b.serve(ctx, wc, func(c *client) *Error { return b.s.react(c, p, reply) })
}

// Messages (§3.5, §4.4) and commands (§4.1).

// Message saves a new message, as a save without a message_id.
func (b backend) Message(ctx context.Context, wc *wire.Conn, p wire.MessageParams, reply wire.Reply[wire.MessageResult]) error {
	return b.Edit(ctx, wc, wire.EditParams{RoomID: p.RoomID, Body: p.Body, ReplyTo: p.ReplyTo, Ext: p.Ext}, reply)
}

// Edit saves an existing message: an edit, a move, or a deletion, as
// saveMessage handles new ones.
func (b backend) Edit(ctx context.Context, wc *wire.Conn, p wire.EditParams, reply wire.Reply[wire.MessageResult]) error {
	return b.serve(ctx, wc, func(c *client) *Error { return b.s.saveMessage(c, p, reply) })
}

// Command runs a command. Its result, which a command renders as an
// object, the extensions' too, goes out as a CommandResult, whose Unknown
// keeps any field of an extension's own.
func (b backend) Command(ctx context.Context, wc *wire.Conn, p wire.CommandParams, reply wire.Reply[wire.CommandResult]) error {
	return b.serve(ctx, wc, func(c *client) *Error {
		req := request{wireBody: p.Body, send: func(result any, frames []wire.Frame) bool {
			var typed wire.CommandResult
			if err := json.Unmarshal(encodeJSON(result), &typed); err != nil {
				b.s.logger.Error("a command's result is not an object", "error", err)
				_ = reply.Fail(internalError())
				return false
			}
			return reply.Send(typed, frames...) == nil
		}}
		result, replied, err := b.s.command(c, req, p)
		if err == nil && !replied {
			req.reply(result)
		}
		return err
	})
}
