package embeds

import (
	"bytes"
	"encoding/json/jsontext"
	"slices"
	"testing"
)

// hosted is an embed as a test keeps one.
type hosted struct {
	Embed
	avatarFor string
}

func (h *hosted) Hosted() *Embed { return &h.Embed }

func avatarFor(h *hosted) string { return h.avatarFor }

func TestTable(t *testing.T) {
	table := New[*hosted]()
	upload := &hosted{ID: "embed_1", Kind: "upload", MessageID: "1", BaseURL: "https://chat.example", Secret: "s"}
	avatar := &hosted{ID: "embed_2", Kind: "upload", BaseURL: "https://chat.example", Secret: "t", avatarFor: "ada"}
	emoji := &hosted{ID: "embed_3", Kind: "upload", EmojiKey: ":party:", BaseURL: "https://chat.example", Secret: "u"}
	for _, h := range []*hosted{upload, avatar, emoji} {
		table.Add(h)
	}
	table.AddWrite("token", upload)
	if table.Get("embed_1") != upload || table.Write("token") != upload || table.Len() != 3 || table.WriteCount() != 1 {
		t.Fatal("Get or Write")
	}
	if ids := table.IDs(); !slices.Equal(ids, []string{"embed_1", "embed_2", "embed_3"}) {
		t.Fatalf("IDs %v", ids)
	}
	table.EndWrite("token")
	if table.Write("token") != nil {
		t.Fatal("EndWrite kept the write")
	}

	// Finished uploads are held oldest first, but custom emoji are apart.
	for i, h := range []*hosted{upload, avatar, emoji} {
		h.Finished, h.Size, h.Seq, h.Path = true, int64(10*(i+1)), int64(i+1), "/uploads/"+h.ID+".upload"
		h.Owned = map[string]any{"url": "https://chat.example/files/" + h.ID}
		table.Hold(h)
	}
	table.Hold(upload) // again: unchanged
	if table.UploadCount() != 2 || table.UploadBytes() != 30 || !emoji.Apart() {
		t.Fatalf("held %d uploads of %d bytes", table.UploadCount(), table.UploadBytes())
	}
	if held := slices.Collect(table.Uploads()); len(held) != 2 || held[0] != upload || held[1] != avatar {
		t.Fatalf("Uploads %v", held)
	}

	entries := table.Entries(avatarFor)
	if len(entries) != 3 || len(table.Entries(avatarFor)) != 0 {
		t.Fatalf("entries %v", entries)
	}
	stored := make(map[string]jsontext.Value)
	for _, e := range entries {
		stored[e.ID] = e.Value
	}
	want := `{"kind":"upload","avatar_for":"ada","base_url":"https://chat.example","secret":"t","finished":true,"owned":{"url":"https://chat.example/files/embed_2"},"file":"embed_2.upload","size":20,"seq":2}`
	if string(stored["embed_2"]) != want {
		t.Fatalf("stored\n%s\nwant\n%s", stored["embed_2"], want)
	}

	restored := New[*hosted]()
	files, err := restored.Restore(stored, func(e Embed, avatarFor string) *hosted {
		return &hosted{Embed: e, avatarFor: avatarFor}
	}, func(file string) string { return "/restored/" + file })
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 || !files["/restored/embed_1.upload"] || restored.UploadCount() != 2 || restored.UploadBytes() != 30 {
		t.Fatalf("restored files %v, %d uploads", files, restored.UploadCount())
	}
	if h := restored.Get("embed_2"); h.avatarFor != "ada" || h.Path != "/restored/embed_2.upload" {
		t.Fatalf("restored %+v", h)
	}
	for _, e := range restored.Snapshot(avatarFor) {
		if !bytes.Equal(e.Value, stored[e.ID]) {
			t.Errorf("%s is stored again as %s, want %s", e.ID, e.Value, stored[e.ID])
		}
	}

	// Removing an upload releases it.
	if table.Remove("embed_1") != upload || table.Remove("embed_1") != nil {
		t.Fatal("Remove")
	}
	if table.UploadCount() != 1 || table.UploadBytes() != 20 {
		t.Fatalf("after Remove: %d uploads of %d bytes", table.UploadCount(), table.UploadBytes())
	}
	if e := table.Entries(avatarFor); len(e) != 1 || e[0].Value != nil {
		t.Fatalf("a removed embed's entry: %v", e)
	}
	if _, err := New[*hosted]().Restore(map[string]jsontext.Value{"embed_9": jsontext.Value(`[]`)}, nil, nil); err == nil {
		t.Fatal("an embed that is no object restored")
	}
}

// New embed_ids and upload sequence numbers go on from the stored counters.
func TestCounters(t *testing.T) {
	table := New[*hosted]()
	if id := table.NewID(); id != "embed_1" || table.NextSeq() != 1 {
		t.Fatalf("first embed_id %s", id)
	}
	restored := New[*hosted]()
	restored.RestoreCounters(table.Counters())
	if id := restored.NewID(); id != "embed_2" || restored.NextSeq() != 2 {
		t.Errorf("restored counters issued %s", id)
	}
}
