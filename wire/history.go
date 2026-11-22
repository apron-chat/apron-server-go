package wire

import (
	"context"
	"encoding/json/jsontext"
	"sort"
	"strconv"
)

// HistoryParams are the params of history (§4.3): an empty RoomID is every
// room the connection receives; After and Before are inclusive log_id
// bounds, nil when left out, which also sets the direction: from the oldest
// with After, and from the newest without; Limit is positive, 0 for the
// backend's default.
type HistoryParams struct {
	RoomID string
	After  *int64
	Before *int64
	Limit  int
}

// HistoryResult is the result of history (§4.3).
type HistoryResult struct {
	Rooms       []Room           `json:"rooms,omitempty"`
	Messages    []Message        `json:"messages,omitempty"`
	Reactions   []jsontext.Value `json:"reactions,omitempty"`
	Memberships []Membership     `json:"memberships,omitempty"`
	FirstLogID  string           `json:"first_log_id,omitempty"`
	LastLogID   string           `json:"last_log_id,omitempty"`
	More        bool             `json:"more"`
	LatestLogID string           `json:"latest_log_id,omitempty"`
	// HistoryLogID is the inclusive lower bound of what is retrievable, or
	// nil for nothing.
	HistoryLogID *string `json:"history_log_id"`
}

// Historian handles history, and adds capability history (§4.3).
type Historian interface {
	History(ctx context.Context, c *Conn, p HistoryParams) (HistoryResult, error)
}

// Record is one logged record, for Page: exactly one of its fields is set,
// and LogID is its log_id as a number.
type Record struct {
	LogID      int64
	Room       *Room
	Message    *Message
	Membership *Membership
	Reactions  jsontext.Value
}

// Page selects a window of a log as a history result (§4.3.2): the records
// in log, in ascending log_id order, between the bounds, at most limit of
// them, from the oldest with After and the newest without. lower is the
// log's effective lower bound (history_log_id) and latest its latest_log_id,
// both 0 for an empty log; records below lower are not returned. A limit
// below 1 is 1.
//
// A backend whose log is a slice of records, such as a host's recent
// history, answers history with Page and nothing more.
func Page(log []Record, p HistoryParams, limit int, lower, latest int64) HistoryResult {
	result := HistoryResult{}
	if latest > 0 {
		result.LatestLogID = strconv.FormatInt(latest, 10)
	}
	if lower > 0 {
		h := strconv.FormatInt(lower, 10)
		result.HistoryLogID = &h
	}
	limit = max(limit, 1)
	if p.Limit > 0 && p.Limit < limit {
		limit = p.Limit
	}
	after := lower
	if p.After != nil {
		after = max(*p.After, lower)
	}
	start := sort.Search(len(log), func(i int) bool { return log[i].LogID >= after })
	end := len(log)
	if p.Before != nil {
		before := *p.Before
		end = sort.Search(len(log), func(i int) bool { return log[i].LogID > before })
	}
	if start >= end {
		return result
	}
	window := log[start:end]
	if len(window) > limit {
		result.More = true
		if p.After != nil {
			window = window[:limit]
		} else {
			window = window[len(window)-limit:]
		}
	}
	result.FirstLogID = strconv.FormatInt(window[0].LogID, 10)
	result.LastLogID = strconv.FormatInt(window[len(window)-1].LogID, 10)
	for _, r := range window {
		switch {
		case r.Room != nil:
			result.Rooms = append(result.Rooms, *r.Room)
		case r.Message != nil:
			result.Messages = append(result.Messages, *r.Message)
		case r.Membership != nil:
			result.Memberships = append(result.Memberships, *r.Membership)
		case r.Reactions != nil:
			result.Reactions = append(result.Reactions, r.Reactions)
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
		to   **int64
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
