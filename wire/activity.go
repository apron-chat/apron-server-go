package wire

import (
	"context"
	"encoding/json/jsontext"
)

// ReactParams are the params of reactions (§4.7): the sender's whole set
// of emoji on a message, which replaces the last; empty clears it.
type ReactParams struct {
	MessageID LogID
	// Emojis are non-empty strings, in the order sent; duplicates are the
	// backend's to collapse, as it compares emoji (§4.7).
	Emojis []string
}

// Reactions is a reactions record (§4.7): a reactions notification, and a
// history result's reactions (§4.3). Each element replaces its user's set
// of emoji on the message.
type Reactions struct {
	LogID     LogID      `json:"log_id"`
	MessageID LogID      `json:"message_id"`
	RoomID    string     `json:"room_id"`
	Reactions []Reaction `json:"reactions"`
}

// Method makes a reactions record a Notification.
func (Reactions) Method() string { return "reactions" }

// Reaction is one user's set of emoji in a reactions record: From is a
// recorded user object (§3.3.1), and Emojis are empty when the user took
// theirs away.
type Reaction struct {
	From   User     `json:"from"`
	Emojis []string `json:"emojis"`
}

// ReactionHandler handles reactions, and adds capability reactions (§4.7).
// It broadcasts the reaction set, including to c, before it replies.
type ReactionHandler interface {
	React(ctx context.Context, c *Conn, p ReactParams, reply Reply[OK]) error
}

// ActivityParams are an activity notification's (§4.6): typing, and how far
// the user has read. A nil field was left out.
type ActivityParams struct {
	// RoomID is the room, or "" for the default room.
	RoomID string
	// Typing is the seconds to show the user as typing, 0 to stop.
	Typing *int
	// ReadMessageID is the message the user has read up to.
	ReadMessageID *LogID
}

// Activity is an activity notification a server sends (§4.6): a change
// to the transient state of From in a room, such as relayed to the room's
// members, or to the user's own connections. A field left out (a nil
// Typing, a zero ReadMessageID) does not change its state. Its members
// encode in the order of their names.
type Activity struct {
	// From is a recorded user object (§3.3.1).
	From User `json:"from"`
	// ReadMessageID is the message From has read up to, in the room.
	ReadMessageID LogID  `json:"read_message_id,omitzero"`
	RoomID        string `json:"room_id"`
	// Typing is the seconds to show From as typing, 0 to stop.
	Typing *int `json:"typing,omitzero"`
	// Unknown holds the members this package has no field for, as they
	// arrived, and is sent after the others.
	Unknown jsontext.Value `json:",embed"`
}

// Method makes an Activity a Notification.
func (Activity) Method() string { return "activity" }

// ActivityHandler handles activity notifications, and adds capability
// activity (§4.6). They get no reply: params that are not valid are
// ignored. A backend relays the activity it applies as Activity
// notifications.
type ActivityHandler interface {
	Activity(ctx context.Context, c *Conn, p ActivityParams)
}
