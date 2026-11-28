package aprond

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"github.com/apron-chat/apron-server-go/store"
)

// The store's format (SERVER.md, "Storage"). A store says which format its
// entries are in with one entry, meta "format", {"version": n}: written
// when the server first writes to a new store, or opens one that has none,
// which every server before format versions wrote, version 1. A server
// opens a store of its own format or an older one, which it upgrades as
// it loads it, one version at a time (storeUpgrades), and refuses one of a
// newer format, whose entries it might misread.
//
// What needs no new version: a field added to an entry, which an older
// server ignores, and which reads as its initial value, the zero value,
// when it is absent, as from a store an older server wrote, such as a
// room's retained_id. A change that an older server would misread, or
// that makes it lose what a newer one keeps, takes one: storeFormat goes
// up, and storeUpgrades gets the step that brings the previous version's
// entries to it.
const storeFormat = 1

// entryFormat is the meta entry that says a store's format.
const entryFormat = "format"

type storedFormat struct {
	Version int `json:"version"`
}

// storeUpgrades[i] brings the entries of a store of format version i+1
// to version i+2, as they are loaded, before anything reads them; none
// yet.
var storeUpgrades []func(entries map[string]map[string]jsontext.Value) error

// checkFormatLocked reads the format of the store's entries, refuses a
// newer one than the server knows, and upgrades an older one; it marks
// the format to be stored when the store did not say it, or said an older
// one.
func (s *Server) checkFormatLocked(entries map[string]map[string]jsontext.Value) error {
	version := 1
	raw, stored := entries[entryMeta][entryFormat]
	if stored {
		var f storedFormat
		if err := json.Unmarshal(raw, &f); err != nil || f.Version < 1 {
			return fmt.Errorf("stored %s %q is no format version: %s", entryMeta, entryFormat, raw)
		}
		version = f.Version
	}
	if version > storeFormat {
		return fmt.Errorf("the store is format version %d, newer than this aprond's %d: run an aprond that knows it", version, storeFormat)
	}
	for v := version; v < storeFormat; v++ {
		if err := storeUpgrades[v-1](entries); err != nil {
			return fmt.Errorf("upgrading the store from format version %d to %d: %w", v, v+1, err)
		}
	}
	s.formatDue = !stored || version != storeFormat
	return nil
}

// formatEntry is the entry that says the store's format, when it is due.
func (s *Server) formatEntryLocked() []store.Entry {
	if !s.formatDue {
		return nil
	}
	s.formatDue = false
	return []store.Entry{{Kind: entryMeta, ID: entryFormat, Value: encodeJSON(storedFormat{Version: storeFormat})}}
}
