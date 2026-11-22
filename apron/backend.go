package apron

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"maps"

	"github.com/apron-chat/apron-server-go/wire"
)

// The server speaks the protocol through package wire, as any server built
// on it does: wire carries the connections, frames, sign-in order, and
// deduplication, and backend answers for the Server. Every request goes to
// the Server's operations, registered as wire methods (registerOps), which
// reply while they hold s.mu where their results describe state
// (request.reply).

// backend is the Server as wire's Backend, kept unexported so that wire's
// interfaces are not part of the Server's API.
type backend struct{ s *Server }

var (
	_ wire.Backend       = backend{}
	_ wire.Connecter     = backend{}
	_ wire.Disconnecter  = backend{}
	_ wire.Refresher     = backend{}
	_ wire.SchemeChecker = backend{}
)

// newWire makes the Server's wire server, and registers its operations.
func (s *Server) newWire() {
	config := s.config
	s.wire = &wire.Server{
		Backend:        backend{s},
		OriginPatterns: config.OriginPatterns,
		AllowAnyOrigin: config.AllowAnyOrigin,
		// Native apps' passkey origins (§4.10) open WebSockets too.
		AllowOrigin:    s.nativeOrigin,
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
	s.wire.Handle(name, wire.Method{
		ReadOnly:     readOnlyOps[name],
		Notification: name == "activity",
		Handle: func(ctx context.Context, wc *wire.Conn, r *wire.Request) (any, error) {
			c := clientOf(wc)
			if c == nil {
				return nil, internalError()
			}
			// Extension code the request runs gets its context (Host).
			previous := c.request
			c.request = ctx
			defer func() { c.request = previous }()
			result, replied, err := op(s, c, newRequest(r))
			switch {
			case err != nil:
				return nil, err
			case replied:
				return nil, nil
			}
			return result, nil
		},
	})
}

// run runs an operation for one of wire's typed interfaces, as registerOp
// does for a method: its result goes out as it replies (request.reply), or
// once it returns.
func (b backend) run(ctx context.Context, wc *wire.Conn, op func(c *client, req request) (any, bool, *Error)) error {
	c := clientOf(wc)
	r := wire.RequestFrom(ctx)
	if c == nil || r == nil {
		return internalError()
	}
	previous := c.request
	c.request = ctx
	defer func() { c.request = previous }()
	req := newRequest(r)
	result, replied, err := op(c, req)
	switch {
	case err != nil:
		return err
	case !replied:
		req.reply(result)
	}
	return nil
}

func clientOf(wc *wire.Conn) *client {
	c, _ := wc.Data().(*client)
	return c
}

// Connected keeps a new WebSocket's client.
func (b backend) Connected(wc *wire.Conn) error {
	s := b.s
	r := wc.Request()
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

// Disconnected forgets a closed connection, network or session.
func (b backend) Disconnected(wc *wire.Conn) {
	c := clientOf(wc)
	if c == nil {
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
func (b backend) Hello(wc *wire.Conn) wire.ServerInfo {
	s := b.s
	c := clientOf(wc)
	s.mu.Lock()
	defer s.unlock()
	if c == nil {
		return s.serverParams("", false)
	}
	return s.serverFrameLocked(c)
}

// CheckScheme lets every scheme through to Auth, which knows the server's
// own rules, such as a service token's sign-in where no scheme offers
// tokens.
func (backend) CheckScheme(*wire.Conn, wire.AuthParams) error { return nil }

// Auth signs a connection in. authenticate replies itself when it succeeds,
// while it holds s.mu, and sets the connection's user (attachLocked).
func (b backend) Auth(ctx context.Context, wc *wire.Conn, _ wire.AuthParams) (wire.AuthResult, error) {
	c := clientOf(wc)
	if c == nil {
		return wire.AuthResult{}, internalError()
	}
	previous := c.request
	c.request = ctx
	defer func() { c.request = previous }()
	if _, err := b.s.authenticate(c, newRequest(wire.RequestFrom(ctx))); err != nil {
		return wire.AuthResult{}, err
	}
	return wire.AuthResult{}, nil
}

// Refresh brings a duplicate's result up to date (§1.2): `me` answers with
// the current profile, and a message result lists only the write URLs that
// are still unused. Other results name what the request made, which does
// not change.
func (b backend) Refresh(wc *wire.Conn, method string, result jsontext.Value) any {
	s := b.s
	c := clientOf(wc)
	if c == nil {
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

var _ wire.Rooms = backend{}

func (b backend) RoomJoin(ctx context.Context, wc *wire.Conn, roomID, userID string) error {
	return b.run(ctx, wc, func(c *client, req request) (any, bool, *Error) {
		return b.s.joinRoom(c, req, roomID, userID)
	})
}

func (b backend) RoomLeave(ctx context.Context, wc *wire.Conn, roomID, userID string) error {
	return b.run(ctx, wc, func(c *client, req request) (any, bool, *Error) {
		return b.s.leaveRoom(c, req, roomID, userID)
	})
}

func (b backend) RoomList(ctx context.Context, wc *wire.Conn, p wire.RoomListParams) (wire.RoomListResult, error) {
	return wire.RoomListResult{}, b.run(ctx, wc, func(c *client, req request) (any, bool, *Error) {
		return b.s.listRooms(c, req, p)
	})
}

func (b backend) RoomSet(ctx context.Context, wc *wire.Conn, p wire.RoomSetParams) (string, error) {
	return "", b.run(ctx, wc, func(c *client, req request) (any, bool, *Error) {
		return b.s.setRoom(c, req, p)
	})
}

// History (§4.3).

var _ wire.Historian = backend{}

func (b backend) History(ctx context.Context, wc *wire.Conn, p wire.HistoryParams) (wire.HistoryResult, error) {
	return wire.HistoryResult{}, b.run(ctx, wc, func(c *client, req request) (any, bool, *Error) {
		return b.s.history(c, req, p)
	})
}

// Profiles (§3.3.2).

var _ wire.Profiler = backend{}

// Me changes the user's profile. updateProfile replies itself, with the
// user's complete object, and sets the connection's user.
func (b backend) Me(ctx context.Context, wc *wire.Conn, p wire.MeParams) (wire.User, error) {
	return wire.User{}, b.run(ctx, wc, func(c *client, req request) (any, bool, *Error) {
		return b.s.updateProfile(c, req, p)
	})
}

// Status (§4.5).

var _ wire.Statuser = backend{}

func (b backend) Status(ctx context.Context, wc *wire.Conn, p wire.StatusParams) error {
	return b.run(ctx, wc, func(c *client, req request) (any, bool, *Error) {
		return b.s.status(c, req, p)
	})
}

// Activity (§4.6): a notification, which gets no reply.

var _ wire.Activity = backend{}

func (b backend) Activity(ctx context.Context, wc *wire.Conn, p wire.ActivityParams) {
	c := clientOf(wc)
	if c == nil {
		return
	}
	previous := c.request
	c.request = ctx
	defer func() { c.request = previous }()
	_ = b.s.activity(c, p)
}

// Reactions (§4.7).

var _ wire.Reactor = backend{}

func (b backend) React(ctx context.Context, wc *wire.Conn, p wire.ReactParams) error {
	return b.run(ctx, wc, func(c *client, req request) (any, bool, *Error) {
		return b.s.react(c, req, p)
	})
}

// Messages (§3.5, §4.4) and commands (§4.1).

var (
	_ wire.Messenger = backend{}
	_ wire.Editor    = backend{}
	_ wire.Commander = backend{}
)

func (b backend) Message(ctx context.Context, wc *wire.Conn, p wire.MessageParams) (wire.MessageResult, error) {
	return wire.MessageResult{}, b.run(ctx, wc, func(c *client, req request) (any, bool, *Error) {
		return b.s.saveMessage(c, req, p)
	})
}

// Edit saves an existing message: an edit, a move, or a deletion, as
// saveMessage handles new ones.
func (b backend) Edit(ctx context.Context, wc *wire.Conn, p wire.MessageParams) (wire.MessageResult, error) {
	return b.Message(ctx, wc, p)
}

func (b backend) Command(ctx context.Context, wc *wire.Conn, p wire.MessageParams) (map[string]any, error) {
	return nil, b.run(ctx, wc, func(c *client, req request) (any, bool, *Error) {
		return b.s.command(c, req, p)
	})
}
