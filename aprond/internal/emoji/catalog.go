package emoji

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"regexp"

	"github.com/apron-chat/apron-server-go/store"
	"github.com/apron-chat/apron-server-go/wire"
)

// Custom emoji (ext:emoji in apron's EXTENSIONS.md) are images users add
// with /emoji add and write as :name: in reactions and text. A Catalog
// keeps them by key, renders the catalog clients load, a JSON object keyed
// by :name:, under a name made of its content's hash, so that its URL
// changes whenever it does, and keeps the catalog in the store.

const (
	// MaxCustom bounds the catalog.
	MaxCustom = 1000
	// StoreKind and StoreID name the store entry that holds the catalog.
	StoreKind, StoreID = "meta", "emoji"
)

// KeyPattern is a custom emoji's key: a name between colons.
var KeyPattern = regexp.MustCompile(`^:[A-Za-z0-9_+-]+:$`)

// Image is a custom emoji's image, a hosted upload, such as the server's
// embed state.
type Image interface {
	comparable
	// ImageID names the upload in the store, ImageURL is where clients
	// load it, ImageAlt describes it, if anything does, and ImageSize is
	// its length in bytes.
	ImageID() string
	ImageURL() string
	ImageAlt() string
	ImageSize() int64
}

// Entry is one custom emoji: its image, the image's size in pixels, if
// known, and the user_id of who added it.
type Entry[I Image] struct {
	Image         I
	By            string
	Width, Height int
}

// Catalog is the custom emoji by key. It is not safe for concurrent use:
// the server calls it holding its lock.
type Catalog[I Image] struct {
	entries map[string]*Entry[I]
	// rendered is the catalog clients load, and name the name it is served
	// under.
	rendered []byte
	name     string
	// dirty reports a change since Entries last took it.
	dirty bool
}

// NewCatalog returns an empty catalog.
func NewCatalog[I Image]() *Catalog[I] {
	c := &Catalog[I]{entries: make(map[string]*Entry[I])}
	c.render()
	return c
}

// Get returns the custom emoji of key, or nil.
func (c *Catalog[I]) Get(key string) *Entry[I] { return c.entries[key] }

// Len counts the custom emoji.
func (c *Catalog[I]) Len() int { return len(c.entries) }

// Bytes totals the sizes of the custom emoji's images.
func (c *Catalog[I]) Bytes() int64 {
	var total int64
	for _, entry := range c.entries {
		total += entry.Image.ImageSize()
	}
	return total
}

// Set makes entry the custom emoji of key, and renders the catalog again.
func (c *Catalog[I]) Set(key string, entry *Entry[I]) {
	c.entries[key] = entry
	c.changed()
}

// Remove forgets the custom emoji of key, renders the catalog again, and
// returns what it forgot, or nil if there was none.
func (c *Catalog[I]) Remove(key string) *Entry[I] {
	entry := c.entries[key]
	if entry != nil {
		delete(c.entries, key)
		c.changed()
	}
	return entry
}

// Rendered returns the catalog clients load, and the name it is served
// under, which changes whenever it does.
func (c *Catalog[I]) Rendered() (name string, catalog []byte) {
	return c.name, c.rendered
}

func (c *Catalog[I]) changed() {
	c.dirty = true
	c.render()
}

// served is a custom emoji as the catalog clients load has it; its fields
// are in the order their names sort, as an object's keys are encoded.
type served struct {
	Alt    string `json:"alt,omitzero"`
	Height int    `json:"height,omitzero"`
	URL    string `json:"url"`
	Width  int    `json:"width,omitzero"`
}

// render renders the catalog clients load, and names it.
func (c *Catalog[I]) render() {
	catalog := make(map[string]served, len(c.entries))
	for key, entry := range c.entries {
		s := served{URL: entry.Image.ImageURL(), Alt: entry.Image.ImageAlt()}
		if entry.Width > 0 && entry.Height > 0 {
			s.Width, s.Height = entry.Width, entry.Height
		}
		catalog[key] = s
	}
	c.rendered, _ = wire.Marshal(catalog)
	sum := sha256.Sum256(c.rendered)
	c.name = "catalog-" + hex.EncodeToString(sum[:8]) + ".json"
}

// stored is a custom emoji as the store keeps it.
type stored struct {
	Embed  string `json:"embed"`
	By     string `json:"by"`
	Width  int    `json:"width,omitzero"`
	Height int    `json:"height,omitzero"`
}

// Entries returns the store entry of the catalog if it changed since it
// was last called.
func (c *Catalog[I]) Entries() []store.Entry {
	if !c.dirty {
		return nil
	}
	c.dirty = false
	all := make(map[string]stored, len(c.entries))
	for key, entry := range c.entries {
		all[key] = stored{Embed: entry.Image.ImageID(), By: entry.By, Width: entry.Width, Height: entry.Height}
	}
	value, err := wire.Marshal(all)
	if err != nil {
		return nil
	}
	return []store.Entry{{Kind: StoreKind, ID: StoreID, Value: value}}
}

// Snapshot returns the store entry of the catalog, as Entries does when it
// changed.
func (c *Catalog[I]) Snapshot() []store.Entry {
	c.dirty = true
	return c.Entries()
}

// Restore takes the stored catalog, raw, nil for none, whose images image
// returns by their IDs and keys: a key whose image it does not return is
// dropped, and the catalog stored again without it.
func (c *Catalog[I]) Restore(raw jsontext.Value, image func(id, key string) (I, bool)) error {
	if raw != nil {
		var all map[string]stored
		if err := json.Unmarshal(raw, &all); err != nil {
			return fmt.Errorf("stored emoji: %w", err)
		}
		for key, s := range all {
			i, ok := image(s.Embed, key)
			if !ok {
				c.dirty = true
				continue
			}
			c.entries[key] = &Entry[I]{Image: i, By: s.By, Width: s.Width, Height: s.Height}
		}
	}
	c.render()
	return nil
}
