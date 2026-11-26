package apron

import "errors"

// HistoryParams are the params of history (§4.3): an empty RoomID is every
// room the connection receives; After and Before are inclusive log_id
// bounds, nil when left out, which also sets the direction: from the oldest
// with After, and from the newest without; Limit is positive, 0 for the
// backend's default. They encode as a client sends them, a bound of 0 as
// "0", a valid bound below every log_id, rather than null.
type HistoryParams struct {
	RoomID string `json:"room_id,omitempty"`
	After  *LogID `json:"after,omitempty"`
	Before *LogID `json:"before,omitempty"`
	Limit  int    `json:"limit,omitzero"`
}

// MarshalJSON writes the params, each bound as a decimal string.
func (p HistoryParams) MarshalJSON() ([]byte, error) {
	bound := func(id *LogID) (*string, error) {
		if id == nil {
			return nil, nil
		}
		if *id < 0 || *id > MaxLogID {
			return nil, errors.New("apron: a history bound is a log_id: " + id.String())
		}
		s := id.String()
		return &s, nil
	}
	after, err := bound(p.After)
	if err != nil {
		return nil, err
	}
	before, err := bound(p.Before)
	if err != nil {
		return nil, err
	}
	return marshal(struct {
		RoomID string  `json:"room_id,omitempty"`
		After  *string `json:"after,omitempty"`
		Before *string `json:"before,omitempty"`
		Limit  int     `json:"limit,omitzero"`
	}{p.RoomID, after, before, p.Limit})
}

// HistoryResult is the result of history (§4.3). Its rooms are logged
// records, without the delivery fields of a RoomView (§4.3.3).
type HistoryResult struct {
	Rooms       []Room       `json:"rooms,omitempty"`
	Messages    []Message    `json:"messages,omitempty"`
	Reactions   []Reactions  `json:"reactions,omitempty"`
	Memberships []Membership `json:"memberships,omitempty"`
	FirstLogID  LogID        `json:"first_log_id,omitzero"`
	LastLogID   LogID        `json:"last_log_id,omitzero"`
	More        bool         `json:"more"`
	LatestLogID LogID        `json:"latest_log_id,omitzero"`
	// HistoryLogID is the inclusive lower bound of what is retrievable, or
	// 0 for nothing, which is sent as null: every result has it (§4.3.3).
	HistoryLogID LogID `json:"history_log_id"`
}

// Record is one logged record, for server.Page: exactly one of Room, Message,
// Membership, and Reactions is set, and LogID is its log_id.
type Record struct {
	LogID      LogID
	Room       *Room
	Message    *Message
	Membership *Membership
	Reactions  *Reactions
}

// Next returns the params of the page after r, a result of p, in p's
// direction (§4.3.2): forward from after = r.LastLogID + 1 when p has After,
// and back from before = r.FirstLogID - 1 when it has not, with p's other
// bounds and limit. It reports false when there is no such page: r's more
// is false, or r is empty, or the continuation passes p's other bound.
//
// The continuation is computed from the slice's bounds, as numbers, never
// from the records, which a server may have compacted (§4.3.4).
func (p HistoryParams) Next(r HistoryResult) (HistoryParams, bool) {
	if !r.More || r.FirstLogID == 0 || r.LastLogID == 0 {
		return p, false
	}
	next := p
	if p.After != nil {
		after := r.LastLogID + 1
		if after > MaxLogID || p.Before != nil && after > *p.Before {
			return p, false
		}
		next.After = &after
		return next, true
	}
	before := r.FirstLogID - 1
	if before < 1 {
		return p, false
	}
	next.Before = &before
	return next, true
}

// LowerBound returns the effective lower bound of the log that r describes
// (§4.3.3): its history_log_id, or, when that is null, latest_log_id + 1,
// past every record. A client that needs a record below it cannot get it:
// it clears what it keeps of the room, and starts again from the bound
// (§4.3.5).
func (r HistoryResult) LowerBound() LogID {
	if r.HistoryLogID != 0 {
		return r.HistoryLogID
	}
	return r.LatestLogID + 1
}

// LowerBound returns the effective lower bound of the room's history, as
// HistoryResult.LowerBound does, from its delivery fields (§4.3.3), and
// reports false when the room record has no history_log_id, as without
// capability history.
func (v RoomView) LowerBound() (LogID, bool) {
	if v.HistoryLogID == nil {
		return 0, false
	}
	if *v.HistoryLogID != 0 {
		return *v.HistoryLogID, true
	}
	return v.LatestLogID + 1, true
}
