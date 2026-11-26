// Package services keeps the service tokens of bots: the credentials of a
// client outside the server's process, such as an agent in another
// language, which signs in as its bot with the token scheme (PROTOCOL.md
// §3.2). Unlike a session token, a service token's expiry is fixed when it
// is issued and never extended by use; unlike a device token, it expires. A
// Table keeps each token's account, name, lifetime, and whether it may
// manage its account, revokes it when it expires, and keeps it in the
// store, by the SHA-256 of the token, never the token itself.
//
// A Table knows its accounts only as values of the type U, such as the
// server's user state, and their user_ids when it writes them. It is not
// safe for concurrent use: the server calls it holding its lock, and the
// function it calls when a token expires takes that lock.
package services

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"iter"
	"maps"
	"strings"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/store"
)

const (
	// Prefix starts every service token, which tells the token scheme it
	// is one.
	Prefix = "apron_service_"
	// MaxPerUser bounds a bot's service tokens.
	MaxPerUser = 10
	// MaxNameBytes bounds a token's name.
	MaxNameBytes = 64
	// EntryKind is the kind of the store entries that hold service tokens,
	// named by the hex SHA-256 of their tokens.
	EntryKind = "service"
)

// Key is the SHA-256 of a token, which a service token is kept under.
type Key = [32]byte

// Token is an issued service token.
type Token[U comparable] struct {
	// ID names the token, to rotate or revoke it; it is not the token.
	ID   string
	User U
	// Name says what the token is for.
	Name             string
	Created, Expires time.Time
	// Manage lets the token's connections manage its account.
	Manage bool
	timer  *time.Timer
}

// Table is the service tokens by key.
type Table[U comparable] struct {
	byKey map[Key]*Token[U]
	// dirty are the keys added, changed, or removed since Entries last
	// took them.
	dirty map[Key]struct{}
	// expired is called, from a timer's goroutine, when a token kept under
	// a key expires: it takes the server's lock, and revokes the token if
	// the table still keeps it under the key (Get).
	expired func(Key, *Token[U])
}

// New returns an empty table, which calls expired when a token expires.
func New[U comparable](expired func(key Key, t *Token[U])) *Table[U] {
	return &Table[U]{byKey: make(map[Key]*Token[U]), dirty: make(map[Key]struct{}), expired: expired}
}

// NewToken makes a new service token, and returns it with the key it would
// be kept under.
func NewToken() (string, Key) {
	secret := make([]byte, 32)
	rand.Read(secret)
	token := Prefix + base64.RawURLEncoding.EncodeToString(secret)
	return token, sha256.Sum256([]byte(token))
}

// NewID makes a new token ID.
func NewID() string { return "svc_" + strings.ToLower(rand.Text()[:16]) }

// Get returns the token kept under key, or nil.
func (t *Table[U]) Get(key Key) *Token[U] { return t.byKey[key] }

// All lists the tokens, in no order. The loop may Remove them.
func (t *Table[U]) All() iter.Seq2[Key, *Token[U]] {
	return maps.All(t.byKey)
}

// Add keeps token under key, until it expires or is removed.
func (t *Table[U]) Add(key Key, token *Token[U]) {
	t.byKey[key] = token
	t.dirty[key] = struct{}{}
	t.schedule(key, token)
}

// Shorten makes the token under key expire at until, if it would expire
// later.
func (t *Table[U]) Shorten(key Key, until time.Time) {
	token := t.byKey[key]
	if token == nil || !until.Before(token.Expires) {
		return
	}
	token.Expires = until
	t.dirty[key] = struct{}{}
	t.schedule(key, token)
}

// Remove forgets the token under key, and returns it, or nil if there is
// none.
func (t *Table[U]) Remove(key Key) *Token[U] {
	token := t.byKey[key]
	if token == nil {
		return nil
	}
	if token.timer != nil {
		token.timer.Stop()
	}
	delete(t.byKey, key)
	t.dirty[key] = struct{}{}
	return token
}

// Stop stops every token's expiry, as the server shuts down.
func (t *Table[U]) Stop() {
	for _, token := range t.byKey {
		if token.timer != nil {
			token.timer.Stop()
		}
	}
}

// schedule calls expired when token expires.
func (t *Table[U]) schedule(key Key, token *Token[U]) {
	if token.timer != nil {
		token.timer.Stop()
	}
	token.timer = time.AfterFunc(time.Until(token.Expires), func() { t.expired(key, token) })
}

// stored is a service token as the store keeps it.
type stored struct {
	ID      string    `json:"id"`
	User    string    `json:"user"`
	Name    string    `json:"name,omitzero"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
	Manage  bool      `json:"manage,omitzero"`
}

// Entries returns the store entries of the tokens added, changed, or
// removed since it was last called, naming each token's account by userID.
func (t *Table[U]) Entries(userID func(U) string) []store.Entry {
	if len(t.dirty) == 0 {
		return nil
	}
	entries := make([]store.Entry, 0, len(t.dirty))
	for key := range t.dirty {
		entry := store.Entry{Kind: EntryKind, ID: hex.EncodeToString(key[:])}
		if token := t.byKey[key]; token != nil {
			entry.Value, _ = apron.Marshal(stored{
				ID: token.ID, User: userID(token.User), Name: token.Name, Created: token.Created, Expires: token.Expires, Manage: token.Manage,
			})
		}
		entries = append(entries, entry)
	}
	clear(t.dirty)
	return entries
}

// Snapshot returns the store entries of every token, as Entries does those
// that changed.
func (t *Table[U]) Snapshot(userID func(U) string) []store.Entry {
	for key := range t.byKey {
		t.dirty[key] = struct{}{}
	}
	return t.Entries(userID)
}

// Restore takes the stored tokens that have not expired at now, of
// accounts that user returns, and deletes the others from the store; an
// entry not named by a key is passed over. Stored names match fields
// ignoring case.
func (t *Table[U]) Restore(entries map[string]jsontext.Value, user func(id string) (U, bool), now time.Time) error {
	for id, raw := range entries {
		decoded, err := hex.DecodeString(id)
		if err != nil || len(decoded) != len(Key{}) {
			continue
		}
		var s stored
		if err := json.Unmarshal(raw, &s, json.MatchCaseInsensitiveNames(true)); err != nil {
			return fmt.Errorf("stored service token %s: %w", id, err)
		}
		key := Key(decoded)
		u, ok := user(s.User)
		if !ok || !now.Before(s.Expires) {
			t.dirty[key] = struct{}{}
			continue
		}
		token := &Token[U]{ID: s.ID, User: u, Name: s.Name, Created: s.Created, Expires: s.Expires, Manage: s.Manage}
		t.byKey[key] = token
		t.schedule(key, token)
	}
	return nil
}
