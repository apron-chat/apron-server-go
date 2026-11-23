package invitations

import (
	"bytes"
	"encoding/json/jsontext"
	"strings"
	"testing"
	"time"
)

func TestTable(t *testing.T) {
	now := time.Now()
	table := New()
	token := NewToken()
	if !strings.HasPrefix(token, TokenPrefix) || !strings.HasPrefix(NewID(), "inv_") {
		t.Fatalf("token %q", token)
	}
	kept := &Invitation{ID: "inv_kept", Created: now, Expires: now.Add(time.Hour), Rooms: []string{"general"}, Data: jsontext.Value(`{"n":12345678901234567890}`)}
	short := &Invitation{ID: "inv_short", Created: now.Add(time.Second), Expires: now.Add(time.Minute)}
	if !table.Add(token, kept, now) || !table.Add(NewToken(), short, now) {
		t.Fatal("Add refused")
	}
	if got := table.Presented(token, now); got != kept {
		t.Fatalf("Presented: %+v", got)
	}
	if table.Presented(token, now.Add(time.Hour)) != nil || table.Presented("apron_invite_wrong", now) != nil {
		t.Fatal("an expired or unknown invitation was presented")
	}
	if list := table.List(now); len(list) != 2 || list[0] != kept || list[1] != short {
		t.Fatalf("List: %v", list)
	}
	if enrolled := kept.Enroll(now); enrolled.Invitation != "inv_kept" || !enrolled.Admitted || !bytes.Equal(enrolled.Data, kept.Data) {
		t.Fatalf("Enroll: %+v", enrolled)
	}
	entries := table.Entries()
	if len(entries) != 2 || len(table.Entries()) != 0 {
		t.Fatalf("entries %v", entries)
	}
	stored := make(map[string]jsontext.Value)
	for _, e := range entries {
		stored[e.ID] = e.Value
	}
	stored["not a key"] = jsontext.Value(`{}`)

	// Restored after the short one expired, it is deleted from the store.
	restored := New()
	if err := restored.Restore(stored, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if list := restored.List(now); len(list) != 1 || list[0].ID != "inv_kept" || len(restored.Entries()) != 1 {
		t.Fatalf("restored %v", list)
	}
	for _, e := range restored.Snapshot() {
		if !bytes.Equal(e.Value, stored[e.ID]) {
			t.Errorf("%s is stored again as %s, want %s", e.ID, e.Value, stored[e.ID])
		}
	}
	restored.Use(token)
	if restored.Presented(token, now) != nil || len(restored.Entries()) != 1 {
		t.Fatal("a used invitation stays")
	}
	if !table.Revoke("inv_short") || table.Revoke("inv_short") {
		t.Fatal("Revoke")
	}
	table.Prune(now.Add(time.Hour))
	if len(table.List(now)) != 0 {
		t.Fatal("Prune kept an expired invitation")
	}
}

func TestTableBound(t *testing.T) {
	now := time.Now()
	table := New()
	for range Max {
		table.Add(NewToken(), &Invitation{Expires: now.Add(time.Hour)}, now)
	}
	if table.Add(NewToken(), &Invitation{Expires: now.Add(time.Hour)}, now) {
		t.Fatal("Add went past Max")
	}
	// Expired invitations make room.
	if !table.Add(NewToken(), &Invitation{Expires: now.Add(2 * time.Hour)}, now.Add(time.Hour)) {
		t.Fatal("Add refused with every invitation expired")
	}
}
