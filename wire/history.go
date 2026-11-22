package wire

import (
	"context"
	"sort"
)

// HistoryParams are the params of history (§4.3): an empty RoomID is every
// room the connection receives; After and Before are inclusive log_id
// bounds, nil when left out, which also sets the direction: from the oldest
// with After, and from the newest without; Limit is positive, 0 for the
// backend's default.
type HistoryParams struct {
	RoomID string
	After  *LogID
	Before *LogID
	Limit  int
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

// Historian handles history, and adds capability history (§4.3).
type Historian interface {
	History(ctx context.Context, c *Conn, p HistoryParams) (HistoryResult, error)
}

// Record is one logged record, for Page: exactly one of Room, Message,
// Membership, and Reactions is set, and LogID is its log_id.
type Record struct {
	LogID      LogID
	Room       *Room
	Message    *Message
	Membership *Membership
	Reactions  *Reactions
}

// Log is a log for Page: its Records, in ascending log_id order, and its
// bounds (§4.3.3). Lower is its effective lower bound, history_log_id, and
// Latest its latest_log_id, both 0 for an empty log; records below Lower
// are not retrievable.
type Log struct {
	Records       []Record
	Lower, Latest LogID
}

// Page selects a window of a log as a history result (§4.3.2): the records
// between the bounds, at most limit of them, from the oldest with After and
// the newest without. A limit below 1 is 1.
//
// A backend whose log is a slice of records, such as a host's recent
// history, answers history with Page and nothing more.
func Page(log Log, p HistoryParams, limit int) HistoryResult {
	result := HistoryResult{LatestLogID: log.Latest, HistoryLogID: log.Lower}
	limit = max(limit, 1)
	if p.Limit > 0 && p.Limit < limit {
		limit = p.Limit
	}
	records := log.Records
	after := log.Lower
	if p.After != nil {
		after = max(*p.After, log.Lower)
	}
	start := sort.Search(len(records), func(i int) bool { return records[i].LogID >= after })
	end := len(records)
	if p.Before != nil {
		before := *p.Before
		end = sort.Search(len(records), func(i int) bool { return records[i].LogID > before })
	}
	if start >= end {
		return result
	}
	window := records[start:end]
	if len(window) > limit {
		result.More = true
		if p.After != nil {
			window = window[:limit]
		} else {
			window = window[len(window)-limit:]
		}
	}
	result.FirstLogID = window[0].LogID
	result.LastLogID = window[len(window)-1].LogID
	for _, r := range window {
		switch {
		case r.Room != nil:
			result.Rooms = append(result.Rooms, *r.Room)
		case r.Message != nil:
			result.Messages = append(result.Messages, *r.Message)
		case r.Membership != nil:
			result.Memberships = append(result.Memberships, *r.Membership)
		case r.Reactions != nil:
			result.Reactions = append(result.Reactions, *r.Reactions)
		}
	}
	return result
}

// parseHistory reads history's params.
func parseHistory(r *Request) (p HistoryParams, err error) {
	if p.RoomID, _, err = r.IDParam("room_id"); err != nil {
		return p, err
	}
	for _, bound := range []struct {
		name string
		to   **LogID
	}{{"after", &p.After}, {"before", &p.Before}} {
		value, present, err := r.LogID(bound.name)
		if err != nil {
			return p, err
		}
		if present {
			*bound.to = &value
		}
	}
	if p.Limit, _, err = r.Count("limit"); err != nil {
		return p, err
	}
	return p, nil
}

func historyHandler(h Historian) func(context.Context, *Conn, *Request) (any, error) {
	return func(ctx context.Context, c *Conn, r *Request) (any, error) {
		p, err := parseHistory(r)
		if err != nil {
			return nil, err
		}
		return h.History(ctx, c, p)
	}
}
