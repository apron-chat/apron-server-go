package aprond

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/aprond/internal/roomlog"
	"github.com/apron-chat/apron-server-go/store"
)

// retainNow runs a retention sweep as of now.
func retainNow(s *Server, now time.Time) {
	s.mu.Lock()
	s.retainLocked(now)
	s.unlock()
}

// reply sends a request and returns its reply, an error too.
func (c *benchClient) reply(tb testing.TB, method string, params any) []byte {
	tb.Helper()
	id := c.send(method, params)
	for {
		select {
		case frame := <-c.t.replies:
			if stringField(frame, "id") == id {
				return frame
			}
		case <-time.After(10 * time.Second):
			tb.Fatalf("no reply to %s", method)
		}
	}
}

func historyLogID(tb testing.TB, s *Server, roomID string) int64 {
	tb.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rooms.Get(roomID).Log().HistoryID()
}

// With HistoryRecords, a sweep keeps each room's newest records: its
// history_log_id moves past the rest, which history, room_list, the
// store, and a restart no longer have; a message being uploaded stays
// until it is written, and a discarded one's upload goes with it; a reply
// to a discarded message is refused; and the log without room_id starts
// past what any room discarded.
func TestHistoryRetention(t *testing.T) {
	st := store.NewMemory()
	bs := newBenchServer(t, Config{Store: st, HistoryRecords: 5, PublicURL: "http://aprond.test", UploadDir: t.TempDir()})
	s := bs.s
	var iv invariants
	g := bs.guest()
	first := stringField(g.call(t, "message", map[string]any{"body": map[string]any{"text": "the first"}}), "message_id")
	upload := g.call(t, "message", map[string]any{"body": map[string]any{"text": "a file", "embeds": []any{map[string]any{"kind": "upload"}}}})
	uploadID, writeURL, embedID := stringField(upload, "message_id"), stringField(upload, "write_url"), stringField(upload, "embed_id")
	for range 10 {
		g.call(t, "message", map[string]any{"body": map[string]any{"text": "more"}})
	}
	created := historyLogID(t, s, defaultRoomID)
	retainNow(s, time.Now())
	iv.check(t, s)
	// The upload is not written yet: it and what follows stay.
	s.mu.RLock()
	pending := s.rooms.Message(uploadID)
	s.mu.RUnlock()
	if pending == nil || historyLogID(t, s, defaultRoomID) > pending.Records()[0].ID || historyLogID(t, s, defaultRoomID) == created {
		t.Fatalf("history_log_id %d, upload message %v", historyLogID(t, s, defaultRoomID), pending)
	}
	// Written, it goes, its file with it.
	path, _ := url.Parse(writeURL)
	put := httptest.NewRecorder()
	s.Handler().ServeHTTP(put, httptest.NewRequest(http.MethodPut, path.Path, bytes.NewReader([]byte("data"))))
	if put.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", put.Code, put.Body)
	}
	// Its finished snapshot falls out of the newest five too.
	for range 6 {
		g.call(t, "message", map[string]any{"body": map[string]any{"text": "after"}})
	}
	retainNow(s, time.Now())
	iv.check(t, s)
	s.mu.RLock()
	uploadGone, embedGone, firstGone := s.rooms.Message(uploadID) == nil, s.embeds.Get(embedID) == nil, s.rooms.Message(first) == nil
	records := s.rooms.Get(defaultRoomID).Log().Records()
	s.mu.RUnlock()
	if !uploadGone || !embedGone || !firstGone || records > 6 {
		t.Fatalf("upload message gone %v, its embed gone %v, first gone %v, %d records", uploadGone, embedGone, firstGone, records)
	}
	floor := historyLogID(t, s, defaultRoomID)
	// history and room_list say where history starts; a page has no more.
	page := g.call(t, "history", map[string]any{"room_id": defaultRoomID, "limit": 100})
	if got := stringField(page, "history_log_id"); got != formatID(floor) || bytes.Contains(page, []byte("the first")) {
		t.Fatalf("history_log_id %s, want %d: %s", got, floor, page)
	}
	if !bytes.Contains(page, []byte(`"joined":true`)) {
		t.Errorf("no record of the members after the discarded memberships: %s", page)
	}
	all := g.call(t, "history", map[string]any{"limit": 100})
	if got := stringField(all, "history_log_id"); got != formatID(floor) {
		t.Errorf("history without room_id starts at %s, want %d", got, floor)
	}
	listed := g.call(t, "room_list", map[string]any{"room_id": defaultRoomID})
	if got := stringField(listed, "history_log_id"); got != formatID(floor) {
		t.Errorf("room_list history_log_id %s, want %d", got, floor)
	}
	if reply := g.reply(t, "message", map[string]any{"body": map[string]any{"text": "re"}, "reply_to": map[string]any{"message_id": first}}); !bytes.Contains(reply, []byte(`"error"`)) {
		t.Errorf("a reply to a discarded message: %s", reply)
	}
	// The store has only what is retained.
	if err := s.Sync(bs.ctx); err != nil {
		t.Fatal(err)
	}
	var entries []store.Entry
	_ = st.Load(func(e store.Entry) error {
		entries = append(entries, e)
		if e.Kind == entryMessage && e.ID == first {
			t.Error("the store still has the discarded message")
		}
		return nil
	})
	snapshot := store.NewMemory()
	_ = snapshot.Apply(entries)
	restored, err := Open(Config{Store: snapshot})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Shutdown(context.Background())
	var restoredIV invariants
	restoredIV.check(t, restored)
	if got := historyLogID(t, restored, defaultRoomID); got != floor {
		t.Errorf("restored history_log_id %d, want %d", got, floor)
	}
	restored.mu.RLock()
	restoredFloor, restoredFirst := restored.logFloor, restored.rooms.Message(first)
	restored.mu.RUnlock()
	if restoredFloor != floor || restoredFirst != nil {
		t.Errorf("restored log floor %d (want %d), first message %v", restoredFloor, floor, restoredFirst)
	}
}

// With HistoryMaxAge, a sweep discards records older than it, but a
// room's latest record; a server that retains nothing discards nothing.
func TestHistoryMaxAge(t *testing.T) {
	bs := newBenchServer(t, Config{HistoryMaxAge: time.Hour})
	var iv invariants
	g := bs.guest()
	for range 3 {
		g.call(t, "message", map[string]any{"body": map[string]any{"text": "old"}})
	}
	retainNow(bs.s, time.Now())
	iv.check(t, bs.s)
	created := historyLogID(t, bs.s, defaultRoomID)
	retainNow(bs.s, time.Now().Add(2*time.Hour))
	iv.check(t, bs.s)
	bs.s.mu.RLock()
	log := bs.s.rooms.Get(defaultRoomID).Log()
	latest, records := log.Since(0, 1)[0], log.Records()
	bs.s.mu.RUnlock()
	// What is left is the latest message, and the record of the members
	// after it, for the discarded memberships.
	if records != 2 || latest.Kind != roomlog.KindMembership || historyLogID(t, bs.s, defaultRoomID) <= created {
		t.Fatalf("%d records left, the latest %s", records, latest.Raw)
	}

	off := newBenchServer(t, Config{})
	g = off.guest()
	g.call(t, "message", map[string]any{"body": map[string]any{"text": "kept"}})
	before := historyLogID(t, off.s, defaultRoomID)
	retainNow(off.s, time.Now().Add(1000*time.Hour))
	if historyLogID(t, off.s, defaultRoomID) != before {
		t.Fatal("a server without retention discarded history")
	}
}

// A pass discards at most passBudget records, leaving the rest due for
// the next, which comes after backlogPause, not retentionInterval: a
// server that starts with retention on a long history catches up in
// passes that each hold the lock briefly, with one record of the members
// for a part with memberships while it holds.
func TestRetentionPasses(t *testing.T) {
	budget, interval, pause := passBudget, retentionInterval, backlogPause
	passBudget, retentionInterval, backlogPause = 10, time.Hour, time.Millisecond
	t.Cleanup(func() { passBudget, retentionInterval, backlogPause = budget, interval, pause })
	st := store.NewMemory()
	before := newBenchServer(t, Config{Store: st})
	for i := range 20 {
		before.member(i)
	}
	g := before.guest()
	for range 100 {
		g.call(t, "message", map[string]any{"body": map[string]any{"text": "kept until retention"}})
	}
	if err := before.s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The restart retires the guest; then retention starts.
	s, err := Open(Config{Store: st, HistoryRecords: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown(context.Background())
	first := s.RetentionStatus()
	if !first.On || first.Passes != 1 || first.Records != 10 || first.Backlog == 0 {
		t.Fatalf("after the first pass: %+v", first)
	}
	deadline := time.Now().Add(10 * time.Second)
	for s.RetentionStatus().Backlog > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("still due: %+v", s.RetentionStatus())
		}
		time.Sleep(time.Millisecond)
	}
	status := s.RetentionStatus()
	var iv invariants
	iv.check(t, s)
	s.mu.RLock()
	log := s.rooms.Get(defaultRoomID).Log()
	members := 0
	for _, record := range log.Since(0, 1<<30) {
		if record.Kind == roomlog.KindMembership && bytes.Count(record.Raw, []byte(`"joined":true`)) == 20 {
			members++
		}
	}
	records := log.Records()
	s.mu.RUnlock()
	if status.Passes < 10 || status.Floor == 0 || records > 7 || members != 1 {
		t.Fatalf("after catching up: %+v, %d records, %d records of the members", status, records, members)
	}
}
