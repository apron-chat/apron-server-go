// Package embeds keeps the embeds the server hosts (PROTOCOL.md §4.8): an
// upload, or live text, a terminal or an append. A Table keeps each embed
// by its embed_id, the unused or in-progress writes by their one-time
// tokens, and the finished uploads oldest first with their total size, for
// the server to evict the oldest beyond its bound; it names new embeds and
// orders finished uploads; and it keeps the embeds in the store, by
// embed_id.
//
// A Table knows the server's embeds only as values of the type E, each of
// which holds an Embed, the state the store keeps, and whatever else the
// server keeps with it, such as the user an avatar is for, which it names
// when it writes them. It is not safe for concurrent use: the server calls
// it holding its lock.
package embeds

import (
	"cmp"
	"container/list"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"iter"
	"maps"
	"path/filepath"
	"slices"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/store"
)

// EntryKind is the kind of the store entries that hold embeds, named by
// embed_id.
const EntryKind = "embed"

// Embed is a hosted embed's state that the store keeps.
type Embed struct {
	ID, Kind string
	// MessageID is the message the embed belongs to, if any; EmojiKey is
	// set instead for an /emoji add upload (ext:emoji), and ServerAvatarBy,
	// the sender's user_id, for a /server avatar upload (§3.1).
	MessageID      string
	EmojiKey       string
	ServerAvatarBy string
	// BaseURL starts the embed's URLs, and Secret makes its file and live
	// text URLs unguessable.
	BaseURL, Secret string
	Title           string
	// Alt is the sender's og.image.alt, kept in the server's og.
	Alt      string
	Finished bool
	// Owned holds the server fields restored on every save: an upload's url
	// and og, live text's url, a finished terminal's output.
	Owned map[string]any
	// Path is the file holding a finished upload's content, Size its
	// length, ContentType its type, and Seq its place among the finished
	// uploads, across restarts.
	Path        string
	Size        int64
	ContentType string
	Seq         int64
	// upload is the upload's element in Table.uploads, if it is there.
	upload *list.Element
}

// Apart reports whether a finished upload is kept apart from the uploads
// that the server's bound on their total size evicts (Table.Hold): custom
// emoji have a bound of their own, and the server avatar is one image.
func (e *Embed) Apart() bool {
	return e.EmojiKey != "" || e.ServerAvatarBy != ""
}

// Hosted is the type of the server's embeds, each of which holds an Embed.
type Hosted interface {
	comparable
	Hosted() *Embed
}

// Table is the hosted embeds by embed_id.
type Table[E Hosted] struct {
	byID map[string]E
	// writes are the embeds by the tokens of their unused or in-progress
	// writes.
	writes map[string]E
	// uploads are the finished uploads, but those kept apart, oldest first,
	// and uploadBytes their total size.
	uploads     *list.List
	uploadBytes int64
	// dirty are the embed_ids added, changed, or removed since Entries last
	// took them.
	dirty map[string]struct{}
	// number is the latest embed_id's (NewID), and seq the latest finished
	// upload's Seq (NextSeq).
	number uint64
	seq    int64
}

// New returns an empty table.
func New[E Hosted]() *Table[E] {
	return &Table[E]{byID: make(map[string]E), writes: make(map[string]E), uploads: list.New(), dirty: make(map[string]struct{})}
}

// NewID returns an embed_id never used before, embed_<n>.
func (t *Table[E]) NewID() string {
	t.number++
	return fmt.Sprintf("embed_%d", t.number)
}

// NextSeq returns the Seq of an upload that has just finished, after every
// other's, across restarts.
func (t *Table[E]) NextSeq() int64 {
	t.seq++
	return t.seq
}

// Counters are the latest embed_id's number and upload Seq, which the
// server stores with its other counters.
func (t *Table[E]) Counters() (number uint64, seq int64) { return t.number, t.seq }

// RestoreCounters takes the stored counters.
func (t *Table[E]) RestoreCounters(number uint64, seq int64) { t.number, t.seq = number, seq }

// Get returns the embed with embed_id id, or the zero E.
func (t *Table[E]) Get(id string) E { return t.byID[id] }

// IDs lists the embed_ids, sorted.
func (t *Table[E]) IDs() []string { return slices.Sorted(maps.Keys(t.byID)) }

// All lists the embeds, in no order.
func (t *Table[E]) All() iter.Seq2[string, E] { return maps.All(t.byID) }

// Len counts the embeds.
func (t *Table[E]) Len() int { return len(t.byID) }

// Add keeps e.
func (t *Table[E]) Add(e E) {
	id := e.Hosted().ID
	t.byID[id] = e
	t.dirty[id] = struct{}{}
}

// Touch marks the embed with embed_id id as changed, to be stored again.
func (t *Table[E]) Touch(id string) { t.dirty[id] = struct{}{} }

// Remove forgets the embed with embed_id id, and its upload, if it is
// held, and returns it, or the zero E if there is none. Its write token is
// the caller's to end (EndWrite).
func (t *Table[E]) Remove(id string) E {
	e, ok := t.byID[id]
	if !ok {
		return e
	}
	t.Release(e)
	delete(t.byID, id)
	t.dirty[id] = struct{}{}
	return e
}

// AddWrite names e's write by its token.
func (t *Table[E]) AddWrite(token string, e E) { t.writes[token] = e }

// Write returns the embed whose write token is, or the zero E.
func (t *Table[E]) Write(token string) E { return t.writes[token] }

// EndWrite forgets a write token.
func (t *Table[E]) EndWrite(token string) { delete(t.writes, token) }

// WriteCount counts the unused or in-progress writes.
func (t *Table[E]) WriteCount() int { return len(t.writes) }

// Hold adds a finished upload to the uploads, newest, unless it is kept
// apart.
func (t *Table[E]) Hold(e E) {
	embed := e.Hosted()
	if embed.Apart() || embed.upload != nil {
		return
	}
	embed.upload = t.uploads.PushBack(e)
	t.uploadBytes += embed.Size
}

// Release takes an upload out of the uploads, if it is held.
func (t *Table[E]) Release(e E) {
	embed := e.Hosted()
	if embed.upload == nil {
		return
	}
	t.uploads.Remove(embed.upload)
	embed.upload = nil
	t.uploadBytes -= embed.Size
}

// UploadCount counts the uploads held.
func (t *Table[E]) UploadCount() int { return t.uploads.Len() }

// UploadBytes totals the sizes of the uploads held.
func (t *Table[E]) UploadBytes() int64 { return t.uploadBytes }

// Uploads lists the uploads held, oldest first. The loop may Remove them.
func (t *Table[E]) Uploads() iter.Seq[E] {
	return func(yield func(E) bool) {
		for element := t.uploads.Front(); element != nil; {
			e := element.Value.(E)
			element = element.Next()
			if !yield(e) {
				return
			}
		}
	}
}

// stored is an embed as the store keeps it.
type stored struct {
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

// Entries returns the store entries of the embeds added, changed, or
// removed since it was last called, naming the user an avatar is for by
// avatarFor, "" for an embed that is no avatar.
func (t *Table[E]) Entries(avatarFor func(E) string) []store.Entry {
	if len(t.dirty) == 0 {
		return nil
	}
	entries := make([]store.Entry, 0, len(t.dirty))
	for id := range t.dirty {
		entry := store.Entry{Kind: EntryKind, ID: id}
		if e, ok := t.byID[id]; ok {
			embed := e.Hosted()
			s := stored{
				Kind: embed.Kind, MessageID: embed.MessageID, AvatarFor: avatarFor(e), EmojiFor: embed.EmojiKey,
				ServerAvatarBy: embed.ServerAvatarBy, BaseURL: embed.BaseURL, Secret: embed.Secret,
				Title: embed.Title, Alt: embed.Alt, Finished: embed.Finished,
				Owned: embed.Owned, Size: embed.Size, ContentType: embed.ContentType, Seq: embed.Seq,
			}
			if embed.Path != "" {
				s.File = filepath.Base(embed.Path)
			}
			value, err := apron.Marshal(s)
			if err != nil {
				// An embed that cannot be stored is left as it was.
				continue
			}
			entry.Value = value
		}
		entries = append(entries, entry)
	}
	clear(t.dirty)
	return entries
}

// Snapshot returns the store entries of every embed, as Entries does those
// that changed.
func (t *Table[E]) Snapshot(avatarFor func(E) string) []store.Entry {
	for id := range t.byID {
		t.dirty[id] = struct{}{}
	}
	return t.Entries(avatarFor)
}

// Restore takes the stored embeds, which embed makes the server's embeds
// of, from the Embed and the user_id of the user an avatar is for, and
// holds their finished uploads, oldest first; path names an upload's file
// by the base name stored. It returns the upload files the embeds hold.
func (t *Table[E]) Restore(entries map[string]jsontext.Value, embed func(e Embed, avatarFor string) E, path func(file string) string) (map[string]bool, error) {
	files := make(map[string]bool)
	var uploads []E
	for id, raw := range entries {
		var s stored
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("stored %s %q: %w", EntryKind, id, err)
		}
		e := embed(Embed{
			ID: id, Kind: s.Kind, MessageID: s.MessageID, EmojiKey: s.EmojiFor, ServerAvatarBy: s.ServerAvatarBy,
			BaseURL: s.BaseURL, Secret: s.Secret, Title: s.Title, Alt: s.Alt, Finished: s.Finished,
			Owned: s.Owned, Size: s.Size, ContentType: s.ContentType, Seq: s.Seq,
		}, s.AvatarFor)
		if s.File != "" {
			e.Hosted().Path = path(s.File)
			files[e.Hosted().Path] = true
			uploads = append(uploads, e)
		}
		t.byID[id] = e
	}
	slices.SortFunc(uploads, func(a, b E) int { return cmp.Compare(a.Hosted().Seq, b.Hosted().Seq) })
	for _, e := range uploads {
		t.Hold(e)
	}
	return files, nil
}
