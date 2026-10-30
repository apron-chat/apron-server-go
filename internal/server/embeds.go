package server

import (
	"container/list"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // Registered for image.DecodeConfig.
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"maps"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"

	_ "golang.org/x/image/webp" // Registered for image.DecodeConfig.
)

// HTTP paths for embed content (§4.8.3). Write URLs carry a one-time token;
// file and live text URLs (livePath, live.go) carry the embed_id and an
// unguessable secret, since HTTP requests are not authenticated.
const (
	writePath = "/write/"
	filePath  = "/files/"
)

// maxEmbedsPerMessage bounds body.embeds.
const maxEmbedsPerMessage = 32

// embedState is a server-hosted embed: an upload, or live text (terminal or
// append, live.go). Other embed kinds are body content with only an
// embed_id.
type embedState struct {
	id   string
	kind string
	// messageID is the message the embed belongs to; avatarFor is set instead
	// for a /avatar upload (§4.8.6), emojiKey for an /emoji add upload,
	// which emojiBy sent (ext:emoji), and serverAvatarBy, the sender's
	// user_id, for a /server avatar upload (§3.1).
	messageID      string
	avatarFor      *userState
	emojiKey       string
	emojiBy        string
	serverAvatarBy string
	baseURL        string
	secret         string
	token          string
	title          string
	// alt is the sender's og.image.alt, kept in the server's og.
	alt string
	// timer expires an unused write URL, then bounds live text's duration.
	timer    *time.Timer
	started  bool
	finished bool
	removed  bool
	// writing is set while a writer sends live text.
	writing bool
	// owned holds the server fields restored on every save: an upload's url
	// and og, live text's url, a finished terminal's output.
	owned map[string]any
	// post is the message a /embed-terminal or /embed-append command posts
	// when its write starts; nil for an embed sent in a message.
	post *pendingPost

	// path is the file holding a finished upload's content, size its length,
	// and upload its element in Server.uploads.
	path        string
	size        int64
	upload      *list.Element
	seq         int64
	contentType string
	live        *liveText
}

// image reports whether the embed is an avatar, the server's included, or
// a custom emoji: an image of a type avatars take (§4.8.6) that belongs to
// no message.
func (e *embedState) image() bool {
	return e.avatarFor != nil || e.emojiKey != "" || e.serverAvatarBy != ""
}

// apart reports whether the upload is kept apart from Server.uploads and
// MaxUploadStorageBytes: custom emoji have a bound of their own
// (MaxEmojiStorageBytes), and the server avatar is one image.
func (e *embedState) apart() bool {
	return e.emojiKey != "" || e.serverAvatarBy != ""
}

func (e *embedState) endLive() {
	if e.live != nil {
		e.live.end()
	}
}

func (e *embedState) fileURL() string {
	return e.baseURL + filePath + e.id + "/" + e.secret
}

// resolveEmbedsLocked applies embed identity (§4.8.2) to a submitted
// body. An embed with embed_id keeps that embed of the current snapshot, with
// its kind and server-owned fields restored from the server's records; one
// without is new and gets an embed_id, and a new upload, terminal, or append
// embed gets a write URL, listed in written. Senders' og is dropped: the
// server only describes media it hosts. A message has at most one append
// embed (§4.8.5).
func (s *Server) resolveEmbedsLocked(c *client, messageID string, current map[string]any, body map[string]any) ([]any, []any, *rpcError) {
	submitted, _ := body["embeds"].([]any)
	previous := make(map[string]map[string]any)
	if current != nil {
		if currentBody, ok := current["body"].(map[string]any); ok {
			for _, value := range asList(currentBody["embeds"]) {
				if embed, ok := value.(map[string]any); ok {
					if id, ok := embed["embed_id"].(string); ok {
						previous[id] = embed
					}
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
	var written []any
	for _, value := range submitted {
		embed := cloneObject(value.(map[string]any))
		if id, has := embed["embed_id"].(string); has {
			embed["kind"] = previous[id]["kind"]
			delete(embed, "og")
			delete(embed, "write_url")
			if e := s.embeds[id]; e != nil {
				delete(embed, "url")
				delete(embed, "output")
				maps.Copy(embed, cloneObject(e.owned))
			}
			embeds = append(embeds, embed)
			continue
		}
		kind := embed["kind"].(string)
		s.embedNumber++
		id := fmt.Sprintf("embed_%d", s.embedNumber)
		alt := ogImageAlt(embed["og"])
		delete(embed, "og")
		delete(embed, "write_url")
		embed["embed_id"] = id
		if kind == "upload" || liveKind(kind) {
			delete(embed, "url")
			delete(embed, "output")
			e := s.newWriteLocked(c, id, kind, messageID)
			e.alt = alt
			e.title, _ = embed["title"].(string)
			if liveKind(kind) {
				text, _ := body["text"].(string)
				s.startLiveLocked(e, text)
				embed["url"] = e.liveURL()
			}
			written = append(written, map[string]any{"embed_id": id, "kind": kind, "write_url": e.writeURL()})
		}
		embeds = append(embeds, embed)
	}
	return embeds, written, nil
}

func (e *embedState) writeURL() string {
	return e.baseURL + writePath + e.token
}

func asList(value any) []any {
	list, _ := value.([]any)
	return list
}

func ogImageAlt(og any) string {
	if og, ok := og.(map[string]any); ok {
		if image, ok := og["image"].(map[string]any); ok {
			alt, _ := image["alt"].(string)
			return alt
		}
	}
	return ""
}

// newWriteLocked registers a hosted embed awaiting its write. An unused write
// URL expires, and the embed is dropped.
func (s *Server) newWriteLocked(c *client, id, kind, messageID string) *embedState {
	e := &embedState{
		id:        id,
		kind:      kind,
		messageID: messageID,
		baseURL:   c.baseURL,
		secret:    rand.Text(),
		token:     rand.Text(),
	}
	s.embeds[id] = e
	s.touchEmbed(id)
	s.writes[e.token] = e
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
func (s *Server) releaseEmbedsLocked(current map[string]any, body map[string]any) {
	if current == nil {
		return
	}
	currentBody, _ := current["body"].(map[string]any)
	remaining := make(map[string]bool)
	for _, value := range asList(body["embeds"]) {
		if id, ok := value.(map[string]any)["embed_id"].(string); ok {
			remaining[id] = true
		}
	}
	for _, value := range asList(currentBody["embeds"]) {
		if embed, ok := value.(map[string]any); ok {
			if id, ok := embed["embed_id"].(string); ok && !remaining[id] {
				s.removeEmbedLocked(id)
			}
		}
	}
}

// removeEmbedLocked forgets a hosted embed and its content, invalidating its
// write, file, and live text URLs.
func (s *Server) removeEmbedLocked(id string) {
	e := s.embeds[id]
	if e == nil {
		return
	}
	e.removed = true
	if e.timer != nil {
		e.timer.Stop()
	}
	e.endLive()
	if e.upload != nil {
		s.uploads.Remove(e.upload)
		e.upload = nil
		s.uploadBytes -= e.size
	}
	if e.path != "" {
		_ = os.Remove(e.path)
	}
	delete(s.writes, e.token)
	delete(s.embeds, id)
	s.touchEmbed(id)
}

// failWriteLocked finishes a write that never started or failed: the embed is
// published out of its message (§4.8.3). A command's pending message is
// never posted.
func (s *Server) failWriteLocked(e *embedState) {
	if m := s.messages[e.messageID]; m != nil {
		s.republishLocked(m, func(body map[string]any) bool {
			return replaceEmbed(body, e.id, nil)
		})
		s.removeEmbedLocked(e.id)
		s.releasePushLocked(m)
		return
	}
	s.removeEmbedLocked(e.id)
}

// replaceEmbed swaps one embed in a body for update, or removes it when
// update is nil. It reports whether the embed was present.
func replaceEmbed(body map[string]any, id string, update func(map[string]any)) bool {
	embeds := asList(body["embeds"])
	for i, value := range embeds {
		embed, ok := value.(map[string]any)
		if !ok || embed["embed_id"] != id {
			continue
		}
		if update == nil {
			embeds = append(embeds[:i:i], embeds[i+1:]...)
			if len(embeds) == 0 {
				delete(body, "embeds")
			} else {
				body["embeds"] = embeds
			}
		} else {
			update(embed)
		}
		return true
	}
	return false
}

// setAvatarEmbedLocked records the hosted upload behind a user's avatar,
// deleting the content of the one it replaces.
func (s *Server) setAvatarEmbedLocked(u *userState, e *embedState) {
	if u.avatarEmbed != nil && u.avatarEmbed != e {
		s.removeEmbedLocked(u.avatarEmbed.id)
	}
	u.avatarEmbed = e
	s.touchUser(u.id)
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
	e := s.writes[token]
	if e == nil || e.removed || e.finished || (e.started && e.live == nil) {
		s.unlock()
		refuseWrite(w, "This write URL is unknown, used, or expired", http.StatusNotFound)
		return
	}
	if e.live != nil {
		if status, message := s.startWriterLocked(e); status != 0 {
			s.unlock()
			refuseWrite(w, message, status)
			return
		}
		s.unlock()
		s.writeLive(w, r, e)
		return
	}
	e.started = true
	delete(s.writes, token)
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
	case e.avatarFor != nil, e.serverAvatarBy != "":
		limit = s.config.MaxAvatarBytes
	case e.emojiKey != "":
		limit = s.emojiRoomLocked(e.emojiKey)
		if limit < s.config.MaxEmojiBytes {
			tooLargeMessage = s.emojiFullMessage()
		}
	default:
		limit = min(limit, s.config.MaxMessageUploadBytes-s.messageUploadBytesLocked(e.messageID))
	}
	s.mu.RUnlock()
	if tooLargeMessage == "" {
		tooLargeMessage = fmt.Sprintf("This upload is limited to %d bytes", limit)
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(uploadReadTimeout))
	file, size, err := s.receiveUpload(w, r, limit)
	var head []byte
	if file != nil {
		head = make([]byte, 512)
		n, _ := file.ReadAt(head, 0)
		head = head[:n]
	}
	contentType := uploadContentType(r.Header.Get("Content-Type"), head)
	if err == nil && e.image() && !validAvatarImage(contentType, file) {
		err = errNotAvatar
	}
	var og map[string]any
	if err == nil {
		e.contentType = contentType
		og = describeUpload(e, file)
	}
	if file != nil {
		file.Close()
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
	case e.removed:
		fail(http.StatusGone, "The embed was removed from its message")
		return
	case errors.As(err, &tooLarge):
		fail(http.StatusRequestEntityTooLarge, tooLargeMessage)
		return
	case errors.Is(err, errNotAvatar):
		fail(http.StatusUnsupportedMediaType, "Avatars and emoji must be PNG, JPEG, GIF, or WebP images")
		return
	case err != nil:
		fail(http.StatusBadRequest, "The upload did not finish")
		return
	case !e.image() && s.messageUploadBytesLocked(e.messageID)+size > s.config.MaxMessageUploadBytes:
		// Another upload for the message finished while this one arrived.
		fail(http.StatusRequestEntityTooLarge, fmt.Sprintf("A message's uploads are limited to %d bytes", s.config.MaxMessageUploadBytes))
		return
	}
	if e.emojiKey != "" {
		if status, message := s.refuseEmojiLocked(e, size); status != 0 {
			fail(status, message)
			return
		}
	}
	if e.serverAvatarBy != "" {
		if status, message := s.refuseServerAvatarLocked(e); status != 0 {
			fail(status, message)
			return
		}
	}
	e.finished = true
	e.path = file.Name()
	e.size = size
	if !e.apart() {
		e.upload = s.uploads.PushBack(e)
		s.uploadBytes += size
	}
	s.uploadSeq++
	e.seq = s.uploadSeq
	s.touchEmbed(e.id)
	e.owned = map[string]any{"url": e.fileURL()}
	if og != nil {
		e.owned["og"] = og
	}
	if u := e.avatarFor; u != nil {
		if s.users[u.id] == u {
			s.setAvatarEmbedLocked(u, e)
			u.avatar = e.fileURL()
			s.touchUser(u.id)
			s.notifyProfileLocked(u, nil, nil)
		} else {
			s.removeEmbedLocked(e.id)
		}
	} else if e.emojiKey != "" {
		s.setEmojiLocked(e, og)
	} else if e.serverAvatarBy != "" {
		s.setServerAvatarLocked(e)
	} else if m := s.messages[e.messageID]; m != nil {
		s.republishLocked(m, func(body map[string]any) bool {
			return replaceEmbed(body, e.id, func(embed map[string]any) {
				maps.Copy(embed, cloneObject(e.owned))
			})
		})
	}
	s.evictUploadsLocked(e)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = fmt.Fprintf(w, "{\"url\":%q}\n", e.fileURL())
}

var errNotAvatar = errors.New("not an avatar image")

// receiveUpload copies a request body of at most limit bytes into a new file
// in the upload directory. On error the file, if any, is still returned so
// the caller can remove it.
func (s *Server) receiveUpload(w http.ResponseWriter, r *http.Request, limit int64) (*os.File, int64, error) {
	if limit <= 0 {
		return nil, 0, &http.MaxBytesError{Limit: limit}
	}
	file, err := os.CreateTemp(s.uploadDir, "*"+uploadSuffix)
	if err != nil {
		slog.Error("cannot store upload", "error", err)
		return nil, 0, err
	}
	size, err := io.Copy(file, http.MaxBytesReader(w, r.Body, limit))
	return file, size, err
}

// messageUploadBytesLocked totals the finished uploads of one message.
func (s *Server) messageUploadBytesLocked(messageID string) int64 {
	var total int64
	for element := s.uploads.Front(); element != nil; element = element.Next() {
		if e := element.Value.(*embedState); e.messageID == messageID && !e.image() {
			total += e.size
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
	for element := s.uploads.Front(); element != nil && s.uploadBytes > s.config.MaxUploadStorageBytes; {
		e := element.Value.(*embedState)
		element = element.Next()
		if e == keep || e.image() {
			continue
		}
		if _, seen := evicted[e.messageID]; !seen {
			order = append(order, e.messageID)
		}
		evicted[e.messageID] = append(evicted[e.messageID], e.id)
		s.removeEmbedLocked(e.id)
	}
	for _, messageID := range order {
		if m := s.messages[messageID]; m != nil {
			ids := evicted[messageID]
			s.republishLocked(m, func(body map[string]any) bool {
				changed := false
				for _, id := range ids {
					changed = replaceEmbed(body, id, nil) || changed
				}
				return changed
			})
		}
	}
}

// uploadSuffix names upload files, so stale ones can be told apart from
// anything else in the directory.
const uploadSuffix = ".upload"

// openUploadDir prepares the upload directory: the configured one, or a new
// temporary one. Upload files no restored embed holds are removed after the
// state is restored.
func (s *Server) openUploadDir() {
	dir := s.config.UploadDir
	if dir == "" {
		temp, err := os.MkdirTemp("", "aprond-uploads-")
		if err != nil {
			slog.Error("cannot create a temporary upload directory", "error", err)
			return
		}
		s.uploadDir, s.tempUploadDir = temp, true
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Error("cannot create the upload directory", "dir", dir, "error", err)
	}
	s.uploadDir = dir
}

// uploadContentType prefers the sender's declared type, except for types a
// browser would run as a document, and otherwise sniffs the content.
func uploadContentType(declared string, head []byte) string {
	if mediaType, _, err := mime.ParseMediaType(declared); err == nil && mediaType != "application/octet-stream" {
		if !activeType(mediaType) {
			return mediaType
		}
	}
	mediaType, _, _ := mime.ParseMediaType(http.DetectContentType(head))
	if activeType(mediaType) {
		return "application/octet-stream"
	}
	return mediaType
}

// activeType reports whether a browser could run or apply content of the
// type on this origin: markup, scripts, stylesheets, and WebAssembly.
func activeType(mediaType string) bool {
	for _, marker := range []string{"html", "xml", "xsl", "script", "jscript", "css", "wasm"} {
		if strings.Contains(mediaType, marker) {
			return true
		}
	}
	return false
}

// validAvatarImage checks that an avatar upload's content decodes as the
// image type it declares.
func validAvatarImage(contentType string, file *os.File) bool {
	if !avatarType(contentType) || file == nil {
		return false
	}
	_, format, err := image.DecodeConfig(io.NewSectionReader(file, 0, 1<<30))
	return err == nil && "image/"+format == contentType
}

func avatarType(contentType string) bool {
	switch contentType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// describeUpload builds the og the server sets on a finished upload
// (§4.8.4): image for a preview, or video or audio to play. Other files
// have none, so clients show a file card.
func describeUpload(e *embedState, file *os.File) map[string]any {
	media := map[string]any{"url": e.fileURL(), "type": e.contentType}
	og := map[string]any{}
	switch {
	case avatarType(e.contentType):
		if config, _, err := image.DecodeConfig(io.NewSectionReader(file, 0, 1<<30)); err == nil {
			media["width"], media["height"] = config.Width, config.Height
		}
		if e.alt != "" {
			media["alt"] = e.alt
		}
		og["image"] = media
	case strings.HasPrefix(e.contentType, "video/"):
		og["video"] = media
	case strings.HasPrefix(e.contentType, "audio/"):
		og["audio"] = media
	default:
		return nil
	}
	if e.title != "" {
		og["title"] = e.title
	}
	return og
}

// inlineType reports whether a hosted file may be shown in the browser
// rather than downloaded.
func inlineType(contentType string) bool {
	return avatarType(contentType) || contentType == "text/plain" ||
		strings.HasPrefix(contentType, "video/") || strings.HasPrefix(contentType, "audio/")
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
	if e == nil || !e.finished || e.kind != "upload" {
		s.mu.RUnlock()
		http.NotFound(w, r)
		return
	}
	name, contentType, title := e.path, e.contentType, e.title
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
	if inlineType(contentType) {
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
	e := s.embeds[id]
	if !ok || e == nil || subtle.ConstantTimeCompare([]byte(secret), []byte(e.secret)) != 1 {
		return nil
	}
	return e
}
