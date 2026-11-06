// Package store keeps an apron server's state across restarts.
//
// The server saves its state as entries: opaque values keyed by a kind and an
// ID, whose contents only the server knows. So a backend is small: it loads
// every entry when the server starts, and applies batches of changes
// atomically after that ([Store]). This package has two backends, [SQLite]
// and [Memory], and [Open] picks one by name, as aprond's --store flag does.
// Give one, or any other implementation of Store, to the server as
// apron.Config.Store.
package store

import (
	"errors"
	"fmt"
	"strings"
)

// Entry is one stored value, which Kind and ID name together.
type Entry struct {
	// Kind groups entries, such as "user" or "room"; ID names one in its
	// kind.
	Kind string
	ID   string
	// Value is the entry's content, JSON as the server writes it. A nil
	// Value in a batch deletes the entry.
	Value []byte
}

// Store keeps entries across restarts. A server calls Load once as it
// starts, then Apply from one goroutine, in order, and Close as it shuts
// down.
type Store interface {
	// Load calls visit for every stored entry, in no particular order.
	Load(visit func(Entry) error) error
	// Apply writes a batch of puts and deletes atomically.
	Apply(batch []Entry) error
	// Close releases the store; it must not be used afterwards.
	Close() error
}

// Open opens a store named by a URL-like string:
//
//	memory             entries kept only in this process
//	sqlite:<path>      a SQLite database file, created if missing
func Open(name string) (Store, error) {
	scheme, path, _ := strings.Cut(name, ":")
	switch scheme {
	case "memory":
		return NewMemory(), nil
	case "sqlite":
		if path == "" {
			return nil, errors.New("sqlite store needs a path, such as sqlite:aprond.db")
		}
		return OpenSQLite(strings.TrimPrefix(path, "//"))
	}
	return nil, fmt.Errorf("unknown store %q; use memory or sqlite:<path>", name)
}
