package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/archive"
	"github.com/apron-chat/apron-server-go/apron/client"
)

// roomFlag is a flag of room_ids, which may be repeated.
type roomFlag []string

func (r *roomFlag) String() string { return strings.Join(*r, ",") }

func (r *roomFlag) Set(s string) error {
	if !apron.ValidID(s) {
		return fmt.Errorf("not a room_id: %q", s)
	}
	*r = append(*r, s)
	return nil
}

// export writes the history of rooms as an archive (ARCHIVE.md) to
// stdout: its header, then each room's records, page by page, from the
// room's effective lower bound (§4.3.3), or, with --resume, from where the
// archive's stretch of the room ends, up to the room's latest_log_id when
// the export began, then the room's listing, whose history_log_id is
// where the stretch starts (§3.1), and last the users.
//
// It is polite: one request at a time, pages of a modest size, at most
// --rate requests a second, and it waits out the server's rate limits
// (retry_after, ext:limits) rather than send what they would refuse.
//
// With --resume, it writes a whole new archive, the old one's parts first:
// an archive has one header, whose exported_at is when it was written,
// which an archive appended to would no longer say. The new listing of a
// room comes after the old one, which it replaces, as a later listing
// does (§3).
func export(ctx context.Context, c *cli, args []string) error {
	flags := c.flags("export")
	var only roomFlag
	flags.Var(&only, "room", "a room_id to export; repeat for more; none exports every room you joined")
	resume := flags.String("resume", "", "an archive of the same server to continue, each room from where its stretch ends")
	pageSize := flags.Int("limit", 100, "how many changes of the log to ask for in each request")
	rate := flags.Float64("rate", 5, "at most this many requests a second; 0 is no limit but one at a time")
	if err := parse(flags, args, 0, 0); err != nil {
		return err
	}
	var old *archive.Archive
	if *resume != "" {
		a, err := readArchive(*resume)
		if err != nil {
			return err
		}
		if strings.TrimSuffix(a.Header.Server, "/") != strings.TrimSuffix(c.url, "/") {
			return fmt.Errorf("%s is an archive of %q, not of %s", *resume, a.Header.Server, c.url)
		}
		old = a
	}
	conn, err := c.connect(ctx, true)
	if err != nil {
		return err
	}
	defer conn.Close()
	for _, capability := range []string{"rooms", "history"} {
		if err := needs(conn, capability); err != nil {
			return fmt.Errorf("%w, which an export needs", err)
		}
	}
	e := &exporter{conn: conn, pace: pacer(*rate), pageSize: *pageSize, old: old}
	if err := e.list(ctx, only); err != nil {
		return err
	}
	you := conn.User()
	w, err := archive.NewWriter(c.stdout, archive.Header{
		Server: c.url, ExportedAt: archive.FormatTime(time.Now()), You: you, Media: media(c.url, conn.Hello()),
	})
	if err != nil {
		return err
	}
	if old != nil {
		if old.Header.You.UserID != "" && old.Header.You.UserID != you.UserID {
			fmt.Fprintf(c.stderr, "aproncli: %s was written as %s, and this export as %s\n", *resume, old.Header.You.UserID, you.UserID)
		}
		if err := copyArchive(w, *resume); err != nil {
			return err
		}
	}
	records := 0
	for _, room := range e.rooms {
		n, err := e.room(ctx, w, room)
		records += n
		if err != nil {
			return err
		}
	}
	// The parents of threads are listed too (§3.1), without their log,
	// unless the archive holds it already.
	for _, parent := range e.parents {
		if old != nil {
			if _, listed := old.Rooms[parent.RoomID]; listed {
				continue
			}
		}
		parent.HistoryLogID = new(apron.LogID)
		if err := w.Write(listing(parent, e.joined[parent.RoomID])); err != nil {
			return err
		}
	}
	if err := w.Write(archive.Part{Users: e.users}); err != nil {
		return err
	}
	fmt.Fprintf(c.stderr, "aproncli: exported %d records of %d rooms in %d requests\n", records, len(e.rooms), e.requests)
	return nil
}

// exporter is an export's state.
type exporter struct {
	conn     *client.Conn
	pace     func(context.Context) error
	pageSize int
	old      *archive.Archive
	requests int

	// rooms are the rooms to export, and parents the parents of their
	// threads that are not, as room_list listed them when the export
	// began; joined says which the user had joined.
	rooms, parents []apron.RoomView
	joined         map[string]bool
	users          []apron.User
}

// list lists the rooms to export: those named, or else every room the
// user joined, threads too.
func (e *exporter) list(ctx context.Context, only []string) error {
	if err := e.pace(ctx); err != nil {
		return err
	}
	e.requests++
	listed, err := e.conn.RoomList(ctx, apron.RoomListParams{Members: true})
	if err != nil {
		return fmt.Errorf("listing rooms: %s", describe(err))
	}
	e.joined = map[string]bool{}
	views := map[string]apron.RoomView{}
	for _, v := range listed.Joined {
		e.joined[v.RoomID], views[v.RoomID] = true, v
	}
	for _, v := range listed.NotJoined {
		views[v.RoomID] = v
	}
	users := map[string]bool{}
	addUsers := func(list []apron.CurrentUser) {
		for _, u := range list {
			if !users[u.UserID] {
				users[u.UserID] = true
				e.users = append(e.users, u.User)
			}
		}
	}
	addUsers(listed.Users)
	// find returns a room's listing, asking for it alone if the listing of
	// every room left it out, as a server may for rooms not joined.
	find := func(id string) (apron.RoomView, error) {
		if v, ok := views[id]; ok {
			return v, nil
		}
		if err := e.pace(ctx); err != nil {
			return apron.RoomView{}, err
		}
		e.requests++
		one, err := e.conn.RoomList(ctx, apron.RoomListParams{RoomID: id, Members: true})
		if err != nil {
			return apron.RoomView{}, fmt.Errorf("listing %s: %s", id, describe(err))
		}
		addUsers(one.Users)
		for _, v := range append(one.Joined, one.NotJoined...) {
			if v.RoomID == id {
				e.joined[id], views[id] = slices.ContainsFunc(one.Joined, func(j apron.RoomView) bool { return j.RoomID == id }), v
				return v, nil
			}
		}
		return apron.RoomView{}, fmt.Errorf("no room %s that you can see", id)
	}
	if len(only) == 0 {
		e.rooms = listed.Joined
	}
	for _, id := range only {
		v, err := find(id)
		if err != nil {
			return err
		}
		e.rooms = append(e.rooms, v)
	}
	for _, v := range e.rooms {
		parent := v.ParentRoomID
		if parent == "" || slices.ContainsFunc(e.rooms, func(r apron.RoomView) bool { return r.RoomID == parent }) ||
			slices.ContainsFunc(e.parents, func(r apron.RoomView) bool { return r.RoomID == parent }) {
			continue
		}
		if p, err := find(parent); err == nil {
			e.parents = append(e.parents, p)
		}
	}
	return nil
}

// room writes a room's records, page by page, then its listing, and
// returns how many records it wrote.
func (e *exporter) room(ctx context.Context, w *archive.Writer, v apron.RoomView) (int, error) {
	// H is the room's latest_log_id when the export began: the export
	// stops there (§4.3.5).
	h := v.LatestLogID
	lower, ok := v.LowerBound()
	if !ok {
		return 0, fmt.Errorf("room %s has no history_log_id, though the server offers history", v.RoomID)
	}
	// The stretch starts at the effective lower bound, or where the old
	// archive's starts, if it continues from there.
	start, after := lower, lower
	if e.old != nil {
		if s, end, ok := e.old.Stretch(v.RoomID); ok && end+1 >= lower {
			start, after = s, end+1
		}
	}
	records := 0
	for after <= h {
		from, gap := after, false
		for page, err := range e.conn.HistoryPages(ctx, apron.HistoryParams{RoomID: v.RoomID, After: &from, Before: &h, Limit: e.pageSize}) {
			e.requests++
			if err != nil {
				return records, fmt.Errorf("reading the history of %s: %s", v.RoomID, describe(err))
			}
			if bound := page.LowerBound(); bound > after {
				// The server no longer holds what the stretch needs: it
				// starts again from the bound (§4.3.5), and what is below
				// it in the archive is a reference.
				start, after, gap = bound, bound, true
				break
			}
			if err := w.WriteHistory(page); err != nil {
				return records, err
			}
			records += len(page.Rooms) + len(page.Messages) + len(page.Reactions) + len(page.Memberships)
			after = page.LastLogID + 1
			if err := e.pace(ctx); err != nil {
				return records, err
			}
		}
		if !gap {
			break
		}
	}
	view := v
	switch {
	case start > h && (v.HistoryLogID == nil || *v.HistoryLogID == 0):
		// No log, as a room whose history_log_id is null (§4.3.3).
		view.HistoryLogID = new(apron.LogID)
	default:
		view.HistoryLogID = &start
	}
	return records, w.Write(listing(view, e.joined[v.RoomID]))
}

// listing is the part that lists a room.
func listing(v apron.RoomView, joined bool) archive.Part {
	if joined {
		return archive.Part{Joined: []apron.RoomView{v}}
	}
	return archive.Part{NotJoined: []apron.RoomView{v}}
}

// pacer returns a func that waits until a request may go, at most rate a
// second.
func pacer(rate float64) func(context.Context) error {
	if rate <= 0 {
		return func(context.Context) error { return nil }
	}
	interval := time.Duration(float64(time.Second) / rate)
	var next time.Time
	return func(ctx context.Context) error {
		if wait := time.Until(next); wait > 0 {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		next = time.Now().Add(interval)
		return nil
	}
}

// media returns the origins an archive's media loads from (§5): the
// server's own, and its ext:media origins.
func media(address string, hello apron.ServerInfo) []string {
	var origins []string
	if u, err := url.Parse(address); err == nil && u.Host != "" {
		scheme := "https"
		if u.Scheme == "ws" || u.Scheme == "http" {
			scheme = "http"
		}
		origins = append(origins, scheme+"://"+u.Host)
	}
	if hello.Has("ext:media") {
		var m struct {
			Origins []jsontext.Value `json:"origins"`
		}
		if json.Unmarshal(hello.Ext["media"], &m) == nil {
			for _, raw := range m.Origins {
				var origin string
				if json.Unmarshal(raw, &origin) == nil && validOrigin(origin) && !slices.Contains(origins, origin) {
					origins = append(origins, origin)
				}
			}
		}
	}
	return origins
}

// validOrigin reports whether s is an ext:media origin: scheme://host[:port],
// https, or http on a loopback host.
func validOrigin(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.User != nil {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		return host == "localhost" || host == "127.0.0.1" || host == "::1"
	}
	return false
}

// readArchive reads an archive's file.
func readArchive(path string) (*archive.Archive, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	a, err := archive.Read(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return a, nil
}

// copyArchive writes the parts of the archive at path, as they decode: a
// line or record that is not valid is left out.
func copyArchive(w *archive.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r, err := archive.NewReader(f)
	if err != nil {
		return err
	}
	for {
		p, err := r.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := w.Write(p); err != nil {
			return err
		}
	}
}
