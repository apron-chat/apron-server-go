package store

import (
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
)

// BenchmarkSQLiteApply applies batches the size of a new message's (its record,
// its message, its room, and the counters), as the server's store writer
// does, one transaction each.
func BenchmarkSQLiteApply(b *testing.B) {
	value := []byte(`{"kind":1,"raw":{"body":{"text":"hello, everyone in the room"},"from":{"user_id":"guest_1"},"log_id":"1795817355191","message_id":"1795817355191","room_id":"general"},"rooms":["general"]}`)
	for _, size := range []int{4, 64} {
		b.Run(fmt.Sprintf("entries=%d", size), func(b *testing.B) {
			st, err := OpenSQLite(filepath.Join(b.TempDir(), "bench.db"))
			if err != nil {
				b.Fatal(err)
			}
			defer st.Close()
			batch := make([]Entry, size)
			b.ReportAllocs()
			for i := range b.N {
				for j := range batch {
					batch[j] = Entry{Kind: "record", ID: strconv.Itoa(i*size + j), Value: value}
				}
				if err := st.Apply(batch); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
