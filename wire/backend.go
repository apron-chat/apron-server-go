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
	// handles.
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

// Editor handles a message request with message_id, an edit or a deletion,
// and adds capability edit (§4.4).
type Editor interface {
	Edit(ctx context.Context, c *Conn, p MessageParams) (MessageResult, error)
}

// Profiler handles me (§3.3.2): it changes the user's profile and returns
// the complete current user object, which becomes the connection's.
type Profiler interface {
	Me(ctx context.Context, c *Conn, p MeParams) (User, error)
}

// Commander handles command requests, and adds capability command (§4.1).
// The body is present; p has no message_id and is not deleted. The result is
// nil or extra result fields, such as embeds.
type Commander interface {
	Command(ctx context.Context, c *Conn, p MessageParams) (map[string]any, error)
}

// Connecter is told of a new network connection before its server frame
// is made, such as to keep the backend's own state for it (Conn.SetData).
// An error closes the connection; an *Error is sent to the client first,
// after the server frame.
type Connecter interface {
	Connected(c *Conn) error
}

// Disconnecter is told when a connection ends, signed in or not, network
// or Local.
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
