package devices

import (
	"bytes"
	"crypto/sha256"
	"encoding/json/jsontext"
	"strings"
	"testing"
	"time"
)

func TestTable(t *testing.T) {
	now := time.Now()
	table := New[string]()
	token, key := table.Mint("ada", "laptop", now)
	if key != sha256.Sum256([]byte(token)) || !strings.HasPrefix(token, Prefix) {
		t.Fatalf("Mint: %q", token)
	}
	if d, ok := table.Get(key); !ok || d.User != "ada" || d.Name != "laptop" || !d.Created.Equal(now) {
		t.Fatalf("device %+v", d)
	}
	_, phone := table.Mint("ada", "phone", now.Add(time.Second))
	table.Mint("bob", "laptop", now)
	if keys, devices := table.Of("ada"); len(keys) != 2 || keys[0] != key || keys[1] != phone || devices[1].Name != "phone" {
		t.Fatalf("Of: %v %+v", keys, devices)
	}
	userID := func(u string) string { return u }
	entries := table.Entries(userID)
	if len(entries) != 3 || len(table.Entries(userID)) != 0 {
		t.Fatalf("entries %v", entries)
	}
	stored := make(map[string]jsontext.Value)
	for _, e := range entries {
		stored[e.ID] = e.Value
	}
	stored["not a key"] = jsontext.Value(`{}`)
	restored := New[string]()
	if err := restored.Restore(stored, func(id string) (string, bool) { return id, id == "ada" }); err != nil {
		t.Fatal(err)
	}
	// bob's token is deleted from the store as it is restored.
	if keys, _ := restored.Of("ada"); len(keys) != 2 || len(restored.Entries(userID)) != 1 {
		t.Fatalf("restored %v", keys)
	}
	// A token is stored again as it was.
	for _, e := range restored.Snapshot(userID) {
		if !bytes.Equal(e.Value, stored[e.ID]) {
			t.Errorf("%s is stored again as %s, want %s", e.ID, e.Value, stored[e.ID])
		}
	}
	restored.Delete(key)
	if _, ok := restored.Get(key); ok {
		t.Fatal("Delete kept the token")
	}
	if e := restored.Entries(userID); len(e) != 1 || e[0].Value != nil {
		t.Fatalf("a deleted token's entry: %v", e)
	}
}

func TestValidName(t *testing.T) {
	for name, want := range map[string]bool{
		"laptop": true, "a": true, "Work-PC.2_x": true, strings.Repeat("a", MaxNameBytes): true,
		"": false, "-a": false, "_a": false, "a b": false, "é": false, strings.Repeat("a", MaxNameBytes+1): false,
	} {
		if ValidName(name) != want {
			t.Errorf("ValidName(%q) = %v", name, !want)
		}
	}
}
