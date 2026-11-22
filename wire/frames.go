package wire

import (
	"encoding/json/jsontext"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/apron-chat/apron-server-go/internal/wirecore"
)

// Version is the protocol version this package speaks.
const Version = 10

// ServerInfo is the server frame (PROTOCOL.md §3.1). Server fills in Apron
// and Ping, and adds the capabilities of the interfaces the backend
// implements.
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
	Ext  map[string]any `json:"ext,omitempty"`
}

// User is a user object (§3.3.1).
type User struct {
	UserID string         `json:"user_id"`
	Name   string         `json:"name,omitempty"`
	Avatar string         `json:"avatar,omitempty"`
	Roles  []string       `json:"roles,omitempty"`
	Status string         `json:"status,omitempty"`
	Seen   string         `json:"seen,omitempty"`
	Ext    map[string]any `json:"ext,omitempty"`
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

// Body is a message's content (§3.5).
type Body struct {
	Text     string           `json:"text"`
	Format   string           `json:"format,omitempty"`
	Mentions []string         `json:"mentions,omitempty"`
	Embeds   []jsontext.Value `json:"embeds,omitempty"`
}

// Empty reports whether a body has no text and no embeds: a new message
// with such a body is invalid_params (§3.5.1).
func (b *Body) Empty() bool { return b == nil || b.Text == "" && len(b.Embeds) == 0 }

// Formats of a Body.
const (
	Plain    = "plain"
	Markdown = "markdown"
)

// Ref refers to a message by its message_id (§3.5.4).
type Ref struct {
	MessageID string `json:"message_id"`
}

// Message is a message snapshot, or a transient notice when MessageID is
// empty (§3.5).
type Message struct {
	MessageID  string         `json:"message_id,omitempty"`
	LogID      string         `json:"log_id,omitempty"`
	RoomID     string         `json:"room_id,omitempty"`
	From       User           `json:"from"`
	Body       *Body          `json:"body,omitempty"`
	ReplyTo    *Ref           `json:"reply_to,omitempty"`
	PrevLogID  string         `json:"prev_log_id,omitempty"`
	PrevRoomID string         `json:"prev_room_id,omitempty"`
	Deleted    bool           `json:"deleted,omitzero"`
	Ext        map[string]any `json:"ext,omitempty"`
}

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
	Ext    map[string]any `json:"ext,omitempty"`
	Raw    jsontext.Value `json:"-"`
}

// AuthResult is the result of auth (§3.2). A result with You nil
// authenticated nothing, such as the begin step of a ceremony; with You, the
// connection is signed in as You.
type AuthResult struct {
	You   *User  `json:"you,omitempty"`
	Token string `json:"token,omitempty"`
	// Extra holds more result fields, such as a webauthn begin's.
	Extra map[string]any `json:"-"`
}

// MessageParams are the params of a message request, or of a command
// (§3.5, §4.1, §4.4).
type MessageParams struct {
	// MessageID is set on a save of an existing message (§4.4), and empty
	// on a new one.
	MessageID string
	// RoomID is the room, or "" for the default room.
	RoomID string
	// Body is nil when the request leaves it out, as a deletion does.
	Body *Body
	// BodyRaw is the body exactly as sent, fields this package does not
	// know included, for a backend that keeps them; nil without a body.
	BodyRaw jsontext.Value
	ReplyTo *Ref
	Deleted bool
	// Ext merges into the message's ext (§4.13); nil when left out.
	Ext Ext
}

// MessageResult is the result of a message request: the new message's
// message_id, or nothing.
type MessageResult struct {
	MessageID string `json:"message_id,omitempty"`
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

// Encode renders a notification frame.
func Encode(method string, params any) (Frame, error) {
	return marshal(map[string]any{"method": method, "params": params})
}

func marshal(value any) ([]byte, error) { return wirecore.Marshal(value) }

// ValidID reports whether id may be a room_id or user_id (§2.4): not empty,
// with no whitespace or control characters.
func ValidID(id string) bool {
	return id != "" && !strings.ContainsFunc(id, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

// Clock issues log_ids (§2.1): the time in milliseconds, or the previous
// log_id plus one, so they strictly increase. The zero Clock is ready.
type Clock struct {
	mu   sync.Mutex
	last int64
}

// Next returns the next log_id.
func (c *Clock) Next() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = wirecore.NextLogID(c.last)
	return strconv.FormatInt(c.last, 10)
}
