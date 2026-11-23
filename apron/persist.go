package apron

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/apron-chat/apron-server-go/apron/internal/accounts"
	"github.com/apron-chat/apron-server-go/apron/internal/devices"
	"github.com/apron-chat/apron-server-go/apron/internal/embeds"
	"github.com/apron-chat/apron-server-go/apron/internal/invitations"
	"github.com/apron-chat/apron-server-go/apron/internal/push"
	"github.com/apron-chat/apron-server-go/apron/internal/roomlog"
	"github.com/apron-chat/apron-server-go/apron/internal/rooms"
	"github.com/apron-chat/apron-server-go/apron/internal/services"
	"github.com/apron-chat/apron-server-go/apron/internal/sessions"
	"github.com/apron-chat/apron-server-go/apron/internal/users"
	"github.com/apron-chat/apron-server-go/apron/internal/webpush"
	"github.com/apron-chat/apron-server-go/store"
)

// Persistence writes the server's state through to a store.Store. Every
// change made under s.mu marks what it changed: the server's own state in
// its dirty set (touch*), and the modules that keep their own, the log
// (roomlog), the rooms (rooms), the user_ids used (accounts), the users
// (users), the push registrations (push), the extensions' values (kv),
// the sessions (sessions), the device tokens (devices), the service tokens
// (services), the invitations (invitations), the hosted embeds (embeds),
// and the custom emoji (emoji), in theirs.
// When the lock is released, unlock renders the marked state into one
// batch, and the store writer (durable) applies batches to the store in
// order, off the lock. At start, Open rebuilds the state from the store.
//
// Entry kinds and IDs:
//
//	meta     "counters"   log_id, guest, embed, and upload sequences
//	meta     "vapid"      the webpush VAPID private key (§4.9)
//	meta     "emoji"      the custom emoji catalog (ext:emoji, emoji)
//	meta     "profile"    the server's title, description, and avatar set
//	                      with /server (§3.1)
//	used_id  lowercased user_id ever assigned (accounts)
//	record   log_id       a logged record and the rooms whose logs hold it
//	                      (roomlog)
//	room     room_id      a room, its members, and their read cursors
//	                      (rooms)
//	message  message_id   a message's room, sender, snapshots, and
//	                      reactions (rooms)
//	user     user_id      a user, guests too, retired at the next start
//	                      (users)
//	session  hex SHA-256 of the bearer token (sessions)
//	device   hex SHA-256 of the /invite-device token (devices)
//	service  hex SHA-256 of a bot's service token (services)
//	invitation  hex SHA-256 of an invitation's token (invitations)
//	embed    embed_id (embeds)
//	push     user_id, a space, and url (url alone before registrations
//	         belonged to their user) (push)
//	ext/<n>  a key        a value Extension <n> keeps (Host.Store, kv)
const (
	entryMeta    = "meta"
	entryRoom    = rooms.EntryKind
	entryMessage = rooms.MessageEntryKind
	entryUser    = users.EntryKind
	entryPush    = push.EntryKind
)

// dirtySet is the state changed since the last batch, by key.
type dirtySet struct {
	messages map[string]bool
	vapid    bool
	profile  bool
}

func newDirtySet() dirtySet {
	return dirtySet{
		messages: make(map[string]bool),
	}
}

func (s *Server) touchMessage(m *messageState) { s.dirty.messages[m.ID()] = true }

// unlock writes the changes made under s.mu through to the store, then
// releases the lock.
func (s *Server) unlock() {
	s.flushLocked()
	s.mu.Unlock()
}

type storedMeta struct {
	LastID        int64  `json:"last_id"`
	GuestNumber   uint64 `json:"guest_number"`
	AccountNumber uint64 `json:"account_number,omitzero"`
	EmbedNumber   uint64 `json:"embed_number"`
	UploadSeq     int64  `json:"upload_seq"`
}

// metaLocked is the counters entry of the current state.
func (s *Server) metaLocked() storedMeta {
	guests, accounts := s.ids.Counters()
	return storedMeta{LastID: s.log.LastIssued(), GuestNumber: guests, AccountNumber: accounts, EmbedNumber: s.embedNumber, UploadSeq: s.uploadSeq}
}

func sessionID(key [32]byte) string { return sessions.ID(key) }

// userID names a user in what the store keeps.
func userID(u *userState) string { return u.id }

// flushLocked queues the marked changes as one batch for the store writer,
// with the fences waiting for it.
func (s *Server) flushLocked() {
	if s.store.Stopped() {
		s.dirty = newDirtySet()
		s.log.Entries()
		s.users.Entries(avatarEmbedID)
		s.pushes.Entries()
		s.rooms.Entries()
		s.ids.Entries()
		s.extData.Entries()
		s.sessions.Entries(userID)
		s.devices.Entries(userID)
		s.services.Entries(userID)
		s.invitations.Entries()
		s.emoji.Entries()
		s.embeds.Entries(avatarOwner)
		s.store.Queue(nil)
		return
	}
	batch := append(s.ids.Entries(), s.entriesLocked(s.dirty, s.log.Entries())...)
	batch = append(batch, s.users.Entries(avatarEmbedID)...)
	batch = append(batch, s.pushes.Entries()...)
	batch = append(batch, s.rooms.Entries()...)
	batch = append(batch, s.extData.Entries()...)
	batch = append(batch, s.sessions.Entries(userID)...)
	batch = append(batch, s.devices.Entries(userID)...)
	batch = append(batch, s.services.Entries(userID)...)
	batch = append(batch, s.invitations.Entries()...)
	batch = append(batch, s.emoji.Entries()...)
	batch = append(batch, s.embeds.Entries(avatarOwner)...)
	meta := s.metaLocked()
	if meta != s.storedMeta {
		s.storedMeta = meta
		batch = append(batch, store.Entry{Kind: entryMeta, ID: "counters", Value: encodeJSON(meta)})
	}
	s.dirty = newDirtySet()
	s.store.Queue(batch)
}

// entriesLocked renders the state named by a dirty set as store entries,
// with records', those of the log.
func (s *Server) entriesLocked(d dirtySet, records []store.Entry) []store.Entry {
	var batch []store.Entry
	put := func(kind, id string, value any) {
		encoded := encodeJSON(value)
		if encoded == nil {
			slog.Error("cannot encode state for the store", "kind", kind, "id", id)
			return
		}
		batch = append(batch, store.Entry{Kind: kind, ID: id, Value: encoded})
	}
	batch = append(batch, records...)
	for id := range d.messages {
		if m := s.messages[id]; m != nil {
			batch = append(batch, rooms.MessageEntry(m.Message))
		}
	}
	if d.vapid && s.vapidStored {
		put(entryMeta, "vapid", s.vapid.Encoded())
	}
	if d.profile {
		put(entryMeta, "profile", s.storedProfileLocked())
	}
	return batch
}

// avatarEmbedID names the hosted embed behind a user's avatar in what the
// store keeps, "" for none.
func avatarEmbedID(u *userState) string {
	if u.avatarEmbed == nil {
		return ""
	}
	return u.avatarEmbed.ID
}

// closeStoreLocked writes the last changes, stops the store writer, and
// closes the store.
func (s *Server) closeStoreLocked() {
	if s.store.Stopped() {
		return
	}
	s.flushLocked()
	s.store.Stop()
	if err := s.config.Store.Close(); err != nil {
		slog.Error("cannot close the store", "error", err)
	}
}

// restoreLocked rebuilds the server's state from the store's entries. It
// returns the upload files the restored embeds hold.
func (s *Server) restoreLocked() (map[string]bool, error) {
	entries := make(map[string]map[string]jsontext.Value)
	err := s.config.Store.Load(func(e store.Entry) error {
		if entries[e.Kind] == nil {
			entries[e.Kind] = make(map[string]jsontext.Value)
		}
		entries[e.Kind][e.ID] = e.Value
		return nil
	})
	if err != nil {
		return nil, err
	}
	decode := func(kind, id string, raw jsontext.Value, into any) error {
		if err := json.Unmarshal(raw, into); err != nil {
			return fmt.Errorf("stored %s %q: %w", kind, id, err)
		}
		return nil
	}

	if raw, ok := entries[entryMeta]["counters"]; ok {
		if err := decode(entryMeta, "counters", raw, &s.storedMeta); err != nil {
			return nil, err
		}
		s.log.RestoreSequence(s.storedMeta.LastID)
		s.ids.RestoreCounters(s.storedMeta.GuestNumber, s.storedMeta.AccountNumber)
		s.embedNumber, s.uploadSeq = s.storedMeta.EmbedNumber, s.storedMeta.UploadSeq
	}
	for kind, values := range entries {
		if strings.HasPrefix(kind, extEntryPrefix) {
			s.extData.Restore(kind, values)
		}
	}
	if err := s.restoreVAPIDLocked(entries[entryMeta]["vapid"]); err != nil {
		return nil, err
	}
	s.ids.Restore(entries[accounts.UsedKind])

	if err := s.rooms.Restore(entries[rooms.EntryKind]); err != nil {
		return nil, err
	}
	records, err := s.log.Restore(entries[roomlog.EntryKind], func(id string) *roomlog.Timeline {
		if r := s.rooms.Get(id); r != nil {
			return r.Log()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	for id, raw := range entries[entryMessage] {
		m, err := rooms.RestoreMessage(id, raw, records)
		if err != nil {
			return nil, err
		}
		if m == nil {
			continue // A message without its records cannot be served.
		}
		s.messages[id] = &messageState{Message: m}
	}

	now := time.Now()
	avatarEmbeds, err := s.users.Restore(entries[users.EntryKind], restoredUser, now)
	if err != nil {
		return nil, err
	}
	s.rooms.RestoreMembers(func(r *roomState, id string) (*userState, bool) {
		u := s.users.Get(id)
		if u != nil {
			u.joined[r.ID()] = r
		}
		return u, u != nil
	})

	files, err := s.embeds.Restore(entries[embeds.EntryKind], func(e embeds.Embed, avatarFor string) *embedState {
		return &embedState{Embed: e, avatarFor: s.users.Get(avatarFor), started: e.Finished}
	}, s.files.Path)
	if err != nil {
		return nil, err
	}
	for id, embed := range avatarEmbeds {
		if e := s.embeds.Get(embed); e != nil {
			s.users.Get(id).avatarEmbed = e
		}
	}
	if err := s.restoreEmojiLocked(entries[entryMeta]["emoji"]); err != nil {
		return nil, err
	}
	if err := s.restoreProfileLocked(entries[entryMeta]["profile"]); err != nil {
		return nil, err
	}

	// With push disabled, stored registrations stay in the store, unused,
	// for when push is enabled again.
	storedPushes := entries[entryPush]
	if s.config.DisablePush {
		storedPushes = nil
	}
	if err := s.pushes.Restore(storedPushes, func(p *pushRegistration) bool {
		return s.users.Get(p.User) != nil && p.Live(now)
	}); err != nil {
		return nil, err
	}
	if err := s.sessions.Restore(entries[sessions.EntryKind], func(id string) (*userState, bool) {
		u := s.users.Get(id)
		return u, u != nil && u.Account()
	}, now); err != nil {
		return nil, err
	}
	if err := s.invitations.Restore(entries[invitations.EntryKind], now); err != nil {
		return nil, err
	}
	if err := s.services.Restore(entries[services.EntryKind], func(id string) (*userState, bool) {
		u := s.users.Get(id)
		return u, u != nil
	}, now); err != nil {
		return nil, err
	}
	if err := s.devices.Restore(entries[devices.EntryKind], func(id string) (*userState, bool) {
		u := s.users.Get(id)
		return u, u != nil && u.Account()
	}); err != nil {
		return nil, err
	}

	// Nothing is connected yet (users.Table.Restore): guests are retired as
	// if their last connections had just closed, and writes that had not
	// finished fail.
	for _, id := range s.users.IDs() {
		if u := s.users.Get(id); !u.Account() {
			s.retireLocked(u)
		} else {
			s.grantRolesLocked(u)
			s.scheduleMuteLocked(u)
			for id := range u.RoomMutes {
				s.scheduleRoomMuteLocked(u, id)
			}
		}
	}
	for _, id := range s.embeds.IDs() {
		if e := s.embeds.Get(id); e != nil && !e.Finished {
			s.failWriteLocked(e)
		}
	}
	return files, nil
}

// restoreVAPIDLocked sets the webpush VAPID key (§4.9): Config's, else the
// one in the store, else a new one, which is stored.
func (s *Server) restoreVAPIDLocked(raw jsontext.Value) error {
	if s.vapid != nil {
		return nil
	}
	if raw != nil {
		var encoded string
		if err := json.Unmarshal(raw, &encoded); err != nil {
			return fmt.Errorf("stored VAPID key: %w", err)
		}
		key, err := webpush.ParseVAPIDKey(encoded)
		if err != nil {
			return fmt.Errorf("stored VAPID key: %w", err)
		}
		s.vapid, s.vapidStored = key, true
		return nil
	}
	key, err := webpush.NewVAPIDKey()
	if err != nil {
		return err
	}
	s.vapid, s.vapidStored = key, true
	s.dirty.vapid = true
	return nil
}
