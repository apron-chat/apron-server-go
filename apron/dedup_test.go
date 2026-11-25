package apron

import (
	"fmt"
	"strings"
	"testing"

	"github.com/apron-chat/apron-server-go/wire/wiretest"
)

// A guest's requests kept for deduplication (§1.2) go when it is retired,
// as its user_id never signs in again; while it is connected, they are
// bounded in bytes. Without both, a guest that kept large me results, its
// profile with a large ext, under many ids, then left, and another after
// it, and so on, held the server's memory with no connection open.
func TestDepartedGuestsKeepNoDedup(t *testing.T) {
	app, httpServer := newTestServer(t, DefaultConfig())
	pad := strings.Repeat("x", 15<<10)
	var userIDs []string
	for g := range 3 {
		c := dialTestClient(t, httpServer)
		c.result(t, "me", "pad", map[string]any{"ext": map[string]any{"pad": pad}})
		for i := range 100 {
			c.result(t, "me", fmt.Sprint("same-", i), map[string]any{})
		}
		users, requests, bytes := wiretest.DedupUsage(app.wire)
		if users != 1 || requests < 10 || bytes > 1<<20 {
			t.Fatalf("guest %d: %d users, %d requests, %d bytes kept; want its own, within 1 MiB", g, users, requests, bytes)
		}
		_ = c.ws.CloseNow()
		waitDisconnected(t, app, c.userID)
		userIDs = append(userIDs, c.userID)
		if users, requests, bytes := wiretest.DedupUsage(app.wire); users != 0 || requests != 0 || bytes != 0 {
			t.Fatalf("after guest %d left: %d users, %d requests, %d bytes kept; want none", g, users, requests, bytes)
		}
	}
}
