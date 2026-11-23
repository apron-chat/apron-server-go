package wire

import (
	"encoding/json/jsontext"
	"errors"
	"slices"
	"strings"
	"unicode"

	"github.com/apron-chat/apron-server-go/wire/internal/wirecore"
)

// Version is the protocol version this package speaks.
const Version = 10

// ServerInfo is the server frame (PROTOCOL.md §3.1). Server fills in Apron
// and Ping, and adds the capabilities of the methods it handles.
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

// Method makes a server frame a Notification (§3.1); Conn.Announce sends
// one with what Server fills in.
func (ServerInfo) Method() string { return "server" }

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
	u.Unknown = cloneRaw(u.Unknown)
	return u
}

// The system identities (Appendix A.1): a message from one says who else
// received it.
var (
	// Private is the sender of a transient notice to one connection, such as
	// a command's reply. Its messages have no message_id.
	Private = User{UserID: "~private", Name: "Only you"}
	// RoomNotice is the sender of a logged notice to every member of a room.
	RoomNotice = User{UserID: "~room", Name: "Room"}
	// ServerNotice is the sender of a logged notice to everyone.
	ServerNotice = User{UserID: "~server", Name: "Server"}
)

// Ref refers to a message by its message_id (§3.5.4).
type Ref struct {
	MessageID LogID `json:"message_id"`
}

// Message is a message snapshot, or a transient notice when MessageID is
// 0 (§3.5).
type Message struct {
	MessageID  LogID  `json:"message_id,omitzero"`
	LogID      LogID  `json:"log_id,omitzero"`
	RoomID     string `json:"room_id,omitempty"`
	From       User   `json:"from"`
	Body       *Body  `json:"body,omitzero"`
	ReplyTo    *Ref   `json:"reply_to,omitempty"`
	PrevLogID  LogID  `json:"prev_log_id,omitzero"`
	PrevRoomID string `json:"prev_room_id,omitempty"`
	Deleted    bool   `json:"deleted,omitzero"`
	Ext        Ext    `json:"ext,omitempty"`
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
	return Message{RoomID: roomID, From: Private, Body: &Body{Text: text}}
}

// AuthParams are the params of auth (§3.2). Raw holds every param, for the
// schemes whose fields are not here, such as webauthn's.
type AuthParams struct {
	Scheme string         `json:"scheme"`
	Name   string         `json:"name,omitempty"`
	UserID string         `json:"user_id,omitempty"`
	Agent  string         `json:"agent,omitempty"`
	Token  string         `json:"token,omitempty"`
	Ext    Ext            `json:"ext,omitempty"`
	Raw    jsontext.Value `json:"-"`
}

// AuthResult is the result of auth (§3.2). A result with You nil
// authenticated nothing, such as the begin step of a ceremony; with You, the
// connection is signed in as You. You is complete (§3.3.1), so a field it
// carries empty, such as a status of "", which a complete object carries
// (§4.5.2), is in its Cleared.
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
// a bot (package wire/bot).
type MessageParams struct {
	// RoomID is the room, or "" for the default room.
	RoomID string `json:"room_id,omitempty"`
	// Body is never nil. Its Unknown holds the fields this package does
	// not know, for a backend that keeps them.
	Body    *Body `json:"body,omitzero"`
	ReplyTo *Ref  `json:"reply_to,omitempty"`
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
	ReplyTo *Ref
	// Ext merges into the message's ext (§4.13); nil when left out.
	Ext Ext
}

// CommandParams are the params of a command (§4.1): those of a new
// message, whose body.text is the command line, and whose other fields
// are its arguments. They encode as a client sends them.
type CommandParams struct {
	// RoomID is the room the command runs in, or "" for the default room.
	RoomID string `json:"room_id,omitempty"`
	// Body is never nil.
	Body    *Body `json:"body,omitzero"`
	ReplyTo *Ref  `json:"reply_to,omitempty"`
	Ext     Ext   `json:"ext,omitempty"`
}

// CommandResult is the result of a command (§4.1): empty, or the write
// URLs of the embeds it made.
type CommandResult struct {
	// Embeds are the embed writes the command reserved (§4.8.2), as their
	// objects.
	Embeds []jsontext.Value `json:"embeds,omitempty"`
	// Unknown holds more result fields, an extension's, as one JSON
	// object's members, sent after the others.
	Unknown jsontext.Value `json:",embed"`
}

// MessageResult is the result of a message request: the new message's
// message_id, or nothing, and the write URLs of the embeds it made.
type MessageResult struct {
	MessageID LogID `json:"message_id,omitzero"`
	// Embeds are the embed writes the message reserved, for the sender
	// only (§4.8.3), as their objects.
	Embeds []jsontext.Value `json:"embeds,omitempty"`
}

// MeParams are the params of me (§3.3.2): the profile fields to change. A
// nil field is left as it is; an empty one clears it.
type MeParams struct {
	Name   *string
	Avatar *string
	// Status is the chosen status, with capability status (§4.5.1). A
	// server that does not accept a value sets "" (§4.5.1).
	Status *string
	// Ext merges into the user's ext (§4.13).
	Ext Ext
}

// Frame is one encoded frame, which can be sent to many connections without
// being encoded again.
type Frame []byte

// Notification is the params of a notification the server sends, which
// names its method, such as a Message ("message"), a UserUpdate ("user"),
// or a RoomUpdate ("room_update").
type Notification interface {
	Method() string
}

// Encode renders a notification frame, such as to broadcast it
// (Server.Broadcast) without encoding it for every connection.
func Encode(n Notification) (Frame, error) {
	if n == nil {
		return nil, errors.New("wire: encoding a nil notification")
	}
	return EncodeRaw(n.Method(), n)
}

// EncodeRaw renders a notification frame of any method and params, such
// as an extension's own notification.
func EncodeRaw(method string, params any) (Frame, error) {
	return marshal(map[string]any{"method": method, "params": params})
}

// Marshal encodes a value as JSON as wire encodes every frame:
// deterministically, with object keys sorted, nil slices and maps as null,
// and <, >, and & as they are, which only matter for JSON embedded in HTML.
// A backend that keeps or compares what it sends encodes with it, so that
// its bytes are the frames'.
func Marshal(value any) ([]byte, error) { return wirecore.Marshal(value) }

func marshal(value any) ([]byte, error) { return wirecore.Marshal(value) }

// ValidID reports whether id may be a room_id or user_id (§2.4): not empty,
// with no whitespace or control characters.
func ValidID(id string) bool {
	return id != "" && !strings.ContainsFunc(id, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}
