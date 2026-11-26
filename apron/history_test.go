package apron_test

import (
	"testing"

	"github.com/apron-chat/apron-server-go/apron"
)

func TestHistoryNext(t *testing.T) {
	id := func(n apron.LogID) *apron.LogID { return &n }
	page := apron.HistoryResult{FirstLogID: 100, LastLogID: 200, More: true}
	for _, c := range []struct {
		name   string
		p      apron.HistoryParams
		r      apron.HistoryResult
		want   apron.HistoryParams
		wantOK bool
	}{
		{"forward", apron.HistoryParams{RoomID: "r", After: id(1), Before: id(500), Limit: 10}, page,
			apron.HistoryParams{RoomID: "r", After: id(201), Before: id(500), Limit: 10}, true},
		{"backward", apron.HistoryParams{RoomID: "r", Limit: 10}, page,
			apron.HistoryParams{RoomID: "r", Before: id(99), Limit: 10}, true},
		{"backward to after", apron.HistoryParams{After: nil, Before: id(300)}, page,
			apron.HistoryParams{Before: id(99)}, true},
		{"no more", apron.HistoryParams{After: id(1)}, apron.HistoryResult{FirstLogID: 1, LastLogID: 2}, apron.HistoryParams{}, false},
		{"empty", apron.HistoryParams{After: id(1)}, apron.HistoryResult{More: true}, apron.HistoryParams{}, false},
		{"forward past before", apron.HistoryParams{After: id(1), Before: id(200)}, page, apron.HistoryParams{}, false},
		{"back past the start", apron.HistoryParams{}, apron.HistoryResult{FirstLogID: 1, LastLogID: 5, More: true}, apron.HistoryParams{}, false},
	} {
		got, ok := c.p.Next(c.r)
		if ok != c.wantOK || ok && !equalParams(got, c.want) {
			t.Errorf("%s: Next = %+v, %v; want %+v, %v", c.name, got, ok, c.want, c.wantOK)
		}
	}
}

func equalParams(a, b apron.HistoryParams) bool {
	same := func(x, y *apron.LogID) bool { return x == nil && y == nil || x != nil && y != nil && *x == *y }
	return a.RoomID == b.RoomID && a.Limit == b.Limit && same(a.After, b.After) && same(a.Before, b.Before)
}

func TestLowerBound(t *testing.T) {
	if got := (apron.HistoryResult{HistoryLogID: 5, LatestLogID: 9}).LowerBound(); got != 5 {
		t.Errorf("history_log_id: %v", got)
	}
	if got := (apron.HistoryResult{LatestLogID: 9}).LowerBound(); got != 10 {
		t.Errorf("null history_log_id: %v", got)
	}
	zero, five := apron.LogID(0), apron.LogID(5)
	for _, c := range []struct {
		view apron.RoomView
		want apron.LogID
		ok   bool
	}{
		{apron.RoomView{LatestLogID: 9, HistoryLogID: &five}, 5, true},
		{apron.RoomView{LatestLogID: 9, HistoryLogID: &zero}, 10, true},
		{apron.RoomView{LatestLogID: 9}, 0, false},
	} {
		if got, ok := c.view.LowerBound(); got != c.want || ok != c.ok {
			t.Errorf("%+v: LowerBound = %v, %v", c.view, got, ok)
		}
	}
}
