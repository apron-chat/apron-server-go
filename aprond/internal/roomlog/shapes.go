package roomlog

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"strings"
	"sync"

	"github.com/apron-chat/apron-server-go/apron"
)

// The shapes of the records. Each declares its members in the order of
// their names, as the server's maps encoded them (keys sorted), and leaves
// out what the maps left out, so that a record encodes byte for byte as it
// did; the store holds those bytes. Unknown keeps members a shape has no
// field for, such as those of a stored value written by another version,
// which then encode after the others. A message's Body sorts its members
// itself, since a client's body may have any.

// Author is a recorded user object (§3.3.1), carried in logged records: a
// message's or reaction's from, a membership's user, a read cursor's from.
// Avatars and ext travel only in current objects (§3.3, §4.8.6).
type Author struct {
	Name    string         `json:"name,omitzero"`
	UserID  string         `json:"user_id"`
	Unknown jsontext.Value `json:",embed"`
}

// User is the author as package apron's recorded user object, such as an
// activity notification's from (§4.6), with the members it has no field
// for.
func (a Author) User() apron.User {
	return apron.User{UserID: a.UserID, Name: a.Name, Unknown: a.Unknown}
}

// Membership is a membership record (§4.2.2).
type Membership struct {
	LogID   string   `json:"log_id"`
	Members []Member `json:"members"`
	RoomID  string   `json:"room_id"`
}

// Member is one user's join or leave in a membership record.
type Member struct {
	Joined bool   `json:"joined"`
	User   Author `json:"user"`
}

// NewMembership is the record of user joining or leaving room.
func NewMembership(id int64, room string, user Author, joined bool) *Record {
	return New(id, KindMembership, Membership{
		LogID: FormatID(id), RoomID: room, Members: []Member{{User: user, Joined: joined}},
	})
}

// Reactions is a reactions record (§4.7); reaction sets carry no
// prev_log_id (§4.3.1).
type Reactions struct {
	LogID     string     `json:"log_id"`
	MessageID string     `json:"message_id"`
	Reactions []Reaction `json:"reactions"`
	RoomID    string     `json:"room_id"`
}

// Reaction is one user's set of emoji, empty when they took theirs away.
type Reaction struct {
	Emojis []string `json:"emojis"`
	From   Author   `json:"from"`
}

// NewReactions is the record of reactions to a message in room.
func NewReactions(id int64, message, room string, reactions []Reaction) *Record {
	return New(id, KindReactions, Reactions{LogID: FormatID(id), MessageID: message, Reactions: reactions, RoomID: room})
}

// RoomRecord is a room record (§4.2.4): the server's fields, and the client
// fields, title, description, and ext, which are left out when empty.
type RoomRecord struct {
	Description  string         `json:"description,omitzero"`
	Ext          apron.Ext      `json:"ext,omitempty"`
	LogID        string         `json:"log_id"`
	ParentRoomID string         `json:"parent_room_id,omitzero"`
	PrevLogID    string         `json:"prev_log_id,omitzero"`
	Private      bool           `json:"private,omitzero"`
	RoomID       string         `json:"room_id"`
	Title        string         `json:"title,omitzero"`
	Unknown      jsontext.Value `json:",embed"`
}

// Message is a message snapshot (§3.5), as the server logs it and sends it
// in `message` notifications and history. Its client fields, body,
// reply_to, and ext, are what the sender saved; the others are the
// server's.
type Message struct {
	Body       *Body          `json:"body,omitzero"`
	Deleted    bool           `json:"deleted,omitzero"`
	Ext        apron.Ext      `json:"ext,omitempty"`
	From       Author         `json:"from"`
	LogID      string         `json:"log_id"`
	MessageID  string         `json:"message_id"`
	PrevLogID  string         `json:"prev_log_id,omitzero"`
	PrevRoomID string         `json:"prev_room_id,omitzero"`
	ReplyTo    *Ref           `json:"reply_to,omitzero"`
	RoomID     string         `json:"room_id"`
	Unknown    jsontext.Value `json:",embed"`
}

// Ref names a message, as a snapshot's reply_to.
type Ref struct {
	MessageID string `json:"message_id"`
}

// NewMessage is the record of a message snapshot, logged at the log_id id.
func NewMessage(id int64, snapshot *Message) *Record {
	record := New(id, KindMessage, snapshot)
	record.Message = snapshot.MessageID
	return record
}

// DecodeMessage decodes a message snapshot, as its record holds it, or
// returns nil when raw is not one.
func DecodeMessage(raw jsontext.Value) *Message {
	var m Message
	if json.Unmarshal(raw, &m) != nil || m.MessageID == "" {
		return nil
	}
	return &m
}

// ReplyID is the message_id the snapshot replies to, "" for none.
func (m *Message) ReplyID() string {
	if m.ReplyTo == nil {
		return ""
	}
	return m.ReplyTo.MessageID
}

// Mentions are the user_ids the snapshot's body mentions (§3.5.5).
func (m *Message) Mentions() []string {
	if m.Body == nil {
		return nil
	}
	return m.Body.Mentions
}

// Body is a message body (§3.5) as a snapshot holds it: a apron.Body, which
// keeps a member the sender left out out, one it sent empty empty, and
// those it has no field for (Unknown). Its members encode in the order of
// their names, Unknown's among them, as the maps the server kept bodies in
// did.
type Body struct{ apron.Body }

// MarshalJSONTo writes the body's members, sorted by name: those
// apron.Body writes, read back as they are and written again in order,
// without a map of them.
func (b Body) MarshalJSONTo(enc *jsontext.Encoder) error {
	raw, err := json.Marshal(b.Body)
	if err != nil {
		return err
	}
	r := bodyReaders.Get().(*bodyReader)
	defer bodyReaders.Put(r)
	members, err := r.members(raw)
	if err != nil {
		return err
	}
	slices.SortFunc(members, func(a, b bodyMember) int { return strings.Compare(a.name, b.name) })
	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	for _, m := range members {
		if err := enc.WriteToken(jsontext.String(m.name)); err != nil {
			return err
		}
		if err := enc.WriteValue(m.value); err != nil {
			return err
		}
	}
	return enc.WriteToken(jsontext.EndObject)
}

// bodyMember is a member of an encoded body: its name, and its value as
// encoded.
type bodyMember struct {
	name  string
	value jsontext.Value
}

// bodyReader reads the members of an encoded body; bodyReaders keeps them
// for reuse, as every message snapshot encodes a body.
type bodyReader struct {
	in   bytes.Reader
	dec  *jsontext.Decoder
	list []bodyMember
}

var bodyReaders = sync.Pool{New: func() any {
	r := new(bodyReader)
	r.dec = jsontext.NewDecoder(&r.in)
	return r
}}

// members returns the members of the object raw, in order, their values
// slices of raw. They are valid until the next call.
func (r *bodyReader) members(raw []byte) ([]bodyMember, error) {
	r.in.Reset(raw)
	r.dec.Reset(&r.in)
	r.list = r.list[:0]
	if _, err := r.dec.ReadToken(); err != nil {
		return nil, err
	}
	for r.dec.PeekKind() == '"' {
		token, err := r.dec.ReadToken()
		if err != nil {
			return nil, err
		}
		name := token.String()
		start := r.dec.InputOffset()
		if _, err := r.dec.ReadValue(); err != nil {
			return nil, err
		}
		// The value follows the name's colon, and any space around it.
		value := bytes.TrimLeft(raw[start:r.dec.InputOffset()], ": \t\r\n")
		r.list = append(r.list, bodyMember{name: name, value: value})
	}
	if _, err := r.dec.ReadToken(); err != nil {
		return nil, err
	}
	return r.list, nil
}
