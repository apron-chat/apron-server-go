// Package invitations keeps the outstanding invitations to make an account,
// and what an invitation leaves with the account it made. A client
// presents an invitation's token with a sign-in that can make an account,
// such as a passkey registration on a connection that is not signed in;
// the server makes the account and uses the invitation up in one change. A
// Table keeps each invitation until it is used, revoked, or expires, bounds
// how many are outstanding, and keeps them in the store, by the SHA-256 of
// the token, never the token itself.
//
// A Table is not safe for concurrent use: the server calls it holding its
// lock.
package invitations

import (
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/apron-chat/apron-server-go/store"
	"github.com/apron-chat/apron-server-go/wire"
)

const (
	// Max bounds the invitations outstanding at once.
	Max = 1000
	// MaxRooms bounds the rooms one invitation joins.
	MaxRooms = 32
	// TokenPrefix starts every invitation token, so that one is
	// recognizable where it leaks.
	TokenPrefix = "apron_invite_"
	// EntryKind is the kind of the store entries that hold invitations,
	// named by the hex SHA-256 of their tokens.
	EntryKind = "invitation"
)

// Key is the SHA-256 of a token, which an invitation is kept under.
type Key = [32]byte

// Invitation is an outstanding invitation. It encodes as the store keeps
// it.
type Invitation struct {
	// ID names the invitation, to revoke it; it is not its token.
	ID string `json:"id"`
	// By names who made it: an extension, or "" for the program that
	// embeds the server.
	By      string    `json:"by,omitzero"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
	// Name and UserID are what the account is named and asks for, unless
	// the sign-in asks for others.
	Name   string `json:"name,omitzero"`
	UserID string `json:"user_id,omitzero"`
	// Rooms are joined after the default room, and Roles granted.
	Rooms []string `json:"rooms,omitzero"`
	Roles []string `json:"roles,omitzero"`
	// Data is left with the account, as JSON.
	Data jsontext.Value `json:"data,omitzero"`
}

// Enrollment is what an invitation left with the account it made, kept
// with the account, so that it is stored in the same change. It encodes as
// the store keeps it, within the account's entry.
type Enrollment struct {
	// Invitation is the ID of the invitation, and By who made it.
	Invitation string         `json:"invitation"`
	By         string         `json:"by,omitzero"`
	At         time.Time      `json:"at"`
	Data       jsontext.Value `json:"data,omitzero"`
	// Admitted reports that the invitation still admits the account to a
	// server whose sign-in is restricted.
	Admitted bool `json:"admitted,omitzero"`
}

// Enroll is the enrollment inv leaves with an account it makes at now.
func (inv *Invitation) Enroll(now time.Time) *Enrollment {
	return &Enrollment{Invitation: inv.ID, By: inv.By, At: now, Data: inv.Data, Admitted: true}
}

// Table is the outstanding invitations by key.
type Table struct {
	byKey map[Key]*Invitation
	// dirty are the keys added or deleted since Entries last took them.
	dirty map[Key]struct{}
}

// New returns an empty table.
func New() *Table {
	return &Table{byKey: make(map[Key]*Invitation), dirty: make(map[Key]struct{})}
}

// NewToken makes a new invitation token.
func NewToken() string { return TokenPrefix + rand.Text() }

// NewID makes a new invitation ID.
func NewID() string { return "inv_" + strings.ToLower(rand.Text()[:16]) }

// Add keeps inv under token, after forgetting the invitations expired at
// now, unless Max are outstanding, which it reports.
func (t *Table) Add(token string, inv *Invitation, now time.Time) bool {
	t.Prune(now)
	if len(t.byKey) >= Max {
		return false
	}
	key := sha256.Sum256([]byte(token))
	t.byKey[key] = inv
	t.dirty[key] = struct{}{}
	return true
}

// Presented returns the invitation token names, or nil when there is none,
// or it expired at now.
func (t *Table) Presented(token string, now time.Time) *Invitation {
	inv := t.byKey[sha256.Sum256([]byte(token))]
	if inv == nil || !now.Before(inv.Expires) {
		return nil
	}
	return inv
}

// Use forgets the invitation token names, which made its account.
func (t *Table) Use(token string) {
	t.delete(sha256.Sum256([]byte(token)))
}

// Revoke forgets the invitation named id, and reports whether there was
// one.
func (t *Table) Revoke(id string) bool {
	found := false
	for key, inv := range t.byKey {
		if inv.ID == id {
			t.delete(key)
			found = true
		}
	}
	return found
}

// List lists the invitations outstanding at now, oldest first.
func (t *Table) List(now time.Time) []*Invitation {
	var list []*Invitation
	for _, inv := range t.byKey {
		if now.Before(inv.Expires) {
			list = append(list, inv)
		}
	}
	slices.SortFunc(list, func(a, b *Invitation) int {
		return cmp.Or(a.Created.Compare(b.Created), cmp.Compare(a.ID, b.ID))
	})
	return list
}

// Prune forgets the invitations expired at now.
func (t *Table) Prune(now time.Time) {
	for key, inv := range t.byKey {
		if !now.Before(inv.Expires) {
			t.delete(key)
		}
	}
}

func (t *Table) delete(key Key) {
	delete(t.byKey, key)
	t.dirty[key] = struct{}{}
}

// Entries returns the store entries of the invitations added or deleted
// since it was last called.
func (t *Table) Entries() []store.Entry {
	if len(t.dirty) == 0 {
		return nil
	}
	entries := make([]store.Entry, 0, len(t.dirty))
	for key := range t.dirty {
		entry := store.Entry{Kind: EntryKind, ID: hex.EncodeToString(key[:])}
		if inv := t.byKey[key]; inv != nil {
			entry.Value, _ = wire.Marshal(inv)
		}
		entries = append(entries, entry)
	}
	clear(t.dirty)
	return entries
}

// Snapshot returns the store entries of every invitation, as Entries does
// those that changed.
func (t *Table) Snapshot() []store.Entry {
	for key := range t.byKey {
		t.dirty[key] = struct{}{}
	}
	return t.Entries()
}

// Restore takes the stored invitations that have not expired at now, and
// deletes the others from the store; an entry not named by a key is passed
// over. Stored names match fields ignoring case.
func (t *Table) Restore(entries map[string]jsontext.Value, now time.Time) error {
	for id, raw := range entries {
		decoded, err := hex.DecodeString(id)
		if err != nil || len(decoded) != len(Key{}) {
			continue
		}
		var inv Invitation
		if err := json.Unmarshal(raw, &inv, json.MatchCaseInsensitiveNames(true)); err != nil {
			return fmt.Errorf("stored invitation %s: %w", id, err)
		}
		key := Key(decoded)
		if !now.Before(inv.Expires) {
			t.dirty[key] = struct{}{}
			continue
		}
		t.byKey[key] = &inv
	}
	return nil
}
