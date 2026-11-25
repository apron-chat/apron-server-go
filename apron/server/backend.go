package server

import (
	"context"

	"github.com/apron-chat/apron-server-go/apron"
)

// Backend answers for a Server: who it is (the server frame), and who each
// connection is (auth). Everything else it does, it adds with the
// handlers of the Server's Config, such as Messages and Rooms, each of
// which adds its requests and its capability, or with methods of its own
// (Server.Handle).
//
// The Server calls a Backend, and every handler, from one goroutine per
// connection, for one frame at a time, so calls for different connections
// are concurrent.
type Backend interface {
	// Hello returns the server frame for a new connection (§3.1), network
	// or Local, with ctx the connection's context. Server sets Apron and
	// Ping, and adds the capabilities of what it handles. A connection
	// refused at capacity (Config.MaxConnections) is sent one too, before
	// its error, so Hello is called for it without OnConnect, and must not
	// count on what OnConnect keeps with a connection.
	Hello(ctx context.Context, c *Conn) apron.ServerInfo
	// Auth signs a connection in (§3.2), and answers with reply.Send, or
	// returns an error. A result with You set signs the connection in as
	// You (Conn.SetUser) as Send queues it, so a backend that calls Send
	// while it holds its own lock has the connection signed in for every
	// notification it sends after, such as with Server.PublishUser or
	// Server.Broadcast. Notifications the backend sends to the connection
	// otherwise while Auth runs are sent after its result (§3.2.2), or,
	// when the request fails, not at all; Send's then frames go right
	// after the result.
	//
	// Only what is queued while Auth runs is held: once Auth has returned,
	// whatever is sent to the connection goes out as to any connection,
	// signed in or not, as a notice before sign-in may (§3.2.2). So a
	// backend that starts senders of its own for the connection during
	// Auth, such as a goroutine that relays a host's events to it, stops
	// them before it returns an error, or the frames they send after
	// reach a connection that is not signed in.
	Auth(ctx context.Context, c *Conn, p apron.AuthParams, reply Reply[apron.AuthResult]) error
}

// MessageHandler saves new messages (§3.5.1): the core message request,
// which every server needs (§3.6), with a MessageHandler or a method of its
// own. The body is present and well formed, and room_id is empty (the
// default room) or a valid ID. It answers with the new message_id; the
// backend broadcasts the snapshot, including to c, before it replies. A
// message whose body is empty (Body.Empty) is invalid_params, neither saved
// nor broadcast, once the backend has checked what it names, such as its
// room.
type MessageHandler interface {
	Message(ctx context.Context, c *Conn, p apron.MessageParams, reply Reply[apron.MessageResult]) error
}

// EditHandler handles a message request with message_id: an edit, a move,
// or a deletion, and adds capability edit (§4.4). A deletion without
// message_id is invalid_params before it gets here.
type EditHandler interface {
	Edit(ctx context.Context, c *Conn, p apron.EditParams, reply Reply[apron.MessageResult]) error
}

// ProfileHandler handles me (§3.3.2), the core request that changes the
// user's own profile, such as a rename: every server needs it, as it needs
// a MessageHandler, since clients count on it, and the Server warns as it
// starts serving without one. It answers with the complete current user
// object, with the fields the change cleared (CurrentUser.Cleared), whose
// User becomes the connection's (Conn.SetUser) as Send queues it. The
// backend tells the user's other connections, and those who share a room
// with the user, itself (§3.3.3), with Server.PublishUser, c its Except.
// ApplyMe makes the request's changes, and User.Complete the result:
//
//	func (b *Backend) Me(ctx context.Context, c *server.Conn, p apron.MeParams, reply server.Reply[apron.CurrentUser]) error {
//		b.mu.Lock()
//		defer b.mu.Unlock()
//		old := *c.User()
//		cur := server.ApplyMe(old, p)
//		if len(cur.Name) > 64 {
//			return apron.InvalidParams("A name is at most 64 bytes")
//		}
//		// Everyone shares the one room: shares is nil.
//		b.srv.PublishUser(server.UserChange{Old: old, New: cur, Except: c}, nil)
//		return reply.Send(cur.Complete(old))
//	}
type ProfileHandler interface {
	Me(ctx context.Context, c *Conn, p apron.MeParams, reply Reply[apron.CurrentUser]) error
}

// CommandHandler handles command requests, and adds capability command
// (§4.1). The body is present. The result is empty, or the embeds the
// command made.
type CommandHandler interface {
	Command(ctx context.Context, c *Conn, p apron.CommandParams, reply Reply[apron.CommandResult]) error
}
