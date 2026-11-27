// Package archive reads and writes Apron archives (apron-web's ARCHIVE.md,
// archive version 1): a chat saved as one file of JSON Lines, what a
// client had of one server. Its first line is a [Header], and each later
// line a [Part], which holds the results of the requests a client makes,
// room_list and history (PROTOCOL.md §4.2.1, §4.3), in the shape of the
// wire, so that a reader decodes it as it decodes a server's frames.
//
// A [Writer] writes the header, then a part at a time, such as each page
// of history as it arrives and each room's listing once its pages are in.
// A [Reader] reads them back, refusing a file that is not an archive of
// this version, and skipping each line and record that is not valid (§1),
// as an archive is untrusted; [Read] replays a whole archive into an
// [Archive]: each room's latest listing, its users, and the state its
// records leave (apron.Replay).
//
// Where an archive's stretch of a room's log starts is the room's listing's
// history_log_id (§3.1): it holds every record of the room from there to
// the listing's latest_log_id. A record of a room the archive does not
// list, or below its stretch, is a reference, such as the message that a
// message of the stretch replies to: [Archive.Reference].
//
// The package imports only package apron, and is on the client side of
// the module's boundary.
package archive

import (
	"bufio"
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
)

// Version is the archive version this package reads and writes.
const Version = 1

// Errors of NewReader and Read.
var (
	// ErrNotArchive is a file whose first line is not an archive's header.
	ErrNotArchive = errors.New("archive: not an Apron archive")
	// ErrVersion is an archive of a version this package does not know.
	ErrVersion = errors.New("archive: an archive version this reader does not know")
)

// Header is an archive's first line (§2).
type Header struct {
	// Archive is the archive version, apron_archive; a Writer sets it to
	// Version when it is 0.
	Archive int `json:"apron_archive"`
	// Server is the server's WebSocket URL, such as wss://chat.example/ws.
	Server string `json:"server,omitempty"`
	// ExportedAt is when the archive was written, in ISO 8601, as
	// FormatTime writes it.
	ExportedAt string `json:"exported_at,omitempty"`
	// You is who the archive was written as, or nil, signed out.
	You *apron.User `json:"you,omitzero"`
	// Media are the origins that the archive's media loads from (§5), such
	// as https://chat.example: the server's own, and its ext:media origins.
	Media []string `json:"media,omitempty"`
	// Unknown holds the members this package has no field for.
	Unknown jsontext.Value `json:",embed"`
}

// FormatTime writes t as an archive's exported_at: ISO 8601 in UTC, to
// the millisecond, such as 2026-10-27T12:00:00.000Z.
func FormatTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// Part is a line after the header (§3): any of the arrays of a room_list
// result (Joined, NotJoined, Users) and of a history result (Rooms,
// Messages, Reactions, Memberships).
//
// A listed room's HistoryLogID says where the archive's stretch of the
// room's log starts: nil, which is left out, at the oldest record of the
// room in the archive, and a pointer to 0, which is null, nowhere, as the
// archive holds only the room's listing (§3.1).
type Part struct {
	// Joined are rooms the archive's you had joined, and NotJoined the
	// other rooms it lists, as room_list lists them with members (§4.2.1):
	// a later listing of a room replaces an earlier one.
	Joined    []apron.RoomView `json:"joined,omitempty"`
	NotJoined []apron.RoomView `json:"not_joined,omitempty"`
	// Users are user objects as the writer knew them: a later object of a
	// user replaces an earlier one.
	Users       []apron.User       `json:"users,omitempty"`
	Rooms       []apron.Room       `json:"rooms,omitempty"`
	Messages    []apron.Message    `json:"messages,omitempty"`
	Reactions   []apron.Reactions  `json:"reactions,omitempty"`
	Memberships []apron.Membership `json:"memberships,omitempty"`
}

// HistoryPart is the part that holds a history result's records, without
// its other keys, such as more, which a reader ignores.
func HistoryPart(h apron.HistoryResult) Part {
	return Part{Rooms: h.Rooms, Messages: h.Messages, Reactions: h.Reactions, Memberships: h.Memberships}
}

// Empty reports whether p holds nothing.
func (p Part) Empty() bool {
	return len(p.Joined)+len(p.NotJoined)+len(p.Users)+len(p.Rooms)+len(p.Messages)+len(p.Reactions)+len(p.Memberships) == 0
}

// History returns the part's records as a history result's, for
// apron.Replay.ApplyHistory.
func (p Part) History() apron.HistoryResult {
	return apron.HistoryResult{Rooms: p.Rooms, Messages: p.Messages, Reactions: p.Reactions, Memberships: p.Memberships}
}

// moment leaves out of a user object what describes a moment rather than
// the chat: its status and seen (§2).
func moment(u apron.User) apron.User {
	u.Status, u.Seen = "", ""
	return u
}

// momentMember does to a listed member what moment does to a user, and
// to the fields it clears.
func momentMember(u apron.CurrentUser) apron.CurrentUser {
	u.User = moment(u.User)
	u.Cleared = slices.DeleteFunc(slices.Clone(u.Cleared), func(f string) bool { return f == "status" || f == "seen" })
	return u
}

// Writer writes an archive: its header, then its parts, a line each.
type Writer struct {
	w io.Writer
}

// NewWriter writes h as an archive's header to w, and returns a Writer
// of its parts. A header's you, and the users of the parts, leave out
// status and seen (§2), as do the members of the listings.
func NewWriter(w io.Writer, h Header) (*Writer, error) {
	if h.Archive == 0 {
		h.Archive = Version
	}
	if h.You != nil {
		you := moment(*h.You)
		h.You = &you
	}
	aw := &Writer{w}
	return aw, aw.line(h)
}

// Write writes a part as one line; an empty part writes nothing.
func (w *Writer) Write(p Part) error {
	if p.Empty() {
		return nil
	}
	p.Users = slices.Clone(p.Users)
	for i := range p.Users {
		p.Users[i] = moment(p.Users[i])
	}
	for _, views := range []*[]apron.RoomView{&p.Joined, &p.NotJoined} {
		*views = slices.Clone(*views)
		for i, view := range *views {
			if view.Members != nil {
				members := make([]apron.CurrentUser, len(view.Members))
				for j, m := range view.Members {
					members[j] = momentMember(m)
				}
				(*views)[i].Members = members
			}
		}
	}
	return w.line(p)
}

// WriteHistory writes a history result's records as one part.
func (w *Writer) WriteHistory(h apron.HistoryResult) error { return w.Write(HistoryPart(h)) }

func (w *Writer) line(v any) error {
	raw, err := apron.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.w.Write(append(raw, '\n'))
	return err
}

// Reader reads an archive's parts, after its header.
type Reader struct {
	// Header is the archive's header.
	Header Header

	r       *bufio.Reader
	buf     []byte
	skipped int
}

// NewReader reads the header of the archive r holds, and returns a Reader
// of its parts. It refuses a file whose first line is not a header, with
// ErrNotArchive, and one of an archive version it does not know, with
// ErrVersion (§2).
func NewReader(r io.Reader) (*Reader, error) {
	ar := &Reader{r: bufio.NewReader(r)}
	line, err := ar.line()
	if err == io.EOF {
		return nil, ErrNotArchive
	}
	if err != nil {
		return nil, err
	}
	var members map[string]jsontext.Value
	if json.Unmarshal(line, &members) != nil || members["apron_archive"] == nil {
		return nil, ErrNotArchive
	}
	var version int
	if json.Unmarshal(members["apron_archive"], &version) != nil {
		return nil, ErrNotArchive
	}
	if version != Version {
		return nil, fmt.Errorf("%w: %s", ErrVersion, members["apron_archive"])
	}
	// The header's other members are read as a part's records are: one of
	// the wrong type is left out.
	ar.Header.Archive = version
	_ = json.Unmarshal(members["server"], &ar.Header.Server)
	_ = json.Unmarshal(members["exported_at"], &ar.Header.ExportedAt)
	var you apron.User
	if json.Unmarshal(members["you"], &you) == nil && apron.ValidID(you.UserID) {
		ar.Header.You = &you
	}
	_ = json.Unmarshal(members["media"], &ar.Header.Media)
	for _, name := range []string{"apron_archive", "server", "exported_at", "you", "media"} {
		delete(members, name)
	}
	if len(members) > 0 {
		ar.Header.Unknown, _ = apron.Marshal(members)
	}
	return ar, nil
}

// line returns the next line that is not blank, without its line ending.
func (r *Reader) line() ([]byte, error) {
	for {
		// The line is read into a buffer of the Reader's, which the next
		// line reuses: what a part keeps of its line is decoded from it.
		r.buf = r.buf[:0]
		var err error
		for {
			var fragment []byte
			fragment, err = r.r.ReadSlice('\n')
			r.buf = append(r.buf, fragment...)
			if err != bufio.ErrBufferFull {
				break
			}
		}
		line := r.buf
		if err != nil && (err != io.EOF || len(line) == 0) {
			return nil, err
		}
		line = bytes.TrimRight(line, "\r\n")
		if len(bytes.TrimSpace(line)) > 0 {
			return line, nil
		}
		if err == io.EOF {
			return nil, io.EOF
		}
	}
}

// Next returns the next part, or io.EOF after the last. It skips a line
// that is not a JSON object, an array of a part that is not an array, and
// a record that is not valid: one that does not decode as the protocol's
// record does, or lacks the IDs that key it (§1).
func (r *Reader) Next() (Part, error) {
	for {
		line, err := r.line()
		if err != nil {
			return Part{}, err
		}
		var arrays map[string]jsontext.Value
		if json.Unmarshal(line, &arrays) != nil {
			r.skipped++
			continue
		}
		var p Part
		decode(r, arrays["joined"], &p.Joined, validView)
		decode(r, arrays["not_joined"], &p.NotJoined, validView)
		decode(r, arrays["users"], &p.Users, func(u *apron.User, _ jsontext.Value) bool { return apron.ValidID(u.UserID) })
		decode(r, arrays["rooms"], &p.Rooms, func(room *apron.Room, _ jsontext.Value) bool { return apron.ValidID(room.RoomID) })
		decode(r, arrays["messages"], &p.Messages, validMessage)
		decode(r, arrays["reactions"], &p.Reactions, validReactions)
		decode(r, arrays["memberships"], &p.Memberships, validMembership)
		return p, nil
	}
}

// Skipped is how many lines, arrays, and records the Reader has skipped as
// not valid.
func (r *Reader) Skipped() int { return r.skipped }

// decode decodes the records of an array, if there is one, into list: each
// that decodes and that valid is true of, which may also fix it up from
// its JSON.
func decode[T any](r *Reader, array jsontext.Value, list *[]T, valid func(*T, jsontext.Value) bool) {
	if array == nil {
		return
	}
	if array.Kind() != '[' {
		r.skipped++
		return
	}
	// The records are read in place, one at a time, rather than copied
	// out of the array first (a decoder of a bytes.Buffer reads it where
	// it is); the array is valid JSON, a member of a line that decoded.
	dec := jsontext.NewDecoder(bytes.NewBuffer(array))
	if _, err := dec.ReadToken(); err != nil {
		r.skipped++
		return
	}
	// The list grows once, by the records the array has: counting them
	// costs less than growing it as they are decoded.
	*list = slices.Grow(*list, count(array))
	for dec.PeekKind() != ']' {
		raw, err := dec.ReadValue()
		if err != nil {
			r.skipped++
			return
		}
		// Each record is decoded in its place in the list, which it
		// leaves when it is not valid.
		var zero T
		*list = append(*list, zero)
		record := &(*list)[len(*list)-1]
		if json.Unmarshal(raw, record) != nil || !valid(record, raw) {
			*list = (*list)[:len(*list)-1]
			r.skipped++
		}
	}
}

// count returns how many values a JSON array has, or 0 for one that is
// not valid.
func count(array jsontext.Value) int {
	dec := jsontext.NewDecoder(bytes.NewBuffer(array))
	if _, err := dec.ReadToken(); err != nil {
		return 0
	}
	n := 0
	for dec.PeekKind() != ']' {
		if dec.SkipValue() != nil {
			return 0
		}
		n++
	}
	return n
}

// validView checks a listed room, and keeps a history_log_id of null,
// which a RoomView decodes as nil, as a pointer to 0: an archive tells
// null, no stretch, from absent, a stretch from the oldest record (§3.1).
func validView(v *apron.RoomView, raw jsontext.Value) bool {
	if !apron.ValidID(v.RoomID) {
		return false
	}
	var members map[string]jsontext.Value
	if json.Unmarshal(raw, &members) == nil {
		if value, ok := members["history_log_id"]; ok && value.Kind() == 'n' {
			v.HistoryLogID = new(apron.LogID)
		}
	}
	for _, m := range v.Members {
		if !apron.ValidID(m.UserID) {
			return false
		}
	}
	return true
}

func validMessage(m *apron.Message, _ jsontext.Value) bool {
	return m.MessageID != 0 && m.LogID >= m.MessageID && apron.ValidID(m.RoomID) && apron.ValidID(m.From.UserID)
}

func validReactions(rec *apron.Reactions, _ jsontext.Value) bool {
	if rec.LogID == 0 || rec.MessageID == 0 || !apron.ValidID(rec.RoomID) {
		return false
	}
	for _, set := range rec.Reactions {
		if !apron.ValidID(set.From.UserID) {
			return false
		}
	}
	return true
}

func validMembership(rec *apron.Membership, _ jsontext.Value) bool {
	if rec.LogID == 0 || !apron.ValidID(rec.RoomID) {
		return false
	}
	for _, m := range rec.Members {
		if !apron.ValidID(m.User.UserID) {
			return false
		}
	}
	return true
}
