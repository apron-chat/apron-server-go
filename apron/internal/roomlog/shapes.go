package roomlog

import (
	"encoding/json/jsontext"

	"github.com/apron-chat/apron-server-go/wire"
)

// The shapes of the records the server builds itself. Each declares its
// members in the order of their names, as the server's maps encoded them
// (keys sorted), and leaves out what the maps left out, so that a record
// encodes byte for byte as it did; the store holds those bytes. Unknown
// keeps members a shape has no field for, such as those of a stored value
// written by another version, which then encode after the others.

// Author is a recorded user object (§3.3.1), carried in logged records: a
// message's or reaction's from, a membership's user, a read cursor's from.
// Avatars and ext travel only in current objects (§3.3, §4.8.6).
type Author struct {
	Name    string         `json:"name,omitzero"`
	UserID  string         `json:"user_id"`
	Unknown jsontext.Value `json:",embed"`
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
	Ext          wire.Ext       `json:"ext,omitempty"`
	LogID        string         `json:"log_id"`
	ParentRoomID string         `json:"parent_room_id,omitzero"`
	PrevLogID    string         `json:"prev_log_id,omitzero"`
	Private      bool           `json:"private,omitzero"`
	RoomID       string         `json:"room_id"`
	Title        string         `json:"title,omitzero"`
	Unknown      jsontext.Value `json:",embed"`
}
