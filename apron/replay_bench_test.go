package apron_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"runtime"
	"strconv"
	"testing"
	"time"

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
			Body: apron.Body{Text: "Hello, everyone: this is line " + strconv.Itoa(i) + " of chat, of a usual length."},
		}
	}
	return ms
}

// BenchmarkReplayHistory applies a million message snapshots, a page of 100 at a
// time, as a client that loads a room's whole history does, and reports
// what each costs and what the Replay keeps of it.
func BenchmarkReplayHistory(b *testing.B) {
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

// BenchmarkReplayRaw applies a million message snapshots as they were
// encoded, decoded (decoded), as a client that decodes each page does, or
// kept raw (raw, Replay.ApplyRawMessage), and reports what each costs to
// apply and what the Replay keeps of it, the decoded strings included.
// read is what reading them all back (Messages) then costs, each.
func BenchmarkReplayRaw(b *testing.B) {
	const records = 1_000_000
	ms := benchMessages(records)
	raws := make([]jsontext.Value, records)
	for i := range ms {
		raws[i], _ = apron.Marshal(ms[i])
	}
	ms = nil
	for _, how := range []string{"decoded", "raw"} {
		b.Run(how, func(b *testing.B) {
			var kept uint64
			var reading time.Duration
			for b.Loop() {
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				var r apron.Replay
				for _, raw := range raws {
					if how == "raw" {
						r.ApplyRawMessage(raw)
						continue
					}
					var m apron.Message
					if err := json.Unmarshal(raw, &m); err != nil {
						b.Fatal(err)
					}
					r.ApplyMessage(m)
				}
				runtime.GC()
				runtime.ReadMemStats(&after)
				kept = after.HeapAlloc - before.HeapAlloc
				start := time.Now()
				if len(r.Messages("general")) != records {
					b.Fatal("not every message was kept")
				}
				reading += time.Since(start)
				runtime.KeepAlive(&r)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds()-reading.Nanoseconds())/float64(b.N*records), "ns/record")
			b.ReportMetric(float64(reading.Nanoseconds())/float64(b.N*records), "read-ns/record")
			b.ReportMetric(float64(kept)/records, "kept-B/record")
		})
	}
}
