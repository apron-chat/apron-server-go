package archive

import (
	"io"
	"slices"

	"github.com/apron-chat/apron-server-go/apron"
)

// Archive is a whole archive, replayed (§3): its header, each room's latest
// listing, each user's latest object, and the state its records leave by
// replay (§2.3), whatever their order.
type Archive struct {
	Header Header
	// Rooms are the rooms the archive lists, by room_id: the latest
	// listing of each, which replaces the earlier ones.
	Rooms map[string]Listing
	// Users are the latest object of each user, by user_id.
	Users map[string]apron.User
	// Log is the state the archive's records leave, references included.
	Log apron.Replay
	// Skipped is how many lines, arrays, and records the archive had that
	// were not valid, which were skipped.
	Skipped int

	// oldest and newest are each room's first and last log_id among the
	// archive's records.
	oldest, newest map[string]apron.LogID
}

// Listing is a listed room (§3.1): its record as room_list lists it, and
// whether the archive's you had joined it.
type Listing struct {
	apron.RoomView
	Joined bool
}

// Read reads the archive r holds, as NewReader and Reader.Next do, and
// replays it into an Archive.
func Read(r io.Reader) (*Archive, error) {
	ar, err := NewReader(r)
	if err != nil {
		return nil, err
	}
	a := &Archive{Header: ar.Header}
	a.Add(Part{})
	for {
		p, err := ar.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		a.Add(p)
	}
	a.Skipped = ar.Skipped()
	return a, nil
}

// Add applies a part to the archive, as a later line of it.
func (a *Archive) Add(p Part) {
	if a.Rooms == nil {
		a.Rooms, a.Users = map[string]Listing{}, map[string]apron.User{}
		a.oldest, a.newest = map[string]apron.LogID{}, map[string]apron.LogID{}
	}
	for _, v := range p.Joined {
		a.Rooms[v.RoomID] = Listing{v, true}
	}
	for _, v := range p.NotJoined {
		a.Rooms[v.RoomID] = Listing{v, false}
	}
	for _, u := range p.Users {
		a.Users[u.UserID] = u
	}
	saw := func(roomID string, id apron.LogID) {
		if oldest, ok := a.oldest[roomID]; !ok || id < oldest {
			a.oldest[roomID] = id
		}
		a.newest[roomID] = max(a.newest[roomID], id)
	}
	for _, room := range p.Rooms {
		saw(room.RoomID, room.LogID)
	}
	for _, m := range p.Messages {
		saw(m.RoomID, m.LogID)
	}
	for _, rec := range p.Reactions {
		saw(rec.RoomID, rec.LogID)
	}
	for _, rec := range p.Memberships {
		saw(rec.RoomID, rec.LogID)
	}
	a.Log.ApplyHistory(p.History())
}

// Stretch returns the stretch of a room's log that the archive holds
// (§3.1): every record of the room from start to end, both inclusive,
// that the server held. start is its listing's history_log_id, or, when
// the listing leaves it out, the room's oldest record in the archive; end
// is its latest_log_id, or, without one, the room's newest record. It
// reports false for a room the archive does not list, and one whose
// history_log_id is null, which the archive holds only the listing of.
func (a *Archive) Stretch(roomID string) (start, end apron.LogID, ok bool) {
	listing, ok := a.Rooms[roomID]
	if !ok {
		return 0, 0, false
	}
	switch {
	case listing.HistoryLogID == nil:
		start, ok = a.oldest[roomID]
		if !ok {
			return 0, 0, false
		}
	case *listing.HistoryLogID == 0:
		return 0, 0, false
	default:
		start = *listing.HistoryLogID
	}
	end = listing.LatestLogID
	if end == 0 {
		end = a.newest[roomID]
	}
	if end < start {
		// An empty stretch: the room's log starts after its latest.
		end = start - 1
	}
	return start, end, true
}

// Reference reports whether a record of a room at a log_id is a reference
// (§3.1): of a room the archive does not list, or outside its stretch.
// A reader uses a message snapshot that is a reference only as the target
// of a reply_to, and does not show it in its room.
func (a *Archive) Reference(roomID string, logID apron.LogID) bool {
	start, end, ok := a.Stretch(roomID)
	return !ok || logID < start || logID > end
}

// Messages returns the messages a reader shows in a room: those whose
// kept snapshot is in the room and in its stretch, tombstones too, in the
// order they were posted in.
func (a *Archive) Messages(roomID string) []apron.Message {
	return slices.DeleteFunc(a.Log.Messages(roomID), func(m apron.Message) bool { return a.Reference(roomID, m.LogID) })
}
