package services

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTable(t *testing.T) {
	var mu sync.Mutex
	var expired []string
	table := New(func(key Key, token *Token[string]) {
		mu.Lock()
		defer mu.Unlock()
		expired = append(expired, token.ID)
	})
	defer table.Stop()
	token, key, err := NewToken()
	if err != nil || key != sha256.Sum256([]byte(token)) || !strings.HasPrefix(token, Prefix) {
		t.Fatalf("NewToken: %q %v", token, err)
	}
	now := time.Now()
	table.Add(key, &Token[string]{ID: NewID(), User: "agent", Name: "python", Created: now, Expires: now.Add(time.Hour), Manage: true})
	short := mustKey(t)
	table.Add(short, &Token[string]{ID: "svc_short", User: "agent", Created: now, Expires: now.Add(time.Hour)})
	table.Shorten(short, now.Add(10*time.Millisecond))
	table.Shorten(key, now.Add(2*time.Hour)) // later: unchanged
	if got := table.Get(key); got == nil || !got.Expires.Equal(now.Add(time.Hour)) || !strings.HasPrefix(got.ID, "svc_") {
		t.Fatalf("token %+v", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(expired)
		mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the shortened token did not expire")
		}
		time.Sleep(time.Millisecond)
	}
	// The table leaves revoking an expired token to its owner.
	if expired[0] != "svc_short" || table.Remove(short) == nil || table.Remove(short) != nil {
		t.Fatalf("expired %v", expired)
	}

	userID := func(u string) string { return u }
	entries := table.Entries(userID)
	if len(entries) != 2 || len(table.Entries(userID)) != 0 {
		t.Fatalf("entries %v", entries)
	}
	stored := make(map[string]jsontext.Value)
	for _, e := range entries {
		if e.Value != nil {
			stored[e.ID] = e.Value
		}
	}
	gone, old := mustKey(t), mustKey(t)
	stored[hex.EncodeToString(gone[:])] = jsontext.Value(`{"id":"svc_gone","user":"gone","created":"2020-01-01T00:00:00Z","expires":"2090-01-01T00:00:00Z"}`)
	stored[hex.EncodeToString(old[:])] = jsontext.Value(`{"id":"svc_old","user":"agent","created":"2020-01-01T00:00:00Z","expires":"2020-01-02T00:00:00Z"}`)
	stored["not a key"] = jsontext.Value(`{}`)
	restored := New(func(Key, *Token[string]) {})
	defer restored.Stop()
	if err := restored.Restore(stored, func(id string) (string, bool) { return id, id == "agent" }, now); err != nil {
		t.Fatal(err)
	}
	// The expired token, and the gone account's, are deleted from the store.
	if again := restored.Entries(userID); len(again) != 2 || again[0].Value != nil || again[1].Value != nil {
		t.Fatalf("deleted %v", again)
	}
	for _, e := range restored.Snapshot(userID) {
		if !bytes.Equal(e.Value, stored[e.ID]) {
			t.Errorf("%s is stored again as %s, want %s", e.ID, e.Value, stored[e.ID])
		}
	}
	if got := restored.Get(key); got == nil || !got.Manage || got.Name != "python" {
		t.Fatalf("restored %+v", got)
	}
}

func mustKey(t *testing.T) Key {
	t.Helper()
	_, key, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	return key
}
