package apron

import (
	"encoding/json/jsontext"
)

// Room is a room record (§4.2): its server and client fields, as it is
// logged, and as a history result carries it (§4.3.3).
type Room struct {
	RoomID       string `json:"room_id"`
	LogID        LogID  `json:"log_id,omitzero"`
	PrevLogID    LogID  `json:"prev_log_id,omitzero"`
	ParentRoomID string `json:"parent_room_id,omitempty"`
	Private      bool   `json:"private,omitzero"`
	Title        string `json:"title,omitempty"`
	Description  string `json:"description,omitempty"`
	Ext          Ext    `json:"ext,omitempty"`
	// Unknown holds the members this package has no field for, as they
	// arrived, and is sent after the others.
	Unknown jsontext.Value `json:",embed"`
}

// RoomView is a room record as one client sees it, with its delivery fields
// (§4.2), as room_list and room_update carry it. With capability history,
// LatestLogID and HistoryLogID are set (§4.3.3).
type RoomView struct {
	Room
	LatestLogID LogID `json:"latest_log_id,omitzero"`
	// HistoryLogID is the inclusive lower bound of the room's history: nil
	// leaves the field out, and a pointer to 0 sends null, for no history
	// (§4.3.3). Decoding, a null is nil, as for any pointer.
	HistoryLogID *LogID `json:"history_log_id,omitzero"`
	// Members are current user objects (§4.2.1), each with the fields it
	// carries empty, such as a status of "" (§4.5.2), in its Cleared. With
	// members, every room has them, empty too (§4.2.1): nil leaves the
	// field out, and an empty slice sends [].
	Members     []CurrentUser `json:"members,omitzero"`
	MemberCount int           `json:"member_count,omitzero"`
}

// RoomRef names a room, as in room_update's left.
type RoomRef struct {
	RoomID string `json:"room_id"`
}

// Member is one user's membership in a membership record.
type Member struct {
	User   User `json:"user"`
	Joined bool `json:"joined"`
}

// Membership is a membership record (§4.2.2).
type Membership struct {
	LogID   LogID    `json:"log_id"`
	RoomID  string   `json:"room_id"`
	Members []Member `json:"members"`
}

// RoomUpdate is a room_update notification (§4.2.3).
type RoomUpdate struct {
	Joined      []RoomView   `json:"joined,omitempty"`
	Left        []RoomRef    `json:"left,omitempty"`
	Memberships []Membership `json:"memberships,omitempty"`
	Updated     []RoomView   `json:"updated,omitempty"`
	// Users are complete current objects (§3.3.1).
	Users []CurrentUser `json:"users,omitempty"`
}

// Method makes a RoomUpdate a Notification.
func (RoomUpdate) Method() string { return "room_update" }

// RoomFilter is a room_list filter (§4.2.1): which rooms it lists.
type RoomFilter string

// The room_list filters.
const (
	// RoomsAll lists both joined and not_joined, the default.
	RoomsAll RoomFilter = "all"
	// RoomsJoined lists only joined: the rooms the user joined.
	RoomsJoined RoomFilter = "joined"
	// RoomsNotJoined lists only not_joined: the visible rooms the user did
	// not join.
	RoomsNotJoined RoomFilter = "not_joined"
)

// RoomListParams are the params of room_list (§4.2.1). Filter is RoomsAll
// when the client sent none; an empty ID was not sent.
type RoomListParams struct {
	Filter       RoomFilter
	ParentRoomID string
	RoomID       string
	Members      bool
	UserID       string
	// LatestLogID, if set, lists only the rooms that changed after it.
	LatestLogID *LogID
}

// RoomListResult is the result of room_list. A server.Server sends the
// arrays the filter asks for, empty or not, and omits the others; Left
// only with a latest_log_id the backend applied.
type RoomListResult struct {
	Joined    []RoomView `json:"joined,omitzero"`
	NotJoined []RoomView `json:"not_joined,omitzero"`
	Left      []RoomRef  `json:"left,omitzero"`
	// Users are the members' complete current objects (§3.3.1), each with
	// the fields it carries empty, such as a status of "" (§4.5.2), in its
	// Cleared; an empty slice sends [].
	Users []CurrentUser `json:"users,omitzero"`
}

// RoomSetParams are the params of room_set (§4.2.4): a new room without
// RoomID, else an edit that merges into that room's client fields, where an
// omitted field keeps its value and "" clears it, but for Private, which is
// fixed at creation, and Ext, which merges one level into its keys.
type RoomSetParams struct {
	RoomID       string
	ParentRoomID string
	// Title and Description are nil when the request leaves them out.
	Title       *string
	Description *string
	// Private is nil when the request leaves it out.
	Private *bool
	// Ext is nil when the request leaves it out.
	Ext Ext
}

// RoomSetResult is the result of room_set: the room made or edited.
type RoomSetResult struct {
	RoomID string `json:"room_id"`
}

// RoomJoinParams are the params of room_join (§4.2.2): the room, a valid
// ID, and the user to add, or "" for the sender. They encode as a client
// sends them.
type RoomJoinParams struct {
	RoomID string `json:"room_id"`
	UserID string `json:"user_id,omitempty"`
}

// RoomLeaveParams are the params of room_leave (§4.2.2): the room, a valid
// ID, and the user to remove, or "" for the sender.
type RoomLeaveParams struct {
	RoomID string
	UserID string
}
