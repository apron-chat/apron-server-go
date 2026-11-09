package apron

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// waitNoWrites waits until the server holds no write for a user, which a
// guest's sign-out may have retired.
func waitNoWrites(t *testing.T, app *Server, userID string) {
	t.Helper()
	app.mu.RLock()
	u := app.users[userID]
	app.mu.RUnlock()
	if u == nil {
		t.Fatalf("no user %s", userID)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		app.mu.RLock()
		n := len(u.writes)
		app.mu.RUnlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s still has %d writes", userID, n)
		}
	}
}

// Signing an account out ends the writes it has not finished: a write URL
// not yet used stops working, and live text being written ends with what
// arrived, its writer told why.
func TestSignOutEndsWrites(t *testing.T) {
	app, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]

	_, unusedURL, _ := liveCommand(t, a, "unused", map[string]any{"body": map[string]any{"text": "/embed-terminal later"}})
	_, liveURL, _ := liveCommand(t, a, "live", map[string]any{"body": map[string]any{"text": "/embed-terminal now"}})
	pipe, writerDone := startLiveWrite(t, app, liveURL, "partial")
	posted := b.notification(t, "message")
	a.notification(t, "message")

	app.mu.Lock()
	u := app.users[a.userID]
	app.signOutLocked(u)
	app.unlock()

	if status := <-writerDone; status != http.StatusForbidden {
		t.Fatalf("live writer status %d", status)
	}
	_, _ = pipe.Write([]byte(" more"))
	_ = pipe.Close()
	finished := b.notification(t, "message")
	embeds := embedsOf(t, finished)
	if finished["message_id"] != posted["message_id"] || embeds[0]["output"] != "partial" {
		t.Fatalf("finished: %#v", finished)
	}
	if status, _, _ := httpDo(t, http.MethodPut, unusedURL, strings.NewReader("late"), ""); status != http.StatusNotFound {
		t.Fatalf("unused write URL after sign-out: %d", status)
	}
	app.mu.RLock()
	n := len(u.writes)
	app.mu.RUnlock()
	if n != 0 {
		t.Fatalf("writes left: %d", n)
	}
}

// Leaving a private room ends the writes into it: the room's members no
// longer include their author.
func TestLeavingAPrivateRoomEndsWritesIntoIt(t *testing.T) {
	app, httpServer := newTestServer(t, DefaultConfig())
	a := dialTestClient(t, httpServer)
	roomID, _ := saveRoom(t, a, "secret", map[string]any{"title": "Secret", "private": true})
	_, intoRoom, _ := liveCommand(t, a, "into", map[string]any{"room_id": roomID, "body": map[string]any{"text": "/embed-terminal private"}})
	_, intoGeneral, _ := liveCommand(t, a, "general", map[string]any{"body": map[string]any{"text": "/embed-terminal public"}})

	leaveRoom(t, a, roomID)
	if status, _, _ := httpDo(t, http.MethodPut, intoRoom, strings.NewReader("late"), ""); status != http.StatusNotFound {
		t.Fatalf("write into a room left: %d", status)
	}
	// A write elsewhere goes on.
	app.mu.RLock()
	n := len(app.users[a.userID].writes)
	app.mu.RUnlock()
	if n != 1 {
		t.Fatalf("writes left: %d", n)
	}
	if status, _, _ := httpDo(t, http.MethodPut, intoGeneral, strings.NewReader("done"), ""); status != http.StatusNoContent {
		t.Fatalf("write into general: %d", status)
	}
	waitNoWrites(t, app, a.userID)
}

// A write checks its author's access when it starts, as when an extension's
// allowlist dropped the account before it began.
func TestAWriteChecksItsAuthorWhenItStarts(t *testing.T) {
	app, httpServer := newTestServer(t, DefaultConfig())
	a := dialTestClient(t, httpServer)
	roomID, _ := saveRoom(t, a, "secret", map[string]any{"title": "Secret", "private": true})
	_, writeURL, _ := liveCommand(t, a, "into", map[string]any{"room_id": roomID, "body": map[string]any{"text": "/embed-append hi"}})
	// Taken out of the room behind the server's back, with no leave to
	// end the write.
	app.mu.Lock()
	r := app.rooms[roomID]
	delete(r.members, a.userID)
	app.unlock()
	if status, _, body := httpDo(t, http.MethodPut, writeURL, strings.NewReader("late"), ""); status != http.StatusForbidden || !strings.Contains(string(body), "may no longer write") {
		t.Fatalf("write by an author who lost access: %d %s", status, body)
	}
	waitNoWrites(t, app, a.userID)
}
