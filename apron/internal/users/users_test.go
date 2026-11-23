package users

import (
	"encoding/json/jsontext"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron/internal/presence"
	"github.com/apron-chat/apron-server-go/internal/authn"
	"github.com/apron-chat/apron-server-go/store"
)

type testUser struct {
	id string
	User
}

func (u *testUser) UserID() string { return u.id }
func (u *testUser) Kept() *User    { return &u.User }

func noAvatar(*testUser) string { return "" }

func entryIDs(entries []store.Entry) (put, deleted []string) {
	for _, e := range entries {
		if e.Value == nil {
			deleted = append(deleted, e.ID)
		} else {
			put = append(put, e.ID)
		}
	}
	slices.Sort(put)
	slices.Sort(deleted)
	return put, deleted
}

func TestTableIdentities(t *testing.T) {
	table := NewTable[*testUser]()
	ada := &testUser{id: "ada", User: New("Ada", time.Now())}
	table.Add(ada)
	if ada.Account() || table.Get("ada") != ada || table.Len() != 1 {
		t.Fatal("a new user is a guest the table finds")
	}
	email := authn.Identity{Scheme: "email", Subject: "ada@example.com"}
	table.SetIdentity(ada, email, nil)
	table.SetIdentity(ada, authn.Identity{Scheme: "webauthn", Subject: "h"}, jsontext.Value(`{"n":1}`))
	if !ada.Account() || table.ByIdentity(email) != ada || table.Identities() != 2 || string(ada.Records["webauthn"]) != `{"n":1}` {
		t.Fatalf("after SetIdentity: %#v", ada.User)
	}
	table.DeleteIdentity(ada, "webauthn")
	if ada.Records["webauthn"] != nil || ada.Identities["webauthn"] != "" || table.Identities() != 1 {
		t.Fatalf("after DeleteIdentity: %#v", ada.User)
	}
	if put, _ := entryIDs(table.Entries(noAvatar)); !slices.Equal(put, []string{"ada"}) {
		t.Fatalf("entries %v", put)
	}
	if table.Entries(noAvatar) != nil {
		t.Fatal("entries again")
	}
	table.Delete("ada")
	if table.Get("ada") != nil || table.ByIdentity(email) != nil || table.Identities() != 0 {
		t.Fatal("a deleted user is found")
	}
	if _, deleted := entryIDs(table.Entries(noAvatar)); !slices.Equal(deleted, []string{"ada"}) {
		t.Fatalf("deleted %v", deleted)
	}
}

func TestMutes(t *testing.T) {
	now := time.Now()
	if (Mute{}).Active(now) || !(Mute{Forever: true}).Active(now) || !(Mute{Until: now.Add(time.Second)}).Active(now) || (Mute{Until: now}).Active(now) {
		t.Fatal("Active")
	}
	table := NewTable[*testUser]()
	u := &testUser{id: "u", User: New("", now)}
	u.Mute = Mute{Until: now.Add(-time.Second)}
	u.RoomMutes["a"] = Mute{Until: now.Add(-time.Second)}
	u.RoomMutes["b"] = Mute{Forever: true}
	table.Add(u)
	entries := table.Entries(noAvatar)
	if got := string(entries[0].Value); got != `{"left_at":{},"room_mutes":{"b":{"forever":true}},"pings":{},"created":`+strconv.FormatInt(now.UnixMilli(), 10)+`}` {
		t.Fatalf("stored %s", got)
	}
}

// Restore converts what servers stored before identities, restores a user
// that was connected when the server stopped as no longer, and stores both
// again.
func TestRestore(t *testing.T) {
	now := time.UnixMilli(1795400000000)
	table := NewTable[*testUser]()
	avatars, err := table.Restore(map[string]jsontext.Value{
		"old":   jsontext.Value(`{"name":"Old","email":"old@example.com","external":"github:8","granted":["moderator"],"avatar_embed":"embed_1"}`),
		"shown": jsontext.Value(`{"identities":{"email":"s@example.com"},"status":"dnd","shown":true,"scope":{"rooms":["general"],"threads":true}}`),
		"quiet": jsontext.Value(`{"identities":{"email":"q@example.com"},"status":"unknown","seen":5}`),
	}, func(id string, u User) *testUser { return &testUser{id, u} }, now)
	if err != nil {
		t.Fatal(err)
	}
	old, shown, quiet := table.Get("old"), table.Get("shown"), table.Get("quiet")
	if avatars["old"] != "embed_1" || len(avatars) != 1 {
		t.Fatalf("avatars %v", avatars)
	}
	if old.Identities["email"] != "old@example.com" || old.Identities["external"] != "github:8" || !slices.Equal(old.Granted["external"], []string{"moderator"}) {
		t.Fatalf("old: %#v", old.User)
	}
	if table.ByIdentity(authn.Identity{Scheme: "external", Subject: "github:8"}) != old {
		t.Fatal("a converted identity finds no account")
	}
	if shown.Presence.Status != presence.Offline || shown.Presence.Seen != now.UnixMilli() || !shown.Scope.Threads() {
		t.Fatalf("shown: %#v", shown.User)
	}
	if quiet.Presence.Chosen != presence.None || quiet.Presence.Seen != 5 || quiet.LeftAt == nil || quiet.Created != (time.Time{}) {
		t.Fatalf("quiet: %#v", quiet.User)
	}
	if put, _ := entryIDs(table.Entries(noAvatar)); !slices.Equal(put, []string{"old", "shown"}) {
		t.Fatalf("stored again at once: %v", put)
	}
	if _, err := table.Restore(map[string]jsontext.Value{"bad": jsontext.Value(`{"passkey":1}`)}, func(id string, u User) *testUser { return &testUser{id, u} }, now); err == nil {
		t.Fatal("a bad passkey record restores")
	}
}
