package aprond

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/aprond/internal/roomlog"
	"github.com/apron-chat/apron-server-go/store"
)

// invariants checks what history retention keeps true (SERVER.md,
// "History retention", invariants), against what it saw at the last
// check:
//
//   - H1: each room's history_log_id never falls, and is at most its
//     latest_log_id;
//   - H2: the log's floor, where history without room_id starts, never
//     falls, and is the latest history_log_id of a room that discarded;
//   - H3: no room's log holds a record before its history_log_id, and
//     room views say the same history_log_id;
//   - H4: the server's log is the rooms' logs together, in log_id order;
//   - H5: every message has a snapshot, every snapshot of it is in the
//     log, and every snapshot in the log is its message's;
//   - H6: every embed of a message belongs to a message there is.
type invariants struct {
	history map[string]int64
	floor   int64
}

func (iv *invariants) check(tb testing.TB, s *Server) {
	tb.Helper()
	if iv.history == nil {
		iv.history = make(map[string]int64)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	held := make(map[*roomlog.Record]bool)
	var latestRetained int64
	for _, r := range s.rooms.All() {
		log := r.Log()
		h := log.HistoryID()
		if h < iv.history[r.ID()] {
			tb.Errorf("H1: room %s's history_log_id fell from %d to %d", r.ID(), iv.history[r.ID()], h)
		}
		if h > log.Latest() {
			tb.Errorf("H1: room %s's history_log_id %d is past its latest_log_id %d", r.ID(), h, log.Latest())
		}
		iv.history[r.ID()] = h
		latestRetained = max(latestRetained, log.Retained())
		for _, record := range log.Since(0, math.MaxInt) {
			if record.ID < h {
				tb.Errorf("H3: room %s holds record %d before its history_log_id %d", r.ID(), record.ID, h)
			}
			held[record] = true
		}
		if view := s.roomViewOfLocked(r); int64(*view.HistoryLogID) != h {
			tb.Errorf("H3: room %s's view says history_log_id %d, its log %d", r.ID(), *view.HistoryLogID, h)
		}
	}
	if s.logFloor < iv.floor || s.logFloor != latestRetained {
		tb.Errorf("H2: the log's floor is %d, was %d, want %d", s.logFloor, iv.floor, latestRetained)
	}
	iv.floor = s.logFloor
	records := s.log.Records()
	if !slices.IsSortedFunc(records, func(a, b *roomlog.Record) int { return int(a.ID - b.ID) }) || len(records) != len(held) {
		tb.Errorf("H4: the log has %d records, its rooms %d", len(records), len(held))
	}
	inLog := make(map[*roomlog.Record]bool, len(records))
	for _, record := range records {
		inLog[record] = true
		if !held[record] {
			tb.Errorf("H4: the log holds record %d that no room does", record.ID)
		}
		if record.Kind == roomlog.KindMessage {
			if m := s.rooms.Message(record.Message); m == nil || !slices.Contains(m.Records(), record) {
				tb.Errorf("H5: snapshot %d is not its message %s's", record.ID, record.Message)
			}
		}
	}
	for id, m := range s.rooms.Messages() {
		if len(m.Records()) == 0 {
			tb.Errorf("H5: message %s has no snapshot", id)
		}
		for _, record := range m.Records() {
			if !inLog[record] {
				tb.Errorf("H5: message %s's snapshot %d is not in the log", id, record.ID)
			}
		}
	}
	for _, e := range s.embeds.All() {
		if e.MessageID != "" && !e.removed && s.rooms.Message(e.MessageID) == nil {
			tb.Errorf("H6: embed %s belongs to message %s, which is gone", e.ID, e.MessageID)
		}
	}
}

// sameState compares what two servers hold of the rooms' logs and the
// messages, as a restart must restore them.
func sameState(tb testing.TB, want, got *Server) {
	tb.Helper()
	summary := func(s *Server) string {
		s.mu.RLock()
		defer s.mu.RUnlock()
		var b bytes.Buffer
		for _, r := range s.rooms.All() {
			fmt.Fprintf(&b, "room %s from %d to %d:", r.ID(), r.Log().HistoryID(), r.Log().Latest())
			for _, record := range r.Log().Since(0, math.MaxInt) {
				fmt.Fprintf(&b, " %d", record.ID)
			}
			b.WriteByte('\n')
		}
		ids := make([]string, 0)
		for id := range s.rooms.Messages() {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		fmt.Fprintf(&b, "messages %v floor %d\n", ids, s.logFloor)
		return b.String()
	}
	if w, g := summary(want), summary(got); w != g {
		tb.Errorf("restored\n%s\nwant\n%s", g, w)
	}
}

// Interleaved posts, edits, moves, deletions, reactions, joins and
// leaves, and retention passes of a few records each, keep the
// invariants after every pass, and a restart restores what was kept.
func TestRetentionInvariants(t *testing.T) {
	budget := passBudget
	passBudget = 3
	t.Cleanup(func() { passBudget = budget })
	for seed := range uint64(8) {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, 0))
			st := store.NewMemory()
			bs := newBenchServer(t, Config{Store: st, HistoryRecords: 8})
			// Accounts, which a restart keeps, as it retires guests.
			_, adaToken := bs.member(0)
			_, bobToken := bs.member(1)
			ada, bob := bs.resume(adaToken, true), bs.resume(bobToken, true)
			other := stringField(ada.call(t, "room_set", map[string]any{"title": "other"}), "room_id")
			roomIDs := []string{defaultRoomID, other}
			var mine []string
			var iv invariants
			for step := range 400 {
				room := roomIDs[rng.IntN(2)]
				switch op := rng.IntN(10); {
				case op < 4 || len(mine) == 0:
					id := stringField(ada.call(t, "message", map[string]any{"room_id": room, "body": map[string]any{"text": fmt.Sprint("post ", step)}}), "message_id")
					mine = append(mine, id)
				case op == 4:
					ada.reply(t, "message", map[string]any{"room_id": room, "message_id": mine[rng.IntN(len(mine))], "body": map[string]any{"text": "edit or move"}})
				case op == 5:
					ada.reply(t, "message", map[string]any{"room_id": room, "message_id": mine[rng.IntN(len(mine))], "deleted": true})
				case op == 6:
					bob.reply(t, "reactions", map[string]any{"message_id": mine[rng.IntN(len(mine))], "emojis": []string{"👍"}})
				case op == 7:
					bob.reply(t, "room_join", map[string]any{"room_id": other})
				case op == 8:
					bob.reply(t, "room_leave", map[string]any{"room_id": other})
				default:
					retainNow(bs.s, time.Now())
					iv.check(t, bs.s)
				}
			}
			for retainOnce(bs.s) > 0 {
				iv.check(t, bs.s)
			}
			iv.check(t, bs.s)
			if err := bs.s.Sync(bs.ctx); err != nil {
				t.Fatal(err)
			}
			var entries []store.Entry
			_ = st.Load(func(e store.Entry) error { entries = append(entries, e); return nil })
			snapshot := store.NewMemory()
			_ = snapshot.Apply(entries)
			restored, err := Open(Config{Store: snapshot})
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Shutdown(context.Background())
			var restoredIV invariants
			restoredIV.check(t, restored)
			sameState(t, bs.s, restored)
		})
	}
}

// retainOnce runs one pass and returns the records it left due.
func retainOnce(s *Server) int {
	s.mu.Lock()
	defer s.unlock()
	return s.retainLocked(time.Now())
}
