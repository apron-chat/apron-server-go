package server

import (
	"encoding/json/jsontext"
	"strconv"
	"sync/atomic"
	"testing"
)

// BenchmarkDedupUsers claims and finishes requests of users users, each
// request with a new id, under the Server's bounds, small enough here
// that both evict: each user's oldest go as perUser bounds them, and the
// least recently active users' as users does. Run in parallel, every
// request takes the one lock.
func BenchmarkDedupUsers(b *testing.B) {
	for _, users := range []int{10, 10000} {
		b.Run("users="+strconv.Itoa(users), func(b *testing.B) {
			d := newDedup(64, 1024, 1<<20, 64<<20)
			outcome := dedupOutcome{result: jsontext.Value(`{"message_id":"1700000000000"}`)}
			params := []byte(`{"room_id":"general","body":{"text":"Hello, everyone: this is a line of chat of a usual length."}}`)
			userIDs := make([]string, users)
			for i := range userIDs {
				userIDs[i] = "guest_" + strconv.Itoa(i)
			}
			var next atomic.Int64
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					n := next.Add(1)
					userID := userIDs[n%int64(users)]
					key := dedupKey{id: "k3J9xQ2mZ8aB-" + strconv.FormatInt(n, 10)}
					entry, prior := d.claim(userID, key, "message", params)
					if prior != nil {
						continue
					}
					d.finish(userID, key, entry, outcome, false)
				}
			})
		})
	}
}
