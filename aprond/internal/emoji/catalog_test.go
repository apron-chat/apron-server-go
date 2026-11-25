package emoji

import (
	"bytes"
	"strings"
	"testing"
)

// image is an upload as a test keeps one.
type image struct {
	id, url, alt string
	size         int64
}

func (i *image) ImageID() string  { return i.id }
func (i *image) ImageURL() string { return i.url }
func (i *image) ImageAlt() string { return i.alt }
func (i *image) ImageSize() int64 { return i.size }

func TestCatalog(t *testing.T) {
	c := NewCatalog[*image]()
	emptyName, empty := c.Rendered()
	if string(empty) != "{}" || !strings.HasPrefix(emptyName, "catalog-") || c.Entries() != nil {
		t.Fatalf("empty catalog %s %s", emptyName, empty)
	}
	party := &image{id: "embed_1", url: "https://chat.example/files/embed_1/s", alt: "A party", size: 10}
	wave := &image{id: "embed_2", url: "https://chat.example/files/embed_2/s", size: 5}
	c.Set(":party:", &Entry[*image]{Image: party, By: "ada", Width: 3, Height: 2})
	c.Set(":wave:", &Entry[*image]{Image: wave, By: "bob", Width: 3})
	name, rendered := c.Rendered()
	if name == emptyName || c.Len() != 2 || c.Bytes() != 15 || c.Get(":party:").By != "ada" {
		t.Fatalf("catalog %s", name)
	}
	// A size goes with both of its dimensions only.
	want := `{":party:":{"alt":"A party","height":2,"url":"https://chat.example/files/embed_1/s","width":3},":wave:":{"url":"https://chat.example/files/embed_2/s"}}`
	if string(rendered) != want {
		t.Fatalf("rendered\n%s\nwant\n%s", rendered, want)
	}
	entries := c.Entries()
	if len(entries) != 1 || entries[0].Kind != StoreKind || entries[0].ID != StoreID || c.Entries() != nil {
		t.Fatalf("entries %v", entries)
	}
	stored := entries[0].Value
	if string(stored) != `{":party:":{"embed":"embed_1","by":"ada","width":3,"height":2},":wave:":{"embed":"embed_2","by":"bob","width":3}}` {
		t.Fatalf("stored %s", stored)
	}

	// A key whose image is gone is dropped, and the catalog stored again.
	restored := NewCatalog[*image]()
	if err := restored.Restore(stored, func(id, key string) (*image, bool) {
		return party, id == "embed_1" && key == ":party:"
	}); err != nil {
		t.Fatal(err)
	}
	if restored.Len() != 1 || restored.Get(":wave:") != nil || len(restored.Entries()) != 1 {
		t.Fatal("the gone image's key stays")
	}
	again := NewCatalog[*image]()
	_ = again.Restore(stored, func(id, key string) (*image, bool) { return &image{id: id}, true })
	if snapshot := again.Snapshot(); len(snapshot) != 1 || !bytes.Equal(snapshot[0].Value, stored) {
		t.Fatalf("stored again as %s", snapshot[0].Value)
	}
	if removed := c.Remove(":wave:"); removed == nil || removed.Image != wave || c.Remove(":wave:") != nil || len(c.Entries()) != 1 {
		t.Fatal("Remove")
	}
	if err := NewCatalog[*image]().Restore([]byte(`[]`), nil); err == nil {
		t.Fatal("a catalog that is no object restored")
	}
}
