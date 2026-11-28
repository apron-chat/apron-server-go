package aprond

import (
	"time"

	"github.com/apron-chat/apron-server-go/aprond/internal/roomlog"
)

// History retention (§4.3.3, Config.HistoryRecords and HistoryMaxAge;
// SERVER.md, "History retention"). The server keeps every record unless
// either is set. With them, a sweep every retentionInterval, and one as
// the server starts, discards the start of each room's log: the records
// past the newest HistoryRecords, and those older than HistoryMaxAge, by
// their log_id, the time they were logged. It never discards a room's
// latest record, nor a record of a message whose embeds are not finished,
// such as live text still being written, or anything after it in its
// rooms. The room's history_log_id moves past what is discarded, and
// never back; a part that holds memberships goes after a membership
// record of every current member, each joined. A record that no room
// holds any more is taken out of the server's log and deleted from the
// store, and a message none of whose snapshots is held any more goes
// with it, its uploads with it. The log of the joined rooms, which history
// without room_id pages, starts where the latest of the rooms' retained
// histories that discarded anything starts, so that every room's records
// after it are there.

// retentionInterval is how often the sweep discards history; a variable
// for tests.
var retentionInterval = time.Minute

// retaining reports whether the server retains history at all.
func (s *Server) retaining() bool {
	return s.config.HistoryRecords > 0 || s.config.HistoryMaxAge > 0
}

// sweepHistory discards history every retentionInterval until Shutdown.
func (s *Server) sweepHistory() {
	ticker := time.NewTicker(retentionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopped:
			return
		case now := <-ticker.C:
			s.mu.Lock()
			if !s.closed {
				s.retainLocked(now)
			}
			s.unlock()
		}
	}
}

// retainLocked discards the history that retention lets go of as of now
// (§4.3.3).
func (s *Server) retainLocked(now time.Time) {
	if !s.retaining() {
		return
	}
	writing := s.writingFloorsLocked()
	var discarded []*roomlog.Record
	for _, r := range s.rooms.All() {
		log := r.Log()
		var before int64
		if n := s.config.HistoryRecords; n > 0 && log.Records() > n {
			before = log.At(log.Records() - n)
		}
		if age := s.config.HistoryMaxAge; age > 0 {
			before = max(before, now.Add(-age).UnixMilli())
		}
		if floor, ok := writing[r.ID()]; ok {
			before = min(before, floor)
		}
		if before > log.HistoryID() {
			discarded = append(discarded, s.rooms.Retain(r, before)...)
		}
	}
	if len(discarded) == 0 {
		return
	}
	for _, m := range s.rooms.Forget(discarded) {
		// Its uploads are released, as a deletion's are (§4.4).
		s.releaseEmbedsLocked(m.Current(), nil)
	}
	s.logFloor = s.logFloorLocked()
	// The unread counts kept counted discarded messages: they are counted
	// again from what is retained.
	for id := range s.pushes.UserIDs() {
		if u := s.users.Get(id); u != nil {
			u.unread = nil
		}
	}
	// The entries that lag behind their records, a large room's and a hot
	// message's (rooms.EntryDelay), go with this batch, as the records
	// they would be restored from go.
	s.flushAll = true
}

// writingFloorsLocked returns, by room_id, the log_id of the oldest record
// of a message whose embeds are not finished, such as an upload not yet
// written or live text still being written, in each room that holds one:
// retention discards nothing from it on.
func (s *Server) writingFloorsLocked() map[string]int64 {
	floors := make(map[string]int64)
	for _, e := range s.embeds.All() {
		if e.MessageID == "" || e.Finished || e.removed {
			continue
		}
		m := s.rooms.Message(e.MessageID)
		if m == nil {
			continue
		}
		for _, record := range m.Records() {
			for _, id := range record.Rooms {
				if floor, ok := floors[id]; !ok || record.ID < floor {
					floors[id] = record.ID
				}
			}
		}
	}
	return floors
}

// logFloorLocked is where the log of several rooms, which history without
// room_id pages, starts (§4.3.3): the latest start of the rooms' retained
// histories that discarded anything, so that after it every room's records
// are there; 0 when no room discarded any.
func (s *Server) logFloorLocked() int64 {
	var floor int64
	for _, r := range s.rooms.All() {
		floor = max(floor, r.Log().Retained())
	}
	return floor
}
