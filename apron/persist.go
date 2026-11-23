package apron

import (
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/apron-chat/apron-server-go/apron/internal/devices"
	"github.com/apron-chat/apron-server-go/apron/internal/invitations"
	"github.com/apron-chat/apron-server-go/apron/internal/roomlog"
	"github.com/apron-chat/apron-server-go/apron/internal/rooms"
	"github.com/apron-chat/apron-server-go/apron/internal/services"
	"github.com/apron-chat/apron-server-go/apron/internal/sessions"
	"github.com/apron-chat/apron-server-go/apron/internal/webpush"
	"github.com/apron-chat/apron-server-go/internal/auth/email"
	"github.com/apron-chat/apron-server-go/internal/auth/external"
	"github.com/apron-chat/apron-server-go/internal/auth/passkey"
	"github.com/apron-chat/apron-server-go/internal/authn"
	"github.com/apron-chat/apron-server-go/store"
)

// Persistence writes the server's state through to a store.Store. Every
// change made under s.mu marks what it changed: the server's own state in
// its dirty set (touch*), and the modules that keep their own, the log
// (roomlog), the rooms (rooms), the extensions' values (kv), and the
// sessions (sessions), in theirs. When the lock is released, unlock renders the marked state into
// one batch, and the store writer (durable) applies batches to the store
// in order, off the lock. At start, Open rebuilds the state from the
// store.
//
// Entry kinds and IDs:
//
//	meta     "counters"   log_id, guest, embed, and upload sequences
//	meta     "vapid"      the webpush VAPID private key (§4.9)
//	meta     "emoji"      the custom emoji catalog (ext:emoji, emoji)
//	meta     "profile"    the server's title, description, and avatar set
//	                      with /server (§3.1)
//	used_id  lowercased user_id ever assigned
//	record   log_id       a logged record and the rooms whose logs hold it
//	                      (roomlog)
//	room     room_id      a room, its members, and their read cursors
//	                      (rooms)
//	message  message_id   a message's room, sender, snapshots, and
//	                      reactions (rooms)
//	user     user_id      guests too, retired at the next start
//	session  hex SHA-256 of the bearer token (sessions)
//	device   hex SHA-256 of the /invite-device token (devices)
//	service  hex SHA-256 of a bot's service token (services)
//	invitation  hex SHA-256 of an invitation's token (invitations)
//	embed    embed_id
//	push     user_id, a space, and url (url alone before registrations
//	         belonged to their user)
//	ext/<n>  a key        a value Extension <n> keeps (Host.Store, kv)
const (
	entryMeta    = "meta"
	entryUsedID  = "used_id"
	entryRoom    = rooms.EntryKind
	entryMessage = rooms.MessageEntryKind
	entryUser    = "user"
	entryEmbed   = "embed"
	entryPush    = "push"
)

// dirtySet is the state changed since the last batch, by key. For users,
// sessions, embeds, and pushes, a key missing from the server's maps at
// flush time is deleted from the store.
type dirtySet struct {
	messages map[string]bool
	users    map[string]bool
	embeds   map[string]bool
	pushes   map[string]bool
	usedIDs  []string
	vapid    bool
	profile  bool
}

func newDirtySet() dirtySet {
	return dirtySet{
		messages: make(map[string]bool),
		users:    make(map[string]bool),
		embeds:   make(map[string]bool),
		pushes:   make(map[string]bool),
	}
}

func (s *Server) touchMessage(m *messageState) { s.dirty.messages[m.ID()] = true }
func (s *Server) touchUser(id string)          { s.dirty.users[id] = true }
func (s *Server) touchEmbed(id string)         { s.dirty.embeds[id] = true }
func (s *Server) touchPush(key string)         { s.dirty.pushes[key] = true }
func (s *Server) touchUsedID(lowercased string) {
	s.dirty.usedIDs = append(s.dirty.usedIDs, lowercased)
}

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

type storedUser struct {
	Name        string           `json:"name,omitzero"`
	Avatar      string           `json:"avatar,omitzero"`
	Ext         extObject        `json:"ext,omitzero"`
	AvatarEmbed string           `json:"avatar_embed,omitzero"`
	LeftAt      map[string]int64 `json:"left_at,omitzero"`
	// Identities are what prove the account, by scheme; Records what each
	// scheme keeps with it; and Grants the roles each identity grants it
	// (auth.go).
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
	// users stored before it was kept. Bot names the Extension whose bot
	// the account is (Registrar.Bot).
	Created int64  `json:"created,omitzero"`
	Bot     string `json:"bot,omitzero"`
	// Enrolled is what the invitation that made the account left with it.
	Enrolled *invitations.Enrollment `json:"enrolled,omitzero"`
	// Scope is the rooms the account is limited to (scope.go).
	Scope *storedScope `json:"scope,omitzero"`
	// Consents are the hosts of apps the account allowed on the external
	// sign-in page, with when, in Unix milliseconds (consents.go).
	Consents map[string]int64 `json:"consents,omitzero"`
	External string           `json:"external,omitzero"`
	Granted  []string         `json:"granted,omitzero"`
}

type storedMute struct {
	Forever bool      `json:"forever,omitzero"`
	Until   time.Time `json:"until,omitzero"`
}

type storedEmbed struct {
	Kind      string `json:"kind"`
	MessageID string `json:"message_id,omitzero"`
	AvatarFor string `json:"avatar_for,omitzero"`
	EmojiFor  string `json:"emoji_for,omitzero"`
	// ServerAvatarBy is who sent a /server avatar upload.
	ServerAvatarBy string         `json:"server_avatar_by,omitzero"`
	BaseURL        string         `json:"base_url"`
	Secret         string         `json:"secret"`
	Title          string         `json:"title,omitzero"`
	Alt            string         `json:"alt,omitzero"`
	Finished       bool           `json:"finished,omitzero"`
	Owned          map[string]any `json:"owned,omitzero"`
	File           string         `json:"file,omitzero"`
	Size           int64          `json:"size,omitzero"`
	ContentType    string         `json:"content_type,omitzero"`
	Seq            int64          `json:"seq,omitzero"`
}

type storedPush struct {
	User   string `json:"user"`
	Kind   string `json:"kind"`
	URL    string `json:"url,omitzero"`
	Token  string `json:"token,omitzero"`
	PushID string `json:"push_id,omitzero"`
	// Device is the hex SHA-256 of the /invite-device token the registering
	// connection signed in with.
	Device string `json:"device,omitzero"`
	// Session is the hex SHA-256 of the session token it signed in with.
	Session string `json:"session,omitzero"`
	// P256DH and Auth are the subscription's keys, in base64url.
	P256DH string `json:"p256dh,omitzero"`
	Auth   string `json:"auth,omitzero"`
	// Wake lists the registration's scopes.
	Wake    []string  `json:"wake"`
	Renewed time.Time `json:"renewed,omitzero"`
}

// metaLocked is the counters entry of the current state.
func (s *Server) metaLocked() storedMeta {
	return storedMeta{LastID: s.log.LastIssued(), GuestNumber: s.guestNumber, AccountNumber: s.accountNumber, EmbedNumber: s.embedNumber, UploadSeq: s.uploadSeq}
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
		s.rooms.Entries()
		s.extData.Entries()
		s.sessions.Entries(userID)
		s.devices.Entries(userID)
		s.services.Entries(userID)
		s.invitations.Entries()
		s.emoji.Entries()
		s.store.Queue(nil)
		return
	}
	batch := append(s.entriesLocked(s.dirty, s.log.Entries()), s.rooms.Entries()...)
	batch = append(batch, s.extData.Entries()...)
	batch = append(batch, s.sessions.Entries(userID)...)
	batch = append(batch, s.devices.Entries(userID)...)
	batch = append(batch, s.services.Entries(userID)...)
	batch = append(batch, s.invitations.Entries()...)
	batch = append(batch, s.emoji.Entries()...)
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
	del := func(kind, id string) {
		batch = append(batch, store.Entry{Kind: kind, ID: id})
	}
	for _, id := range d.usedIDs {
		put(entryUsedID, id, true)
	}
	batch = append(batch, records...)
	for id := range d.messages {
		if m := s.messages[id]; m != nil {
			batch = append(batch, rooms.MessageEntry(m.Message))
		}
	}
	for id := range d.users {
		if u := s.users[id]; u != nil {
			put(entryUser, id, storedUserOf(u))
		} else {
			del(entryUser, id)
		}
	}
	for id := range d.embeds {
		if e := s.embeds[id]; e != nil {
			put(entryEmbed, id, storedEmbedOf(e))
		} else {
			del(entryEmbed, id)
		}
	}
	for key := range d.pushes {
		if p := s.pushes[key]; p != nil {
			stored := storedPush{
				User: p.userID, Kind: p.kind, URL: p.url, Token: p.token, PushID: p.pushID,
				Wake: p.wake.names(), Renewed: p.renewed,
			}
			if p.device != [32]byte{} {
				stored.Device = sessionID(p.device)
			}
			if p.session != [32]byte{} {
				stored.Session = sessionID(p.session)
			}
			if p.keys != nil {
				stored.P256DH, stored.Auth = webpush.EncodeBase64URL(p.keys.P256DH), webpush.EncodeBase64URL(p.keys.Auth)
			}
			put(entryPush, key, stored)
		} else {
			del(entryPush, key)
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

func storedUserOf(u *userState) storedUser {
	stored := storedUser{
		Name: u.name, Avatar: u.avatar, Ext: u.ext, LeftAt: u.leftAt, Pings: u.pings, Seen: u.seen, Shown: u.status != statusOffline,
		Bot: u.botOf, Identities: u.identities, Records: u.records, Grants: u.granted,
		Email: u.identities[email.Name], External: u.identities[external.Name],
		Passkey: u.records[passkey.Name], Granted: u.granted[external.Name],
		Enrolled: u.enrolled, Scope: storedScopeOf(u.scope), Consents: u.consents,
	}
	if !u.created.IsZero() {
		stored.Created = u.created.UnixMilli()
	}
	if status := u.chosen; status != statusOnline {
		stored.Status = &status
	}
	now := time.Now()
	if u.mute.active(now) {
		stored.Mute = &storedMute{Forever: u.mute.forever, Until: u.mute.until}
	}
	for id, mute := range u.roomMutes {
		if mute.active(now) {
			if stored.RoomMutes == nil {
				stored.RoomMutes = make(map[string]storedMute)
			}
			stored.RoomMutes[id] = storedMute{Forever: mute.forever, Until: mute.until}
		}
	}
	if u.avatarEmbed != nil {
		stored.AvatarEmbed = u.avatarEmbed.id
	}
	return stored
}

func storedEmbedOf(e *embedState) storedEmbed {
	stored := storedEmbed{
		Kind: e.kind, MessageID: e.messageID, BaseURL: e.baseURL, Secret: e.secret,
		Title: e.title, Alt: e.alt, Finished: e.finished,
		Owned: e.owned, Size: e.size, ContentType: e.contentType, Seq: e.seq,
	}
	if e.avatarFor != nil {
		stored.AvatarFor = e.avatarFor.id
	}
	stored.EmojiFor = e.emojiKey
	stored.ServerAvatarBy = e.serverAvatarBy
	if e.path != "" {
		stored.File = filepath.Base(e.path)
	}
	return stored
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
		s.guestNumber, s.accountNumber = s.storedMeta.GuestNumber, s.storedMeta.AccountNumber
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
	for id := range entries[entryUsedID] {
		s.usedIDs[id] = true
	}

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

	storedUsers := make(map[string]storedUser)
	for id, raw := range entries[entryUser] {
		var stored storedUser
		if err := decode(entryUser, id, raw, &stored); err != nil {
			return nil, err
		}
		storedUsers[id] = stored
		u := newUserState(id, stored.Name)
		u.avatar, u.ext = stored.Avatar, stored.Ext
		u.created = time.Time{}
		if stored.Created != 0 {
			u.created = time.UnixMilli(stored.Created)
		}
		u.botOf = stored.Bot
		if err := s.restoreIdentitiesLocked(u, stored); err != nil {
			return nil, fmt.Errorf("stored user %q: %w", id, err)
		}
		if stored.LeftAt != nil {
			u.leftAt = stored.LeftAt
		}
		switch {
		case stored.Status != nil && settableStatus(*stored.Status):
			u.chosen = *stored.Status
		case stored.Status != nil:
			u.chosen = statusNone
		}
		u.seen = stored.Seen
		if stored.Mute != nil {
			u.mute = muteState{forever: stored.Mute.Forever, until: stored.Mute.Until}
		}
		for id, mute := range stored.RoomMutes {
			u.roomMutes[id] = muteState{forever: mute.Forever, until: mute.Until}
		}
		if stored.Pings != nil {
			u.pings = stored.Pings
		}
		u.consents = stored.Consents
		s.users[id] = u
	}
	s.rooms.RestoreMembers(func(r *roomState, id string) (*userState, bool) {
		u := s.users[id]
		if u != nil {
			u.joined[r.ID()] = r
		}
		return u, u != nil
	})

	files := make(map[string]bool)
	var uploads []*embedState
	for id, raw := range entries[entryEmbed] {
		var stored storedEmbed
		if err := decode(entryEmbed, id, raw, &stored); err != nil {
			return nil, err
		}
		e := &embedState{
			id: id, kind: stored.Kind, messageID: stored.MessageID, avatarFor: s.users[stored.AvatarFor], emojiKey: stored.EmojiFor,
			serverAvatarBy: stored.ServerAvatarBy,
			baseURL:        stored.BaseURL, secret: stored.Secret, title: stored.Title, alt: stored.Alt,
			finished: stored.Finished, started: stored.Finished, owned: stored.Owned, size: stored.Size,
			contentType: stored.ContentType, seq: stored.Seq,
		}
		if stored.File != "" {
			e.path = s.files.Path(stored.File)
			files[e.path] = true
			uploads = append(uploads, e)
		}
		s.embeds[id] = e
	}
	slices.SortFunc(uploads, func(a, b *embedState) int { return int(a.seq - b.seq) })
	for _, e := range uploads {
		if !e.apart() {
			e.upload = s.uploads.PushBack(e)
			s.uploadBytes += e.size
		}
	}
	for id, stored := range storedUsers {
		if e := s.embeds[stored.AvatarEmbed]; e != nil {
			s.users[id].avatarEmbed = e
		}
	}
	if err := s.restoreEmojiLocked(entries[entryMeta]["emoji"]); err != nil {
		return nil, err
	}
	if err := s.restoreProfileLocked(entries[entryMeta]["profile"]); err != nil {
		return nil, err
	}

	now := time.Now()
	// With push disabled, stored registrations stay in the store, unused,
	// for when push is enabled again.
	storedPushes := entries[entryPush]
	if s.config.DisablePush {
		storedPushes = nil
	}
	for key, raw := range storedPushes {
		var stored storedPush
		if err := decode(entryPush, key, raw, &stored); err != nil {
			return nil, err
		}
		p := &pushRegistration{
			userID: stored.User, kind: stored.Kind, url: stored.URL, token: stored.Token, pushID: stored.PushID,
			wake: parseWakeNames(stored.Wake), renewed: stored.Renewed, lastUnread: -1,
		}
		if stored.P256DH != "" {
			keys, err := webpush.ParseKeys(stored.P256DH, stored.Auth)
			if err != nil {
				return nil, fmt.Errorf("stored push %q: %w", key, err)
			}
			p.keys = &keys
		}
		if device, err := hex.DecodeString(stored.Device); err == nil && len(device) == 32 {
			p.device = [32]byte(device)
		}
		if session, err := hex.DecodeString(stored.Session); err == nil && len(session) == 32 {
			p.session = [32]byte(session)
		}
		if u := s.users[stored.User]; u != nil && p.live(now) {
			s.addPushLocked(u, p)
		} else {
			s.touchPush(key)
		}
	}
	if err := s.sessions.Restore(entries[sessions.EntryKind], func(id string) (*userState, bool) {
		u := s.users[id]
		return u, u != nil && u.account()
	}, now); err != nil {
		return nil, err
	}
	if err := s.invitations.Restore(entries[invitations.EntryKind], now); err != nil {
		return nil, err
	}
	if err := s.services.Restore(entries[services.EntryKind], func(id string) (*userState, bool) {
		u := s.users[id]
		return u, u != nil
	}, now); err != nil {
		return nil, err
	}
	if err := s.devices.Restore(entries[devices.EntryKind], func(id string) (*userState, bool) {
		u := s.users[id]
		return u, u != nil && u.account()
	}); err != nil {
		return nil, err
	}

	// Nothing is connected yet: guests are retired as if their last
	// connections had just closed, and writes that had not finished fail.
	// An account stored as shown other than offline was connected when the
	// server stopped without closing its connections, as in a crash, which
	// stores nothing. The shown status becomes offline now, so seen is now,
	// as for a disconnect (§4.5.2): no earlier time is known not to be
	// before others last saw the account, since the store holds no time of
	// the crash, and nobody could see it in between. A shutdown, which
	// closes every connection first, leaves no account stored so; an
	// invisible one is never stored so, so its connections never move seen.
	for _, id := range slices.Sorted(maps.Keys(s.users)) {
		if u := s.users[id]; !u.account() {
			s.retireLocked(u)
		} else {
			s.grantRolesLocked(u)
			s.scheduleMuteLocked(u)
			for id := range u.roomMutes {
				s.scheduleRoomMuteLocked(u, id)
			}
			u.shown = u.shownStatus()
			u.status = u.shown
			if storedUsers[id].Shown && u.shown == statusOffline {
				u.seen = now.UnixMilli()
				s.touchUser(id)
			}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(s.embeds)) {
		if e := s.embeds[id]; e != nil && !e.finished {
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

// restoreIdentitiesLocked gives a stored account its identities, their
// records, and the roles they grant; the schemes get the records once they
// are all added (restoreSchemesLocked). An account stored by a server from
// before Identities has its email address, passkeys, external identity, and
// the external sign-in's roles in fields of their own, and is stored again
// with them as identities.
func (s *Server) restoreIdentitiesLocked(u *userState, stored storedUser) error {
	u.identities, u.records = maps.Clone(stored.Identities), maps.Clone(stored.Records)
	u.granted = maps.Clone(stored.Grants)
	u.enrolled = stored.Enrolled
	u.scope = stored.Scope.restored()
	legacy := map[string]string{email.Name: stored.Email, external.Name: stored.External}
	if len(stored.Passkey) > 0 && u.identities[passkey.Name] == "" {
		var old struct {
			Handle []byte `json:"handle"`
		}
		if err := json.Unmarshal(stored.Passkey, &old); err != nil {
			return err
		}
		legacy[passkey.Name] = passkey.Subject(old.Handle)
		if u.records == nil {
			u.records = make(map[string]jsontext.Value)
		}
		u.records[passkey.Name] = stored.Passkey
	}
	for scheme, subject := range legacy {
		if subject == "" || u.identities[scheme] != "" {
			continue
		}
		if u.identities == nil {
			u.identities = make(map[string]string)
		}
		u.identities[scheme] = subject
		s.touchUser(u.id)
	}
	if len(stored.Granted) > 0 && stored.Grants == nil {
		u.granted = map[string][]string{external.Name: stored.Granted}
		s.touchUser(u.id)
	}
	for scheme, subject := range u.identities {
		s.accounts[authn.Identity{Scheme: scheme, Subject: subject}] = u
	}
	return nil
}
