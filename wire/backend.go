package wire

import "context"

// Backend answers for a Server: who it is (the server frame), and who each
// connection is (auth). Everything else it does, it adds with the Server's
// handler fields, such as Messages and Rooms, each of which adds its
// requests and its capability, or with methods of its own (Server.Handle).
//
// The Server calls a Backend, and every handler, from one goroutine per
// connection, for one frame at a time, so calls for different connections
// are concurrent.
type Backend interface {
	// Hello returns the server frame for a new connection (§3.1), network
	// or Local, with ctx the connection's context. Server sets Apron and
	// Ping, and adds the capabilities of what it handles. A connection
	// refused at capacity (Server.MaxConnections) is sent one too, before
	// its error, so Hello is called for it without OnConnect, and must not
	// count on what OnConnect keeps with a connection.
	Hello(ctx context.Context, c *Conn) ServerInfo
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
	//
	// A backend that replies raw (Reply.Request) sets the connection's
	// user itself.
	Auth(ctx context.Context, c *Conn, p AuthParams, reply Reply[AuthResult]) error
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
	Message(ctx context.Context, c *Conn, p MessageParams, reply Reply[MessageResult]) error
}

// EditHandler handles a message request with message_id: an edit, a move,
// or a deletion, and adds capability edit (§4.4). A deletion without
// message_id is invalid_params before it gets here.
type EditHandler interface {
	Edit(ctx context.Context, c *Conn, p EditParams, reply Reply[MessageResult]) error
}

// ProfileHandler handles me (§3.3.2): it changes the user's profile and
// answers with the complete current user object, with the fields the
// change cleared (CurrentUser.Cleared), whose User becomes the
// connection's (Conn.SetUser) as Send queues it. The backend tells the
// user's other connections, and those who share a room with the user,
// itself, such as with Server.PublishUser (§3.3.3).
type ProfileHandler interface {
	Me(ctx context.Context, c *Conn, p MeParams, reply Reply[CurrentUser]) error
}

// CommandHandler handles command requests, and adds capability command
// (§4.1). The body is present. The result is empty, or the embeds the
// command made.
type CommandHandler interface {
	Command(ctx context.Context, c *Conn, p CommandParams, reply Reply[CommandResult]) error
}
