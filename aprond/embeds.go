package aprond

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"mime"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/aprond/internal/embeds"
	"github.com/apron-chat/apron-server-go/aprond/internal/livetext"
	"github.com/apron-chat/apron-server-go/aprond/internal/roomlog"
	"github.com/apron-chat/apron-server-go/aprond/internal/uploads"
)

// HTTP paths for embed content (§4.8.3). Write URLs carry a one-time token;
// file and live text URLs (livePath, live.go) carry the embed_id and an
// unguessable secret, since HTTP requests are not authenticated.
const (
	writePath = "/write/"
	filePath  = "/files/"
)

// embedState is a server-hosted embed: an upload, or live text (terminal or
// append, live.go). Other embed kinds are body content with only an
// embed_id. An embeds.Table keeps them, with what the store keeps of each
// (embeds.Embed), and the server keeps what ties them to users, messages,
// and live text.
type embedState struct {
	embeds.Embed
	// avatarFor is set for a /avatar upload (§4.8.6), and emojiBy for an
	// /emoji add upload, who sent it (ext:emoji).
	avatarFor *userState
	emojiBy   string
	// token is the one-time token of the write URL.
	token string
	// timer expires an unused write URL, then bounds live text's duration.
	timer   *time.Timer
	started bool
	// timedOut is set when live text ends at LiveTextMaxDuration.
	timedOut bool
	removed  bool
	// writing is set while a writer sends live text.
	writing bool
	// post is the message a /embed-terminal or /embed-append command posts
	// when its write starts; nil for an embed sent in a message.
	post *pendingPost
	// author is the user whose request made the write URL. Until the write
	// finishes, the server checks that the author may still write it
	// (writeAllowedLocked), and ends it when they may not (endWritesLocked).
	// revoked is set when it ended that way.
	author  *userState
	revoked bool
	// live holds a terminal's or an append's text until it finishes; a
	// finished terminal serves its output from Owned. Restored embeds have
	// none.
	live *livetext.Text
}

// Hosted is what the store keeps of the embed (embeds.Hosted).
func (e *embedState) Hosted() *embeds.Embed { return &e.Embed }

// avatarOwner names the user an avatar upload is for in the store, "" for
// an embed that is no avatar.
func avatarOwner(e *embedState) string {
	if e.avatarFor == nil {
		return ""
	}
	return e.avatarFor.id
}

// image reports whether the embed is an avatar, the server's included, or
// a custom emoji: an image of a type avatars take (§4.8.6) that belongs to
// no message.
func (e *embedState) image() bool {
	return e.avatarFor != nil || e.EmojiKey != "" || e.ServerAvatarBy != ""
}

func (e *embedState) endLive() {
	if e.live != nil {
		e.live.End()
	}
}

func (e *embedState) fileURL() string {
	return e.BaseURL + filePath + e.ID + "/" + e.Secret
}

// resolveEmbedsLocked applies embed identity (§4.8.2) to a submitted
// body. An embed with embed_id keeps that embed of the current snapshot, with
// its kind and server-owned fields restored from the server's records; one
// without is new and gets an embed_id, and a new upload, terminal, or append
// embed gets a write URL, listed in written. Senders' og is dropped: the
// server only describes media it hosts. A message has at most one append
// embed (§4.8.5).
func (s *Server) resolveEmbedsLocked(c *client, messageID string, current *roomlog.Message, body map[string]any) ([]any, []apron.EmbedWrite, *Error) {
	submitted, _ := body["embeds"].([]any)
	previous := make(map[string]map[string]any)
	if current != nil && current.Body != nil {
		for _, raw := range current.Body.Embeds {
			var embed map[string]any
			if json.Unmarshal(raw, &embed) == nil {
				if id, ok := embed["embed_id"].(string); ok {
					previous[id] = embed
				}
			}
		}
	}
	kept := make(map[string]bool)
	appends := 0
	for i, value := range submitted {
		embed := value.(map[string]any)
		raw, has := embed["embed_id"]
		if !has {
			if embed["kind"] == kindAppend {
				appends++
			}
			continue
		}
		id, _ := raw.(string)
		if previous[id] == nil || kept[id] {
			return nil, nil, invalidParams("body.embeds[%d]: unknown embed_id %v", i, raw)
		}
		kept[id] = true
		if previous[id]["kind"] == kindAppend {
			appends++
		}
	}
	if appends > 1 {
		return nil, nil, invalidParams("A message has at most one append embed")
	}

	embeds := make([]any, 0, len(submitted))
	var written []apron.EmbedWrite
	for _, value := range submitted {
		embed := cloneObject(value.(map[string]any))
		if id, has := embed["embed_id"].(string); has {
			embed["kind"] = previous[id]["kind"]
			delete(embed, "og")
			delete(embed, "write_url")
			if e := s.embeds.Get(id); e != nil {
				delete(embed, "url")
				delete(embed, "output")
				if e.Kind == kindTerminal && e.Owned != nil {
					// A renamed terminal's og follows its title.
					if title, _ := embed["title"].(string); title != e.Title {
						e.Title = title
						e.Owned["og"] = e.terminalOG()
						s.embeds.Touch(id)
					}
				}
				maps.Copy(embed, cloneObject(e.Owned))
			}
			embeds = append(embeds, embed)
			continue
		}
		kind := embed["kind"].(string)
		id := s.embeds.NewID()
		alt := ogImageAlt(embed["og"])
		delete(embed, "og")
		delete(embed, "write_url")
		embed["embed_id"] = id
		if kind == "upload" || liveKind(kind) {
			delete(embed, "url")
			delete(embed, "output")
			e := s.newWriteLocked(c, id, kind, messageID)
			e.Alt = alt
			e.Title, _ = embed["title"].(string)
			if liveKind(kind) {
				text, _ := body["text"].(string)
				s.startLiveLocked(e, text)
				// A terminal's og, which says what it is (§4.8.1).
				maps.Copy(embed, cloneObject(e.Owned))
			}
			written = append(written, e.write())
		}
		embeds = append(embeds, embed)
	}
	return embeds, written, nil
}

func (e *embedState) writeURL() string {
	return e.BaseURL + writePath + e.token
}

// write is the embed's write, as the result of the request that reserved
// it lists it for the sender (§4.8.3).
func (e *embedState) write() apron.EmbedWrite {
	return apron.EmbedWrite{EmbedID: e.ID, Kind: e.Kind, WriteURL: e.writeURL()}
}

// writeResult is the result of a command that made the embed's write: its
// write URL (§4.8.3).
func (e *embedState) writeResult() apron.CommandResult {
	return apron.CommandResult{Embeds: []apron.EmbedWrite{e.write()}}
}

func asList(value any) []any {
	list, _ := value.([]any)
	return list
}

func ogImageAlt(og any) string {
	if og, ok := og.(map[string]any); ok {
		if image, ok := og["image"].(map[string]any); ok {
			alt, _ := image["alt"].(string)
			if len(alt) > maxAltBytes {
				// Alt text describes an image in a sentence or two; the
				// server keeps it, and the emoji catalog serves it to all.
				alt = string(livetext.WholeRunes([]byte(alt[:maxAltBytes])))
			}
			return alt
		}
	}
	return ""
}

// maxAltBytes bounds the alt text the server keeps for an upload's image.
const maxAltBytes = 1 << 10

// newWriteLocked registers a hosted embed awaiting its write. An unused write
// URL expires, and the embed is dropped.
func (s *Server) newWriteLocked(c *client, id, kind, messageID string) *embedState {
	e := &embedState{
		Embed:  embeds.Embed{ID: id, Kind: kind, MessageID: messageID, BaseURL: c.baseURL, Secret: rand.Text()},
		token:  rand.Text(),
		author: c.user,
	}
	if c.user.writes == nil {
		c.user.writes = make(map[*embedState]struct{})
	}
	c.user.writes[e] = struct{}{}
	s.embeds.Add(e)
	s.embeds.AddWrite(e.token, e)
	e.timer = time.AfterFunc(s.config.UploadStartTimeout, func() {
		s.mu.Lock()
		defer s.unlock()
		if !e.started && !e.removed {
			s.failWriteLocked(e)
		}
	})
	return e
}

// releaseEmbedsLocked deletes the hosted content of embeds a save removed
// from the current snapshot (§4.8.2); a nil body, as for a tombstone,
// removes every embed.
func (s *Server) releaseEmbedsLocked(current *roomlog.Message, body map[string]any) {
	if current == nil || current.Body == nil {
		return
	}
	remaining := make(map[string]bool)
	for _, value := range asList(body["embeds"]) {
		if id, ok := value.(map[string]any)["embed_id"].(string); ok {
			remaining[id] = true
		}
	}
	for _, raw := range current.Body.Embeds {
		if id := embedOf(raw).EmbedID; id != "" && !remaining[id] {
			s.removeEmbedLocked(id)
		}
	}
}

// embedRef is what the server reads of an embed object in a snapshot.
type embedRef struct {
	EmbedID string `json:"embed_id"`
	Kind    string `json:"kind"`
}

// embedOf reads an embed object of a snapshot's body, zero for one that is
// no object.
func embedOf(raw jsontext.Value) embedRef {
	var ref embedRef
	_ = json.Unmarshal(raw, &ref)
	return ref
}

// removeEmbedLocked forgets a hosted embed and its content, invalidating its
// write, file, and live text URLs.
func (s *Server) removeEmbedLocked(id string) {
	e := s.embeds.Remove(id)
	if e == nil {
		return
	}
	e.removed = true
	if e.timer != nil {
		e.timer.Stop()
	}
	e.endLive()
	if e.Path != "" {
		_ = os.Remove(e.Path)
	}
	s.embeds.EndWrite(e.token)
	e.doneWriting()
}

// doneWriting forgets the write as one its author has outstanding.
func (e *embedState) doneWriting() {
	if e.author != nil {
		delete(e.author.writes, e)
	}
}

// writeAllowedLocked reports whether e's author may still write it: their
// account may sign in (an allowlist names it), and they can see the room the
// message is, or will be, posted in. Avatars, emoji, and the server avatar
// belong to no room. It is checked when a write starts, when an upload
// finishes, and when the author's access narrows (endWritesLocked).
func (s *Server) writeAllowedLocked(e *embedState) bool {
	u := e.author
	if u == nil {
		return true
	}
	if u.retired {
		// A guest that left, as by reloading its page, has no access left
		// to lose: what it started goes on, as it did before it left.
		return true
	}
	if !s.admittedLocked(u) {
		return false
	}
	roomID := ""
	if e.post != nil {
		roomID = e.post.roomID
	} else if m := s.rooms.Message(e.MessageID); m != nil {
		roomID = m.RoomID()
	}
	if roomID == "" {
		return true
	}
	r := s.rooms.Get(roomID)
	return r != nil && r.VisibleTo(u)
}

// endWritesLocked ends u's outstanding writes that keep refuses, or all of
// them when keep is nil, as when u is signed out. A write that had not
// started, or an upload still arriving, fails as an expired one does
// (§4.8.3): its embed leaves its message, and a command's message is never
// posted. Live text being written finishes with what arrived, and its
// writer's next write fails.
func (s *Server) endWritesLocked(u *userState, keep func(*embedState) bool) {
	for e := range u.writes {
		if e.removed || e.Finished {
			e.doneWriting()
			continue
		}
		if keep != nil && keep(e) {
			continue
		}
		e.revoked = true
		if e.live != nil && e.started {
			s.finishLiveLocked(e)
			continue
		}
		s.failWriteLocked(e)
	}
}

// endRefusedWritesLocked ends u's writes that u may no longer make, after
// u's access narrowed.
func (s *Server) endRefusedWritesLocked(u *userState) {
	if len(u.writes) > 0 {
		s.endWritesLocked(u, s.writeAllowedLocked)
	}
}

// failWriteLocked finishes a write that never started or failed: the embed is
// published out of its message (§4.8.3). A command's pending message is
// never posted.
func (s *Server) failWriteLocked(e *embedState) {
	if m := s.rooms.Message(e.MessageID); m != nil {
		s.republishLocked(m, func(body *roomlog.Body) bool {
			return replaceEmbed(body, e.ID, nil)
		})
		s.removeEmbedLocked(e.ID)
		s.releasePushLocked(m)
		return
	}
	s.removeEmbedLocked(e.ID)
}

// replaceEmbed swaps one embed in a body for update, which edits it as an
// object, or removes it when update is nil; the other embeds stay as they
// are. It reports whether the embed was present. A body left without
// embeds has no embeds member, as one sent without.
func replaceEmbed(body *roomlog.Body, id string, update func(map[string]any)) bool {
	for i, raw := range body.Embeds {
		if embedOf(raw).EmbedID != id {
			continue
		}
		embeds := slices.Clone(body.Embeds)
		if update == nil {
			embeds = slices.Delete(embeds, i, i+1)
		} else {
			var embed map[string]any
			_ = json.Unmarshal(raw, &embed)
			update(embed)
			embeds[i] = encodeJSON(embed)
		}
		body.Embeds = embeds
		if len(embeds) == 0 {
			object := bodyObject(&body.Body)
			delete(object, "embeds")
			*body = *storedBody(object)
		}
		return true
	}
	return false
}

// setAvatarEmbedLocked records the hosted upload behind a user's avatar,
// deleting the content of the one it replaces.
func (s *Server) setAvatarEmbedLocked(u *userState, e *embedState) {
	if u.avatarEmbed != nil && u.avatarEmbed != e {
		s.removeEmbedLocked(u.avatarEmbed.ID)
	}
	u.avatarEmbed = e
	s.users.Touch(u.id)
}

func setEmbedCORS(w http.ResponseWriter, methods string) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", methods)
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
}

// handleWrite accepts an upload body at its write URL, once, or live text
// (writeLive).
func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request) {
	setEmbedCORS(w, "PUT, OPTIONS")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", "PUT, OPTIONS")
		refuseWrite(w, "Write with PUT", http.StatusMethodNotAllowed)
		return
	}
	token := strings.TrimPrefix(r.URL.Path, writePath)
	s.mu.Lock()
	e := s.embeds.Write(token)
	if e == nil || e.removed || e.Finished || (e.started && e.live == nil) {
		s.unlock()
		refuseWrite(w, "This write URL is unknown, used, or expired", http.StatusNotFound)
		return
	}
	if !s.writeAllowedLocked(e) {
		e.revoked = true
		s.failWriteLocked(e)
		s.unlock()
		refuseWrite(w, writeRevokedMessage, http.StatusForbidden)
		return
	}
	if e.live != nil {
		if status, message := s.startWriterLocked(e); status != 0 {
			s.unlock()
			refuseWrite(w, message, status)
			return
		}
		live := e.live
		s.unlock()
		s.writeLive(w, r, e, live)
		return
	}
	e.started = true
	s.embeds.EndWrite(token)
	e.timer.Stop()
	s.unlock()
	s.writeUpload(w, r, e)
}

// refuseWrite answers a write without reading its body, and closes the
// connection. net/http would otherwise read what is left of the body before
// answering, to reuse the connection, and wait for as long as the writer
// holds the body open, such as a live text writer waiting for the answer.
func refuseWrite(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Connection", "close")
	_ = http.NewResponseController(w).SetReadDeadline(time.Now())
	http.Error(w, message, status)
}

func (s *Server) writeUpload(w http.ResponseWriter, r *http.Request, e *embedState) {
	limit := s.config.MaxUploadBytes
	tooLargeMessage := ""
	s.mu.RLock()
	switch {
	case e.avatarFor != nil, e.ServerAvatarBy != "":
		limit = s.config.MaxAvatarBytes
	case e.EmojiKey != "":
		limit = s.emojiRoomLocked(e.EmojiKey)
		if limit < s.config.MaxEmojiBytes {
			tooLargeMessage = s.emojiFullMessage()
		}
	default:
		limit = min(limit, s.config.MaxMessageUploadBytes-s.messageUploadBytesLocked(e.MessageID))
	}
	s.mu.RUnlock()
	if tooLargeMessage == "" {
		tooLargeMessage = fmt.Sprintf("This upload is limited to %d bytes", limit)
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(uploadReadTimeout))
	file, size, err := s.files.Receive(w, r, limit)
	// What arrived counts as arriving until the upload is recorded, and so
	// counted among the held uploads (embeds.Table.Hold), or its file
	// removed.
	defer s.files.Release(size)
	var head []byte
	if file != nil {
		head = make([]byte, 512)
		n, _ := file.ReadAt(head, 0)
		head = head[:n]
	}
	contentType := uploads.ContentType(r.Header.Get("Content-Type"), head)
	if err == nil && e.image() && !uploads.ValidImage(contentType, file) {
		err = errNotAvatar
	}
	var og map[string]any
	if err == nil {
		e.ContentType = contentType
		og = uploads.Describe(e.fileURL(), e.ContentType, e.Title, e.Alt, file)
	}
	if file != nil {
		// A finished upload is on disk before the store records it, so a
		// crash cannot leave a stored embed pointing at a short file.
		// One that failed is removed instead.
		if err == nil {
			if syncErr := file.Sync(); syncErr != nil {
				slog.Error("cannot store upload", "error", syncErr)
				err = uploads.ErrStore
			}
		}
		if closeErr := file.Close(); err == nil && closeErr != nil {
			slog.Error("cannot store upload", "error", closeErr)
			err = uploads.ErrStore
		}
	}
	s.mu.Lock()
	defer s.unlock()
	fail := func(status int, message string) {
		if file != nil {
			_ = os.Remove(file.Name())
		}
		if !e.removed {
			s.failWriteLocked(e)
		}
		http.Error(w, message, status)
	}
	var tooLarge *http.MaxBytesError
	switch {
	case e.revoked:
		fail(http.StatusForbidden, writeRevokedMessage)
		return
	case e.removed:
		fail(http.StatusGone, "The embed was removed from its message")
		return
	case !s.writeAllowedLocked(e):
		e.revoked = true
		fail(http.StatusForbidden, writeRevokedMessage)
		return
	case errors.As(err, &tooLarge):
		fail(http.StatusRequestEntityTooLarge, tooLargeMessage)
		return
	case errors.Is(err, errNotAvatar):
		fail(http.StatusUnsupportedMediaType, "Avatars and emoji must be PNG, JPEG, GIF, or WebP images")
		return
	case errors.Is(err, uploads.ErrArriving):
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
		fail(http.StatusInsufficientStorage, "The server is receiving too many uploads at once; try again shortly")
		return
	case errors.Is(err, uploads.ErrStore):
		fail(http.StatusInternalServerError, "The server could not store the upload")
		return
	case err != nil:
		fail(http.StatusBadRequest, "The upload did not finish")
		return
	case !e.image() && s.messageUploadBytesLocked(e.MessageID)+size > s.config.MaxMessageUploadBytes:
		// Another upload for the message finished while this one arrived.
		fail(http.StatusRequestEntityTooLarge, fmt.Sprintf("A message's uploads are limited to %d bytes", s.config.MaxMessageUploadBytes))
		return
	}
	if e.EmojiKey != "" {
		if status, message := s.refuseEmojiLocked(e, size); status != 0 {
			fail(status, message)
			return
		}
	}
	if e.ServerAvatarBy != "" {
		if status, message := s.refuseServerAvatarLocked(e); status != 0 {
			fail(status, message)
			return
		}
	}
	e.Finished = true
	e.doneWriting()
	e.Path = file.Name()
	e.Size = size
	s.embeds.Hold(e)
	e.Seq = s.embeds.NextSeq()
	s.embeds.Touch(e.ID)
	e.Owned = map[string]any{"url": e.fileURL()}
	if og != nil {
		e.Owned["og"] = og
	}
	if u := e.avatarFor; u != nil {
		if s.users.Get(u.id) == u {
			before := u.views()
			s.setAvatarEmbedLocked(u, e)
			u.Avatar = e.fileURL()
			s.users.Touch(u.id)
			s.notifyProfileLocked(u, nil, before)
		} else {
			s.removeEmbedLocked(e.ID)
		}
	} else if e.EmojiKey != "" {
		s.setEmojiLocked(e, og)
	} else if e.ServerAvatarBy != "" {
		s.setServerAvatarLocked(e)
	} else if m := s.rooms.Message(e.MessageID); m != nil {
		s.republishLocked(m, func(body *roomlog.Body) bool {
			return replaceEmbed(body, e.ID, func(embed map[string]any) {
				maps.Copy(embed, cloneObject(e.Owned))
			})
		})
	}
	s.evictUploadsLocked(e)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = fmt.Fprintf(w, "{\"url\":%q}\n", e.fileURL())
}

var errNotAvatar = errors.New("not an avatar image")

// writeRevokedMessage refuses a write whose author may no longer make it
// (writeAllowedLocked).
const writeRevokedMessage = "The account that made this write URL may no longer write here"

// messageUploadBytesLocked totals the finished uploads of one message.
func (s *Server) messageUploadBytesLocked(messageID string) int64 {
	var total int64
	for e := range s.embeds.Uploads() {
		if e.MessageID == messageID && !e.image() {
			total += e.Size
		}
	}
	return total
}

// evictUploadsLocked removes the oldest message uploads, other than keep,
// while hosted uploads exceed MaxUploadStorageBytes. Each affected message
// is republished once without its evicted embeds, as for a failed write
// (§4.8.3). Avatars count toward the total but are never removed; custom
// emoji and the server avatar are not in it.
func (s *Server) evictUploadsLocked(keep *embedState) {
	evicted := make(map[string][]string)
	var order []string
	for e := range s.embeds.Uploads() {
		if s.embeds.UploadBytes() <= s.config.MaxUploadStorageBytes {
			break
		}
		if e == keep || e.image() {
			continue
		}
		if _, seen := evicted[e.MessageID]; !seen {
			order = append(order, e.MessageID)
		}
		evicted[e.MessageID] = append(evicted[e.MessageID], e.ID)
		s.removeEmbedLocked(e.ID)
	}
	for _, messageID := range order {
		if m := s.rooms.Message(messageID); m != nil {
			ids := evicted[messageID]
			s.republishLocked(m, func(body *roomlog.Body) bool {
				changed := false
				for _, id := range ids {
					changed = replaceEmbed(body, id, nil) || changed
				}
				return changed
			})
		}
	}
}

// handleFile serves a finished upload. Hosted files never run as documents on
// this origin: they are sandboxed, not sniffed, and downloaded unless they
// are media or plain text.
func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Read with GET", http.StatusMethodNotAllowed)
		return
	}
	s.mu.RLock()
	e := s.lookupEmbedLocked(strings.TrimPrefix(r.URL.Path, filePath))
	if e == nil || !e.Finished || e.Kind != "upload" {
		s.mu.RUnlock()
		http.NotFound(w, r)
		return
	}
	name, contentType, title := e.Path, e.ContentType, e.Title
	// An upload removed after this still reads from the open file.
	content, err := os.Open(name)
	s.mu.RUnlock()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer content.Close()
	header := w.Header()
	header.Set("Content-Type", contentType)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	header.Set("Cache-Control", "private, max-age=3600")
	disposition := "attachment"
	if uploads.Inline(contentType) {
		disposition = "inline"
	}
	if title != "" {
		disposition = mime.FormatMediaType(disposition, map[string]string{"filename": title})
	}
	header.Set("Content-Disposition", disposition)
	http.ServeContent(w, r, "", time.Time{}, content)
}

// lookupEmbedLocked resolves "embed_id/secret" from a file or live text path.
func (s *Server) lookupEmbedLocked(path string) *embedState {
	id, secret, ok := strings.Cut(path, "/")
	e := s.embeds.Get(id)
	if !ok || e == nil || subtle.ConstantTimeCompare([]byte(secret), []byte(e.Secret)) != 1 {
		return nil
	}
	return e
}
