package wire

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// Room is a room record (§3.4, §4.2). LatestLogID, HistoryLogID, Members,
// and MemberCount describe the room as one client sees it.
type Room struct {
	RoomID       string         `json:"room_id"`
	LogID        string         `json:"log_id,omitempty"`
	PrevLogID    string         `json:"prev_log_id,omitempty"`
	ParentRoomID string         `json:"parent_room_id,omitempty"`
	Private      bool           `json:"private,omitzero"`
	Title        string         `json:"title,omitempty"`
	Description  string         `json:"description,omitempty"`
	Ext          map[string]any `json:"ext,omitempty"`
	LatestLogID  string         `json:"latest_log_id,omitempty"`
	// HistoryLogID is the inclusive lower bound of the room's history, or
	// nil for none (§4.3.3).
	HistoryLogID *string `json:"history_log_id"`
	Members      []User  `json:"members,omitempty"`
	MemberCount  int     `json:"member_count,omitzero"`
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
	LogID   string   `json:"log_id"`
	RoomID  string   `json:"room_id"`
	Members []Member `json:"members"`
}

// RoomUpdate is a room_update notification (§4.2.3).
type RoomUpdate struct {
	Joined      []Room       `json:"joined,omitempty"`
	Left        []RoomRef    `json:"left,omitempty"`
	Memberships []Membership `json:"memberships,omitempty"`
	Updated     []Room       `json:"updated,omitempty"`
	Users       []User       `json:"users,omitempty"`
}

// The values of RoomListParams.Filter.
const (
	FilterAll       = "all"
	FilterJoined    = "joined"
	FilterNotJoined = "not_joined"
)

// RoomListParams are the params of room_list (§4.2.1). Filter is one of the
// Filter constants, FilterAll when the client sent none.
type RoomListParams struct {
	Filter       string `json:"filter,omitempty"`
	ParentRoomID string `json:"parent_room_id,omitempty"`
	RoomID       string `json:"room_id,omitempty"`
	Members      bool   `json:"members,omitzero"`
	UserID       string `json:"user_id,omitempty"`
	LatestLogID  string `json:"latest_log_id,omitempty"`
}

// RoomListResult is the result of room_list. Server sends the arrays the
// filter asks for, empty or not, and omits the others; Left only with a
// latest_log_id the backend applied.
type RoomListResult struct {
	Joined    []Room    `json:"joined,omitzero"`
	NotJoined []Room    `json:"not_joined,omitzero"`
	Left      []RoomRef `json:"left,omitzero"`
	Users     []User    `json:"users,omitempty"`
}

// RoomSetParams are the params of room_set (§4.2.4): a new room without
// RoomID, else an edit of that room's client fields.
type RoomSetParams struct {
	RoomID       string         `json:"room_id,omitempty"`
	ParentRoomID string         `json:"parent_room_id,omitempty"`
	Private      *bool          `json:"private,omitempty"`
	Title        string         `json:"title,omitempty"`
	Description  string         `json:"description,omitempty"`
	Ext          map[string]any `json:"ext,omitempty"`
	Raw          jsontext.Value `json:"-"`
}

// Rooms handles room_list, room_join, room_leave, and room_set, and adds
// capability rooms (§4.2). A backend sends room_update itself. userID is
// empty unless the client names another user to add or remove; a backend
// that does not support that returns an Unsupported error.
type Rooms interface {
	RoomList(ctx context.Context, c *Conn, p RoomListParams) (RoomListResult, error)
	RoomJoin(ctx context.Context, c *Conn, roomID, userID string) error
	RoomLeave(ctx context.Context, c *Conn, roomID, userID string) error
	RoomSet(ctx context.Context, c *Conn, p RoomSetParams) (roomID string, err error)
}

func roomHandler(rooms Rooms, name string) func(context.Context, *Conn, *Request) (any, error) {
	switch name {
	case "room_list":
		return func(ctx context.Context, c *Conn, r *Request) (any, error) {
			raw := r.Params
			var p RoomListParams
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, InvalidParams("Invalid room_list: " + err.Error())
			}
			switch p.Filter {
			case "":
				p.Filter = FilterAll
			case FilterAll, FilterJoined, FilterNotJoined:
			default:
				return nil, InvalidParams(`filter is "joined", "not_joined", or "all"`)
			}
			for _, id := range []string{p.RoomID, p.ParentRoomID, p.UserID} {
				if id != "" && !ValidID(id) {
					return nil, InvalidParams("Invalid ID " + id)
				}
			}
			result, err := rooms.RoomList(ctx, c, p)
			if err != nil {
				return nil, err
			}
			// The arrays the filter asks for are present even when empty
			// (§4.2.1), and the others absent.
			if p.Filter != FilterNotJoined && result.Joined == nil {
				result.Joined = []Room{}
			}
			if p.Filter != FilterJoined && result.NotJoined == nil {
				result.NotJoined = []Room{}
			}
			if p.Filter == FilterJoined {
				result.NotJoined = nil
			}
			if p.Filter == FilterNotJoined {
				result.Joined = nil
			}
			if !p.Members {
				result.Users = nil
				for _, list := range [][]Room{result.Joined, result.NotJoined} {
					for i := range list {
						list[i].Members, list[i].MemberCount = nil, 0
					}
				}
			}
			return result, nil
		}
	case "room_join", "room_leave":
		return func(ctx context.Context, c *Conn, r *Request) (any, error) {
			raw := r.Params
			var p struct {
				RoomID string `json:"room_id"`
				UserID string `json:"user_id,omitempty"`
			}
			if err := json.Unmarshal(raw, &p); err != nil || !ValidID(p.RoomID) || (p.UserID != "" && !ValidID(p.UserID)) {
				return nil, InvalidParams(name + " needs a room_id")
			}
			if name == "room_join" {
				return nil, rooms.RoomJoin(ctx, c, p.RoomID, p.UserID)
			}
			return nil, rooms.RoomLeave(ctx, c, p.RoomID, p.UserID)
		}
	case "room_set":
		return func(ctx context.Context, c *Conn, r *Request) (any, error) {
			raw := r.Params
			var p RoomSetParams
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, InvalidParams("Invalid room_set: " + err.Error())
			}
			p.Raw = raw
			for _, id := range []string{p.RoomID, p.ParentRoomID} {
				if id != "" && !ValidID(id) {
					return nil, InvalidParams("Invalid room_id " + id)
				}
			}
			roomID, err := rooms.RoomSet(ctx, c, p)
			if err != nil {
				return nil, err
			}
			return map[string]any{"room_id": roomID}, nil
		}
	}
	return nil
}
