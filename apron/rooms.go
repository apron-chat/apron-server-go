package apron

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"

	"github.com/apron-chat/apron-server-go/apron/internal/core"
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
	// Raw, if set, is Room encoded, as Marshal encodes a Room, which is
	// sent in its place, as it is, once checked to be valid JSON, before
	// the delivery fields: for a server that keeps its room records
	// encoded, which then sends them as they are rather than decode each
	// to have it encoded again. A RoomView decoded has none.
	Raw jsontext.Value `json:"-"`
}

// roomViewJSON is a RoomView as encoding/json encodes one, without its
// MarshalJSONTo.
type roomViewJSON RoomView

// MarshalJSONTo writes the view: its Room, or Raw in its place, then its
// delivery fields.
func (v *RoomView) MarshalJSONTo(enc *jsontext.Encoder) error {
	if v.Raw == nil {
		return json.MarshalEncode(enc, (*roomViewJSON)(v))
	}
	if v.Raw.Kind() != '{' {
		return errors.New("apron: a RoomView's Raw is not a JSON object")
	}
	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	// The room's fields go first, and the members a Room has no field
	// for last, after the delivery fields, as a RoomView encodes its
	// Room's Unknown.
	var unknown []core.Field
	var err error
	ok := core.DecodeInPlace(v.Raw, func(dec *jsontext.Decoder) bool {
		if _, err = dec.ReadToken(); err != nil {
			return false
		}
		for dec.PeekKind() != '}' {
			var name, value jsontext.Value
			if name, err = dec.ReadValue(); err != nil {
				return false
			}
			if value, err = dec.ReadValue(); err != nil {
				return false
			}
			value = core.Part(dec, v.Raw, value)
			if !isRoomField(name) {
				var unquoted string
				if err = json.Unmarshal(name, &unquoted); err != nil {
					return false
				}
				unknown = append(unknown, core.Field{Name: unquoted, Value: value})
				continue
			}
			if err = enc.WriteValue(name); err != nil {
				return false
			}
			if err = enc.WriteValue(value); err != nil {
				return false
			}
		}
		return true
	})
	if !ok {
		if err == nil {
			err = errors.New("apron: a RoomView's Raw is not a JSON object")
		}
		return err
	}
	if v.LatestLogID != 0 {
		if err := enc.WriteToken(jsontext.String("latest_log_id")); err != nil {
			return err
		}
		if err := v.LatestLogID.MarshalJSONTo(enc); err != nil {
			return err
		}
	}
	if v.HistoryLogID != nil {
		if err := enc.WriteToken(jsontext.String("history_log_id")); err != nil {
			return err
		}
		if err := v.HistoryLogID.MarshalJSONTo(enc); err != nil {
			return err
		}
	}
	if v.Members != nil {
		if err := enc.WriteToken(jsontext.String("members")); err != nil {
			return err
		}
		if err := enc.WriteToken(jsontext.BeginArray); err != nil {
			return err
		}
		for i := range v.Members {
			if err := v.Members[i].MarshalJSONTo(enc); err != nil {
				return err
			}
		}
		if err := enc.WriteToken(jsontext.EndArray); err != nil {
			return err
		}
	}
	if v.MemberCount != 0 {
		if err := enc.WriteToken(jsontext.String("member_count")); err != nil {
			return err
		}
		if err := enc.WriteToken(jsontext.Int(int64(v.MemberCount))); err != nil {
			return err
		}
	}
	for _, m := range unknown {
		if err := enc.WriteToken(jsontext.String(m.Name)); err != nil {
			return err
		}
		if err := enc.WriteValue(m.Value); err != nil {
			return err
		}
	}
	return enc.WriteToken(jsontext.EndObject)
}

// isRoomField reports whether a member's name, quoted as it arrived, is
// one of a Room's fields; a name with escapes counts as another member's.
func isRoomField(quoted jsontext.Value) bool {
	switch string(quoted) {
	case `"room_id"`, `"log_id"`, `"prev_log_id"`, `"parent_room_id"`, `"private"`, `"title"`, `"description"`, `"ext"`:
		return true
	}
	return false
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
// when the client sent none; an empty ID was not sent. They encode as a
// client sends them, an empty Filter left out, which is RoomsAll.
type RoomListParams struct {
	Filter       RoomFilter `json:"filter,omitempty"`
	ParentRoomID string     `json:"parent_room_id,omitempty"`
	RoomID       string     `json:"room_id,omitempty"`
	Members      bool       `json:"members,omitzero"`
	UserID       string     `json:"user_id,omitempty"`
	// LatestLogID, if set, lists only the rooms that changed after it.
	LatestLogID *LogID `json:"latest_log_id,omitzero"`
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
// ID, and the user to remove, or "" for the sender. They encode as a client
// sends them.
type RoomLeaveParams struct {
	RoomID string `json:"room_id"`
	UserID string `json:"user_id,omitempty"`
}
