package rooms

import (
	"bytes"
	"encoding/json/jsontext"
	"slices"
	"testing"

	"github.com/apron-chat/apron-server-go/apron/internal/roomlog"
)

type user struct {
	id    string
	scope *Scope
}

func (u *user) MemberID() string     { return u.id }
func (u *user) MemberScope() *Scope  { return u.scope }
func (u *user) from() roomlog.Author { return roomlog.Author{UserID: u.id} }

func newTable() *Table[*user] { return NewTable[*user](roomlog.NewLog()) }

func join(t *testing.T, table *Table[*user], r *Room[*user], users ...*user) {
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
