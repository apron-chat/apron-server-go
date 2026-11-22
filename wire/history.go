package wire

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"sort"
	"strconv"
)

// HistoryParams are the params of history (§4.3): an empty RoomID is every
// room the connection receives; After and Before are inclusive log_id
// bounds, 0 for none; Limit is positive, 0 for the backend's default.
type HistoryParams struct {
	RoomID string
	After  int64
	Before int64
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
// both 0 for an empty log; records below lower are not returned.
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
	if p.Limit > 0 && p.Limit < limit {
		limit = p.Limit
	}
	after := max(p.After, lower)
	before := p.Before
	start := sort.Search(len(log), func(i int) bool { return log[i].LogID >= after })
	end := len(log)
	if before > 0 {
		end = sort.Search(len(log), func(i int) bool { return log[i].LogID > before })
	}
	if start >= end {
		return result
	}
	window := log[start:end]
	if len(window) > limit {
		result.More = true
		if p.After > 0 {
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

func parseHistory(raw jsontext.Value) (HistoryParams, error) {
	var p struct {
		RoomID string `json:"room_id,omitempty"`
		After  string `json:"after,omitempty"`
		Before string `json:"before,omitempty"`
		Limit  *int   `json:"limit,omitempty"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return HistoryParams{}, InvalidParams("Invalid history: " + err.Error())
	}
	if p.RoomID != "" && !ValidID(p.RoomID) {
		return HistoryParams{}, InvalidParams("Invalid room_id")
	}
	out := HistoryParams{RoomID: p.RoomID}
	for _, bound := range []struct {
		text string
		to   *int64
	}{{p.After, &out.After}, {p.Before, &out.Before}} {
		if bound.text == "" {
			continue
		}
		n, err := strconv.ParseInt(bound.text, 10, 64)
		if err != nil || n <= 0 {
			return HistoryParams{}, InvalidParams("after and before are log_ids")
		}
		*bound.to = n
	}
	if p.Limit != nil {
		if *p.Limit <= 0 {
			return HistoryParams{}, InvalidParams("limit is a positive count")
		}
		out.Limit = *p.Limit
	}
	return out, nil
}

func (s *Server) historyHandler(h Historian) HandlerFunc {
	return func(ctx context.Context, c *Conn, raw jsontext.Value) (any, error) {
		p, err := parseHistory(raw)
		if err != nil {
			return nil, err
		}
		return h.History(ctx, c, p)
	}
}
