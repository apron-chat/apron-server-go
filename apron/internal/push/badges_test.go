package push

import (
	"sync"
	"testing"
	"time"
)

// A badge push is scheduled only for a user with a registration that
// takes them, once until it is due; it is handed back to the server, and
// canceled with the user's last registration, or when the table stops.
func TestBadges(t *testing.T) {
	var mu sync.Mutex
	due := make(chan *PendingBadge, 4)
	var table *Table
	table = NewTable(func(userID string, b *PendingBadge) {
		mu.Lock()
		defer mu.Unlock()
		if userID == "ada" && table.TakeBadge(userID, b) {
			due <- b
		}
	})
	table.SetBadgeDelay(time.Hour)
	mu.Lock()
	table.ScheduleBadge("ada")
	if table.BadgePending("ada") {
		t.Fatal("a badge push for a user without registrations")
	}
	relay := New(Registration{User: "ada", Kind: "relay", URL: "https://relay.example", Wake: Badge})
	table.Add(relay)
	table.ScheduleBadge("ada")
	if !table.BadgePending("ada") {
		t.Fatal("no badge push pending")
	}
	table.Remove(relay)
	if table.BadgePending("ada") {
		t.Fatal("a badge push outlived the user's registrations")
	}
	table.Add(relay)
	table.SetBadgeDelay(0)
	table.ScheduleBadge("ada")
	mu.Unlock()
	b := <-due
	mu.Lock()
	if table.BadgePending("ada") || table.TakeBadge("ada", b) {
		t.Error("a badge push sent stays pending")
	}
	table.SetBadgeDelay(time.Hour)
	table.ScheduleBadge("ada")
	table.Stop()
	table.ScheduleBadge("ada")
	if table.BadgePending("ada") {
		t.Error("a badge push pending after Stop")
	}
	mu.Unlock()
	table.Wait()
}
