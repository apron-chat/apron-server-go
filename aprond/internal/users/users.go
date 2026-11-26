// Package users keeps what the server keeps of each user across its
// connections and restarts (PROTOCOL.md §3.3): the profile, the identities
// that prove an account and what each sign-in scheme keeps with them, the
// roles they grant, the status (§4.5) and mutes, the rooms left and the
// rooms that count toward unread (§4.9), and what made the account: an
// extension's bot, an invitation, the rooms a scope limits it to, and the
// apps it allowed. A Table finds users by user_id and accounts by the
// identities that prove them, tracks the users changed, and keeps them in
// the store, guests too, which the server retires at its next start. It
// also ends its users' timed mutes, and publishes their coalesced
// statuses, on timers that call the server back (Timers).
//
// A Table knows its users only as values of the type U, such as the
// server's user state, which holds a User with the user's live links:
// connections and joined rooms. It is not safe for concurrent use: the
// server calls it holding its lock.
package users

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"iter"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/aprond/internal/invitations"
	"github.com/apron-chat/apron-server-go/aprond/internal/presence"
	"github.com/apron-chat/apron-server-go/aprond/internal/rooms"
	"github.com/apron-chat/apron-server-go/internal/auth/email"
	"github.com/apron-chat/apron-server-go/internal/auth/external"
	"github.com/apron-chat/apron-server-go/internal/auth/passkey"
	"github.com/apron-chat/apron-server-go/internal/authn"
	"github.com/apron-chat/apron-server-go/store"
)

// EntryKind is the kind of the store entries that hold users, named by
// their user_ids.
const EntryKind = "user"

// User is what the server keeps of a user.
type User struct {
	Name   string
	Avatar string
	Ext    apron.Ext
	// Identities are what prove the account, by scheme: its email address,
	// its passkeys' user handle, its external sign-in. A guest has none.
	// Records are what each scheme keeps with the account, such as passkey
	// credentials.
	Identities map[string]string
	Records    map[string]jsontext.Value
	// Granted are the roles the account's identities grant it, by scheme,
	// as of each identity's latest proof (authn.Proof.Roles), such as an
	// external sign-in's (§4.12).
	Granted map[string][]string
	// Created is when the user was made; zero for users stored before it
	// was kept.
	Created time.Time
	// Bot names the extension whose bot the account is, which holds the
	// "bot" role (§3.3.4).
	Bot string
	// Enrolled is what the invitation that made the account left with it,
	// nil if none did.
	Enrolled *invitations.Enrollment
	// Scope limits the rooms the account may see, nil for none.
	Scope *rooms.Scope
	// Consents are the https redirect URIs of apps the account allowed on
	// the external sign-in page, with when, in Unix milliseconds.
	Consents map[string]int64
	// LeftAt maps each room the user left, and has not rejoined, to the
	// log_id of the leave, for room_list's `left` (§4.2.1); Pings maps the
	// rooms they have not joined where a message mentioned or replied to
	// them to the log_id of the first such message, for unread counts
	// (§4.9).
	LeftAt map[string]int64
	Pings  map[string]int64
	// Presence is the status: the chosen status (§4.5.1), and the shown
	// status as published, with its seen (§4.5.2).
	Presence presence.State
	// Mute and RoomMutes are the user's mutes, everywhere and of rooms by
	// room_id (§4.5).
	Mute      Mute
	RoomMutes map[string]Mute
}

// New returns a new user named name, made at now.
func New(name string, now time.Time) User {
	return User{
		Name:      name,
		Created:   now,
		LeftAt:    make(map[string]int64),
		Pings:     make(map[string]int64),
		Presence:  presence.New(),
		RoomMutes: make(map[string]Mute),
	}
}

// Account reports whether the user signed in with a credential (a passkey,
// an email address, or an external identity) or is an extension's bot, so
// it persists across connections and restarts, rather than being a guest.
func (u *User) Account() bool {
	return len(u.Identities) > 0 || u.Bot != ""
}

// Mute is a mute (§4.5): until a time, forever, or, when zero, none.
type Mute struct {
	Forever bool
	Until   time.Time
}

// Active reports whether the mute is in effect at now.
func (m Mute) Active(now time.Time) bool {
	return m.Forever || now.Before(m.Until)
}

// Holder is the type of the values a Table keeps: a user's state, which
// holds its User.
type Holder interface {
	comparable
	// UserID is the user's user_id, and Kept its User.
	UserID() string
	Kept() *User
}

// Table is the users by user_id, and the accounts by identity.
type Table[U Holder] struct {
	byID       map[string]U
	byIdentity map[authn.Identity]U
	// dirty are the user_ids of the users added, changed, or deleted since
	// Entries last took them.
	dirty  map[string]struct{}
	timers timers[U]
}

// NewTable returns an empty table, which calls back timers when a user's
// timer fires.
func NewTable[U Holder](callbacks Timers[U]) *Table[U] {
	return &Table[U]{
		byID: make(map[string]U), byIdentity: make(map[authn.Identity]U), dirty: make(map[string]struct{}),
		timers: timers[U]{Timers: callbacks, mutes: make(map[string]map[string]*time.Timer), held: make(map[string]*time.Timer)},
	}
}

// Get returns the user with user_id id, or the zero U.
func (t *Table[U]) Get(id string) U { return t.byID[id] }

// Len counts the users.
func (t *Table[U]) Len() int { return len(t.byID) }

// All lists the users by user_id, in no order.
func (t *Table[U]) All() iter.Seq2[string, U] { return maps.All(t.byID) }

// IDs lists the users' user_ids, sorted.
func (t *Table[U]) IDs() []string { return slices.Sorted(maps.Keys(t.byID)) }

// Add keeps u, a new user, and its identities.
func (t *Table[U]) Add(u U) {
	id := u.UserID()
	t.byID[id] = u
	for scheme, subject := range u.Kept().Identities {
		t.byIdentity[authn.Identity{Scheme: scheme, Subject: subject}] = u
	}
	t.Touch(id)
}

// Delete forgets the user with user_id id, stops its timers, and deletes it
// from the store.
func (t *Table[U]) Delete(id string) {
	t.cancel(id)
	if u, ok := t.byID[id]; ok {
		for scheme, subject := range u.Kept().Identities {
			if identity := (authn.Identity{Scheme: scheme, Subject: subject}); t.byIdentity[identity] == u {
				delete(t.byIdentity, identity)
			}
		}
	}
	delete(t.byID, id)
	t.Touch(id)
}

// Touch marks the user with user_id id changed, for the store.
func (t *Table[U]) Touch(id string) { t.dirty[id] = struct{}{} }

// ByIdentity returns the account identity proves, or the zero U.
func (t *Table[U]) ByIdentity(identity authn.Identity) U { return t.byIdentity[identity] }

// Identities counts the identities that prove accounts.
func (t *Table[U]) Identities() int { return len(t.byIdentity) }

// SetIdentity makes identity prove u, its account's identity of its scheme,
// with the scheme's record, if not nil.
func (t *Table[U]) SetIdentity(u U, identity authn.Identity, record jsontext.Value) {
	kept := u.Kept()
	if kept.Identities == nil {
		kept.Identities = make(map[string]string)
	}
	kept.Identities[identity.Scheme] = identity.Subject
	if record != nil {
		t.SetRecord(u, identity.Scheme, record)
	}
	t.byIdentity[identity] = u
	t.Touch(u.UserID())
}

// SetRecord sets what scheme keeps with u.
func (t *Table[U]) SetRecord(u U, scheme string, record jsontext.Value) {
	kept := u.Kept()
	if kept.Records == nil {
		kept.Records = make(map[string]jsontext.Value)
	}
	kept.Records[scheme] = record
	t.Touch(u.UserID())
}

// DeleteIdentity forgets u's identity of scheme, and what the scheme kept
// with it.
func (t *Table[U]) DeleteIdentity(u U, scheme string) {
	kept := u.Kept()
	if subject, ok := kept.Identities[scheme]; ok {
		if identity := (authn.Identity{Scheme: scheme, Subject: subject}); t.byIdentity[identity] == u {
			delete(t.byIdentity, identity)
		}
	}
	delete(kept.Identities, scheme)
	delete(kept.Records, scheme)
	t.Touch(u.UserID())
}

// Stored is a user as the store keeps it. A Table reads and writes it;
// it is exported for tests that check what a server stored.
type Stored struct {
	Name        string           `json:"name,omitzero"`
	Avatar      string           `json:"avatar,omitzero"`
	Ext         apron.Ext        `json:"ext,omitzero"`
	AvatarEmbed string           `json:"avatar_embed,omitzero"`
	LeftAt      map[string]int64 `json:"left_at,omitzero"`
	// Identities are what prove the account, by scheme; Records what each
	// scheme keeps with it; and Grants the roles each identity grants it.
	Identities map[string]string         `json:"identities,omitzero"`
	Records    map[string]jsontext.Value `json:"records,omitzero"`
	Grants     map[string][]string       `json:"grants,omitzero"`
	// Passkey, Email, External, and Granted are the identities, and the
	// external sign-in's roles, as servers stored them before Identities:
	// they are stored still, so such a server can read what this one
	// stored, and read where Identities lacks their scheme.
	Passkey jsontext.Value `json:"passkey,omitzero"`
	Email   string         `json:"email,omitzero"`
	// Status is the chosen status (§4.5.1), absent for online; Seen the
	// Unix milliseconds when the shown status others are sent last became
	// offline, absent if unknown, as for users stored before it was kept
	// (§4.5.2); Shown true while that status is other than offline, which a
	// restart after a crash finds still set; and Mute and RoomMutes their
	// mutes.
	// Pings are the rooms they have not joined that count toward unread.
	Status    *string               `json:"status,omitzero"`
	Seen      int64                 `json:"seen,omitzero"`
	Shown     bool                  `json:"shown,omitzero"`
	Mute      *storedMute           `json:"mute,omitzero"`
	RoomMutes map[string]storedMute `json:"room_mutes,omitzero"`
	Pings     map[string]int64      `json:"pings,omitzero"`
	// Created is when the user was made, in Unix milliseconds; absent for
	// users stored before it was kept. Bot names the extension whose bot
	// the account is.
	Created int64  `json:"created,omitzero"`
	Bot     string `json:"bot,omitzero"`
	// Enrolled is what the invitation that made the account left with it.
	Enrolled *invitations.Enrollment `json:"enrolled,omitzero"`
	// Scope is the rooms the account is limited to.
	Scope *storedScope `json:"scope,omitzero"`
	// Consents are the hosts of apps the account allowed on the external
	// sign-in page, with when, in Unix milliseconds.
	Consents map[string]int64 `json:"consents,omitzero"`
	External string           `json:"external,omitzero"`
	Granted  []string         `json:"granted,omitzero"`
}

type storedMute struct {
	Forever bool      `json:"forever,omitzero"`
	Until   time.Time `json:"until,omitzero"`
}

type storedScope struct {
	Rooms   []string `json:"rooms"`
	Threads bool     `json:"threads,omitzero"`
}

// storedOf is u as the store keeps it at now, with the ID of its avatar's
// hosted embed, if any: mutes that ran out are left out.
func storedOf(u *User, avatarEmbed string, now time.Time) Stored {
	s := Stored{
		Name: u.Name, Avatar: u.Avatar, Ext: u.Ext, AvatarEmbed: avatarEmbed, LeftAt: u.LeftAt, Pings: u.Pings,
		Seen: u.Presence.Seen, Shown: u.Presence.Status != presence.Offline,
		Bot: u.Bot, Identities: u.Identities, Records: u.Records, Grants: u.Granted,
		Email: u.Identities[email.Name], External: u.Identities[external.Name],
		Passkey: u.Records[passkey.Name], Granted: u.Granted[external.Name],
		Enrolled: u.Enrolled, Consents: u.Consents,
	}
	if u.Scope != nil {
		s.Scope = &storedScope{Rooms: u.Scope.Rooms(), Threads: u.Scope.Threads()}
	}
	if !u.Created.IsZero() {
		s.Created = u.Created.UnixMilli()
	}
	if status := u.Presence.Chosen; status != presence.Online {
		s.Status = &status
	}
	if u.Mute.Active(now) {
		s.Mute = &storedMute{Forever: u.Mute.Forever, Until: u.Mute.Until}
	}
	for id, mute := range u.RoomMutes {
		if mute.Active(now) {
			if s.RoomMutes == nil {
				s.RoomMutes = make(map[string]storedMute)
			}
			s.RoomMutes[id] = storedMute{Forever: mute.Forever, Until: mute.Until}
		}
	}
	return s
}

// Entries returns the store entries of the users added, changed, or
// deleted since it was last called, naming each one's avatar embed by
// avatarEmbed, "" for none.
func (t *Table[U]) Entries(avatarEmbed func(U) string) []store.Entry {
	if len(t.dirty) == 0 {
		return nil
	}
	now := time.Now()
	entries := make([]store.Entry, 0, len(t.dirty))
	for id := range t.dirty {
		entry := store.Entry{Kind: EntryKind, ID: id}
		if u, ok := t.byID[id]; ok {
			value, err := apron.Marshal(storedOf(u.Kept(), avatarEmbed(u), now))
			if err != nil {
				slog.Error("cannot encode state for the store", "kind", EntryKind, "id", id, "error", err)
				continue
			}
			entry.Value = value
		}
		entries = append(entries, entry)
	}
	clear(t.dirty)
	return entries
}

// Snapshot returns the store entries of every user, as Entries does those
// that changed.
func (t *Table[U]) Snapshot(avatarEmbed func(U) string) []store.Entry {
	for id := range t.byID {
		t.dirty[id] = struct{}{}
	}
	return t.Entries(avatarEmbed)
}

// Restore takes the stored users, as user makes each one's state from its
// User, and returns the IDs of the hosted embeds behind their avatars, by
// user_id. A restored user is not connected: its status is restored as
// such (presence.State.Restore), at now, which stores it again if the
// server stopped while it was connected. A user stored by a server from
// before Identities has its email address, passkeys, external identity,
// and the external sign-in's roles in fields of their own, and is stored
// again with them as identities. A status this server does not support is
// restored as "", as when it is chosen.
func (t *Table[U]) Restore(entries map[string]jsontext.Value, user func(id string, stored User) U, now time.Time) (avatarEmbeds map[string]string, err error) {
	avatarEmbeds = make(map[string]string)
	for id, raw := range entries {
		var s Stored
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("stored %s %q: %w", EntryKind, id, err)
		}
		restored := New(s.Name, time.Time{})
		restored.Avatar, restored.Ext, restored.Bot = s.Avatar, s.Ext, s.Bot
		if s.Created != 0 {
			restored.Created = time.UnixMilli(s.Created)
		}
		migrated, err := restoreIdentities(&restored, s)
		if err != nil {
			return nil, fmt.Errorf("stored %s %q: %w", EntryKind, id, err)
		}
		if s.Scope != nil {
			restored.Scope = rooms.NewScope(s.Scope.Rooms, s.Scope.Threads)
		}
		restored.Enrolled, restored.Consents = s.Enrolled, s.Consents
		if s.LeftAt != nil {
			restored.LeftAt = s.LeftAt
		}
		if s.Pings != nil {
			restored.Pings = s.Pings
		}
		switch {
		case s.Status != nil && presence.Settable(*s.Status):
			restored.Presence.Chosen = *s.Status
		case s.Status != nil:
			restored.Presence.Chosen = presence.None
		}
		restored.Presence.Seen = s.Seen
		if s.Mute != nil {
			restored.Mute = Mute{Forever: s.Mute.Forever, Until: s.Mute.Until}
		}
		for room, mute := range s.RoomMutes {
			restored.RoomMutes[room] = Mute{Forever: mute.Forever, Until: mute.Until}
		}
		u := user(id, restored)
		t.byID[id] = u
		for scheme, subject := range u.Kept().Identities {
			t.byIdentity[authn.Identity{Scheme: scheme, Subject: subject}] = u
		}
		if u.Kept().Presence.Restore(s.Shown, now) || migrated {
			t.Touch(id)
		}
		if s.AvatarEmbed != "" {
			avatarEmbeds[id] = s.AvatarEmbed
		}
	}
	return avatarEmbeds, nil
}

// restoreIdentities gives a stored user its identities, their records, the
// roles they grant, and those stored in the fields of servers from before
// Identities, and reports whether it took any from those, which are
// stored again as identities.
func restoreIdentities(u *User, s Stored) (migrated bool, err error) {
	u.Identities, u.Records, u.Granted = maps.Clone(s.Identities), maps.Clone(s.Records), maps.Clone(s.Grants)
	legacy := map[string]string{email.Name: s.Email, external.Name: s.External}
	if len(s.Passkey) > 0 && u.Identities[passkey.Name] == "" {
		var old struct {
			Handle []byte `json:"handle"`
		}
		if err := json.Unmarshal(s.Passkey, &old); err != nil {
			return false, err
		}
		legacy[passkey.Name] = passkey.Subject(old.Handle)
		if u.Records == nil {
			u.Records = make(map[string]jsontext.Value)
		}
		u.Records[passkey.Name] = s.Passkey
	}
	for scheme, subject := range legacy {
		if subject == "" || u.Identities[scheme] != "" {
			continue
		}
		if u.Identities == nil {
			u.Identities = make(map[string]string)
		}
		u.Identities[scheme] = subject
		migrated = true
	}
	if len(s.Granted) > 0 && s.Grants == nil {
		u.Granted = map[string][]string{external.Name: s.Granted}
		migrated = true
	}
	return migrated, nil
}
