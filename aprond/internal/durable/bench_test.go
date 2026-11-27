package durable

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/apron-chat/apron-server-go/store"
)

// BenchmarkWriter queues small batches, as a server does as it releases
// its lock after each change, as fast as they come, to a SQLite store, and
// waits for the last: what one change costs the writer when changes come
// faster than the store applies them.
func BenchmarkWriter(b *testing.B) {
	st, err := store.OpenSQLite(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	w := Start(st, nil, errFailed, errClosed)
	defer w.Stop()
	value := []byte(`{"kind":1,"raw":{"body":{"text":"hello"},"log_id":"1","message_id":"1","room_id":"general"},"rooms":["general"]}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		w.Queue([]store.Entry{
			{Kind: "record", ID: strconv.Itoa(i), Value: value},
			{Kind: "message", ID: strconv.Itoa(i), Value: value},
			{Kind: "room", ID: "general", Value: value},
			{Kind: "meta", ID: "counters", Value: value},
		})
	}
	f := w.Fence()
	w.Queue(nil)
	if err := f.Wait(context.Background()); err != nil {
		b.Fatal(err)
	}
}
