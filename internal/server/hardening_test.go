package server

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/internal/store"
)

// reply sends a request and returns its reply, skipping the notifications
// before it.
func reply(t *testing.T, c *testClient, method, id string, params map[string]any) map[string]any {
	t.Helper()
	c.write(t, map[string]any{"method": method, "id": id, "params": params})
	for {
		if frame := c.read(t); frame["id"] == id {
			return frame
		}
	}
}

// errorCode returns a reply's error code, 0 for a result.
func errorCode(frame map[string]any) int {
	failure, _ := frame["error"].(map[string]any)
	code, _ := failure["code"].(float64)
	return int(code)
}

func TestRequestIDsAreBounded(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	c := dialTestClient(t, httpServer)
	long := strings.Repeat("x", maxRequestIDBytes+1)
	c.expectError(t, "me", long, map[string]any{}, codeTooLarge)
	c.result(t, "me", long[:maxRequestIDBytes], map[string]any{})
}

func TestRoomTitlesAreBounded(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	c := dialTestClient(t, httpServer)
	c.expectError(t, "room_set", "long", map[string]any{"room_id": "general", "title": strings.Repeat("x", maxTitleBytes+1)}, codeTooLarge)
	c.expectError(t, "room_set", "new", map[string]any{"title": strings.Repeat("x", maxTitleBytes+1)}, codeTooLarge)
}

func TestActionsAreLimited(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	c := dialTestClient(t, httpServer)
	id, _ := save(t, c, "post", map[string]any{"body": map[string]any{"text": "hi"}})
	// Toggling a reaction logs a record each time, until the user's limit.
	admitted := 0
	for i := 0; ; i++ {
		emojis := []any{"👍"}
		if i%2 == 1 {
			emojis = []any{}
		}
		frame := reply(t, c, "reactions", fmt.Sprintf("r%d", i), map[string]any{"message_id": id, "emojis": emojis})
		if code := errorCode(frame); code == codeRetryAfter {
			break
		} else if code != 0 {
			t.Fatalf("reaction %d: %#v", i, frame)
		}
		if admitted++; admitted > 2*actionBurst {
			t.Fatalf("%d reactions admitted, want about %d", admitted, actionBurst)
		}
	}
	if admitted < actionBurst {
		t.Fatalf("limited after %d reactions, want at least %d", admitted, actionBurst)
	}
	// Joins and leaves share the limit; a request that changes nothing does not.
	c.expectError(t, "room_leave", "leave", map[string]any{"room_id": "general"}, codeRetryAfter)
	created := reply(t, c, "room_set", "room", map[string]any{"title": "Other"})
	room := created["result"].(map[string]any)["room_id"].(string)
	if frame := reply(t, c, "room_join", "again", map[string]any{"room_id": room}); errorCode(frame) != 0 {
		t.Fatalf("joining a joined room: %#v", frame)
	}
}

func TestRoomMutesClearOnceHidden(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	c := dialTestClient(t, httpServer)
	created := reply(t, c, "room_set", "private", map[string]any{"title": "Ops", "private": true})
	room := created["result"].(map[string]any)["room_id"].(string)
	if frame := reply(t, c, "status", "mute", map[string]any{"room_id": room, "mute": true}); errorCode(frame) != 0 {
		t.Fatalf("mute: %#v", frame)
	}
	leaveRoom(t, c, room)
	// Muting a room the user cannot see stays unknown, but the mute it has
	// can be cleared.
	c.expectError(t, "status", "again", map[string]any{"room_id": room, "mute": true}, codeInvalidParams)
	if frame := reply(t, c, "status", "unmute", map[string]any{"room_id": room, "mute": false}); errorCode(frame) != 0 {
		t.Fatalf("unmute: %#v", frame)
	}
	c.expectError(t, "status", "twice", map[string]any{"room_id": room, "mute": false}, codeInvalidParams)
}

func TestPasskeyLoginStoresTheSignCounter(t *testing.T) {
	memory := store.NewMemory()
	dir := t.TempDir()
	_, httpServer, stop := startWithStore(t, memory, filepath.Join(dir, "uploads"))
	defer stop()
	owner, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	a := newTestAuthenticator(t)
	registerTestPasskey(t, owner, a)
	// The owner stays connected, so the login changes no status that would
	// store the account anyway. The store is read while the server runs, as
	// a crash would leave it: a shutdown stores every account it
	// disconnects.
	other, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	options := passkeyResult(t, passkeyCall(t, other, "begin", "login", "begin", nil))
	credential := a.assertion(t, options, testPasskeyOrigin, "localhost", 0x05)
	passkeyResult(t, passkeyCall(t, other, "finish", "login", "finish", map[string]any{"credential": credential}))

	storedCounter := func() uint32 {
		var counter uint32
		err := memory.Load(func(e store.Entry) error {
			var stored storedUser
			if e.Kind != entryUser || json.Unmarshal(e.Value, &stored) != nil || stored.Passkey == nil {
				return nil
			}
			counter = stored.Passkey.Credentials[0].Authenticator.SignCount
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return counter
	}
	// The store writer applies batches in order, off the request path.
	for deadline := time.Now().Add(time.Second); storedCounter() != a.counter; {
		if time.Now().After(deadline) {
			t.Fatalf("stored sign counter %d, want %d", storedCounter(), a.counter)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSessionsPerAccountAreBounded(t *testing.T) {
	app, _ := newTestServer(t, DefaultConfig())
	app.mu.Lock()
	defer app.unlock()
	u := addAccountLocked(app, "ada")
	other := addAccountLocked(app, "bea")
	app.sessions[[32]byte{0xff}] = session{user: other, expires: time.Now().Add(time.Hour)}
	now := time.Now()
	var newest [32]byte
	for i := range maxSessionsPerUser + 10 {
		newest = [32]byte{byte(i), byte(i >> 8), 1}
		// Older sessions expire sooner; the newest expires soonest of all,
		// and is still kept.
		expires := now.Add(time.Duration(i+1) * time.Minute)
		if i == maxSessionsPerUser+9 {
			expires = now
		}
		app.sessions[newest] = session{user: u, expires: expires}
		app.capSessionsLocked(u, newest)
	}
	count := 0
	for key, s := range app.sessions {
		if s.user == u {
			count++
			if expires := s.expires; key != newest && expires.Before(now.Add(11*time.Minute)) {
				t.Fatalf("kept a session that expires at %v", expires)
			}
		}
	}
	if count != maxSessionsPerUser {
		t.Fatalf("%d sessions kept, want %d", count, maxSessionsPerUser)
	}
	if _, kept := app.sessions[newest]; !kept {
		t.Fatal("the session just made was forgotten")
	}
	if _, kept := app.sessions[[32]byte{0xff}]; !kept {
		t.Fatal("another account's session was forgotten")
	}
}

// addAccountLocked adds an account without a connection or rooms.
func addAccountLocked(app *Server, id string) *userState {
	u := newUserState(id, id)
	u.passkey = &passkeyUser{user: u}
	app.users[id] = u
	return u
}

func TestArrivingUploadsAreBounded(t *testing.T) {
	config := DefaultConfig()
	config.MaxUploadStorageBytes = 100
	app, httpServer := newTestServer(t, config)
	c := dialTestClient(t, httpServer)
	writeURL := func(id string) string {
		result := postEmbeds(t, c, id, map[string]any{"body": map[string]any{"text": "file", "embeds": []any{map[string]any{"kind": "upload", "title": id + ".txt"}}}})
		return result["embeds"].([]any)[0].(map[string]any)["write_url"].(string)
	}
	first, second := writeURL("first"), writeURL("second")

	// The first upload's body arrives slowly, reserving the storage.
	body, writer := io.Pipe()
	done := make(chan int)
	go func() {
		request, _ := http.NewRequest(http.MethodPut, first, body)
		request.Header.Set("Content-Type", "text/plain")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			done <- 0
			return
		}
		response.Body.Close()
		done <- response.StatusCode
	}()
	if _, err := writer.Write([]byte("hello ")); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		app.mu.RLock()
		arriving := app.uploadsArriving
		app.mu.RUnlock()
		if arriving > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first upload did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	status, header, _ := httpDo(t, http.MethodPut, second, strings.NewReader("world"), "text/plain")
	if status != http.StatusServiceUnavailable || header.Get("Retry-After") == "" {
		t.Fatalf("second upload while the first arrives: %d %v", status, header)
	}
	writer.Close()
	if status := <-done; status != http.StatusCreated {
		t.Fatalf("first upload: %d", status)
	}
	// The refused write kept its URL.
	if status, _, _ := httpDo(t, http.MethodPut, second, strings.NewReader("world"), "text/plain"); status != http.StatusCreated {
		t.Fatalf("second upload after the first: %d", status)
	}
	app.mu.RLock()
	defer app.mu.RUnlock()
	if app.uploadsArriving != 0 {
		t.Fatalf("%d bytes still reserved", app.uploadsArriving)
	}
}

func TestImageAltIsBounded(t *testing.T) {
	alt := strings.Repeat("é", maxAltBytes)
	got := ogImageAlt(map[string]any{"image": map[string]any{"alt": alt}})
	if len(got) > maxAltBytes || !strings.HasPrefix(alt, got) || len(got) < maxAltBytes-1 {
		t.Fatalf("alt cut to %d bytes", len(got))
	}
}
