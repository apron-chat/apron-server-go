package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
)

const (
	variationSelector16 = '\uFE0F'
	combiningKeycap     = '\u20E3'
)

// qualifyEmoji returns a reaction entry in its fully-qualified form (§4.7,
// UTS #51 ED-18): without U+FE0F, then with U+FE0F after each emoji
// character of text presentation that does not start an emoji modifier
// sequence. #, *, and the digits take one only as a keycap's base. Entries
// that differ only in U+FE0F so become the same entry, and other text keeps
// every character but U+FE0F.
func qualifyEmoji(entry string) string {
	runes := []rune(strings.ReplaceAll(entry, string(variationSelector16), ""))
	var b strings.Builder
	b.Grow(len(entry))
	for i, r := range runes {
		b.WriteRune(r)
		var next rune
		if i+1 < len(runes) {
			next = runes[i+1]
		}
		if !unicode.Is(textDefaultEmoji, r) || emojiModifier(next) {
			continue
		}
		if r < 0x80 && next != combiningKeycap {
			continue
		}
		b.WriteRune(variationSelector16)
	}
	return b.String()
}

// emojiModifier reports whether r is a skin tone modifier (U+1F3FB to
// U+1F3FF).
func emojiModifier(r rune) bool {
	return r >= 0x1F3FB && r <= 0x1F3FF
}

// Custom emoji (ext:emoji in apron's EXTENSIONS.md): images users add with
// /emoji add and write as :name: in reactions and text. The server frame
// carries the URL of the catalog, a JSON object keyed by :name:, which the
// server serves at a path named after its content's hash, so the URL
// changes whenever the catalog does.
const (
	emojiPath = "/emoji/"
	// maxCustomEmoji bounds the catalog.
	maxCustomEmoji = 1000
)

// emojiKeyPattern is a custom emoji's key: a name between colons.
var emojiKeyPattern = regexp.MustCompile(`^:[A-Za-z0-9_+-]+:$`)

// customEmoji is one catalog entry: the hosted upload holding its image,
// the image's size, and the user_id of who added it.
type customEmoji struct {
	embed  *embedState
	by     string
	width  int
	height int
}

type storedEmoji struct {
	Embed  string `json:"embed"`
	By     string `json:"by"`
	Width  int    `json:"width,omitzero"`
	Height int    `json:"height,omitzero"`
}

// renderEmojiLocked renders the catalog and names the path it is served
// at, after a change to it.
func (s *Server) renderEmojiLocked() {
	catalog := make(map[string]any, len(s.emoji))
	for key, emoji := range s.emoji {
		entry := map[string]any{"url": emoji.embed.fileURL()}
		if emoji.width > 0 && emoji.height > 0 {
			entry["width"], entry["height"] = emoji.width, emoji.height
		}
		if emoji.embed.alt != "" {
			entry["alt"] = emoji.embed.alt
		}
		catalog[key] = entry
	}
	s.emojiCatalog = encodeJSON(catalog)
	sum := sha256.Sum256(s.emojiCatalog)
	s.emojiName = "catalog-" + hex.EncodeToString(sum[:8]) + ".json"
}

// emojiExt is server.ext.emoji for a connection whose URLs start with
// baseURL, or nil when clients would ignore its catalog_url: one that is
// neither https: nor http: on a loopback host, such as behind a plain-HTTP
// reverse proxy without PublicURL. The server then does not offer
// ext:emoji on that connection.
func (s *Server) emojiExt(baseURL string) map[string]any {
	catalogURL := baseURL + emojiPath + s.emojiName
	parsed, err := url.Parse(catalogURL)
	if err != nil || !(parsed.Scheme == "https" || parsed.Scheme == "http" && loopbackHost(parsed.Hostname())) {
		return nil
	}
	return map[string]any{"catalog_url": catalogURL}
}

// loopbackHost reports whether host, a URL's hostname, is a loopback host,
// as clients judge one: localhost, a name under .localhost, ::1, or an
// address in 127.0.0.0/8.
func loopbackHost(host string) bool {
	host = strings.ToLower(host)
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// emojiBytesLocked totals the images of the custom emoji, which
// MaxEmojiStorageBytes bounds.
func (s *Server) emojiBytesLocked() int64 {
	var total int64
	for _, emoji := range s.emoji {
		total += emoji.embed.size
	}
	return total
}

// emojiRoomLocked is how many bytes an image for key may take: at most
// MaxEmojiBytes, and what MaxEmojiStorageBytes leaves once the image it
// would replace is gone.
func (s *Server) emojiRoomLocked(key string) int64 {
	room := s.config.MaxEmojiStorageBytes - s.emojiBytesLocked()
	if previous := s.emoji[key]; previous != nil {
		room += previous.embed.size
	}
	return min(s.config.MaxEmojiBytes, room)
}

// mayReplaceEmojiLocked reports whether the user userID may give an
// existing key a new image: the user who added it, or an admin.
func (s *Server) mayReplaceEmojiLocked(existing *customEmoji, userID string) bool {
	if existing.by == userID {
		return true
	}
	u := s.users[userID]
	return u != nil && u.hasRole("admin")
}

// refuseEmojiLocked checks a finished /emoji add write of size bytes
// again, since the catalog may have changed since the command: the key
// may have been added by someone the writer may not replace, the catalog
// may be full, or other images may have taken the storage. It returns the
// HTTP status and message to refuse the write with, or 0.
func (s *Server) refuseEmojiLocked(e *embedState, size int64) (int, string) {
	existing := s.emoji[e.emojiKey]
	switch {
	case existing != nil && existing.embed != e && !s.mayReplaceEmojiLocked(existing, e.emojiBy):
		return http.StatusConflict, fmt.Sprintf("%s was added by someone else meanwhile; only who added it or an admin can replace it", e.emojiKey)
	case existing == nil && len(s.emoji) >= maxCustomEmoji:
		return http.StatusConflict, fmt.Sprintf("This server has %d custom emoji, the most it keeps", maxCustomEmoji)
	case size > s.emojiRoomLocked(e.emojiKey):
		return http.StatusRequestEntityTooLarge, s.emojiFullMessage()
	}
	return 0, ""
}

// emojiFullMessage explains that custom emoji images have used their
// storage.
func (s *Server) emojiFullMessage() string {
	return fmt.Sprintf("Custom emoji images are limited to %d bytes together; remove some to add more", s.config.MaxEmojiStorageBytes)
}

// setEmojiLocked makes a finished /emoji add upload its key's image,
// deleting the upload it replaces, and sends every connection the new
// server frame.
func (s *Server) setEmojiLocked(e *embedState, og map[string]any) {
	if previous := s.emoji[e.emojiKey]; previous != nil && previous.embed != e {
		s.removeEmbedLocked(previous.embed.id)
	}
	emoji := &customEmoji{embed: e, by: e.emojiBy}
	if image, ok := og["image"].(map[string]any); ok {
		emoji.width, _ = image["width"].(int)
		emoji.height, _ = image["height"].(int)
	}
	s.emoji[e.emojiKey] = emoji
	s.emojiChangedLocked()
}

// removeEmojiLocked removes a key from the catalog and deletes its image.
func (s *Server) removeEmojiLocked(key string) {
	if emoji := s.emoji[key]; emoji != nil {
		delete(s.emoji, key)
		s.removeEmbedLocked(emoji.embed.id)
		s.emojiChangedLocked()
	}
}

// emojiChangedLocked stores the catalog, renders it under a new name, and
// sends every connection a new server frame (§3.1) with its URL.
func (s *Server) emojiChangedLocked() {
	s.dirty.emoji = true
	s.renderEmojiLocked()
	frames := make(map[string]jsontext.Value)
	for c := range s.clients {
		frame, seen := frames[c.baseURL]
		if !seen {
			// A connection not offered ext:emoji has no catalog_url to
			// change.
			if s.emojiExt(c.baseURL) != nil {
				frame = notification("server", s.serverParams(c.baseURL))
			}
			frames[c.baseURL] = frame
		}
		if frame != nil {
			c.enqueue(frame)
		}
	}
}

func (s *Server) storedEmojiLocked() map[string]storedEmoji {
	stored := make(map[string]storedEmoji, len(s.emoji))
	for key, emoji := range s.emoji {
		stored[key] = storedEmoji{Embed: emoji.embed.id, By: emoji.by, Width: emoji.width, Height: emoji.height}
	}
	return stored
}

// restoreEmojiLocked rebuilds the catalog from the store, after the embeds.
// A key whose upload is gone is dropped, and so is a finished emoji upload
// that no key holds.
func (s *Server) restoreEmojiLocked(raw jsontext.Value) error {
	if raw != nil {
		var stored map[string]storedEmoji
		if err := json.Unmarshal(raw, &stored); err != nil {
			return fmt.Errorf("stored emoji: %w", err)
		}
		for key, entry := range stored {
			e := s.embeds[entry.Embed]
			if e == nil || !e.finished || e.emojiKey != key {
				s.dirty.emoji = true
				continue
			}
			s.emoji[key] = &customEmoji{embed: e, by: entry.By, width: entry.Width, height: entry.Height}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(s.embeds)) {
		if e := s.embeds[id]; e.emojiKey != "" && e.finished && (s.emoji[e.emojiKey] == nil || s.emoji[e.emojiKey].embed != e) {
			s.removeEmbedLocked(id)
		}
	}
	s.renderEmojiLocked()
	return nil
}

// handleEmoji serves the current catalog. Its path changes with its content,
// so it may be cached for good.
func (s *Server) handleEmoji(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Read with GET", http.StatusMethodNotAllowed)
		return
	}
	s.mu.RLock()
	name, catalog := s.emojiName, s.emojiCatalog
	s.mu.RUnlock()
	if strings.TrimPrefix(r.URL.Path, emojiPath) != name {
		http.NotFound(w, r)
		return
	}
	header := w.Header()
	header.Set("Content-Type", "application/json")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(catalog))
}

// emojiCommand runs /emoji add :name: with one upload embed, which adds the
// image as that key or replaces the image of a key the sender added, and
// /emoji remove :name:, which admins use to remove a key. Either sends
// every connection a new server frame: remove at once, add when the
// upload's write finishes, which checks the catalog again
// (refuseEmojiLocked).
func (s *Server) emojiCommand(c *client, _ *roomState, body map[string]any, args string) (map[string]any, *rpcError) {
	u := c.user
	action, key, _ := strings.Cut(args, " ")
	key = strings.TrimSpace(key)
	if !emojiKeyPattern.MatchString(key) || len(key) > maxEmojiBytes {
		return nil, invalidParams("Usage: /emoji add :name: with one image attached, or /emoji remove :name:; a name is letters, digits, _, +, and -, at most %d characters", maxEmojiBytes-2)
	}
	existing := s.emoji[key]
	switch strings.ToLower(action) {
	case "add":
		embeds := asList(body["embeds"])
		if len(embeds) != 1 || embeds[0].(map[string]any)["kind"] != "upload" {
			return nil, invalidParams("/emoji add takes exactly one upload embed: attach an image")
		}
		if existing != nil && !s.mayReplaceEmojiLocked(existing, u.id) {
			return nil, &rpcError{Code: codeDenied, Message: fmt.Sprintf("%s is taken; only who added it or an admin can replace it", key)}
		}
		if existing == nil && len(s.emoji) >= maxCustomEmoji {
			return nil, &rpcError{Code: codeDenied, Message: fmt.Sprintf("This server has %d custom emoji, the most it keeps", maxCustomEmoji)}
		}
		if s.emojiRoomLocked(key) <= 0 {
			return nil, &rpcError{Code: codeDenied, Message: s.emojiFullMessage()}
		}
		if err := s.admitPostLocked(u); err != nil {
			return nil, err
		}
		s.embedNumber++
		e := s.newWriteLocked(c, fmt.Sprintf("embed_%d", s.embedNumber), "upload", "")
		e.emojiKey, e.emojiBy = key, u.id
		e.alt = ogImageAlt(embeds[0].(map[string]any)["og"])
		return map[string]any{
			"embeds": []any{map[string]any{"embed_id": e.id, "kind": "upload", "write_url": e.baseURL + writePath + e.token}},
		}, nil
	case "remove":
		if !u.hasRole("admin") {
			return nil, &rpcError{Code: codeDenied, Message: "Only an admin can remove custom emoji"}
		}
		if existing == nil {
			return nil, invalidParams("There is no custom emoji %s", key)
		}
		s.removeEmojiLocked(key)
		return map[string]any{}, nil
	}
	return nil, invalidParams("Usage: /emoji add :name: with one image attached, or /emoji remove :name:")
}
