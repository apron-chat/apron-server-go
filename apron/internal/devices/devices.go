// Package devices keeps the device tokens that `/invite-device <name>`
// mints (following apron-server-cloudflare's command of that name): bearer
// tokens that sign an account in on another device through the token
// scheme (PROTOCOL.md §3.2), from any origin, until the same name mints a
// new one. Unlike a session, a device token is bound to no origin and does
// not expire. A Table keeps each token's account, name, and when it was
// minted, and keeps them in the store, by the SHA-256 of the token, never
// the token itself.
//
// A Table knows its accounts only as values of the type U, such as the
// server's user state, and their user_ids when it writes them. It is not
// safe for concurrent use: the server calls it holding its lock.
package devices

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"iter"
	"maps"
	"slices"
	"time"

	"github.com/apron-chat/apron-server-go/store"
	"github.com/apron-chat/apron-server-go/wire"
)

const (
	// MaxPerUser bounds the device tokens of one account, which a name
	// again replaces.
	MaxPerUser = 3
	// Prefix starts every device token, which tells the token scheme it
	// is one.
	Prefix = "apron_device_"
	// MaxNameBytes bounds a device's name.
	MaxNameBytes = 32
	// EntryKind is the kind of the store entries that hold device tokens,
	// named by the hex SHA-256 of their tokens.
	EntryKind = "device"
)

// Key is the SHA-256 of a token, which a device token is kept under.
type Key = [32]byte

// Device is a device token: the account it signs in as, and the name it
// was minted under, when.
type Device[U comparable] struct {
	User    U
	Name    string
	Created time.Time
}

// Table is the device tokens by key.
type Table[U comparable] struct {
	byKey map[Key]Device[U]
	// dirty are the keys put or deleted since Entries last took them.
	dirty map[Key]struct{}
}

// New returns an empty table.
func New[U comparable]() *Table[U] {
	return &Table[U]{byKey: make(map[Key]Device[U]), dirty: make(map[Key]struct{})}
}

// ValidName reports whether name can name a device: 1 to MaxNameBytes
// letters, digits, `_`, `.` or `-`, starting with a letter or digit.
func ValidName(name string) bool {
	if len(name) == 0 || len(name) > MaxNameBytes || !alphanumeric(name[0]) {
		return false
	}
	for i := range len(name) {
		if b := name[i]; !alphanumeric(b) && b != '_' && b != '.' && b != '-' {
			return false
		}
	}
	return true
}

func alphanumeric(b byte) bool {
	return 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9'
}

// Get returns the device token kept under key.
func (t *Table[U]) Get(key Key) (Device[U], bool) {
	d, ok := t.byKey[key]
	return d, ok
}

// Mint makes a device token that signs in as user, named name, and returns
// it with the key it is kept under. It does not check name, nor the
// user's other tokens (Of).
func (t *Table[U]) Mint(user U, name string, now time.Time) (string, Key, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", Key{}, err
	}
	token := Prefix + base64.RawURLEncoding.EncodeToString(secret)
	key := sha256.Sum256([]byte(token))
	t.byKey[key] = Device[U]{User: user, Name: name, Created: now}
	t.dirty[key] = struct{}{}
	return token, key, nil
}

// Delete forgets the device token under key, and deletes it from the
// store, as one that may be stored though it is not kept.
func (t *Table[U]) Delete(key Key) {
	delete(t.byKey, key)
	t.dirty[key] = struct{}{}
}

// All lists the device tokens, in no order. The loop may Delete them.
func (t *Table[U]) All() iter.Seq2[Key, Device[U]] {
	return maps.All(t.byKey)
}

// Of lists user's device tokens oldest first, with their keys.
func (t *Table[U]) Of(user U) ([]Key, []Device[U]) {
	var keys []Key
	for key, d := range t.byKey {
		if d.User == user {
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, func(a, b Key) int {
		if order := t.byKey[a].Created.Compare(t.byKey[b].Created); order != 0 {
			return order
		}
		return bytes.Compare(a[:], b[:])
	})
	devices := make([]Device[U], len(keys))
	for i, key := range keys {
		devices[i] = t.byKey[key]
	}
	return keys, devices
}

// stored is a device token as the store keeps it.
type stored struct {
	User    string    `json:"user"`
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
}

// Entries returns the store entries of the device tokens minted or deleted
// since it was last called, naming each token's account by userID.
func (t *Table[U]) Entries(userID func(U) string) []store.Entry {
	if len(t.dirty) == 0 {
		return nil
	}
	entries := make([]store.Entry, 0, len(t.dirty))
	for key := range t.dirty {
		entry := store.Entry{Kind: EntryKind, ID: hex.EncodeToString(key[:])}
		if d, ok := t.byKey[key]; ok {
			entry.Value, _ = wire.Marshal(stored{User: userID(d.User), Name: d.Name, Created: d.Created})
		}
		entries = append(entries, entry)
	}
	clear(t.dirty)
	return entries
}

// Snapshot returns the store entries of every device token, as Entries
// does those that changed.
func (t *Table[U]) Snapshot(userID func(U) string) []store.Entry {
	for key := range t.byKey {
		t.dirty[key] = struct{}{}
	}
	return t.Entries(userID)
}

// Restore takes the stored device tokens. One whose account user does not
// return is deleted from the store; an entry not named by a key is passed
// over.
func (t *Table[U]) Restore(entries map[string]jsontext.Value, user func(id string) (U, bool)) error {
	for id, raw := range entries {
		decoded, err := hex.DecodeString(id)
		if err != nil || len(decoded) != len(Key{}) {
			continue
		}
		var s stored
		if err := json.Unmarshal(raw, &s); err != nil {
			return fmt.Errorf("stored %s %q: %w", EntryKind, id, err)
		}
		key := Key(decoded)
		if u, ok := user(s.User); ok {
			t.byKey[key] = Device[U]{User: u, Name: s.Name, Created: s.Created}
		} else {
			t.Delete(key)
		}
	}
	return nil
}
