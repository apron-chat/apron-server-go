package aprondtest

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"testing"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/aprontest"
	"github.com/apron-chat/apron-server-go/aprond"
	"github.com/apron-chat/apron-server-go/aprond/internal/testhook"
)

// Client is a connection to a Server.
type Client struct {
	// Conn is the connection, for what the methods below do not do, such
	// as reading frames in the order they arrived (aprontest.Client.Next,
	// CallFrames, and Sync).
	Conn *aprontest.Client
	// UserID is the user_id the client last signed in as, and Token the
	// session token that sign-in gave, if any, which signs in again
	// (SignInWithToken); both are "" before.
	UserID, Token string
	// Extension is the name of the extension whose ext aprond.WithExt
	// writes in the messages Post and Edit send, as if the client were
	// that extension's: the first extension Serve was given, unless set.
	Extension string

	t testing.TB
}

// deadline returns a context that ends after timeout, or with the test.
func (c *Client) deadline() (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.t.Context(), timeout)
}

// refusal returns err if it is the server's error reply, and fails the
// test with it otherwise, as when the connection ended.
func (c *Client) refusal(what string, err error) error {
	c.t.Helper()
	if _, ok := errors.AsType[*aprond.Error](err); ok {
		return err
	}
	c.t.Fatalf("aprondtest: %s: %v", what, err)
	return err
}

// Hello is the server frame (§3.1) the client got as it connected.
func (c *Client) Hello() apron.ServerInfo { return c.Conn.Hello() }

// Auth sends an auth request (§3.2) with params, such as {"scheme":
// "guest"}, and returns its result, or the server's error reply. A result
// with You signed the client in, as UserID and Token then say; one
// without, such as the begin step of external sign-in, signed nobody in.
// The SignIn methods below send the protocol's own schemes.
func (c *Client) Auth(params any) (apron.AuthResult, error) {
	c.t.Helper()
	ctx, cancel := c.deadline()
	defer cancel()
	result, err := c.Conn.Auth(ctx, params)
	if err != nil {
		return result, c.refusal("auth", err)
	}
	if result.You != nil {
		c.UserID, c.Token = result.You.UserID, result.Token
	}
	return result, nil
}

// signIn sends an auth request that signs in, and returns the server's
// error reply, or an error for a result that signed nobody in.
func (c *Client) signIn(params any) error {
	c.t.Helper()
	result, err := c.Auth(params)
	if err == nil && result.You == nil {
		err = errors.New("aprondtest: the auth result signed nobody in")
	}
	return err
}

// SignInAsGuest signs the client in as a guest who asked for name (§3.2),
// or returns the server's error reply, as on a server with
// Config.DisableGuests.
func (c *Client) SignInAsGuest(name string) error {
	c.t.Helper()
	return c.signIn(apron.AuthParams{Scheme: "guest", Name: name})
}

// SignInWithToken signs the client in with a token (§3.2.1), such as
// another Client's Token, or returns the server's error reply, as for the
// token of a session that was signed out.
func (c *Client) SignInWithToken(token string) error {
	c.t.Helper()
	return c.signIn(apron.AuthParams{Scheme: "token", Token: token})
}

// AskForCode asks the server to send a sign-in code to address (§4.11),
// the first step of email sign-in, which signs nobody in. The server
// answers alike whether it sends one or not, as to an address that may not
// sign in: Mailbox.Code says whether it did.
func (c *Client) AskForCode(address string) error {
	c.t.Helper()
	_, err := c.Auth(struct {
		Scheme string `json:"scheme"`
		Email  string `json:"email"`
	}{"email", address})
	return err
}

// SignInWithCode signs the client in with a sign-in code that the server
// sent after AskForCode (§4.11), or returns the server's error reply.
func (c *Client) SignInWithCode(code string) error {
	c.t.Helper()
	return c.signIn(apron.AuthParams{Scheme: "email", Token: code})
}

// Call sends a request of method with params, such as an extension's
// method, and decodes its result into result, unless nil, or returns the
// server's error reply. params is any value that encodes as a JSON object,
// such as a struct with json tags, or a map[string]any. The result is
// decoded with encoding/json/v2 and json.MatchCaseInsensitiveNames: a
// member fills the field whose name, or json tag's name, it equals,
// ignoring case, dashes, and underscores, so that message_id fills a field
// MessageID, as do messageId and Message-ID; members that fill no field
// are ignored, and nested objects are decoded alike.
func (c *Client) Call(method string, params, result any) error {
	c.t.Helper()
	ctx, cancel := c.deadline()
	defer cancel()
	raw, err := c.Conn.Call(ctx, method, params)
	if err != nil {
		return c.refusal(method, err)
	}
	if result != nil {
		if err := json.Unmarshal(raw, result, json.MatchCaseInsensitiveNames(true)); err != nil {
			c.t.Fatalf("aprondtest: decoding the result of %s, %s: %v", method, raw, err)
		}
	}
	return nil
}

// messageParams are a message request's params of text to a room with
// options, or the error of options the server would refuse, such as a
// ReplyTo that names no message_id.
func (c *Client) messageParams(roomID, text string, options []aprond.PostOption) (apron.MessageParams, error) {
	return testhook.MessageParams(roomID, text, c.Extension, options)
}

// Post posts a message of text to a room (§3.5.1), as aprond.Session.Post
// does, with options such as aprond.Mention, aprond.ReplyTo, aprond.Markdown,
// and aprond.WithExt, which writes under Extension's name; and returns its
// message_id, or the server's error reply, such as an extension's filter's
// refusal (aprond.Registrar.FilterMessage).
func (c *Client) Post(roomID, text string, options ...aprond.PostOption) (messageID string, err error) {
	c.t.Helper()
	params, err := c.messageParams(roomID, text, options)
	if err != nil {
		return "", err
	}
	ctx, cancel := c.deadline()
	defer cancel()
	result, err := c.Conn.Post(ctx, params)
	if err != nil {
		return "", c.refusal("message", err)
	}
	if result.MessageID == 0 {
		return "", nil
	}
	return result.MessageID.String(), nil
}

// edit is the params of a message request that saves an existing message
// (§4.4).
type edit struct {
	RoomID    string           `json:"room_id"`
	MessageID string           `json:"message_id"`
	Deleted   bool             `json:"deleted,omitzero"`
	Body      *apron.Body      `json:"body,omitempty"`
	ReplyTo   apron.MessageRef `json:"reply_to,omitzero"`
	Ext       apron.Ext        `json:"ext,omitempty"`
}

// Edit saves the message messageID in a room with text as its new body
// (§4.4), with options as Post takes them, or returns the server's error
// reply, such as for a message the client may not edit.
func (c *Client) Edit(roomID, messageID, text string, options ...aprond.PostOption) error {
	c.t.Helper()
	params, err := c.messageParams(roomID, text, options)
	if err != nil {
		return err
	}
	return c.Call("message", edit{RoomID: roomID, MessageID: messageID, Body: &params.Body, ReplyTo: params.ReplyTo, Ext: params.Ext}, nil)
}

// Delete deletes the message messageID in a room (§4.4), which leaves a
// tombstone, or returns the server's error reply.
func (c *Client) Delete(roomID, messageID string) error {
	c.t.Helper()
	return c.Call("message", edit{RoomID: roomID, MessageID: messageID, Deleted: true}, nil)
}

// CreateRoom makes a room with title (§4.2.4), which only its members see
// if private, and the client is the only member of, and returns its
// room_id, or the server's error reply.
func (c *Client) CreateRoom(title string, private bool) (roomID string, err error) {
	c.t.Helper()
	var result apron.RoomSetResult
	err = c.Call("room_set", struct {
		Title   string `json:"title"`
		Private bool   `json:"private,omitzero"`
	}{title, private}, &result)
	return result.RoomID, err
}

// Command runs a command line, such as "/weather Lisbon", in a room
// (§4.1), and returns the text of the notices it sent the client, which
// arrive before its reply (aprond.CommandCall.Notice), and its error reply,
// if any. It takes those notices, as aprontest.Client.Command does, and
// leaves every other frame, such as the message snapshots that arrived
// meanwhile, for Message.
func (c *Client) Command(roomID, line string) (notices []string, err error) {
	c.t.Helper()
	ctx, cancel := c.deadline()
	defer cancel()
	notices, err = c.Conn.Command(ctx, roomID, line)
	if err != nil {
		return notices, c.refusal(line, err)
	}
	return notices, nil
}

// Message waits for a message snapshot (§3.5) that match is true of, the
// first one the client got that a call of Message has not taken yet, takes
// it, and returns it. It fails the test when none comes. Snapshots are
// sent to the members of a room: a guest is a member of the default room,
// "general".
func (c *Client) Message(match func(aprond.Message) bool) aprond.Message {
	c.t.Helper()
	ctx, cancel := c.deadline()
	defer cancel()
	var found aprond.Message
	_, err := c.Conn.Expect(ctx, func(f aprontest.Frame) bool {
		if f.Method != "message" {
			return false
		}
		m, ok := aprond.DecodeMessage(aprond.Event{Kind: aprond.EventMessage, Record: jsontext.Value(f.Params)})
		if ok && match(m) {
			found = m
			return true
		}
		return false
	})
	if err != nil {
		c.t.Fatalf("aprondtest: no message matched: %v", err)
	}
	return found
}

// Closed reports whether the server closes the connection within ten
// seconds, as it does when it signs the client's account out, taking
// every frame until then.
func (c *Client) Closed() bool {
	ctx, cancel := c.deadline()
	defer cancel()
	for {
		if _, err := c.Conn.Next(ctx); err != nil {
			return ctx.Err() == nil
		}
	}
}
