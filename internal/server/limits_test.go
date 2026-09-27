package server

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUploadsAreStoredOnDiskWithinLimits(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "left-by-an-earlier-run"+uploadSuffix)
	other := filepath.Join(dir, "notes.txt")
	for _, name := range []string{stale, other} {
		if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	config := DefaultConfig()
	config.UploadDir = dir
	config.MaxUploadBytes = 100
	config.MaxMessageUploadBytes = 150
	config.MaxUploadStorageBytes = 250
	_, httpServer := newTestServer(t, config)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale upload file kept: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("unrelated file removed: %v", err)
	}
	a := dialTestClient(t, httpServer)
	put := func(writeURL string, size int) int {
		t.Helper()
		status, _, _ := httpDo(t, http.MethodPut, writeURL, bytes.NewReader(bytes.Repeat([]byte("a"), size)), "text/plain")
		return status
	}
	writeURLs := func(result map[string]any) []string {
		var urls []string
		for _, value := range result["embeds"].([]any) {
			urls = append(urls, value.(map[string]any)["write_url"].(string))
		}
		return urls
	}

	// One message's uploads share MaxMessageUploadBytes.
	first := postEmbeds(t, a, "first", map[string]any{"room_id": "general", "body": map[string]any{
		"embeds": []any{map[string]any{"kind": "upload"}, map[string]any{"kind": "upload"}},
	}})
	urls := writeURLs(first)
	if status := put(urls[0], 100); status != http.StatusCreated {
		t.Fatalf("first upload: %d", status)
	}
	firstFile := embedsOf(t, a.notification(t, "message"))[0]["url"].(string)
	if status := put(urls[1], 100); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("upload beyond the message's budget: %d", status)
	}
	if left := embedsOf(t, a.notification(t, "message")); len(left) != 1 {
		t.Fatalf("after the refused upload: %#v", left)
	}

	second := postEmbeds(t, a, "second", map[string]any{"room_id": "general", "body": map[string]any{"embeds": []any{map[string]any{"kind": "upload"}}}})
	if status := put(writeURLs(second)[0], 100); status != http.StatusCreated {
		t.Fatalf("second upload: %d", status)
	}
	secondFile := embedsOf(t, a.notification(t, "message"))[0]["url"].(string)
	if files, _ := filepath.Glob(filepath.Join(dir, "*"+uploadSuffix)); len(files) != 2 {
		t.Fatalf("upload files: %v", files)
	}

	// Past MaxUploadStorageBytes the oldest upload leaves its message.
	third := postEmbeds(t, a, "third", map[string]any{"room_id": "general", "body": map[string]any{"embeds": []any{map[string]any{"kind": "upload"}}}})
	if status := put(writeURLs(third)[0], 100); status != http.StatusCreated {
		t.Fatalf("third upload: %d", status)
	}
	if completed := a.notification(t, "message"); completed["message_id"] != third["message_id"] {
		t.Fatalf("expected the third upload to complete first: %#v", completed)
	}
	evicted := a.notification(t, "message")
	if evicted["message_id"] != first["message_id"] || len(embedsOf(t, evicted)) != 0 {
		t.Fatalf("eviction: %#v", evicted)
	}
	if status, _, _ := httpDo(t, http.MethodGet, firstFile, nil, ""); status != http.StatusNotFound {
		t.Fatalf("evicted file: %d", status)
	}
	if status, _, _ := httpDo(t, http.MethodGet, secondFile, nil, ""); status != http.StatusOK {
		t.Fatalf("kept file: %d", status)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "*"+uploadSuffix)); len(files) != 2 {
		t.Fatalf("upload files after eviction: %v", files)
	}
}

func TestMessagesAreLimitedToMaxEmbeds(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	a := dialTestClient(t, httpServer)
	embeds := make([]any, maxEmbedsPerMessage+1)
	for i := range embeds {
		embeds[i] = map[string]any{"kind": "upload"}
	}
	a.expectError(t, "message", "many", map[string]any{"room_id": "general", "body": map[string]any{"embeds": embeds}}, codeInvalidParams)
}

func TestRoomSetCountsTowardMessagesPerMinute(t *testing.T) {
	config := DefaultConfig()
	config.MessagesPerMinute = 1
	_, httpServer := newTestServer(t, config)
	a := dialTestClient(t, httpServer)
	saveRoom(t, a, "create", map[string]any{"title": "Ops"})
	frame := a.call(t, "room_set", "again", map[string]any{"title": "More"})
	if failure, _ := frame["error"].(map[string]any); failure == nil || failure["code"] != float64(codeRetryAfter) {
		t.Fatalf("second room_set: %#v", frame)
	}
	frame = a.call(t, "message", "post", map[string]any{"room_id": "general", "body": map[string]any{"text": "hi"}})
	if failure, _ := frame["error"].(map[string]any); failure == nil || failure["code"] != float64(codeRetryAfter) {
		t.Fatalf("message after room_set: %#v", frame)
	}
}

func TestHistoryPagesAreBoundedAndRecordsUnescaped(t *testing.T) {
	app, httpServer := newTestServer(t, DefaultConfig())
	a := dialTestClient(t, httpServer)
	save(t, a, "markup", map[string]any{"body": map[string]any{"text": "<a & b>"}})
	app.mu.RLock()
	log := app.rooms["general"].log
	raw := string(log[len(log)-1].raw)
	app.mu.RUnlock()
	if !strings.Contains(raw, "<a & b>") {
		t.Fatalf("stored record escapes markup: %s", raw)
	}

	large := strings.Repeat("x", 200<<10)
	const posts = 30
	for i := range posts {
		save(t, a, "large-"+string(rune('a'+i)), map[string]any{"body": map[string]any{"text": large}})
	}
	page := historyPage(t, a, "general", map[string]any{"limit": 1000})
	newest := logIDs(t, page, "messages")
	if page["more"] != true || len(newest) >= posts || len(newest) == 0 {
		t.Fatalf("page of %d messages, more=%v", len(newest), page["more"])
	}
	if page["last_log_id"] != newest[len(newest)-1] || page["first_log_id"] == nil {
		t.Fatalf("page bounds: %v %v", page["first_log_id"], page["last_log_id"])
	}
	before := parseID(t, page["first_log_id"]) - 1
	rest := historyPage(t, a, "general", map[string]any{"limit": 1000, "before": formatID(before)})
	if got := len(logIDs(t, rest, "messages")) + len(newest); got < posts {
		t.Fatalf("two pages hold %d of %d large messages", got, posts)
	}
}

func TestProfileAndPushSizeLimits(t *testing.T) {
	config := DefaultConfig()
	config.AllowInsecurePush = true
	_, httpServer := newTestServer(t, config)
	a := dialTestClient(t, httpServer)
	a.expectError(t, "me", "ext", map[string]any{"ext": map[string]any{"x": strings.Repeat("y", maxProfileExtBytes)}}, codeInvalidParams)
	a.expectError(t, "push_register", "url", map[string]any{"kind": "relay", "url": "http://relay.example/" + strings.Repeat("p", maxPushURLBytes)}, codeInvalidParams)
}

func TestConnectionAndRateLimits(t *testing.T) {
	config := DefaultConfig()
	config.MaxConnections = 1
	config.MessagesPerMinute = 2
	_, httpServer := newTestServer(t, config)
	a := dialTestClient(t, httpServer)

	// Over capacity: the server frame, then an error without id, then close.
	b, _ := dialRaw(t, httpServer)
	failure := b.read(t)
	if _, has := failure["id"]; has || failure["error"].(map[string]any)["code"] != float64(codeRetryAfter) ||
		failure["error"].(map[string]any)["data"].(map[string]any)["retry_after"] != float64(retryAfterSeconds) {
		t.Fatalf("capacity error: %#v", failure)
	}

	save(t, a, "one", map[string]any{"body": map[string]any{"text": "1"}})
	save(t, a, "two", map[string]any{"body": map[string]any{"text": "2"}})
	frame := a.call(t, "message", "three", map[string]any{"room_id": "general", "body": map[string]any{"text": "3"}})
	limited := frame["error"].(map[string]any)
	if limited["code"] != float64(codeRetryAfter) || limited["data"].(map[string]any)["retry_after"].(float64) < 1 {
		t.Fatalf("rate limit: %#v", frame)
	}
}
