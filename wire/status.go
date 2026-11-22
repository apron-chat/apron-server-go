package wire

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
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
	Seconds  int
}

// Mute is a status request's mute: true, false, or seconds.
type Mute struct {
	On      bool
	Seconds int
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

// Statuser handles the status request, and adds capability status (§4.5).
// A backend sends the user's mutes to its connections as status
// notifications, and statuses as user notifications. The statuses it
// accepts with me, other than online and "", are ServerInfo.Status.
type Statuser interface {
	Status(ctx context.Context, c *Conn, p StatusParams) error
}

func parseStatus(raw jsontext.Value) (StatusParams, error) {
	var p struct {
		RoomID string         `json:"room_id,omitempty"`
		Idle   jsontext.Value `json:"idle,omitzero"`
		Mute   jsontext.Value `json:"mute,omitzero"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return StatusParams{}, InvalidParams("Invalid status: " + err.Error())
	}
	if p.RoomID != "" && !ValidID(p.RoomID) {
		return StatusParams{}, InvalidParams("Invalid room_id")
	}
	out := StatusParams{RoomID: p.RoomID}
	if len(p.Idle) > 0 {
		var attended bool
		var seconds int
		switch {
		case json.Unmarshal(p.Idle, &attended) == nil && !attended:
			out.Idle = &Idle{Attended: true}
		case json.Unmarshal(p.Idle, &seconds) == nil && seconds >= 0:
			out.Idle = &Idle{Seconds: seconds}
		default:
			return StatusParams{}, InvalidParams("idle is false or a number of seconds")
		}
	}
	if len(p.Mute) > 0 {
		var on bool
		var seconds int
		switch {
		case json.Unmarshal(p.Mute, &on) == nil:
			out.Mute = &Mute{On: on}
		case json.Unmarshal(p.Mute, &seconds) == nil && seconds > 0:
			out.Mute = &Mute{On: true, Seconds: seconds}
		default:
			return StatusParams{}, InvalidParams("mute is true, false, or a number of seconds")
		}
	}
	if out.RoomID != "" && out.Mute == nil {
		// room_id says where a mute applies (§4.5.3).
		return StatusParams{}, InvalidParams("room_id goes with mute")
	}
	return out, nil
}

func (s *Server) statusHandler(st Statuser) HandlerFunc {
	return func(ctx context.Context, c *Conn, raw jsontext.Value) (any, error) {
		p, err := parseStatus(raw)
		if err != nil {
			return nil, err
		}
		return nil, st.Status(ctx, c, p)
	}
}
