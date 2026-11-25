package proposals

import (
	"strings"
	"testing"
	"time"
)

func TestApprove(t *testing.T) {
	now := time.Now()
	table := NewTable()
	own := &Proposal{Code: "111111", Expires: now.Add(Lifetime)}
	table.Offer(own, false)
	linked := &Proposal{Code: "222222", State: "s", Expires: now.Add(Lifetime)}
	token := table.Offer(linked, true)
	if len(token) <= HandleBytes || len(table.Links()) != 1 || len(table.States()) != 1 {
		t.Fatalf("token %q, %d links, %d states", token, len(table.Links()), len(table.States()))
	}
	if p := table.Approve(own, "111111", "", now); p != own {
		t.Fatal("the connection's own code does not approve its proposal")
	}
	if p := table.Approve(nil, "222222", "", now); p != nil {
		t.Fatal("another connection's code approves without its state")
	}
	if p := table.Approve(nil, "222222", "s", now); p != linked {
		t.Fatal("a code with its state does not approve")
	}
	if p := table.Approve(nil, token, "", now); p != linked {
		t.Fatal("a link token does not approve")
	}
	if !table.Names(nil, token, "") || table.Names(nil, "unrelated-token", "") {
		t.Fatal("Names")
	}
	// Wrong secrets behind the handle count against the linked proposal.
	wrong := token[:HandleBytes] + strings.Repeat("A", len(token)-HandleBytes)
	for range MaxAttempts {
		table.Approve(own, wrong, "", now)
	}
	if linked.Live(now) || len(table.Links()) != 0 || len(table.States()) != 0 {
		t.Fatal("failed attempts did not drop the linked proposal")
	}
	if !own.Live(now) || own.Live(now.Add(Lifetime)) {
		t.Fatal("Live")
	}
}
