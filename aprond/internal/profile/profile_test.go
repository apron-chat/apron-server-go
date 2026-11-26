package profile

import (
	"bytes"
	"testing"
)

type image string

func (i image) ImageID() string { return string(i) }

// Each change is stored once, and a stored profile comes back as it was,
// without an avatar that is gone.
func TestProfile(t *testing.T) {
	var p Profile[image]
	if p.Entries() != nil || p.Snapshot() != nil {
		t.Error("a profile never set is stored")
	}
	p.SetTitle("Ops <&>")
	p.SetDescription("For *ops*\nonly")
	p.SetAvatar("embed_1")
	entries := p.Entries()
	if len(entries) != 1 || entries[0].Kind != StoreKind || entries[0].ID != StoreID || p.Entries() != nil {
		t.Fatalf("stored %v, then again", entries)
	}
	if want := `{"title":"Ops <&>","description":"For *ops*\nonly","avatar_embed":"embed_1"}`; string(entries[0].Value) != want {
		t.Errorf("stored %s, want %s", entries[0].Value, want)
	}
	var restored Profile[image]
	if err := restored.Restore(entries[0].Value, func(id string) (image, bool) { return image(id), true }); err != nil {
		t.Fatal(err)
	}
	if restored.Title() != p.Title() || restored.Description() != p.Description() || restored.Avatar() != "embed_1" || restored.Entries() != nil {
		t.Errorf("restored %+v", restored)
	}
	if again := restored.Snapshot(); len(again) != 1 || !bytes.Equal(again[0].Value, entries[0].Value) {
		t.Errorf("stored again as %v", again)
	}

	var gone Profile[image]
	if err := gone.Restore(entries[0].Value, func(string) (image, bool) { return "", false }); err != nil {
		t.Fatal(err)
	}
	if again := gone.Entries(); gone.Avatar() != "" || len(again) != 1 || bytes.Contains(again[0].Value, []byte("avatar")) {
		t.Errorf("an avatar that is gone is kept: %v", again)
	}
	p.SetTitle("")
	p.SetDescription("")
	p.SetAvatar("")
	if cleared := p.Entries(); len(cleared) != 1 || string(cleared[0].Value) != "{}" || len(p.Snapshot()) != 1 {
		t.Errorf("a cleared profile stored as %v", cleared)
	}
	if err := gone.Restore([]byte(`[]`), nil); err == nil {
		t.Error("a bad stored profile restored")
	}
}
