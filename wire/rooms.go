package wire

import (
	"context"
	"encoding/json/jsontext"
	"slices"
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
	Members      []User `json:"members,omitempty"`
	MemberCount  int    `json:"member_count,omitzero"`
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
	Users       []User       `json:"users,omitempty"`
}

// Method makes a RoomUpdate a Notification.
func (RoomUpdate) Method() string { return "room_update" }

// The values of RoomListParams.Filter.
const (
	FilterAll       = "all"
	FilterJoined    = "joined"
	FilterNotJoined = "not_joined"
)

// RoomListParams are the params of room_list (§4.2.1). Filter is one of the
// Filter constants, FilterAll when the client sent none; an empty ID was
// not sent.
type RoomListParams struct {
	Filter       string
	ParentRoomID string
	RoomID       string
	Members      bool
	UserID       string
	// LatestLogID, if set, lists only the rooms that changed after it.
	LatestLogID *LogID
}

// RoomListResult is the result of room_list. Server sends the arrays the
// filter asks for, empty or not, and omits the others; Left only with a
// latest_log_id the backend applied.
type RoomListResult struct {
	Joined    []RoomView `json:"joined,omitzero"`
	NotJoined []RoomView `json:"not_joined,omitzero"`
	Left      []RoomRef  `json:"left,omitzero"`
	Users     []User     `json:"users,omitempty"`
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
// ID, and the user to add, or "" for the sender.
type RoomJoinParams struct {
	RoomID string
	UserID string
}

// RoomLeaveParams are the params of room_leave (§4.2.2): the room, a valid
// ID, and the user to remove, or "" for the sender.
type RoomLeaveParams struct {
	RoomID string
	UserID string
}

// RoomHandler handles room_list, room_join, room_leave, and room_set, and
// adds capability rooms (§4.2). A backend sends room_update itself. A
// join's or leave's UserID is empty unless the client names another user
// to add or remove; a backend that does not support that returns an
// Unsupported error. A backend that lists rooms but takes no other room
// request embeds UnimplementedRooms.
type RoomHandler interface {
	// RoomList answers with the rooms, which Send makes what the filter
	// asks for (RoomListResult).
	RoomList(ctx context.Context, c *Conn, p RoomListParams, reply Reply[RoomListResult]) error
	RoomJoin(ctx context.Context, c *Conn, p RoomJoinParams, reply Reply[Empty]) error
	RoomLeave(ctx context.Context, c *Conn, p RoomLeaveParams, reply Reply[Empty]) error
	RoomSet(ctx context.Context, c *Conn, p RoomSetParams, reply Reply[RoomSetResult]) error
}

// UnimplementedRooms refuses room_join, room_leave, and room_set as
// error/unsupported, for a RoomHandler that embeds it and implements only
// RoomList, such as a gateway to a chat system whose rooms its users
// cannot change.
type UnimplementedRooms struct{}

// RoomJoin refuses a join.
func (UnimplementedRooms) RoomJoin(context.Context, *Conn, RoomJoinParams, Reply[Empty]) error {
	return Unsupported("This server does not join rooms on request")
}

// RoomLeave refuses a leave.
func (UnimplementedRooms) RoomLeave(context.Context, *Conn, RoomLeaveParams, Reply[Empty]) error {
	return Unsupported("This server does not leave rooms on request")
}

// RoomSet refuses to make or edit a room.
func (UnimplementedRooms) RoomSet(context.Context, *Conn, RoomSetParams, Reply[RoomSetResult]) error {
	return Unsupported("This server does not make or edit rooms on request")
}

// normalizeRoomList makes a room_list result what its params ask for: the
// arrays the filter asks for are present even when empty (§4.2.1), and the
// others absent, and without members, no room has its members, nor the
// result users.
func normalizeRoomList(p RoomListParams, result RoomListResult) RoomListResult {
	if p.Filter != FilterNotJoined && result.Joined == nil {
		result.Joined = []RoomView{}
	}
	if p.Filter != FilterJoined && result.NotJoined == nil {
		result.NotJoined = []RoomView{}
	}
	if p.Filter == FilterJoined {
		result.NotJoined = nil
	}
	if p.Filter == FilterNotJoined {
		result.Joined = nil
	}
	if !p.Members {
		result.Users = nil
		// Copies, so that the backend's own slices stay as they are.
		for _, list := range []*[]RoomView{&result.Joined, &result.NotJoined} {
			if *list == nil {
				continue
			}
			*list = slices.Clone(*list)
			for i := range *list {
				(*list)[i].Members, (*list)[i].MemberCount = nil, 0
			}
		}
	}
	return result
}
