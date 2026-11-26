package webpush

import (
	"bytes"
	"testing"
)

// A key made at the first start is stored once, and comes back from its
// entry, which it renders again byte for byte.
func TestStoredKey(t *testing.T) {
	var made StoredKey
	if made.Snapshot() != nil {
		t.Error("no key, yet an entry")
	}
	key, err := made.Restore(nil)
	if err != nil {
		t.Fatal(err)
	}
	entries := made.Entries()
	if len(entries) != 1 || entries[0].Kind != StoreKind || entries[0].ID != StoreID || made.Entries() != nil {
		t.Fatalf("a new key stored as %v, then again", entries)
	}
	var restored StoredKey
	again, err := restored.Restore(entries[0].Value)
	if err != nil || again.Public != key.Public {
		t.Fatalf("restored %v, %v", again, err)
	}
	if restored.Entries() != nil {
		t.Error("a restored key is stored again")
	}
	if snapshot := restored.Snapshot(); len(snapshot) != 1 || !bytes.Equal(snapshot[0].Value, entries[0].Value) {
		t.Errorf("stored again as %v, want %s", snapshot, entries[0].Value)
	}
	if _, err := restored.Restore([]byte(`"not a key"`)); err == nil {
		t.Error("a bad stored key restored")
	}
}
