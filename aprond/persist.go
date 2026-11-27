package aprond

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/apron-chat/apron-server-go/aprond/internal/accounts"
	"github.com/apron-chat/apron-server-go/aprond/internal/devices"
	"github.com/apron-chat/apron-server-go/aprond/internal/embeds"
	"github.com/apron-chat/apron-server-go/aprond/internal/emoji"
	"github.com/apron-chat/apron-server-go/aprond/internal/invitations"
	"github.com/apron-chat/apron-server-go/aprond/internal/profile"
	"github.com/apron-chat/apron-server-go/aprond/internal/push"
	"github.com/apron-chat/apron-server-go/aprond/internal/roomlog"
	"github.com/apron-chat/apron-server-go/aprond/internal/rooms"
	"github.com/apron-chat/apron-server-go/aprond/internal/services"
	"github.com/apron-chat/apron-server-go/aprond/internal/sessions"
	"github.com/apron-chat/apron-server-go/aprond/internal/users"
	"github.com/apron-chat/apron-server-go/aprond/internal/webpush"
	"github.com/apron-chat/apron-server-go/store"
)

// Persistence writes the server's state through to a store.Store. Every
// change made under s.mu is marked by the module that keeps what it
// changed: the log (roomlog), the rooms and messages (rooms), the user_ids
// used (accounts), the users (users), the push registrations (push), the
// extensions' values (kv), the sessions (sessions), the device tokens
// (devices), the service tokens (services), the invitations
// (invitations), the hosted embeds (embeds), the custom emoji (emoji), the
// VAPID key the server made (webpush), and the server's profile (profile).
// When the lock is released, unlock has each render what it marked, and
// the counters if they moved, into one batch, and the store writer
// (durable) applies batches to the store in order, off the lock. A large
// room's entry, which lists every member, their activity, and their read
// cursors, is the one exception: what changes with each message and read
// is rendered at most once every rooms.EntryDelay, and at once when a fence
// waits (Sync) or the store closes, while its records go at once (rooms);
// so is the entry of a message with many reactions, whose reactions
// records restoring applies over it.
// At start, Open rebuilds the state from the store.
//
// Entry kinds and IDs:
//
//	meta     "counters"   log_id, guest, embed, and upload sequences
//	meta     "vapid"      the webpush VAPID private key the server made
//	                      (§4.9, webpush)
//	meta     "emoji"      the custom emoji catalog (ext:emoji, emoji)
//	meta     "profile"    the server's title, description, and avatar set
//	                      with /server (§3.1, profile)
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
	embeds, uploads := s.embeds.Counters()
	return storedMeta{LastID: s.log.LastIssued(), GuestNumber: guests, AccountNumber: accounts, EmbedNumber: embeds, UploadSeq: uploads}
}

// userID names a user in what the store keeps.
func userID(u *userState) string { return u.id }

// flushLocked queues the marked changes as one batch for the store writer,
// with the fences waiting for it. Once the writer is stopped, the batch is
// dropped (durable.Writer.Queue), but the changes are still taken.
func (s *Server) flushLocked() {
	// A large room's entry, which lists every member, lags behind what
	// changes with each message for a moment (rooms.EntryDelay), unless a
	// fence waits for every change so far, or the store is closing.
	var roomEntries []store.Entry
	if s.store.Fenced() || s.closingStore {
		roomEntries = s.rooms.Entries()
	} else {
		roomEntries = s.rooms.EntriesDue(time.Now())
		if due, lagging := s.rooms.Lagging(); lagging && s.roomEntryTimer == nil && !s.closed {
			s.roomEntryTimer = time.AfterFunc(time.Until(due), s.flushRoomEntries)
		}
	}
	batch := slices.Concat(
		s.ids.Entries(),
		s.log.Entries(),
		roomEntries,
		s.users.Entries(avatarEmbedID),
		s.pushes.Entries(),
		s.extData.Entries(),
		s.sessions.Entries(userID),
		s.devices.Entries(userID),
		s.services.Entries(userID),
		s.invitations.Entries(),
		s.emoji.Entries(),
		s.embeds.Entries(avatarOwner),
		s.storedVAPID.Entries(),
		s.profile.Entries(),
	)
	if meta := s.metaLocked(); meta != s.storedMeta {
		s.storedMeta = meta
		batch = append(batch, store.Entry{Kind: entryMeta, ID: "counters", Value: encodeJSON(meta)})
	}
	s.store.Queue(batch)
}

// flushRoomEntries writes the entries of the large rooms that lag behind,
// once they are due (rooms.Table.EntriesDue).
func (s *Server) flushRoomEntries() {
	s.mu.Lock()
	s.roomEntryTimer = nil
	s.unlock()
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
	s.closingStore = true
	if s.roomEntryTimer != nil {
		s.roomEntryTimer.Stop()
		s.roomEntryTimer = nil
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
	if raw, ok := entries[entryMeta]["counters"]; ok {
		if err := json.Unmarshal(raw, &s.storedMeta); err != nil {
			return nil, fmt.Errorf("stored %s %q: %w", entryMeta, "counters", err)
		}
		s.log.RestoreSequence(s.storedMeta.LastID)
		s.ids.RestoreCounters(s.storedMeta.GuestNumber, s.storedMeta.AccountNumber)
		s.embeds.RestoreCounters(s.storedMeta.EmbedNumber, s.storedMeta.UploadSeq)
	}
	for kind, values := range entries {
		if strings.HasPrefix(kind, extEntryPrefix) {
			s.extData.Restore(kind, values)
		}
	}
	if s.push.VAPID == nil {
		// The webpush VAPID key (§4.9) is Config's, else the one in the
		// store, else a new one, which is stored.
		if s.push.VAPID, err = s.storedVAPID.Restore(entries[webpush.StoreKind][webpush.StoreID]); err != nil {
			return nil, err
		}
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

	if err := s.rooms.RestoreMessages(entries[entryMessage], records); err != nil {
		return nil, err
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
	if err := s.restoreEmojiLocked(entries[emoji.StoreKind][emoji.StoreID]); err != nil {
		return nil, err
	}
	if err := s.restoreProfileLocked(entries[profile.StoreKind][profile.StoreID]); err != nil {
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
			s.users.ScheduleMutes(u)
		}
	}
	for _, id := range s.embeds.IDs() {
		if e := s.embeds.Get(id); e != nil && !e.Finished {
			s.failWriteLocked(e)
		}
	}
	return files, nil
}
