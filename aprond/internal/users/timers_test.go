package users

import (
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/apron-chat/apron-server-go/aprond/internal/presence"
	"golang.org/x/time/rate"
)

// A timed mute ends itself through the callback, unless it changed since;
// deleting the user, or Stop, cancels its timers.
func TestMuteTimers(t *testing.T) {
	var mu sync.Mutex
	ended := make(chan string, 4)
	var table *Table[*testUser]
	table = NewTable(Timers[*testUser]{MuteEnded: func(u *testUser, roomID string, until time.Time) {
		mu.Lock()
		defer mu.Unlock()
		if table.EndMute(u, roomID, until) {
			ended <- roomID
		}
	}})
	ada := &testUser{id: "ada", User: New("Ada", time.Now())}
	mu.Lock()
	table.Add(ada)
	table.Entries(noAvatar)
	table.SetMute(ada, "", Mute{Forever: true})
	table.SetMute(ada, "general", Mute{Until: time.Now().Add(time.Hour)})
	if table.Pending("ada") != 1 || len(table.Entries(noAvatar)) != 1 {
		t.Fatal("a timed room mute is not pending, or not stored")
	}
	if table.EndMute(ada, "general", time.Now()) {
		t.Error("a mute ended at another time than it was set until")
	}
	table.SetMute(ada, "general", Mute{Until: time.Now()})
	mu.Unlock()
	if roomID := <-ended; roomID != "general" {
		t.Fatalf("ended the mute of %q", roomID)
	}
	mu.Lock()
	if _, muted := ada.RoomMutes["general"]; muted || !ada.Mute.Forever || table.Pending("ada") != 0 {
		t.Errorf("after the room mute ended: %v, %v, %d pending", ada.RoomMutes, ada.Mute, table.Pending("ada"))
	}
	table.SetMute(ada, "general", Mute{})
	if _, muted := ada.RoomMutes["general"]; muted {
		t.Error("a zero mute kept")
	}
	table.SetMute(ada, "", Mute{Until: time.Now().Add(time.Hour)})
	table.Delete("ada")
	if table.Pending("ada") != 0 {
		t.Error("a deleted user's timer is pending")
	}
	table.Add(ada)
	table.ScheduleMutes(ada)
	table.Stop()
	table.SetMute(ada, "ops", Mute{Until: time.Now().Add(time.Hour)})
	if table.Pending("ada") != 0 {
		t.Error("a timer pending after Stop")
	}
	mu.Unlock()
}

// A shown status changes at once within the burst, and beyond it is held
// until the callback, which publishes it if still held.
func TestStatusHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		due := make(chan bool, 1)
		var table *Table[*testUser]
		table = NewTable(Timers[*testUser]{StatusDue: func(u *testUser) {
			mu.Lock()
			defer mu.Unlock()
			due <- table.ReleaseStatus(u)
		}})
		ada := &testUser{id: "ada", User: New("Ada", time.Now())}
		mu.Lock()
		table.Add(ada)
		if table.PublishStatus(ada) {
			t.Error("published a status that did not change")
		}
		ada.Presence.Note(true, true, time.Now())
		if !table.PublishStatus(ada) {
			t.Fatal("a change within the burst is held")
		}
		ada.Presence.SetPublishLimit(rate.NewLimiter(rate.Every(time.Hour), 0))
		held := time.Now()
		if table.PublishStatus(ada) || table.Pending("ada") != 1 || table.PublishStatus(ada) {
			t.Fatal("a change beyond the burst is not held, once")
		}
		if table.ReleaseStatus(&testUser{id: "ada"}) {
			t.Error("released another user's status")
		}
		mu.Unlock()
		// The hold lasts presence.Coalesce, by the bubble's fake clock.
		if !<-due {
			t.Error("the held status was not released")
		}
		if waited := time.Since(held); waited != presence.Coalesce {
			t.Errorf("held for %v, want %v", waited, presence.Coalesce)
		}
	})
}
