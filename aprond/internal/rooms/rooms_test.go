package rooms

import (
	"bytes"
	"encoding/json/jsontext"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/aprond/internal/roomlog"
	"github.com/apron-chat/apron-server-go/store"
)

type user struct {
	id    string
	scope *Scope
}

func (u *user) MemberID() string     { return u.id }
func (u *user) MemberScope() *Scope  { return u.scope }
func (u *user) from() roomlog.Author { return roomlog.Author{UserID: u.id} }

func newTable() *Table[*user, *Message] { return NewTable[*user](roomlog.NewLog(), (*Message).Kept) }

func join(t *testing.T, table *Table[*user, *Message], r *Room[*user], users ...*user) {
	t.Helper()
	for _, u := range users {
		if _, ok := table.Join(r, u, u.from()); !ok {
			t.Fatalf("%s did not join %s", u.id, r.ID())
		}
	}
}

// A private room, and every thread of one, is visible only to the members
// of every private room on the way up; a scope narrows what a user sees.
func TestVisibility(t *testing.T) {
	table := newTable()
	ada, bob, cy := &user{id: "ada"}, &user{id: "bob"}, &user{id: "cy"}
	general := table.Create("general", nil, false, "", roomlog.RoomRecord{Title: "General"})
	secret := table.Create("", general, true, "ada", roomlog.RoomRecord{})
	thread := table.Create("", secret, false, "ada", roomlog.RoomRecord{})
	join(t, table, secret, ada, bob)
	join(t, table, thread, cy)
	for _, c := range []struct {
		room *Room[*user]
		u    *user
		want bool
	}{
		{general, cy, true},
		{secret, ada, true},
		{secret, cy, false},
		{thread, bob, true},
		// A member of the thread who is not in the private room above it.
		{thread, cy, false},
	} {
		if got := c.room.VisibleTo(c.u); got != c.want {
			t.Errorf("%s sees %s: %v, want %v", c.u.id, c.room.ID(), got, c.want)
		}
	}
	if table.Visible(secret.ID(), cy) != nil || table.Visible("nowhere", ada) != nil {
		t.Error("Visible returned a room the user may not see")
	}
	bot := &user{id: "bot", scope: NewScope([]string{general.ID()}, false)}
	if !general.VisibleTo(bot) || NewScope([]string{general.ID()}, false).Threads() {
		t.Error("a scope hides its own room")
	}
	other := table.Create("", general, false, "", roomlog.RoomRecord{})
	if other.VisibleTo(bot) {
		t.Error("a scope without threads shows a thread")
	}
	if _, ok := table.Join(other, bot, bot.from()); ok {
		t.Error("a scoped user joined a room outside its scope")
	}
	bot.scope = NewScope([]string{general.ID()}, true)
	if !other.VisibleTo(bot) {
		t.Error("a scope with threads hides a thread")
	}
	if got := bot.scope.Rooms(); !slices.Equal(got, []string{"general"}) {
		t.Errorf("scope rooms %v", got)
	}
}

// A record in a room reveals another room to whoever sees the first but not
// the second.
func TestRevealsTo(t *testing.T) {
	table := newTable()
	ada, bob := &user{id: "ada"}, &user{id: "bob"}
	general := table.Create("general", nil, false, "", roomlog.RoomRecord{})
	both := table.Create("", nil, true, "", roomlog.RoomRecord{})
	one := table.Create("", nil, true, "", roomlog.RoomRecord{})
	join(t, table, both, ada, bob)
	join(t, table, one, ada)
	if !general.RevealsTo(one) || !both.RevealsTo(one) {
		t.Error("a record seen by more people does not reveal a private room")
	}
	if one.RevealsTo(both) || one.RevealsTo(general) || both.RevealsTo(general) {
		t.Error("a record reveals a room everyone it reaches can see")
	}
	everyone, users := both.Audience()
	if everyone || len(users) != 2 {
		t.Errorf("audience of a private room: %v %v", everyone, users)
	}
}

// Joins and leaves are logged membership records in the room's log; a
// listing past its limit keeps the most recently active.
func TestMembership(t *testing.T) {
	table := newTable()
	ada, bob, cy := &user{id: "ada"}, &user{id: "bob"}, &user{id: "cy"}
	r := table.Create("", nil, false, "ada", roomlog.RoomRecord{Title: "Ops"})
	join(t, table, r, ada, bob, cy)
	if _, again := table.Join(r, ada, ada.from()); again {
		t.Error("a member joined again")
	}
	record, left := table.Leave(r, bob, bob.from())
	if !left || record.Kind != roomlog.KindMembership || r.Log().Latest() != record.ID || r.Member("bob") != nil {
		t.Fatalf("leave: %v %+v", left, record)
	}
	if _, again := table.Leave(r, bob, bob.from()); again {
		t.Error("a user who left left again")
	}
	table.Posted(r, "ada", r.Log().Latest()+1)
	if ids, truncated := r.ListedMemberIDs(1); !truncated || !slices.Equal(ids, []string{"ada"}) {
		t.Errorf("listed %v %v, want the most recently active", ids, truncated)
	}
	if ids, truncated := r.ListedMemberIDs(0); truncated || !slices.Equal(ids, []string{"ada", "cy"}) {
		t.Errorf("listed %v %v, want every member", ids, truncated)
	}
	if !table.Read(r, "cy", Cursor{MessageID: "5", ID: 5}) || table.Read(r, "cy", Cursor{MessageID: "4", ID: 4}) {
		t.Error("a read cursor did not only advance")
	}
	if r.Title() != "Ops" || r.Creator() != "ada" || r.MemberCount() != 2 {
		t.Errorf("room %s by %s with %d members", r.Title(), r.Creator(), r.MemberCount())
	}
}

// Every change is stored, and stored rooms come back as they were, with
// their members and threads.
func TestStoreRoundTrip(t *testing.T) {
	table := newTable()
	ada := &user{id: "ada"}
	general := table.Create("general", nil, false, "", roomlog.RoomRecord{Title: "General"})
	thread := table.Create("", general, true, "ada", roomlog.RoomRecord{Description: "Plans"})
	join(t, table, general, ada)
	join(t, table, thread, ada)
	table.Read(general, "ada", Cursor{From: ada.from(), MessageID: "9", ID: 9})
	table.Update(general, roomlog.RoomRecord{Title: "General 2"})
	entries := table.Entries()
	if len(entries) != 2 || table.Entries() != nil {
		t.Fatalf("%d entries, then more", len(entries))
	}
	values := make(map[string]jsontext.Value)
	for _, e := range entries {
		values[e.ID] = e.Value
	}
	restored := newTable()
	if err := restored.Restore(values); err != nil {
		t.Fatal(err)
	}
	restored.RestoreMembers(func(_ *Room[*user], id string) (*user, bool) { return &user{id: id}, id == "ada" })
	for id, value := range values {
		if again := Entry(restored.Get(id)).Value; !bytes.Equal(again, value) {
			t.Errorf("room %s is stored again as\n%s\nwant\n%s", id, again, value)
		}
	}
	if r := restored.Get(thread.ID()); r.Parent() != restored.Get("general") || len(restored.Get("general").Children()) != 1 {
		t.Error("a thread lost its parent")
	}
	if restored.Entries() != nil {
		t.Error("restoring changed rooms")
	}
}

// A listing sorts joined, unjoined, and left rooms by activity, leaves
// unjoined threads to their parent's listing and private rooms to their
// members, and bounds the unjoined.
func TestList(t *testing.T) {
	table := newTable()
	ada, bob := &user{id: "ada"}, &user{id: "bob"}
	general := table.Create("general", nil, false, "", roomlog.RoomRecord{})
	ops := table.Create("", nil, false, "", roomlog.RoomRecord{})
	thread := table.Create("", general, false, "", roomlog.RoomRecord{})
	secret := table.Create("", nil, true, "", roomlog.RoomRecord{})
	join(t, table, general, ada)
	join(t, table, secret, bob)
	since := table.log.Last()
	join(t, table, ops, ada)
	record, _ := table.Leave(ops, ada, ada.from())
	leftAt := func(id string) int64 {
		if id == ops.ID() {
			return record.ID
		}
		return 0
	}
	ids := func(rooms []*Room[*user]) []string {
		var ids []string
		for _, r := range rooms {
			ids = append(ids, r.ID())
		}
		return ids
	}
	all := table.All()
	joined, notJoined, left := List(ada, all, Listing{Joined: true, NotJoined: true}, leftAt)
	if !slices.Equal(ids(joined), []string{"general"}) || !slices.Equal(ids(notJoined), []string{ops.ID()}) || left != nil {
		t.Errorf("listed %v %v %v", ids(joined), ids(notJoined), ids(left))
	}
	_, notJoined, _ = List(ada, general.Children(), Listing{NotJoined: true, Threads: true, Limit: 1}, leftAt)
	if !slices.Equal(ids(notJoined), []string{thread.ID()}) {
		t.Errorf("threads listed %v", ids(notJoined))
	}
	joined, notJoined, left = List(ada, all, Listing{Joined: true, NotJoined: true, Since: since, HasSince: true}, leftAt)
	if joined != nil || !slices.Equal(ids(notJoined), []string{ops.ID()}) || !slices.Equal(ids(left), []string{ops.ID()}) {
		t.Errorf("listed since %v %v %v", ids(joined), ids(notJoined), ids(left))
	}
}

func TestThreadTitle(t *testing.T) {
	long := strings.Repeat("é", maxThreadTitleRunes+5)
	for description, want := range map[string]string{
		"":                      defaultThreadTitle,
		"  First line\nsecond ": "First line",
		long:                    strings.Repeat("é", maxThreadTitleRunes) + "…",
	} {
		if got := ThreadTitle(description); got != want {
			t.Errorf("ThreadTitle(%q) = %q, want %q", description, got, want)
		}
	}
}

// A large room's entry lags behind its latest_log_id, activity, and read
// cursors for EntryDelay, but never behind its members or its record, and
// Entries takes it at once.
func TestLargeRoomEntryLags(t *testing.T) {
	table := newTable()
	big := table.Create("big", nil, false, "", roomlog.RoomRecord{})
	small := table.Create("small", nil, false, "", roomlog.RoomRecord{})
	var members []*user
	for i := range LaterMembers {
		members = append(members, &user{id: "u" + strings.Repeat("x", i)})
	}
	join(t, table, big, members...)
	join(t, table, small, members[0])
	ids := func(entries []store.Entry) []string {
		var ids []string
		for _, e := range entries {
			if e.Kind == EntryKind {
				ids = append(ids, e.ID)
			}
		}
		slices.Sort(ids)
		return ids
	}
	start := time.Now()
	if got := ids(table.EntriesDue(start)); !slices.Equal(got, []string{"big", "small"}) {
		t.Fatalf("joins stored %v at once, want both rooms", got)
	}
	post := func(r *Room[*user], logID int64) {
		m := table.NewMessage(roomlog.FormatID(logID), members[0].from(), members[0].id)
		table.Snapshot(m, &roomlog.Message{MessageID: roomlog.FormatID(logID), LogID: roomlog.FormatID(logID), RoomID: r.ID()}, logID)
	}
	post(big, table.log.NextID())
	post(small, table.log.NextID())
	table.Read(big, members[1].id, Cursor{MessageID: "1", ID: 1})
	if got := ids(table.EntriesDue(start)); !slices.Equal(got, []string{"small"}) {
		t.Fatalf("posts stored %v at once, want only the small room", got)
	}
	if due, lagging := table.Lagging(); !lagging || due.Before(start.Add(EntryDelay)) {
		t.Fatalf("lagging until %v (%v), want at least %v", due, lagging, EntryDelay)
	}
	if got := ids(table.EntriesDue(start.Add(EntryDelay / 2))); got != nil {
		t.Fatalf("stored %v before the delay", got)
	}
	if got := ids(table.EntriesDue(start.Add(EntryDelay))); !slices.Equal(got, []string{"big"}) {
		t.Fatalf("stored %v after the delay, want the big room", got)
	}
	if _, lagging := table.Lagging(); lagging {
		t.Fatal("still lagging")
	}
	// A join renders a lagging room at once; so does Entries.
	post(big, table.log.NextID())
	join(t, table, big, &user{id: "late"})
	if got := ids(table.EntriesDue(start)); !slices.Equal(got, []string{"big"}) {
		t.Fatalf("a join stored %v, want the big room", got)
	}
	post(big, table.log.NextID())
	if got := ids(table.Entries()); !slices.Equal(got, []string{"big"}) {
		t.Fatalf("Entries stored %v, want the big room", got)
	}
}

// A room's listed members are kept until its members change, or, when
// the listing is truncated, their activity does.
func TestListedMembersKept(t *testing.T) {
	table := newTable()
	r := table.Create("r", nil, false, "", roomlog.RoomRecord{})
	ada, bob, cy := &user{id: "ada"}, &user{id: "bob"}, &user{id: "cy"}
	join(t, table, r, ada, bob)
	first, _ := r.ListedMemberIDs(0)
	if again, _ := r.ListedMemberIDs(0); &again[0] != &first[0] {
		t.Error("the listing was not kept")
	}
	join(t, table, r, cy)
	if ids, _ := r.ListedMemberIDs(0); !slices.Equal(ids, []string{"ada", "bob", "cy"}) {
		t.Fatalf("after a join, listed %v", ids)
	}
	table.Posted(r, "ada", r.Log().Latest()+1)
	if ids, truncated := r.ListedMemberIDs(1); !truncated || !slices.Equal(ids, []string{"ada"}) {
		t.Fatalf("listed %v %v, want the most recently active", ids, truncated)
	}
	table.Posted(r, "cy", r.Log().Latest()+2)
	if ids, _ := r.ListedMemberIDs(1); !slices.Equal(ids, []string{"cy"}) {
		t.Fatalf("after a post, listed %v, want cy", ids)
	}
	table.Leave(r, cy, cy.from())
	if ids, _ := r.ListedMemberIDs(0); !slices.Equal(ids, []string{"ada", "bob"}) {
		t.Fatalf("after a leave, listed %v", ids)
	}
}
