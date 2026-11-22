package wire

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
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
	Signup       []string          `json:"signup,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	Ping         int               `json:"ping,omitempty"`
	Title        string            `json:"title,omitempty"`
	Description  string            `json:"description,omitempty"`
	Avatar       string            `json:"avatar,omitempty"`
	Status       []string          `json:"status,omitempty"`
	Ext          map[string]any    `json:"ext,omitempty"`
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
	Deleted    bool           `json:"deleted,omitempty"`
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
// (§3.5, §4.1). Raw holds every param.
type MessageParams struct {
	MessageID string         `json:"message_id,omitempty"`
	RoomID    string         `json:"room_id,omitempty"`
	Body      *Body          `json:"body,omitempty"`
	ReplyTo   *Ref           `json:"reply_to,omitempty"`
	Deleted   bool           `json:"deleted,omitempty"`
	Ext       map[string]any `json:"ext,omitempty"`
	Raw       jsontext.Value `json:"-"`
}

// MessageResult is the result of a message request: the new message's
// message_id, or nothing.
type MessageResult struct {
	MessageID string `json:"message_id,omitempty"`
}

// MeParams are the params of me (§3.3.2): the profile fields to change. A
// nil field is left as it is; an empty one clears it.
type MeParams struct {
	Name   *string        `json:"name,omitempty"`
	Avatar *string        `json:"avatar,omitempty"`
	Raw    jsontext.Value `json:"-"`
}

// Frame is one encoded frame, which can be sent to many connections without
// being encoded again.
type Frame []byte

// Encode renders a notification frame.
func Encode(method string, params any) (Frame, error) {
	return marshal(map[string]any{"method": method, "params": params})
}

var jsonOptions = json.JoinOptions(
	json.Deterministic(true),
	json.FormatNilSliceAsNull(true),
	json.FormatNilMapAsNull(true),
)

func marshal(value any) ([]byte, error) { return json.Marshal(value, jsonOptions) }

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
	c.last = max(time.Now().UnixMilli(), c.last+1)
	return strconv.FormatInt(c.last, 10)
}
