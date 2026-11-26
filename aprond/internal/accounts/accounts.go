// Package accounts keeps what names the server's users: the user_ids it
// assigns (PROTOCOL.md §3.2, §3.3), none of which is ever assigned again.
// The accounts by the identities that prove them are the users module's.
//
// IDs assigns user_ids, and keeps every user_id ever assigned in the
// store. It is not safe for concurrent use: the server calls it holding
// its lock.
package accounts

import (
	"encoding/json/jsontext"
	"fmt"
	"regexp"
	"strings"

	"github.com/apron-chat/apron-server-go/store"
)

const (
	// GuestPrefix starts every counter-assigned guest ID. The guest_
	// namespace, ignoring case, belongs to the counter: requests in it are
	// never honored, so guest numbers are taken only in sequence and the
	// latest one counts the guests the server has admitted.
	GuestPrefix = "guest_"
	// AccountPrefix starts the user_ids assigned to new accounts that did
	// not request one.
	AccountPrefix = "user_"
	// UsedKind is the kind of the store entries that hold the user_ids
	// ever assigned, lowercased.
	UsedKind = "used_id"
)

// requestableID is the shape of a user_id a user may request: the
// suggested convention for user-chosen IDs (Appendix A.3), letters, digits,
// _, ., and -, starting with a letter, so it never looks like a system
// identity or a log_id-derived room_id, and ending with a letter, digit, or
// _, so a mention of it ends where it does.
var requestableID = regexp.MustCompile(`^[A-Za-z](?:[A-Za-z0-9_.-]{0,62}[A-Za-z0-9_])?$`)

// Requestable reports whether a user may request id: it has the
// requestable shape and lies outside the counters' guest_ and user_
// namespaces.
func Requestable(id string) bool {
	for _, prefix := range []string{GuestPrefix, AccountPrefix} {
		if len(id) >= len(prefix) && strings.EqualFold(id[:len(prefix)], prefix) {
			return false
		}
	}
	return requestableID.MatchString(id)
}

// IDs assigns user_ids.
type IDs struct {
	// used holds every user_id ever assigned, lowercased, so none is
	// reissued (§3.3); granted the user_ids that roles are granted to,
	// lowercased, which are never assigned to a new identity unless used.
	used    map[string]bool
	granted map[string]bool
	// guests and accounts number the guest_<n> and user_<n> assigned.
	guests, accounts uint64
	// claimed are the user_ids used since Entries last took them.
	claimed []string
}

// NewIDs returns IDs that have assigned none.
func NewIDs() *IDs {
	return &IDs{used: make(map[string]bool), granted: make(map[string]bool)}
}

// Grant keeps id, which roles are granted to, for a user that has it
// already, or one that claims it with ClaimGranted.
func (ids *IDs) Grant(id string) { ids.granted[strings.ToLower(id)] = true }

// Claim marks id used and reports whether it was free: never assigned,
// ignoring case, not one that named reports, such as a room's name, and
// not one that roles are granted to.
func (ids *IDs) Claim(id string, named func(id string) bool) bool {
	if ids.granted[strings.ToLower(id)] {
		return false
	}
	return ids.ClaimGranted(id, named)
}

// ClaimGranted is Claim, but takes an id that roles are granted to, such
// as for a bot, which takes them.
func (ids *IDs) ClaimGranted(id string, named func(id string) bool) bool {
	key := strings.ToLower(id)
	if ids.used[key] || named(id) {
		return false
	}
	ids.used[key] = true
	ids.claimed = append(ids.claimed, key)
	return true
}

// Guest honors a requested user_id when it is requestable and free
// (Claim); otherwise it assigns the next unused guest_<n> (§3.2). Each call
// that does not honor a request takes exactly one counter value: with
// guest_ requests refused, only the counter assigns guest_<n>, so the skip
// over taken IDs is a safeguard.
func (ids *IDs) Guest(requested string, named func(id string) bool) string {
	return ids.assign(requested, named, GuestPrefix, &ids.guests)
}

// Account honors a requested user_id as Guest does, and otherwise assigns
// the next unused user_<n>.
func (ids *IDs) Account(requested string, named func(id string) bool) string {
	return ids.assign(requested, named, AccountPrefix, &ids.accounts)
}

func (ids *IDs) assign(requested string, named func(id string) bool, prefix string, counter *uint64) string {
	if Requestable(requested) && ids.Claim(requested, named) {
		return requested
	}
	for {
		*counter++
		if id := fmt.Sprintf("%s%d", prefix, *counter); ids.Claim(id, named) {
			return id
		}
	}
}

// Counters returns how many guest_<n> and user_<n> have been assigned,
// which the store keeps with the server's other counters.
func (ids *IDs) Counters() (guests, accounts uint64) { return ids.guests, ids.accounts }

// RestoreCounters takes the stored counters.
func (ids *IDs) RestoreCounters(guests, accounts uint64) { ids.guests, ids.accounts = guests, accounts }

// Entries returns the store entries of the user_ids used since it was
// last called.
func (ids *IDs) Entries() []store.Entry {
	if len(ids.claimed) == 0 {
		return nil
	}
	entries := make([]store.Entry, len(ids.claimed))
	for i, id := range ids.claimed {
		entries[i] = store.Entry{Kind: UsedKind, ID: id, Value: jsontext.Value("true")}
	}
	ids.claimed = nil
	return entries
}

// Snapshot returns the store entries of every user_id used.
func (ids *IDs) Snapshot() []store.Entry {
	ids.claimed = ids.claimed[:0]
	for id := range ids.used {
		ids.claimed = append(ids.claimed, id)
	}
	return ids.Entries()
}

// Restore takes the stored user_ids used, by their entries' IDs.
func (ids *IDs) Restore(entries map[string]jsontext.Value) {
	for id := range entries {
		ids.used[id] = true
	}
}
