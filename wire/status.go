package wire

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
)

// The status values every server with capability status accepts (§4.5.1),
// and the presences it derives.
const (
	StatusOnline    = "online"
	StatusIdle      = "idle"
	StatusOffline   = "offline"
	StatusDND       = "dnd"
	StatusInvisible = "invisible"
)

// StatusParams are the params of a status request (§4.5.3). A nil field is
// left as it is.
type StatusParams struct {
	// RoomID limits Mute to one room and its threads.
	RoomID string
	// Idle reports the sending connection unattended for its seconds, or
	// attended again when its Attended is set.
	Idle *Idle
	// Mute silences the user's notifications.
	Mute *Mute
}

// Idle is a status request's idle: false (Attended), or seconds.
type Idle struct {
	Attended bool
	Seconds  int64
}

// Mute is a status request's mute: true, false, or seconds.
type Mute struct {
	On      bool
	Seconds int64
}

// MarshalJSON writes a mute as the protocol does: false, true, or seconds.
func (m Mute) MarshalJSON() ([]byte, error) {
	switch {
	case !m.On:
		return []byte("false"), nil
	case m.Seconds > 0:
		return json.Marshal(m.Seconds)
	}
	return []byte("true"), nil
}

// UnmarshalJSON reads a mute as the protocol writes it: false, true, or
// a positive number of seconds.
func (m *Mute) UnmarshalJSON(raw []byte) error {
	var seconds int64
	switch raw := bytes.TrimSpace(raw); {
	case bytes.Equal(raw, []byte("true")):
		*m = Mute{On: true}
	case bytes.Equal(raw, []byte("false")):
		*m = Mute{}
	case !isNull(raw) && json.Unmarshal(raw, &seconds) == nil && seconds > 0:
		*m = Mute{On: true, Seconds: seconds}
	default:
		return errors.New("wire: mute must be true, false, or a positive whole number of seconds")
	}
	return nil
}

// StatusUpdate is a status notification (§4.5.3): one of the user's
// mutes in effect, sent to the user's own connections, such as after a
// status request, or a sign-in. RoomID is the room the mute is for, or ""
// for the user's mute of everything.
type StatusUpdate struct {
	RoomID string `json:"room_id,omitempty"`
	Mute   Mute   `json:"mute"`
}

// Method makes a StatusUpdate a Notification.
func (StatusUpdate) Method() string { return "status" }

// Statuser handles the status request, and adds capability status (§4.5).
// A backend sends the user's mutes to its connections as status
// notifications, and statuses as user notifications. The statuses it
// accepts with me, other than online and "", are ServerInfo.Status.
type Statuser interface {
	Status(ctx context.Context, c *Conn, p StatusParams) error
}

// parseStatus reads a status request's params (§4.5.3): idle is false or
// a non-negative number of seconds, mute true, false, or a positive number
// of seconds, and room_id scopes only the mute, so it is sent with one.
func parseStatus(r *Request) (p StatusParams, err error) {
	var scoped bool
	if p.RoomID, scoped, err = r.IDParam("room_id"); err != nil {
		return p, err
	}
	if raw, ok := r.Param("idle"); ok {
		var seconds int64
		switch raw := bytes.TrimSpace(raw); {
		case bytes.Equal(raw, []byte("false")):
			p.Idle = &Idle{Attended: true}
		case !isNull(raw) && json.Unmarshal(raw, &seconds) == nil && seconds >= 0:
			p.Idle = &Idle{Seconds: seconds}
		default:
			return p, InvalidParams("idle must be false or a non-negative whole number of seconds")
		}
	}
	if raw, ok := r.Param("mute"); ok {
		var mute Mute
		if mute.UnmarshalJSON(raw) != nil {
			return p, InvalidParams("mute must be true, false, or a positive whole number of seconds")
		}
		p.Mute = &mute
	}
	if scoped && p.Mute == nil {
		return p, InvalidParams("room_id scopes a mute; send it with mute")
	}
	return p, nil
}

func statusHandler(st Statuser) func(context.Context, *Conn, *Request) (any, error) {
	return func(ctx context.Context, c *Conn, r *Request) (any, error) {
		p, err := parseStatus(r)
		if err != nil {
			return nil, err
		}
		return nil, st.Status(ctx, c, p)
	}
}
