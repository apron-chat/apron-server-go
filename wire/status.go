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
	Idle *IdleReport
	// Mute silences the user's notifications.
	Mute *Mute
}

// IdleReport is a status request's idle (§4.5.3), which reports whether
// somebody attends the sending connection: false, which is Attended, or
// the Seconds it has been unattended. It is not a status: the presence
// idle (StatusIdle) is what a server derives from it.
type IdleReport struct {
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

// StatusHandler handles the status request, and adds capability status
// (§4.5). A backend sends the user's mutes to its connections as status
// notifications, and statuses as user notifications. The statuses it
// accepts with me, other than online and "", are ServerInfo.Status.
type StatusHandler interface {
	Status(ctx context.Context, c *Conn, p StatusParams, reply Reply[OK]) error
}
