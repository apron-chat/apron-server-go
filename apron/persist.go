package apron

import (
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/apron-chat/apron-server-go/internal/auth/email"
	"github.com/apron-chat/apron-server-go/internal/auth/external"
	"github.com/apron-chat/apron-server-go/internal/auth/passkey"
	"github.com/apron-chat/apron-server-go/internal/authn"
	"github.com/apron-chat/apron-server-go/store"
)

// Persistence writes the server's state through to a store.Store. Every
// change made under s.mu marks what it changed (touch*); when the lock is
// released, unlock serializes the marked state into one batch, and a single
// writer applies batches to the store in order, off the lock. At start, New
// rebuilds the state from the store.
//
// Entry kinds and IDs:
//
//	meta     "counters"   log_id, guest, embed, and upload sequences
//	meta     "vapid"      the webpush VAPID private key (§4.9)
//	meta     "emoji"      the custom emoji catalog (ext:emoji)
//	meta     "profile"    the server's title, description, and avatar set
//	                      with /server (§3.1)
//	used_id  lowercased user_id ever assigned
//	record   log_id       a logged record and the rooms whose logs hold it
//	room     room_id
//	message  message_id
//	user     user_id      guests too, retired at the next start
//	session  hex SHA-256 of the bearer token
//	device   hex SHA-256 of the /invite-device token
//	embed    embed_id
//	push     user_id, a space, and url (url alone before registrations
//	         belonged to their user)
//	ext/<n>  a key        a value Extension <n> keeps (Host.Store)
const (
	entryMeta    = "meta"
	entryUsedID  = "used_id"
	entryRecord  = "record"
	entryRoom    = "room"
	entryMessage = "message"
	entryUser    = "user"
	entrySession = "session"
	entryDevice  = "device"
	entryEmbed   = "embed"
	entryPush    = "push"
)

// storeQueue bounds the batches waiting for the store writer; beyond it,
// requests wait for the store.
const storeQueue = 1024

// dirtySet is the state changed since the last batch, by key. For users,
// sessions, embeds, and pushes, a key missing from the server's maps at
// flush time is deleted from the store.
type dirtySet struct {
	records  map[*logRecord]bool
	rooms    map[string]bool
	messages map[string]bool
	users    map[string]bool
	sessions map[[32]byte]bool
	devices  map[[32]byte]bool
	embeds   map[string]bool
	// invitations are named by their tokens' SHA-256.
	invitations map[[32]byte]bool
	pushes      map[string]bool
	ext         map[extKey]bool
	usedIDs     []string
	vapid       bool
	emoji       bool
	profile     bool
}

func newDirtySet() dirtySet {
	return dirtySet{
		records:     make(map[*logRecord]bool),
		rooms:       make(map[string]bool),
		messages:    make(map[string]bool),
		users:       make(map[string]bool),
		sessions:    make(map[[32]byte]bool),
		devices:     make(map[[32]byte]bool),
		invitations: make(map[[32]byte]bool),
		embeds:      make(map[string]bool),
		pushes:      make(map[string]bool),
		ext:         make(map[extKey]bool),
	}
}

// extKey names one value an extension keeps: its entry kind, ext/<name>,
// and its key.
type extKey struct{ kind, id string }

func (s *Server) touchRecord(r *logRecord)     { s.dirty.records[r] = true }
func (s *Server) touchRoom(r *roomState)       { s.dirty.rooms[r.id] = true }
func (s *Server) touchMessage(m *messageState) { s.dirty.messages[m.id] = true }
func (s *Server) touchUser(id string)          { s.dirty.users[id] = true }
func (s *Server) touchSession(key [32]byte)    { s.dirty.sessions[key] = true }
func (s *Server) touchDevice(key [32]byte)     { s.dirty.devices[key] = true }
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

type storedRecord struct {
	Kind  recordKind     `json:"kind"`
	Raw   jsontext.Value `json:"raw"`
	Rooms []string       `json:"rooms"`
}

type storedCursor struct {
	From      map[string]any `json:"from"`
	MessageID string         `json:"message_id"`
	ID        int64          `json:"id"`
}

type storedRoom struct {
	Parent      string                  `json:"parent,omitzero"`
	Private     bool                    `json:"private,omitzero"`
	Active      map[string]int64        `json:"active,omitzero"`
	Record      jsontext.Value          `json:"record"`
	RecordLogID int64                   `json:"record_log_id"`
	CreatedID   int64                   `json:"created_id"`
	LatestID    int64                   `json:"latest_id"`
	Members     []string                `json:"members"`
	Creator     string                  `json:"creator,omitzero"`
	Reads       map[string]storedCursor `json:"reads,omitzero"`
}

type storedReaction struct {
	From   map[string]any `json:"from"`
	Emojis []string       `json:"emojis"`
}

type storedMessage struct {
	From      map[string]any            `json:"from"`
	LogID     int64                     `json:"log_id"`
	Owner     string                    `json:"owner"`
	RoomID    string                    `json:"room_id"`
	Reactions map[string]storedReaction `json:"reactions,omitzero"`
	Records   []int64                   `json:"records"`
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
	Enrolled *storedEnrollment `json:"enrolled,omitzero"`
	External string            `json:"external,omitzero"`
	Granted  []string          `json:"granted,omitzero"`
}

type storedMute struct {
	Forever bool      `json:"forever,omitzero"`
	Until   time.Time `json:"until,omitzero"`
}

type storedSession struct {
	User       string    `json:"user"`
	Origin     string    `json:"origin"`
	Expires    time.Time `json:"expires"`
	Credential string    `json:"credential,omitzero"`
}

type storedDevice struct {
	User    string    `json:"user"`
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
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
	// P256DH and Auth are the subscription's keys, in base64url.
	P256DH string `json:"p256dh,omitzero"`
	Auth   string `json:"auth,omitzero"`
	// Wake lists the registration's scopes.
	Wake    []string  `json:"wake"`
	Renewed time.Time `json:"renewed,omitzero"`
}

// metaLocked is the counters entry of the current state.
func (s *Server) metaLocked() storedMeta {
	return storedMeta{LastID: s.lastID, GuestNumber: s.guestNumber, AccountNumber: s.accountNumber, EmbedNumber: s.embedNumber, UploadSeq: s.uploadSeq}
}

func sessionID(key [32]byte) string { return hex.EncodeToString(key[:]) }

// flushLocked queues the marked changes as one batch for the store writer,
// with the fences waiting for it.
func (s *Server) flushLocked() {
	fences := s.fences
	s.fences = nil
	if s.storeClosed {
		s.dirty = newDirtySet()
		for _, f := range fences {
			f.release(ErrServerClosed)
		}
		return
	}
	batch := s.entriesLocked(s.dirty)
	meta := s.metaLocked()
	if meta != s.storedMeta {
		s.storedMeta = meta
		batch = append(batch, store.Entry{Kind: entryMeta, ID: "counters", Value: encodeJSON(meta)})
	}
	s.dirty = newDirtySet()
	if len(batch) > 0 || len(fences) > 0 {
		s.storeWrites <- storeBatch{entries: batch, fences: fences}
	}
}

// entriesLocked renders the state named by a dirty set as store entries.
func (s *Server) entriesLocked(d dirtySet) []store.Entry {
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
	for r := range d.records {
		put(entryRecord, formatID(r.id), storedRecord{Kind: r.kind, Raw: r.raw, Rooms: r.rooms})
	}
	for id := range d.rooms {
		if r := s.rooms[id]; r != nil {
			put(entryRoom, id, s.storedRoomLocked(r))
		}
	}
	for id := range d.messages {
		if m := s.messages[id]; m != nil {
			put(entryMessage, id, storedMessageOf(m))
		}
	}
	for id := range d.users {
		if u := s.users[id]; u != nil {
			put(entryUser, id, storedUserOf(u))
		} else {
			del(entryUser, id)
		}
	}
	for key := range d.sessions {
		if session, ok := s.sessions[key]; ok {
			put(entrySession, sessionID(key), storedSession{User: session.user.id, Origin: session.origin, Expires: session.expires, Credential: session.credential})
		} else {
			del(entrySession, sessionID(key))
		}
	}
	for key := range d.devices {
		if device, ok := s.devices[key]; ok {
			put(entryDevice, sessionID(key), storedDevice{User: device.user.id, Name: device.name, Created: device.created})
		} else {
			del(entryDevice, sessionID(key))
		}
	}
	s.invitationEntriesLocked(d, put, del)
	for id := range d.embeds {
		if e := s.embeds[id]; e != nil {
			put(entryEmbed, id, storedEmbedOf(e))
		} else {
			del(entryEmbed, id)
		}
	}
	for key := range d.ext {
		if value, ok := s.extData[key.kind][key.id]; ok {
			batch = append(batch, store.Entry{Kind: key.kind, ID: key.id, Value: value})
		} else {
			del(key.kind, key.id)
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
			if p.keys != nil {
				stored.P256DH, stored.Auth = encodeBase64URL(p.keys.p256dh), encodeBase64URL(p.keys.auth)
			}
			put(entryPush, key, stored)
		} else {
			del(entryPush, key)
		}
	}
	if d.vapid && s.vapidStored {
		put(entryMeta, "vapid", s.vapid.encoded())
	}
	if d.emoji {
		put(entryMeta, "emoji", s.storedEmojiLocked())
	}
	if d.profile {
		put(entryMeta, "profile", s.storedProfileLocked())
	}
	return batch
}

func (s *Server) storedRoomLocked(r *roomState) storedRoom {
	stored := storedRoom{
		Record:      encodeJSON(r.record),
		RecordLogID: r.recordLogID,
		CreatedID:   r.createdID,
		LatestID:    r.latestID,
		Members:     slices.Sorted(maps.Keys(r.members)),
		Creator:     r.creator,
		Private:     r.private,
		Active:      r.active,
	}
	if r.parent != nil {
		stored.Parent = r.parent.id
	}
	if len(r.reads) > 0 {
		stored.Reads = make(map[string]storedCursor, len(r.reads))
		for id, cursor := range r.reads {
			stored.Reads[id] = storedCursor{From: cursor.from, MessageID: cursor.messageID, ID: cursor.id}
		}
	}
	return stored
}

func storedMessageOf(m *messageState) storedMessage {
	stored := storedMessage{From: m.from, LogID: m.logID, Owner: m.owner, RoomID: m.roomID}
	for _, record := range m.records {
		stored.Records = append(stored.Records, record.id)
	}
	if len(m.reactions) > 0 {
		stored.Reactions = make(map[string]storedReaction, len(m.reactions))
		for id, set := range m.reactions {
			stored.Reactions[id] = storedReaction{From: set.from, Emojis: set.emojis}
		}
	}
	return stored
}

func storedUserOf(u *userState) storedUser {
	stored := storedUser{
		Name: u.name, Avatar: u.avatar, Ext: u.ext, LeftAt: u.leftAt, Pings: u.pings, Seen: u.seen, Shown: u.status != statusOffline,
		Bot: u.botOf, Identities: u.identities, Records: u.records, Grants: u.granted,
		Email: u.identities[email.Name], External: u.identities[external.Name],
		Passkey: u.records[passkey.Name], Granted: u.granted[external.Name],
		Enrolled: u.enrolled.stored(),
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

// writeStore applies batches to the store in order until the queue closes,
// then releases the fences behind each. After the store's first failure it
// applies nothing more, so the store keeps the state as of the batch
// before, and every later fence fails.
func (s *Server) writeStore() {
	defer close(s.storeDone)
	for batch := range s.storeWrites {
		err := s.StoreErr()
		if err == nil && len(batch.entries) > 0 {
			if applyErr := s.config.Store.Apply(batch.entries); applyErr != nil {
				err = fmt.Errorf("%w: %w", ErrStoreFailed, applyErr)
				s.storeErr.Store(&err)
				slog.Error("cannot write to the store; the server writes nothing more", "error", applyErr)
				if s.config.OnStoreError != nil {
					s.config.OnStoreError(err)
				}
			}
		}
		for _, f := range batch.fences {
			f.release(err)
		}
	}
}

// closeStoreLocked writes the last changes and closes the store.
func (s *Server) closeStoreLocked() {
	if s.storeClosed {
		return
	}
	s.flushLocked()
	s.storeClosed = true
	close(s.storeWrites)
	<-s.storeDone
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
		s.lastID, s.guestNumber, s.accountNumber = s.storedMeta.LastID, s.storedMeta.GuestNumber, s.storedMeta.AccountNumber
		s.embedNumber, s.uploadSeq = s.storedMeta.EmbedNumber, s.storedMeta.UploadSeq
	}
	for kind, values := range entries {
		if strings.HasPrefix(kind, extEntryPrefix) {
			s.extData[kind] = values
		}
	}
	if err := s.restoreVAPIDLocked(entries[entryMeta]["vapid"]); err != nil {
		return nil, err
	}
	for id := range entries[entryUsedID] {
		s.usedIDs[id] = true
	}

	records := make(map[int64]*logRecord)
	for id, raw := range entries[entryRecord] {
		var stored storedRecord
		if err := decode(entryRecord, id, raw, &stored); err != nil {
			return nil, err
		}
		logID, _ := strconv.ParseInt(id, 10, 64)
		records[logID] = &logRecord{id: logID, kind: stored.Kind, raw: stored.Raw, rooms: stored.Rooms}
	}

	storedRooms := make(map[string]storedRoom)
	for id, raw := range entries[entryRoom] {
		var stored storedRoom
		if err := decode(entryRoom, id, raw, &stored); err != nil {
			return nil, err
		}
		storedRooms[id] = stored
		if stored.Active == nil {
			stored.Active = make(map[string]int64)
		}
		s.rooms[id] = &roomState{
			id: id, record: decodeObject(stored.Record), recordLogID: stored.RecordLogID, createdID: stored.CreatedID,
			latestID: stored.LatestID, creator: stored.Creator, private: stored.Private, active: stored.Active,
			members: make(map[string]*userState), reads: make(map[string]readCursor),
		}
	}
	order := slices.SortedFunc(maps.Keys(s.rooms), func(a, b string) int {
		return int(s.rooms[a].createdID - s.rooms[b].createdID)
	})
	for _, id := range order {
		r := s.rooms[id]
		if parent := s.rooms[storedRooms[id].Parent]; parent != nil {
			r.parent = parent
			parent.children = append(parent.children, r)
		}
		for userID, cursor := range storedRooms[id].Reads {
			r.reads[userID] = readCursor{from: cursor.From, messageID: cursor.MessageID, id: cursor.ID}
		}
	}
	for _, logID := range slices.Sorted(maps.Keys(records)) {
		record := records[logID]
		s.records = append(s.records, record)
		for _, roomID := range record.rooms {
			if r := s.rooms[roomID]; r != nil {
				r.log = append(r.log, record)
			}
		}
	}

	for id, raw := range entries[entryMessage] {
		var stored storedMessage
		if err := decode(entryMessage, id, raw, &stored); err != nil {
			return nil, err
		}
		m := &messageState{id: id, from: stored.From, logID: stored.LogID, owner: stored.Owner, roomID: stored.RoomID, reactions: make(map[string]reactionSet)}
		for userID, set := range stored.Reactions {
			m.reactions[userID] = reactionSet{from: set.From, emojis: set.Emojis}
		}
		for _, logID := range stored.Records {
			if record := records[logID]; record != nil {
				record.message = id
				m.records = append(m.records, record)
			}
		}
		if len(m.records) == 0 {
			continue // A message without its records cannot be served.
		}
		s.messages[id] = m
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
		s.users[id] = u
	}
	for _, id := range order {
		r := s.rooms[id]
		for _, userID := range storedRooms[id].Members {
			if u := s.users[userID]; u != nil {
				r.members[userID] = u
				u.joined[id] = r
			}
		}
	}

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
			e.path = filepath.Join(s.uploadDir, stored.File)
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
			keys, err := parsePushKeys(stored.P256DH, stored.Auth)
			if err != nil {
				return nil, fmt.Errorf("stored push %q: %w", key, err)
			}
			p.keys = &keys
		}
		if device, err := hex.DecodeString(stored.Device); err == nil && len(device) == 32 {
			p.device = [32]byte(device)
		}
		if u := s.users[stored.User]; u != nil && p.live(now) {
			s.addPushLocked(u, p)
		} else {
			s.touchPush(key)
		}
	}
	for id, raw := range entries[entrySession] {
		var stored storedSession
		key, err := hex.DecodeString(id)
		if err != nil || len(key) != 32 {
			continue
		}
		if err := decode(entrySession, id, raw, &stored); err != nil {
			return nil, err
		}
		if u := s.users[stored.User]; u != nil && u.account() && now.Before(stored.Expires) {
			s.sessions[[32]byte(key)] = session{user: u, origin: stored.Origin, expires: stored.Expires, credential: stored.Credential}
		} else {
			s.touchSession([32]byte(key))
		}
	}
	if err := s.restoreInvitationsLocked(entries[entryInvitation], now); err != nil {
		return nil, err
	}
	for id, raw := range entries[entryDevice] {
		var stored storedDevice
		key, err := hex.DecodeString(id)
		if err != nil || len(key) != 32 {
			continue
		}
		if err := decode(entryDevice, id, raw, &stored); err != nil {
			return nil, err
		}
		if u := s.users[stored.User]; u != nil && u.account() {
			s.devices[[32]byte(key)] = deviceToken{user: u, name: stored.Name, created: stored.Created}
		} else {
			s.touchDevice([32]byte(key))
		}
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
		key, err := parseVAPIDKey(encoded)
		if err != nil {
			return fmt.Errorf("stored VAPID key: %w", err)
		}
		s.vapid, s.vapidStored = key, true
		return nil
	}
	key, err := newVAPIDKey()
	if err != nil {
		return err
	}
	s.vapid, s.vapidStored = key, true
	s.dirty.vapid = true
	return nil
}

// removeStaleUploads deletes upload files in the upload directory that no
// restored embed holds, such as those of a run without persistence.
func (s *Server) removeStaleUploads(keep map[string]bool) {
	stale, _ := filepath.Glob(filepath.Join(s.uploadDir, "*"+uploadSuffix))
	for _, name := range stale {
		if !keep[name] {
			_ = os.Remove(name)
		}
	}
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
	u.enrolled = stored.Enrolled.restored()
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
