package aprond

import "testing"

// A room_list with members sends each member as they are now, after a
// listing that encoded them as they were, with their profile, status, and
// the room's record changed since.
func TestRoomListAfterChanges(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	lister, _ := dialRaw(t, httpServer)
	guestAuth(t, lister)
	other, _ := dialRaw(t, httpServer)
	guestAuth(t, other)
	params := map[string]any{"filter": "joined", "members": true}
	// The notifications before each result, such as of the other's join,
	// are not what is checked.
	list := func() map[string]any {
		_, result := lister.request(t, "room_list", lister.nextID("list"), params)
		return result
	}
	me := func(params map[string]any) {
		other.request(t, "me", other.nextID("me"), params)
	}
	find := func(result map[string]any) (user, member map[string]any) {
		for _, u := range asMaps(result["users"]) {
			if u["user_id"] == other.userID {
				user = u
			}
		}
		for _, room := range asMaps(result["joined"]) {
			for _, m := range asMaps(room["members"]) {
				if m["user_id"] == other.userID {
					member = m
				}
			}
		}
		if user == nil || member == nil {
			t.Fatalf("%s not listed in %#v", other.userID, result)
		}
		return user, member
	}
	for range 2 {
		before, _ := find(list())
		if before["name"] == "Renamed" {
			t.Fatal("renamed before the rename")
		}
	}
	me(map[string]any{"name": "Renamed", "status": "dnd"})
	user, member := find(list())
	if user["name"] != "Renamed" || user["status"] != "dnd" || member["status"] != "dnd" {
		t.Fatalf("after the change: user %#v, member %#v", user, member)
	}
	me(map[string]any{"status": "online"})
	if user, member := find(list()); user["status"] != "online" || member["status"] != "online" {
		t.Fatalf("after the second change: user %#v, member %#v", user, member)
	}
}
