package sessions

import (
	"crypto/sha256"
	"encoding/json/jsontext"
	"testing"
	"time"
)

func TestTable(t *testing.T) {
	now := time.Now()
	table := New[string]()
	token, key, err := table.Issue("ada", "https://chat.example", false, now)
	if err != nil || key != sha256.Sum256([]byte(token)) {
		t.Fatalf("Issue: %v", err)
	}
	if s, ok := table.Get(key); !ok || s.User != "ada" || s.Origin != "https://chat.example" || !s.Expires.Equal(now.Add(Lifetime)) {
		t.Fatalf("session %+v", s)
	}
	_, anyKey, _ := table.Issue("ada", "https://chat.example", true, now)
	if s, _ := table.Get(anyKey); s.Origin != "" || !s.AnyOrigin {
		t.Fatalf("a token from any origin keeps one: %+v", s)
	}
	entries := table.Entries(func(u string) string { return u })
	if len(entries) != 2 || len(table.Entries(func(u string) string { return u })) != 0 {
		t.Fatalf("entries %v", entries)
	}
	stored := make(map[string]jsontext.Value)
	for _, e := range entries {
		stored[e.ID] = e.Value
	}
	stored["not a key"] = jsontext.Value(`{}`)
	restored := New[string]()
	if err := restored.Restore(stored, func(id string) (string, bool) { return id, id == "ada" }, now); err != nil {
		t.Fatal(err)
	}
	if restored.Len() != 2 {
		t.Fatalf("restored %d sessions", restored.Len())
	}
	// An expired session, or one whose account is gone, is deleted from
	// the store as it is restored.
	gone := New[string]()
	if err := gone.Restore(stored, func(id string) (string, bool) { return id, false }, now); err != nil {
		t.Fatal(err)
	}
	if gone.Len() != 0 || len(gone.Entries(func(u string) string { return u })) != 2 {
		t.Fatal("sessions of a gone account were kept")
	}
	restored.Prune(now.Add(Lifetime))
	if restored.Len() != 0 {
		t.Fatal("Prune kept expired sessions")
	}
}
