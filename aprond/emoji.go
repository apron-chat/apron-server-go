package aprond

import (
	"bytes"
	"encoding/json/jsontext"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/aprond/internal/emoji"
	"github.com/apron-chat/apron-server-go/aprond/internal/messages"
)

// Custom emoji (ext:emoji in apron's EXTENSIONS.md): images users add with
// /emoji add and write as :name: in reactions and text. The server frame
// carries the URL of the catalog, a JSON object keyed by :name:, which the
// server serves at a path named after its content's hash, so the URL
// changes whenever the catalog does. An emoji.Catalog keeps the custom
// emoji and renders the catalog; the server keeps who may change it, the
// uploads that hold the images, and the server frames that announce it.
const (
	emojiPath = "/emoji/"
	// maxCustomEmoji bounds the catalog.
	maxCustomEmoji = emoji.MaxCustom
)

// emojiKeyPattern is a custom emoji's key: a name between colons.
var emojiKeyPattern = emoji.KeyPattern

// customEmoji is one catalog entry: the hosted upload holding its image,
// the image's size, and the user_id of who added it.
type customEmoji = emoji.Entry[*embedState]

// The embed as a custom emoji's image (emoji.Image): an upload, which the
// store names by its embed_id.
func (e *embedState) ImageID() string  { return e.ID }
func (e *embedState) ImageURL() string { return e.fileURL() }
func (e *embedState) ImageAlt() string { return e.Alt }
func (e *embedState) ImageSize() int64 { return e.Size }

// emojiExt is server.ext.emoji for a connection whose URLs start with
// baseURL, or nil when clients would ignore its catalog_url: one that is
// neither https: nor http: on a loopback host, such as behind a plain-HTTP
// reverse proxy without PublicURL. The server then does not offer
// ext:emoji on that connection.
func (s *Server) emojiExt(baseURL string) map[string]any {
	if !clientsLoad(baseURL) {
		return nil
	}
	name, _ := s.emoji.Rendered()
	return map[string]any{"catalog_url": baseURL + emojiPath + name}
}

// clientsLoad reports whether clients load images and catalogs from URLs
// that start with baseURL: https:, or http: on a loopback host. Behind a
// plain-HTTP reverse proxy without PublicURL, a connection's URLs are
// http: on the proxy's host, which clients refuse, so custom emoji and the
// server avatar are not taken from such a connection.
func clientsLoad(baseURL string) bool {
	parsed, err := url.Parse(baseURL)
	return err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http" && loopbackHost(parsed.Hostname()))
}

// setPublicURL ends the messages that refuse an image, such as a custom
// emoji, from a connection whose URLs clients would not load
// (clientsLoad).
const setPublicURL = "; set --public-url to the server's https: URL"

// emojiNeedHTTPS refuses custom emoji on such a connection.
const emojiNeedHTTPS = "Custom emoji need an https address" + setPublicURL

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

// emojiRoomLocked is how many bytes an image for key may take: at most
// MaxEmojiBytes, and what MaxEmojiStorageBytes leaves once the image it
// would replace is gone.
func (s *Server) emojiRoomLocked(key string) int64 {
	room := s.config.MaxEmojiStorageBytes - s.emoji.Bytes()
	if previous := s.emoji.Get(key); previous != nil {
		room += previous.Image.Size
	}
	return min(s.config.MaxEmojiBytes, room)
}

// mayReplaceEmojiLocked reports whether the user userID may give an
// existing key a new image: the user who added it, or an admin.
func (s *Server) mayReplaceEmojiLocked(existing *customEmoji, userID string) bool {
	if existing.By == userID {
		return true
	}
	u := s.users.Get(userID)
	return u != nil && u.hasRole("admin")
}

// refuseEmojiLocked checks a finished /emoji add write of size bytes
// again, since the catalog may have changed since the command: the key
// may have been added by someone the writer may not replace, the catalog
// may be full, or other images may have taken the storage; and the image's
// URL must be one clients load. It returns the
// HTTP status and message to refuse the write with, or 0.
func (s *Server) refuseEmojiLocked(e *embedState, size int64) (int, string) {
	existing := s.emoji.Get(e.EmojiKey)
	switch {
	case !clientsLoad(e.BaseURL):
		return http.StatusForbidden, emojiNeedHTTPS
	case existing != nil && existing.Image != e && !s.mayReplaceEmojiLocked(existing, e.emojiBy):
		return http.StatusConflict, fmt.Sprintf("%s was added by someone else meanwhile; only who added it or an admin can replace it", e.EmojiKey)
	case existing == nil && s.emoji.Len() >= maxCustomEmoji:
		return http.StatusConflict, fmt.Sprintf("This server has %d custom emoji, the most it keeps", maxCustomEmoji)
	case size > s.emojiRoomLocked(e.EmojiKey):
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
	if previous := s.emoji.Get(e.EmojiKey); previous != nil && previous.Image != e {
		s.removeEmbedLocked(previous.Image.ID)
	}
	entry := &customEmoji{Image: e, By: e.emojiBy}
	if image, ok := og["image"].(map[string]any); ok {
		entry.Width, _ = image["width"].(int)
		entry.Height, _ = image["height"].(int)
	}
	s.emoji.Set(e.EmojiKey, entry)
	s.emojiChangedLocked()
}

// removeEmojiLocked removes a key from the catalog and deletes its image.
func (s *Server) removeEmojiLocked(key string) {
	if entry := s.emoji.Remove(key); entry != nil {
		s.removeEmbedLocked(entry.Image.ID)
		s.emojiChangedLocked()
	}
}

// emojiChangedLocked sends every connection a new server frame (§3.1)
// with the URL of the catalog, which changed.
func (s *Server) emojiChangedLocked() {
	type kind struct {
		baseURL  string
		revealed bool
	}
	frames := make(map[kind]*apron.ServerInfo)
	for c := range s.clients {
		key := kind{c.baseURL, s.revealedLocked(c)}
		frame, seen := frames[key]
		if !seen {
			// A connection not offered ext:emoji has no catalog_url to
			// change.
			if key.revealed && s.emojiExt(c.baseURL) != nil {
				info := s.serverFrameLocked(c)
				frame = &info
			}
			frames[key] = frame
		}
		if frame != nil {
			c.conn.Announce(*frame)
		}
	}
}

// restoreEmojiLocked rebuilds the catalog from the store, after the embeds.
// A key whose upload is gone is dropped, and so is a finished emoji upload
// that no key holds.
func (s *Server) restoreEmojiLocked(raw jsontext.Value) error {
	err := s.emoji.Restore(raw, func(id, key string) (*embedState, bool) {
		e := s.embeds.Get(id)
		return e, e != nil && e.Finished && e.EmojiKey == key
	})
	if err != nil {
		return err
	}
	for _, id := range s.embeds.IDs() {
		if e := s.embeds.Get(id); e.EmojiKey != "" && e.Finished {
			if entry := s.emoji.Get(e.EmojiKey); entry == nil || entry.Image != e {
				s.removeEmbedLocked(id)
			}
		}
	}
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
	name, catalog := s.emoji.Rendered()
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
// (refuseEmojiLocked). A connection not offered ext:emoji, whose image
// URLs clients would refuse, may not add one.
func (s *Server) emojiCommand(c *client, _ *roomState, body map[string]any, args, _ string) (apron.CommandResult, *Error) {
	u := c.user
	action, key := cutWord(args)
	if !emojiKeyPattern.MatchString(key) || len(key) > messages.MaxEmojiBytes {
		return apron.CommandResult{}, invalidParams("Usage: /emoji add :<name>: with one image attached, or /emoji remove :<name>:; a name is letters, digits, _, +, and -, at most %d characters", messages.MaxEmojiBytes-2)
	}
	existing := s.emoji.Get(key)
	// The form's word matches exactly, as command names do.
	switch action {
	case "add":
		embeds := asList(body["embeds"])
		if len(embeds) != 1 || embeds[0].(map[string]any)["kind"] != "upload" {
			return apron.CommandResult{}, invalidParams("/emoji add takes exactly one upload embed: attach an image")
		}
		if s.emojiExt(c.baseURL) == nil {
			// This connection is not offered ext:emoji: the image's URL
			// would be one clients refuse.
			return apron.CommandResult{}, &Error{Code: CodeDenied, Message: emojiNeedHTTPS}
		}
		if existing != nil && !s.mayReplaceEmojiLocked(existing, u.id) {
			return apron.CommandResult{}, &Error{Code: CodeDenied, Message: fmt.Sprintf("%s is taken; only who added it or an admin can replace it", key)}
		}
		if existing == nil && s.emoji.Len() >= maxCustomEmoji {
			return apron.CommandResult{}, &Error{Code: CodeDenied, Message: fmt.Sprintf("This server has %d custom emoji, the most it keeps", maxCustomEmoji)}
		}
		if s.emojiRoomLocked(key) <= 0 {
			return apron.CommandResult{}, &Error{Code: CodeDenied, Message: s.emojiFullMessage()}
		}
		if err := s.admitPostLocked(u); err != nil {
			return apron.CommandResult{}, err
		}
		e := s.newWriteLocked(c, s.embeds.NewID(), "upload", "")
		e.EmojiKey, e.emojiBy = key, u.id
		e.Alt = ogImageAlt(embeds[0].(map[string]any)["og"])
		return e.writeResult(), nil
	case "remove":
		if !u.hasRole("admin") {
			return apron.CommandResult{}, &Error{Code: CodeDenied, Message: "Only an admin can remove custom emoji"}
		}
		if existing == nil {
			return apron.CommandResult{}, invalidParams("There is no custom emoji %s", key)
		}
		s.removeEmojiLocked(key)
		return apron.CommandResult{}, nil
	}
	return apron.CommandResult{}, invalidParams("Usage: /emoji add :<name>: with one image attached, or /emoji remove :<name>:")
}
