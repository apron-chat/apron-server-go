package apron_test

import (
	"runtime"
	"strconv"
	"testing"

	"github.com/apron-chat/apron-server-go/apron"
)

// benchMessages are n snapshots of a line of chat each, from 100 users,
// with increasing log_ids.
func benchMessages(n int) []apron.Message {
	ms := make([]apron.Message, n)
	for i := range ms {
		id := apron.LogID(1_700_000_000_000 + i)
		user := "guest_" + strconv.Itoa(i%100)
		ms[i] = apron.Message{
			MessageID: id, LogID: id, RoomID: "general",
			From: apron.User{UserID: user, Name: user},
			Body: &apron.Body{Text: "Hello, everyone: this is line " + strconv.Itoa(i) + " of chat, of a usual length."},
		}
	}
	return ms
}

// BenchmarkReplay applies a million message snapshots, a page of 100 at a
// time, as a client that loads a room's whole history does, and reports
// what each costs and what the Replay keeps of it.
func BenchmarkReplay(b *testing.B) {
	const records = 1_000_000
	ms := benchMessages(records)
	var kept uint64
	for b.Loop() {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		var r apron.Replay
		for i := 0; i < records; i += 100 {
			r.ApplyHistory(apron.HistoryResult{Messages: ms[i : i+100]})
		}
		runtime.GC()
		runtime.ReadMemStats(&after)
		kept = after.HeapAlloc - before.HeapAlloc
		if len(r.Messages("general")) != records {
			b.Fatal("not every message was kept")
		}
		runtime.KeepAlive(&r)
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*records), "ns/record")
	b.ReportMetric(float64(kept)/records, "kept-B/record")
}
