package accounts

import (
	"encoding/json/jsontext"
	"fmt"
	"slices"
	"testing"
)

func TestRequestable(t *testing.T) {
	for id, want := range map[string]bool{
		"ada": true, "Ada.L-1_": true, "a": true,
		"": false, "1ada": false, "ada-": false, "guest_7": false, "GUEST_x": false, "user_2": false, "a b": false,
	} {
		if Requestable(id) != want {
			t.Errorf("Requestable(%q) = %v", id, !want)
		}
	}
}

func TestIDs(t *testing.T) {
	ids := NewIDs()
	ids.Grant("Root")
	named := func(id string) bool { return id == "general" }
	if got := ids.Guest("ada", named); got != "ada" {
		t.Fatalf("Guest(ada) = %q", got)
	}
	// Taken, ignoring case; a room's name; granted; not requestable.
	for i, requested := range []string{"ADA", "general", "root", "guest_9", ""} {
		if got, want := ids.Guest(requested, named), fmt.Sprintf("guest_%d", i+1); got != want {
			t.Fatalf("Guest(%q) = %q, want %q", requested, got, want)
		}
	}
	if got := ids.Account("", named); got != "user_1" {
		t.Fatalf("Account() = %q", got)
	}
	if ids.Claim("root", named) || !ids.ClaimGranted("root", named) || ids.ClaimGranted("root", named) {
		t.Fatal("a granted user_id is a bot's to claim, once")
	}
	if guests, accounts := ids.Counters(); guests != 5 || accounts != 1 {
		t.Fatalf("counters %d %d", guests, accounts)
	}
	entries := ids.Entries()
	var claimed []string
	for _, e := range entries {
		if e.Kind != UsedKind || string(e.Value) != "true" {
			t.Fatalf("entry %v", e)
		}
		claimed = append(claimed, e.ID)
	}
	if !slices.Equal(claimed, []string{"ada", "guest_1", "guest_2", "guest_3", "guest_4", "guest_5", "user_1", "root"}) || ids.Entries() != nil {
		t.Fatalf("claimed %v", claimed)
	}
	stored := make(map[string]jsontext.Value)
	for _, e := range entries {
		stored[e.ID] = e.Value
	}
	restored := NewIDs()
	restored.Restore(stored)
	restored.RestoreCounters(ids.Counters())
	if restored.Claim("Ada", named) || restored.Guest("", named) != "guest_6" || len(restored.Snapshot()) != len(entries)+1 {
		t.Fatal("restored IDs reissue a user_id")
	}
}
