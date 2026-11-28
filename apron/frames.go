package apron

import (
	"encoding/json/jsontext"
	"errors"
	"slices"
	"strings"
	"unicode"

	"github.com/apron-chat/apron-server-go/apron/internal/core"
)

// Version is the protocol version this package speaks.
const Version = 10

// ServerInfo is the server frame (PROTOCOL.md §3.1). A server.Server fills
// in Apron and Ping, and adds the capabilities of the methods it handles.
type ServerInfo struct {
	Apron        int               `json:"apron"`
	Auth         []string          `json:"auth"`
	Agent        string            `json:"agent,omitempty"`
	Capabilities []string          `json:"capabilities,omitempty"`
	Welcome      string            `json:"welcome,omitempty"`
	Signup       []string          `json:"signup,omitzero"`
	Labels       map[string]string `json:"labels,omitempty"`
	Ping         int               `json:"ping,omitzero"`
	Title        string            `json:"title,omitempty"`
	Description  string            `json:"description,omitempty"`
	Avatar       string            `json:"avatar,omitempty"`
	Status       []string          `json:"status,omitempty"`
	// Push enables push (§4.9): its kinds and wake scopes.
	Push map[string]any `json:"push,omitempty"`
	Ext  Ext            `json:"ext,omitempty"`
	// Unknown holds the members this package has no field for, as they
	// arrived, and is sent after the others.
	Unknown jsontext.Value `json:",embed"`
}

// Method makes a server frame a Notification (§3.1); server.Conn.Announce
// sends one with what a server.Server fills in.
func (ServerInfo) Method() string { return "server" }

// Has reports whether the server frame lists capability, such as "history"
// or "ext:limits" (§4): a client uses a feature only while the latest
// server frame lists it, and falls back without it.
func (s ServerInfo) Has(capability string) bool { return slices.Contains(s.Capabilities, capability) }

// Offers reports whether a client may send auth with scheme (§3.2): one of
// auth, which sign in, or of signup, which make an account. A scheme it
// offers neither way is unsupported. A server may still accept a scheme it
// does not list, such as a token it issued, so a client that holds one
// tries it.
func (s ServerInfo) Offers(scheme string) bool {
	return slices.Contains(s.Auth, scheme) || slices.Contains(s.Signup, scheme)
}

// User is a user object (§3.3.1): a recorded one, such as a message's
// from, or a complete current one. A current object that carries cleared
// fields, as a user notification does, is a CurrentUser.
type User struct {
	UserID string   `json:"user_id"`
	Name   string   `json:"name,omitempty"`
	Avatar string   `json:"avatar,omitempty"`
	Roles  []string `json:"roles,omitempty"`
	Status string   `json:"status,omitempty"`
	Seen   string   `json:"seen,omitempty"`
	Ext    Ext      `json:"ext,omitempty"`
	// Unknown holds the members this package has no field for, as they
	// arrived, and is sent after the others.
	Unknown jsontext.Value `json:",embed"`
}

// clone returns a copy of u that shares nothing with it.
func (u User) clone() User {
	u.Roles = slices.Clone(u.Roles)
	u.Ext = u.Ext.clone()
	u.Unknown = core.CloneRaw(u.Unknown)
	return u
}

// The system identities (Appendix A.1): a message from one says who else
// received it.
var (
	// PrivateNotice is the sender of a transient notice to one
	// connection, such as a command's reply. Its messages have no
	// message_id.
	PrivateNotice = User{UserID: "~private", Name: "Only you"}
	// RoomNotice is the sender of a logged notice to every member of a room.
	RoomNotice = User{UserID: "~room", Name: "Room"}
	// ServerNotice is the sender of a logged notice to everyone.
	ServerNotice = User{UserID: "~server", Name: "Server"}
)

// MessageRef refers to a message by its message_id (§3.5.4).
type MessageRef struct {
	MessageID LogID `json:"message_id"`
}

// Message is a message snapshot, or a transient notice when MessageID is
// 0 (§3.5).
type Message struct {
	MessageID LogID  `json:"message_id,omitzero"`
	LogID     LogID  `json:"log_id,omitzero"`
	RoomID    string `json:"room_id,omitempty"`
	From      User   `json:"from"`
	// Body is the zero Body for a message without one, such as a
	// tombstone (§4.4), which is left out.
	Body Body `json:"body,omitzero"`
	// ReplyTo is the message this one replies to, the zero MessageRef for
	// none, which is left out.
	ReplyTo    MessageRef `json:"reply_to,omitzero"`
	PrevLogID  LogID      `json:"prev_log_id,omitzero"`
	PrevRoomID string     `json:"prev_room_id,omitempty"`
	Deleted    bool       `json:"deleted,omitzero"`
	Ext        Ext        `json:"ext,omitempty"`
	// Unknown holds the members this package has no field for, as they
	// arrived, and is sent after the others.
	Unknown jsontext.Value `json:",embed"`
}

// Method makes a message a Notification: a snapshot, or a transient notice
// (§3.5).
func (Message) Method() string { return "message" }

// Notice is a transient ~private notice with text, shown in roomID, or
// wherever the client likes when roomID is empty.
func Notice(roomID, text string) Message {
	return Message{RoomID: roomID, From: PrivateNotice, Body: Body{Text: text}}
}

// AuthParams are the params of auth (§3.2). A scheme whose fields are not
// here, such as webauthn's, reads them from the request's server.Decoder
// (server.Reply.Request).
type AuthParams struct {
	Scheme string `json:"scheme"`
	Name   string `json:"name,omitempty"`
	UserID string `json:"user_id,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Token  string `json:"token,omitempty"`
	Ext    Ext    `json:"ext,omitempty"`
}

// AuthResult is the result of auth (§3.2). A result with You nil
// authenticated nothing, such as the begin step of a ceremony; with You, the
// connection is signed in as You.
//
// You is complete (§3.3.1): it carries every profile field the server
// publishes, those the user has empty too, named in its Cleared, such as a
// status of "" with capability status (§4.5.2). So a sign-in's You is the
// user, u, with Cleared naming the fields the server publishes that u has
// empty: &CurrentUser{User: u}, for a server that publishes no field u
// can have empty, such as one of user_id and name alone, or
// &CurrentUser{User: u, Cleared: []string{"status"}} for a user without a
// status. User.Complete is for a client that keeps the user already, as a
// me result's does: a signing-in client keeps nothing, and
// u.Complete(User{}) is CurrentUser{User: u}, which clears nothing.
type AuthResult struct {
	You   *CurrentUser `json:"you,omitzero"`
	Token string       `json:"token,omitempty"`
	// Extra holds more result fields, sent beside you and token, such as
	// a webauthn begin's options. It names neither of them: a result that
	// does is error/internal_error.
	Extra map[string]any `json:",embed"`
}

// MessageParams are the params of a new message (§3.5.1): a message
// request without message_id. They encode as a client sends them, such as
// a bot (package apron/bot).
type MessageParams struct {
	// RoomID is the room, or "" for the default room.
	RoomID string `json:"room_id,omitempty"`
	// Body is the message's, which a request has: its Unknown holds the
	// fields this package does not know, for a backend that keeps them.
	Body Body `json:"body,omitzero"`
	// ReplyTo is the message this one replies to, or the zero MessageRef.
	ReplyTo MessageRef `json:"reply_to,omitzero"`
	// Ext is the message's ext (§4.13); nil when left out.
	Ext Ext `json:"ext,omitempty"`
}

// EditParams are the params of a save of an existing message (§4.4): a
// message request with message_id, which edits, moves, or deletes it.
type EditParams struct {
	// MessageID is the message saved, never 0.
	MessageID LogID
	// RoomID is the room to save the message in, or "" for the default
	// room: one other than the message's moves it.
	RoomID string
	// Deleted saves a tombstone: Body is then nil, whatever was sent.
	Deleted bool
	// Body is nil when the request leaves it out. Its Unknown holds the
	// fields this package does not know.
	Body    *Body
	ReplyTo *MessageRef
	// Ext merges into the message's ext (§4.13); nil when left out.
	Ext Ext
}

// CommandParams are the params of a command (§4.1): those of a new
// message, whose body.text is the command line, and whose other fields
// are its arguments. They encode as a client sends them.
type CommandParams struct {
	// RoomID is the room the command runs in, or "" for the default room,
	// which the backend names (§3.6.3): with package apron/server/commands, in its
	// Router.Before.
	RoomID string `json:"room_id,omitempty"`
	// Body is the command's, whose text is the command line.
	Body    Body       `json:"body,omitzero"`
	ReplyTo MessageRef `json:"reply_to,omitzero"`
	Ext     Ext        `json:"ext,omitempty"`
}

// CommandResult is the result of a command (§4.1): empty, or the write
// URLs of the embeds it made.
type CommandResult struct {
	// Embeds are the embed writes the command reserved (§4.8.3).
	Embeds []EmbedWrite `json:"embeds,omitempty"`
	// Unknown holds more result fields, an extension's, as one JSON
	// object's members, sent after the others.
	Unknown jsontext.Value `json:",embed"`
}

// MessageResult is the result of a message request: the new message's
// message_id, or nothing, and the write URLs of the embeds it made.
type MessageResult struct {
	MessageID LogID `json:"message_id,omitzero"`
	// Embeds are the embed writes the message reserved, for the sender
	// only (§4.8.3).
	Embeds []EmbedWrite `json:"embeds,omitempty"`
	// Unknown holds more result fields, an extension's, as one JSON
	// object's members, sent after the others.
	Unknown jsontext.Value `json:",embed"`
}

// EmbedWrite is the write of an upload or live embed that a message or
// command reserved (§4.8.3), in its result, for the sender only: the
// sender sends the embed's content as the body of an HTTP PUT to
// WriteURL, a credential that expires if it is not used.
type EmbedWrite struct {
	EmbedID  string `json:"embed_id"`
	Kind     string `json:"kind"`
	WriteURL string `json:"write_url"`
	// Unknown holds the members this package has no field for, as they
	// arrived, and is sent after the others.
	Unknown jsontext.Value `json:",embed"`
}

// MeParams are the params of me (§3.3.2): the profile fields to change. A
// nil field is left as it is; an empty one clears it.
type MeParams struct {
	Name   *string `json:"name,omitzero"`
	Avatar *string `json:"avatar,omitzero"`
	// Status is the chosen status, with capability status (§4.5.1). A
	// server that does not accept a value sets "" (§4.5.1).
	Status *string `json:"status,omitzero"`
	// Ext merges into the user's ext (§4.13).
	Ext Ext `json:"ext,omitempty"`
}

// Frame is one encoded frame, which can be sent to many connections without
// being encoded again.
type Frame []byte

// FrameHead is what a frame says of itself (§1.1): its method, "" for a
// reply, and its id, params, result, and error, raw, each nil when the
// frame has none, and "null" when it is null. The values are parts of the
// frame, not copies: they are valid while it is, and are not changed.
type FrameHead struct {
	Method                    string
	ID, Params, Result, Error jsontext.Value
}

// Head reads the frame's head, without decoding or copying its values: a
// client that handles every frame it receives, such as a bot, reads the
// method of each, and decodes only the params of those it handles. Its
// error is that of a frame that is not a JSON object, or whose method is
// not a string, which decoding it into a FrameHead would refuse too.
func (f Frame) Head() (FrameHead, error) {
	h, err := core.ReadHead(f)
	return FrameHead{Method: h.Method, ID: h.ID, Params: h.Params, Result: h.Result, Error: h.Error}, err
}

// Notification is the params of a notification the server sends, which
// names its method, such as a Message ("message"), a UserUpdate ("user"),
// or a RoomUpdate ("room_update"). An extension's own notification is a
// type of the backend's with a Method, such as one that returns
// "ext:typing".
type Notification interface {
	Method() string
}

// Encode renders a notification frame, such as to broadcast it
// (server.Server.Broadcast) without encoding it for every connection.
func Encode(n Notification) (Frame, error) {
	if n == nil {
		return nil, errors.New("apron: encoding a nil notification")
	}
	return encodeRaw(n.Method(), n)
}

// encodeRaw renders a notification frame of any method and params.
func encodeRaw(method string, params any) (Frame, error) {
	return marshal(&core.NotificationFrame{Method: method, Params: params})
}

// Marshal encodes a value as JSON as every frame is encoded:
// deterministically, with object keys sorted, nil slices and maps as null,
// and <, >, and & as they are, which only matter for JSON embedded in HTML.
// A backend that keeps or compares what it sends encodes with it, so that
// its bytes are the frames'.
func Marshal(value any) ([]byte, error) { return core.Marshal(value) }

func marshal(value any) ([]byte, error) { return core.Marshal(value) }

// ValidID reports whether id may be a room_id or user_id (§2.4): not empty,
// with no whitespace or control characters.
func ValidID(id string) bool {
	return id != "" && !strings.ContainsFunc(id, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

// OK is the result of a request that answers nothing but success, {}:
// room_join, room_leave, status, and reactions.
type OK struct{}
