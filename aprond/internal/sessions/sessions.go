// Package sessions keeps the bearer tokens that resume an account's
// sign-in (PROTOCOL.md §3.2): each token's account, the frontend origin it
// is bound to, or none for a token from a code (§4.12.4), when it expires,
// and the credential the sign-in that made it proved. It issues tokens,
// bounds an account's, forgets expired ones, and keeps them in the store,
// by the SHA-256 of the token, never the token itself.
//
// A Table knows its accounts only as values of the type U, such as the
// server's user state, and their user_ids when it writes them. It is not
// safe for concurrent use: the server calls it holding its lock.
package sessions

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
	"slices"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/store"
)

const (
	// Lifetime is how long a session lasts from its sign-in, or its latest
	// resume, which renews it.
	Lifetime = 30 * 24 * time.Hour
	// MaxPerUser bounds the sessions kept for one account: far more devices
	// and browsers than one person signs in from.
	MaxPerUser = 100
	// EntryKind is the kind of the store entries that hold sessions, named
	// by the hex SHA-256 of their tokens.
	EntryKind = "session"
)

// Key is the SHA-256 of a token, which a session is kept under.
type Key = [32]byte

// Session is a bearer token's sign-in.
type Session[U comparable] struct {
	// User is the account it resumes.
	User U
	// Origin is the frontend origin it is bound to, unless AnyOrigin.
	Origin    string
	AnyOrigin bool
	Expires   time.Time
	// Credential names the credential the sign-in that made it proved, as
	// scheme:id, if any.
	Credential string
}

// Table is the sessions by key.
type Table[U comparable] struct {
	byKey map[Key]Session[U]
	// dirty are the keys put or deleted since Entries last took them.
	dirty map[Key]struct{}
}

// New returns an empty table.
func New[U comparable]() *Table[U] {
	return &Table[U]{byKey: make(map[Key]Session[U]), dirty: make(map[Key]struct{})}
}

// Get returns the session kept under key.
func (t *Table[U]) Get(key Key) (Session[U], bool) {
	s, ok := t.byKey[key]
	return s, ok
}

// Put keeps s under key.
func (t *Table[U]) Put(key Key, s Session[U]) {
	t.byKey[key] = s
	t.dirty[key] = struct{}{}
}

// Delete forgets the session under key, and deletes it from the store, as
// one that may be stored though it is not kept.
func (t *Table[U]) Delete(key Key) {
	delete(t.byKey, key)
	t.dirty[key] = struct{}{}
}

// All lists the sessions, in no order. The loop may Delete them.
func (t *Table[U]) All() iter.Seq2[Key, Session[U]] {
	return maps.All(t.byKey)
}

// Len counts the sessions.
func (t *Table[U]) Len() int { return len(t.byKey) }

// Issue makes a token that resumes user's sign-in from origin, or with
// anyOrigin from any origin or none (§3.2.1), for Lifetime from now, and
// returns it with the key it is kept under. The user's other sessions past
// MaxPerUser are forgotten (Cap).
func (t *Table[U]) Issue(user U, origin string, anyOrigin bool, now time.Time) (string, Key, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", Key{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(secret)
	key := sha256.Sum256([]byte(token))
	if anyOrigin {
		origin = ""
	}
	t.Put(key, Session[U]{User: user, Origin: origin, AnyOrigin: anyOrigin, Expires: now.Add(Lifetime)})
	t.Cap(user, key)
	return token, key, nil
}

// Cap forgets user's sessions beyond MaxPerUser, but never keep, the one
// just made: the others that expire soonest, which were used least
// recently, go first. Every sign-in adds a session, so an account signing
// in again and again in a loop cannot grow the sessions, and the store,
// without bound.
func (t *Table[U]) Cap(user U, keep Key) {
	var keys []Key
	for key, s := range t.byKey {
		if s.User == user && key != keep {
			keys = append(keys, key)
		}
	}
	excess := len(keys) - (MaxPerUser - 1)
	if excess <= 0 {
		return
	}
	slices.SortFunc(keys, func(a, b Key) int {
		return t.byKey[a].Expires.Compare(t.byKey[b].Expires)
	})
	for _, key := range keys[:excess] {
		t.Delete(key)
	}
}

// Prune forgets the sessions expired at now.
func (t *Table[U]) Prune(now time.Time) {
	for key, s := range t.byKey {
		if !now.Before(s.Expires) {
			t.Delete(key)
		}
	}
}

// Snapshot returns the store entries of every session, as Entries does
// those that changed.
func (t *Table[U]) Snapshot(userID func(U) string) []store.Entry {
	for key := range t.byKey {
		t.dirty[key] = struct{}{}
	}
	return t.Entries(userID)
}

// ID names a key in the store: its hex.
func ID(key Key) string { return hex.EncodeToString(key[:]) }

// stored is a session as the store keeps it.
type stored struct {
	User       string    `json:"user"`
	Origin     string    `json:"origin"`
	AnyOrigin  bool      `json:"any_origin,omitzero"`
	Expires    time.Time `json:"expires"`
	Credential string    `json:"credential,omitzero"`
}

// Entries returns the store entries of the sessions put or deleted since
// it was last called, naming each session's account by userID.
func (t *Table[U]) Entries(userID func(U) string) []store.Entry {
	if len(t.dirty) == 0 {
		return nil
	}
	entries := make([]store.Entry, 0, len(t.dirty))
	for key := range t.dirty {
		entry := store.Entry{Kind: EntryKind, ID: ID(key)}
		if s, ok := t.byKey[key]; ok {
			entry.Value, _ = apron.Marshal(stored{User: userID(s.User), Origin: s.Origin, AnyOrigin: s.AnyOrigin, Expires: s.Expires, Credential: s.Credential})
		}
		entries = append(entries, entry)
	}
	clear(t.dirty)
	return entries
}

// Restore takes the stored sessions. One whose account user does not
// return, or that expired at now, is deleted from the store; an entry not
// named by a key is passed over.
func (t *Table[U]) Restore(entries map[string]jsontext.Value, user func(id string) (U, bool), now time.Time) error {
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
		if u, ok := user(s.User); ok && now.Before(s.Expires) {
			t.byKey[key] = Session[U]{User: u, Origin: s.Origin, AnyOrigin: s.AnyOrigin, Expires: s.Expires, Credential: s.Credential}
		} else {
			t.Delete(key)
		}
	}
	return nil
}
