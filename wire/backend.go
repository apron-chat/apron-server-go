package wire

import (
	"context"
	"encoding/json/jsontext"
)

// Backend answers for a Server: who it is (the server frame), and who each
// connection is (auth). Everything else it does, it adds with the optional
// interfaces below, each of which adds its requests and its capability, or
// with methods of its own (Server.Handle).
//
// The Server calls a Backend from one goroutine per connection, for one
// frame at a time, so calls for different connections are concurrent.
type Backend interface {
	// Hello returns the server frame for a new connection (§3.1). Server
	// sets Apron and Ping, and adds the capabilities of what the Backend
	// handles. A connection refused at capacity (Server.MaxConnections) is
	// sent one too, before its error, so Hello is called for it without
	// Connecter, and must not count on what Connecter keeps with a
	// connection.
	Hello(c *Conn) ServerInfo
	// Auth signs a connection in (§3.2). A result with You set signs the
	// connection in as You; notifications the backend sends to the
	// connection before Auth returns are sent after its result (§3.2.2).
	//
	// A backend that replies itself (Request.Reply) sets the connection's
	// user itself (Conn.SetUser), and its returned result is not sent.
	Auth(ctx context.Context, c *Conn, p AuthParams) (AuthResult, error)
}

// Messenger saves new messages (§3.5.1): the core message request, which
// every server needs (§3.6), with a Messenger or a method of its own. The
// body is present and well formed, and room_id is empty (the default room)
// or a valid ID. It returns the new message_id; the backend broadcasts the
// snapshot, including to c, before it returns. A message whose body is
// empty (Body.Empty) is invalid_params, neither saved nor broadcast, once
// the backend has checked what it names, such as its room.
type Messenger interface {
	Message(ctx context.Context, c *Conn, p MessageParams) (MessageResult, error)
}

// Editor handles a message request with message_id: an edit, a move, or a
// deletion, and adds capability edit (§4.4). A deletion without message_id
// is invalid_params before it gets here.
type Editor interface {
	Edit(ctx context.Context, c *Conn, p EditParams) (MessageResult, error)
}

// Profiler handles me (§3.3.2): it changes the user's profile and returns
// the complete current user object, with the fields the change cleared
// (CurrentUser.Cleared), whose User becomes the connection's. The backend
// tells the user's other connections, and those who share a room with the
// user, itself, such as with Server.PublishUser (§3.3.3).
type Profiler interface {
	Me(ctx context.Context, c *Conn, p MeParams) (CurrentUser, error)
}

// Commander handles command requests, and adds capability command (§4.1).
// The body is present. The result is empty, or the embeds the command
// made.
type Commander interface {
	Command(ctx context.Context, c *Conn, p CommandParams) (CommandResult, error)
}

// Connecter is told of a new network connection before its server frame
// is made, such as to keep the backend's own state for it (Conn.SetData).
// An error closes the connection; an *Error is sent to the client first,
// after the server frame.
type Connecter interface {
	Connected(c *Conn) error
}

// Disconnecter is told when a connection ends, signed in or not, network
// or Local: every one but those refused as they opened, by Connecter or at
// capacity (Server.MaxConnections).
type Disconnecter interface {
	Disconnected(c *Conn)
}

// Refresher brings the result of a request up to date for a duplicate of
// it (§1.2), which gets the original's outcome as the state is now, such as
// the user's current profile for a repeated me. Without one, a duplicate
// gets the original result as it was.
type Refresher interface {
	Refresh(c *Conn, method string, result jsontext.Value) any
}

// SchemeChecker decides which auth schemes a connection may use, such as
// one the server frame does not list for everyone. Without one, the
// schemes are those of the frame's Auth and Signup: any other scheme is
// unsupported, even one PROTOCOL.md does not define (§3.2).
type SchemeChecker interface {
	CheckScheme(c *Conn, p AuthParams) error
}
