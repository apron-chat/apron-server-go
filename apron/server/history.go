package server

import (
	"context"
	"sort"

	"github.com/apron-chat/apron-server-go/apron"
)

// HistoryHandler handles history, and adds capability history (§4.3). A
// backend whose log is a slice of records answers with Page.
type HistoryHandler interface {
	History(ctx context.Context, c *Conn, p apron.HistoryParams, reply Reply[apron.HistoryResult]) error
}

// Log is a log for Page: its Records, in ascending log_id order, and its
// bounds (§4.3.3). Lower is its effective lower bound, history_log_id, and
// Latest its latest_log_id, both 0 for an empty log; records below Lower
// are not retrievable.
type Log struct {
	Records       []apron.Record
	Lower, Latest apron.LogID
}

// Page selects a window of a log as a history result (§4.3.2): the records
// between the bounds, at most limit of them, from the oldest with After and
// the newest without. A limit below 1 is 1.
//
// A backend whose log is a slice of records, such as a host's recent
// history, answers history with Page and nothing more.
func Page(log Log, p apron.HistoryParams, limit int) apron.HistoryResult {
	result := apron.HistoryResult{LatestLogID: log.Latest, HistoryLogID: log.Lower}
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
