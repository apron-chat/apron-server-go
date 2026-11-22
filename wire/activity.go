package wire

import (
	"context"
	"encoding/json/v2"
)

// parseMe reads me's params (§3.3.2).
func parseMe(r *Request) (p MeParams, err error) {
	for _, field := range []struct {
		name string
		to   **string
	}{{"name", &p.Name}, {"avatar", &p.Avatar}, {"status", &p.Status}} {
		value, present, err := r.String(field.name)
		if err != nil {
			return p, err
		}
		if present {
			*field.to = &value
		}
	}
	p.Ext, _, err = r.Ext("ext")
	return p, err
}

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

// Reactor handles reactions, and adds capability reactions (§4.7). It
// broadcasts the reaction set, including to c, before it returns.
type Reactor interface {
	React(ctx context.Context, c *Conn, p ReactParams) error
}

func parseReact(r *Request) (p ReactParams, err error) {
	var present bool
	if p.MessageID, present, err = r.MessageID("message_id"); err == nil && !present {
		err = Missing("message_id")
	}
	if err != nil {
		return p, err
	}
	raw, ok := r.Param("emojis")
	if !ok {
		return p, Missing("emojis")
	}
	if isNull(raw) || json.Unmarshal(raw, &p.Emojis) != nil {
		return p, InvalidParams("emojis must be an array of strings")
	}
	for _, emoji := range p.Emojis {
		if emoji == "" {
			return p, InvalidParams("emojis must be non-empty strings")
		}
	}
	if p.Emojis == nil {
		p.Emojis = []string{}
	}
	return p, nil
}

func reactionsHandler(reactor Reactor) func(context.Context, *Conn, *Request) (any, error) {
	return func(ctx context.Context, c *Conn, r *Request) (any, error) {
		p, err := parseReact(r)
		if err != nil {
			return nil, err
		}
		return nil, reactor.React(ctx, c, p)
	}
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

// Activity handles activity notifications, and adds capability activity
// (§4.6). They get no reply: params that are not valid are ignored.
type Activity interface {
	Activity(ctx context.Context, c *Conn, p ActivityParams)
}

func parseActivity(r *Request) (p ActivityParams, err error) {
	if p.RoomID, _, err = r.IDParam("room_id"); err != nil {
		return p, err
	}
	if raw, ok := r.Param("typing"); ok {
		var typing int
		if json.Unmarshal(raw, &typing) != nil || typing < 0 {
			return p, InvalidParams("typing must be a non-negative integer")
		}
		p.Typing = &typing
	}
	if read, present, err := r.MessageID("read_message_id"); err != nil {
		return p, err
	} else if present {
		p.ReadMessageID = &read
	}
	return p, nil
}

func activityHandler(a Activity) func(context.Context, *Conn, *Request) (any, error) {
	return func(ctx context.Context, c *Conn, r *Request) (any, error) {
		if p, err := parseActivity(r); err == nil {
			a.Activity(ctx, c, p)
		}
		return nil, nil
	}
}
