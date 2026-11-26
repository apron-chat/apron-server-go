package aprond

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron/aprontest"
)

// A guest's requests kept for deduplication (§1.2) go when it is retired,
// as its user_id never signs in again; while it is connected, they are
// bounded in bytes. Without both, a guest that kept large me results, its
// profile with a large ext, under many ids, then left, and another after
// it, and so on, held the server's memory with no connection open.
func TestDepartedGuestsKeepNoDedup(t *testing.T) {
	app, httpServer := newTestServer(t, DefaultConfig())
	pad := strings.Repeat("x", 15<<10)
	for g := range 3 {
		c := dialTestClient(t, httpServer)
		c.result(t, "me", "pad", map[string]any{"ext": map[string]any{"pad": pad}})
		for i := range 100 {
			c.result(t, "me", fmt.Sprint("same-", i), map[string]any{})
		}
		users, requests, bytes := aprontest.DedupUsage(app.srv)
		if users != 1 || requests < 10 || bytes > 1<<20 {
			t.Fatalf("guest %d: %d users, %d requests, %d bytes kept; want its own, within 1 MiB", g, users, requests, bytes)
		}
		_ = c.ws.CloseNow()
		waitDisconnected(t, app, c.userID)
		if users, requests, bytes := aprontest.DedupUsage(app.srv); users != 0 || requests != 0 || bytes != 0 {
			t.Fatalf("after guest %d left: %d users, %d requests, %d bytes kept; want none", g, users, requests, bytes)
		}
	}
}

// A service token that may not manage its account never gets the outcome
// of a request another connection of its account made, which its handler
// refuses it: its retry runs again, and is refused.
func TestRestrictedServiceTokenGetsNoDuplicate(t *testing.T) {
	_, httpServer, _, bot := scopedBotServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conns := map[bool]*testClient{}
	for _, manage := range []bool{true, false} {
		issued, err := bot.IssueToken(ctx, ServiceTokenOptions{Lifetime: time.Hour, ManageAccount: manage})
		if err != nil {
			t.Fatal(err)
		}
		c, _ := dialRaw(t, httpServer)
		c.result(t, "auth", "auth", map[string]any{"scheme": "token", "token": issued.Token})
		c.drain(t)
		conns[manage] = c
	}
	params := map[string]any{"room_id": defaultRoomID, "body": map[string]any{"text": "/invite-device laptop"}}
	call := func(c *testClient) map[string]any {
		t.Helper()
		c.write(t, map[string]any{"method": "command", "id": "invite", "params": params})
		for {
			if frame := c.read(t); frame["id"] == "invite" {
				return frame
			}
		}
	}
	if reply := call(conns[true]); reply["result"] == nil {
		t.Fatalf("/invite-device on a managing token: %#v", reply)
	}
	if reply := call(conns[false]); reply["error"] == nil || reply["error"].(map[string]any)["code"] != float64(CodeDenied) {
		t.Fatalf("its retry on a restricted token: %#v, want denied", reply)
	}
	// The managing token's own retry is still a duplicate.
	if reply := call(conns[true]); reply["result"] == nil {
		t.Fatalf("a retry on the managing token: %#v", reply)
	}
}
