package archive_test

import (
	"bytes"
	"io"
	"strconv"
	"testing"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/archive"
)

const archiveRecords = 1_000_000

// archived writes an archive of n message snapshots, 100 a part.
func archived(b *testing.B, n int) []byte {
	var buf bytes.Buffer
	w, err := archive.NewWriter(&buf, archive.Header{Server: "wss://chat.example/ws"})
	if err != nil {
		b.Fatal(err)
	}
	page := make([]apron.Message, 100)
	for i := 0; i < n; i += len(page) {
		for j := range page {
			id := apron.LogID(1_700_000_000_000 + i + j)
			user := "guest_" + strconv.Itoa(j)
			page[j] = apron.Message{
				MessageID: id, LogID: id, RoomID: "general",
				From: apron.User{UserID: user, Name: user},
				Body: apron.Body{Text: "Hello, everyone: this is line " + strconv.Itoa(i+j) + " of chat, of a usual length."},
			}
		}
		if err := w.WriteHistory(apron.HistoryResult{Messages: page}); err != nil {
			b.Fatal(err)
		}
	}
	return buf.Bytes()
}

// BenchmarkArchiveWrite writes an archive of a million messages.
func BenchmarkArchiveWrite(b *testing.B) {
	var size int
	for b.Loop() {
		size = len(archived(b, archiveRecords))
	}
	b.SetBytes(int64(size))
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*archiveRecords), "ns/record")
}

// BenchmarkArchiveNext reads the parts of an archive of a million
// messages.
func BenchmarkArchiveNext(b *testing.B) {
	raw := archived(b, archiveRecords)
	b.SetBytes(int64(len(raw)))
	for b.Loop() {
		r, err := archive.NewReader(bytes.NewReader(raw))
		if err != nil {
			b.Fatal(err)
		}
		n := 0
		for {
			p, err := r.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
			n += len(p.Messages)
		}
		if n != archiveRecords {
			b.Fatalf("read %d records, want %d", n, archiveRecords)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*archiveRecords), "ns/record")
}

// BenchmarkArchiveRead reads and replays an archive of a million messages.
func BenchmarkArchiveRead(b *testing.B) {
	raw := archived(b, archiveRecords)
	b.SetBytes(int64(len(raw)))
	for b.Loop() {
		if _, err := archive.Read(bytes.NewReader(raw)); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*archiveRecords), "ns/record")
}
