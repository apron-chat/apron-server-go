package wire

import (
	"context"
)

// Room is a room record (§3.4, §4.2). LatestLogID, HistoryLogID, Members,
// and MemberCount describe the room as one client sees it.
type Room struct {
	RoomID       string         `json:"room_id"`
	LogID        LogID          `json:"log_id,omitzero"`
	PrevLogID    LogID          `json:"prev_log_id,omitzero"`
	ParentRoomID string         `json:"parent_room_id,omitempty"`
	Private      bool           `json:"private,omitzero"`
	Title        string         `json:"title,omitempty"`
	Description  string         `json:"description,omitempty"`
	Ext          map[string]any `json:"ext,omitempty"`
	LatestLogID  LogID          `json:"latest_log_id,omitzero"`
	// HistoryLogID is the inclusive lower bound of the room's history, or
	// nil for none (§4.3.3).
	HistoryLogID *LogID `json:"history_log_id"`
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
	Joined    []Room    `json:"joined,omitzero"`
	NotJoined []Room    `json:"not_joined,omitzero"`
	Left      []RoomRef `json:"left,omitzero"`
	Users     []User    `json:"users,omitempty"`
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

// parseRoomList reads room_list's params.
func parseRoomList(r *Request) (p RoomListParams, err error) {
	var present bool
	if p.Filter, present, err = r.String("filter"); err != nil {
		return p, err
	}
	switch {
	case !present:
		p.Filter = FilterAll
	case p.Filter != FilterAll && p.Filter != FilterJoined && p.Filter != FilterNotJoined:
		return p, InvalidParams("filter must be joined, not_joined, or all")
	}
	if p.RoomID, _, err = r.IDParam("room_id"); err != nil {
		return p, err
	}
	if p.ParentRoomID, _, err = r.IDParam("parent_room_id"); err != nil {
		return p, err
	}
	if p.UserID, _, err = r.IDParam("user_id"); err != nil {
		return p, err
	}
	if p.Members, _, err = r.Bool("members"); err != nil {
		return p, err
	}
	if latest, present, err := r.LogID("latest_log_id"); err != nil {
		return p, err
	} else if present {
		p.LatestLogID = &latest
	}
	return p, nil
}

// parseMembership reads the params of room_join and room_leave (§4.2.2).
func parseMembership(r *Request) (roomID, userID string, err error) {
	roomID, present, err := r.IDParam("room_id")
	if err == nil && !present {
		err = Missing("room_id")
	}
	if err != nil {
		return "", "", err
	}
	userID, _, err = r.IDParam("user_id")
	return roomID, userID, err
}

// parseRoomSet reads room_set's params.
func parseRoomSet(r *Request) (p RoomSetParams, err error) {
	if p.RoomID, _, err = r.IDParam("room_id"); err != nil {
		return p, err
	}
	if p.ParentRoomID, _, err = r.IDParam("parent_room_id"); err != nil {
		return p, err
	}
	if title, present, err := r.String("title"); err != nil {
		return p, err
	} else if present {
		p.Title = &title
	}
	if description, present, err := r.String("description"); err != nil {
		return p, err
	} else if present {
		p.Description = &description
	}
	if private, present, err := r.Bool("private"); err != nil {
		return p, err
	} else if present {
		p.Private = &private
	}
	if p.Ext, _, err = r.Ext("ext"); err != nil {
		return p, err
	}
	return p, nil
}

func roomHandler(rooms Rooms, name string) func(context.Context, *Conn, *Request) (any, error) {
	switch name {
	case "room_list":
		return func(ctx context.Context, c *Conn, r *Request) (any, error) {
			p, perr := parseRoomList(r)
			if perr != nil {
				return nil, perr
			}
			result, err := rooms.RoomList(ctx, c, p)
			if err != nil || r.Replied() {
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
			roomID, userID, perr := parseMembership(r)
			if perr != nil {
				return nil, perr
			}
			if name == "room_join" {
				return nil, rooms.RoomJoin(ctx, c, roomID, userID)
			}
			return nil, rooms.RoomLeave(ctx, c, roomID, userID)
		}
	case "room_set":
		return func(ctx context.Context, c *Conn, r *Request) (any, error) {
			p, perr := parseRoomSet(r)
			if perr != nil {
				return nil, perr
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
