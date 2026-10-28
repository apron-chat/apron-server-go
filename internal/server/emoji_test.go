package server

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/apron-chat/apron-server-go/internal/store"
)

// Reaction entries are kept fully qualified, as Unicode's emoji-test.txt
// lists them (§4.7).
func TestQualifyEmoji(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"👍", "👍"},
		{"❤", "❤\uFE0F"},
		{"❤\uFE0F", "❤\uFE0F"},
		{"❤\uFE0F\uFE0F", "❤\uFE0F"},
		{"☺", "☺\uFE0F"},
		{"©", "©\uFE0F"},
		// A skin tone makes the emoji presentation: no U+FE0F before it.
		{"☝🏽", "☝🏽"},
		{"☝\uFE0F🏽", "☝🏽"},
		{"👍🏽", "👍🏽"},
		// ZWJ sequences qualify each element.
		{"🏳\u200D🌈", "🏳\uFE0F\u200D🌈"},
		{"🏴\u200D☠", "🏴\u200D☠\uFE0F"},
		{"👁\u200D🗨", "👁\uFE0F\u200D🗨\uFE0F"},
		{"⛹🏻\u200D♂", "⛹🏻\u200D♂\uFE0F"},
		{"👩🏽\u200D❤\u200D💋\u200D👨🏿", "👩🏽\u200D❤\uFE0F\u200D💋\u200D👨🏿"},
		{"👨\u200D👩\u200D👧", "👨\u200D👩\u200D👧"},
		// Keycaps take U+FE0F; a bare digit, #, or * is no emoji.
		{"#\u20E3", "#\uFE0F\u20E3"},
		{"1\uFE0F\u20E3", "1\uFE0F\u20E3"},
		{"1", "1"},
		{"12", "12"},
		{"#", "#"},
		// Flags and keys stay as they are.
		{"🇺🇦", "🇺🇦"},
		{"🏴\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067\U000E007F", "🏴\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067\U000E007F"},
		{":partyparrot:", ":partyparrot:"},
		{":party+1:", ":party+1:"},
		{"x\uFE0F", "x"},
		{"\uFE0F", ""},
	} {
		if got := qualifyEmoji(tc.in); got != tc.want {
			t.Errorf("qualifyEmoji(%+q) = %+q, want %+q", tc.in, got, tc.want)
		}
	}
}

// Entries that differ only in U+FE0F are the same entry: they collapse, and
// a set that differs from the kept one only in them is unchanged (§4.7).
func TestReactionsIgnoreVariationSelectors(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	c := dialTestClient(t, httpServer)
	id, _ := save(t, c, "create", map[string]any{"body": map[string]any{"text": "react to me"}})

	set := react(t, c, "set", id, "❤", "❤\uFE0F", "🏳\u200D🌈")
	if got := set["reactions"].([]any)[0].(map[string]any)["emojis"]; !reflect.DeepEqual(got, []any{"❤\uFE0F", "🏳\uFE0F\u200D🌈"}) {
		t.Fatalf("emojis %+q, want them collapsed and fully qualified", got)
	}
	c.result(t, "reactions", "same", map[string]any{"message_id": id, "emojis": []any{"🏳\uFE0F\u200D🌈", "❤"}})
	c.expectQuiet(t)
}

// catalogOf fetches the custom emoji catalog a server frame's params refer
// to (ext:emoji), and returns its URL and content.
func catalogOf(t *testing.T, params map[string]any) (string, map[string]any) {
	t.Helper()
	url := params["ext"].(map[string]any)["emoji"].(map[string]any)["catalog_url"].(string)
	status, header, body := httpDo(t, http.MethodGet, url, nil, "")
	if status != http.StatusOK || header.Get("Content-Type") != "application/json" ||
		header.Get("Access-Control-Allow-Origin") != "*" || !strings.Contains(header.Get("Cache-Control"), "immutable") {
		t.Fatalf("GET %s: %d %v", url, status, header)
	}
	var catalog map[string]any
	if err := json.Unmarshal(body, &catalog); err != nil {
		t.Fatalf("catalog %q: %v", body, err)
	}
	return url, catalog
}

// addEmoji runs /emoji add key on c with one upload embed, writes content to
// the returned write URL, and returns the write's HTTP status.
func addEmoji(t *testing.T, c *testClient, key string, content []byte, contentType string) int {
	t.Helper()
	return putEmoji(t, emojiWriteURL(t, c, key), content, contentType)
}

// emojiWriteURL runs /emoji add key on c with one upload embed and returns
// its write URL.
func emojiWriteURL(t *testing.T, c *testClient, key string) string {
	t.Helper()
	embed := map[string]any{"kind": "upload", "og": map[string]any{"image": map[string]any{"alt": "A party"}}}
	result := c.result(t, "command", c.nextID("emoji"), map[string]any{"body": map[string]any{"text": "/emoji add " + key, "embeds": []any{embed}}})
	return result["embeds"].([]any)[0].(map[string]any)["write_url"].(string)
}

// putEmoji writes content to an /emoji add write URL and returns the HTTP
// status.
func putEmoji(t *testing.T, writeURL string, content []byte, contentType string) int {
	t.Helper()
	status, _, _ := httpDo(t, http.MethodPut, writeURL, bytes.NewReader(content), contentType)
	return status
}

// uploadFiles counts the upload files in dir.
func uploadFiles(t *testing.T, dir string) int {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*"+uploadSuffix))
	if err != nil {
		t.Fatal(err)
	}
	return len(files)
}

// /emoji add and /emoji remove change the catalog, which the server serves
// at a new URL and announces with a new server frame to every connection;
// reactions accept the keys it has (ext:emoji).
func TestCustomEmoji(t *testing.T) {
	_, mailbox, httpServer := emailTestServer(t, func(config *Config) {
		config.Roles = map[string][]string{"admin": {"ada@example.com"}}
	})
	watcher, frame := dialRaw(t, httpServer)
	emptyURL, catalog := catalogOf(t, frame["params"].(map[string]any))
	if len(catalog) != 0 {
		t.Fatalf("catalog of a new server: %#v", catalog)
	}
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	admin, _ := dialRaw(t, httpServer)
	propose(t, admin, "ada@example.com")
	signInByEmail(t, admin, mailbox.receive(t).Code, nil)
	for _, c := range []*testClient{admin, a, b} {
		c.drain(t)
	}
	everyone := []*testClient{watcher, a, b, admin}

	if status := addEmoji(t, a, ":party:", testPNG(t), "image/png"); status != http.StatusCreated {
		t.Fatalf("emoji upload: %d", status)
	}
	var url string
	for _, c := range everyone {
		url, catalog = catalogOf(t, c.notification(t, "server"))
	}
	if url == emptyURL {
		t.Fatal("the catalog changed, but not its URL")
	}
	entry, _ := catalog[":party:"].(map[string]any)
	image, _ := entry["url"].(string)
	if len(catalog) != 1 || !strings.HasPrefix(image, httpServer.URL+filePath) || entry["width"] != float64(3) || entry["height"] != float64(2) || entry["alt"] != "A party" {
		t.Fatalf("catalog: %#v", catalog)
	}
	if status, _, _ := httpDo(t, http.MethodGet, image, nil, ""); status != http.StatusOK {
		t.Fatalf("emoji image: %d", status)
	}
	if status, _, _ := httpDo(t, http.MethodGet, emptyURL, nil, ""); status != http.StatusNotFound {
		t.Fatalf("the previous catalog: %d", status)
	}

	// Reactions take the catalog's keys, and no other.
	id, _ := save(t, a, "post", map[string]any{"body": map[string]any{"text": "party?"}})
	b.notification(t, "message")
	admin.notification(t, "message")
	react(t, b, "party", id, ":party:", "👍")
	a.notification(t, "reactions")
	admin.notification(t, "reactions")
	b.expectError(t, "reactions", "unknown", map[string]any{"message_id": id, "emojis": []any{":nope:"}}, codeInvalidParams)

	// Only who added a key, or an admin, replaces its image; only an admin
	// removes it.
	b.expectError(t, "command", "steal", map[string]any{"body": map[string]any{"text": "/emoji add :party:", "embeds": []any{map[string]any{"kind": "upload"}}}}, codeDenied)
	b.expectError(t, "command", "remove", map[string]any{"body": map[string]any{"text": "/emoji remove :party:"}}, codeDenied)
	if status := addEmoji(t, a, ":party:", testPNG(t), "image/png"); status != http.StatusCreated {
		t.Fatalf("emoji replacement: %d", status)
	}
	for _, c := range everyone {
		_, catalog = catalogOf(t, c.notification(t, "server"))
	}
	if replaced := catalog[":party:"].(map[string]any)["url"]; replaced == image {
		t.Fatalf("the replacement kept the image: %#v", catalog)
	}
	if status, _, _ := httpDo(t, http.MethodGet, image, nil, ""); status != http.StatusNotFound {
		t.Fatalf("the replaced image: %d", status)
	}
	image = catalog[":party:"].(map[string]any)["url"].(string)

	// Emoji are images, as avatars are; a refused write changes nothing.
	if status := addEmoji(t, b, ":text:", []byte("plain text"), "text/plain"); status != http.StatusUnsupportedMediaType {
		t.Fatalf("a text emoji: %d", status)
	}
	for i, text := range []string{"/emoji", "/emoji add party", "/emoji add :a b:", "/emoji drop :party:", "/emoji add :" + strings.Repeat("x", maxEmojiBytes) + ":"} {
		b.expectError(t, "command", fmt.Sprint("shape-", i), map[string]any{"body": map[string]any{"text": text, "embeds": []any{map[string]any{"kind": "upload"}}}}, codeInvalidParams)
	}
	b.expectError(t, "command", "no-image", map[string]any{"body": map[string]any{"text": "/emoji add :new:"}}, codeInvalidParams)
	for _, c := range everyone {
		c.expectQuiet(t)
	}

	// An admin's removal sends the server frame before the result, and
	// deletes the image.
	before, _ := admin.request(t, "command", "remove", map[string]any{"body": map[string]any{"text": "/emoji remove :party:"}})
	if len(before) != 1 {
		t.Fatalf("frames before the removal's result: %#v", before)
	}
	if _, catalog = catalogOf(t, notificationParams(t, before[0], "server")); len(catalog) != 0 {
		t.Fatalf("catalog after removal: %#v", catalog)
	}
	for _, c := range []*testClient{watcher, a, b} {
		c.notification(t, "server")
	}
	if status, _, _ := httpDo(t, http.MethodGet, image, nil, ""); status != http.StatusNotFound {
		t.Fatalf("the removed image: %d", status)
	}
	admin.expectError(t, "command", "again", map[string]any{"body": map[string]any{"text": "/emoji remove :party:"}}, codeInvalidParams)

	// A set may keep a removed key, but not take it up.
	a.expectError(t, "reactions", "late", map[string]any{"message_id": id, "emojis": []any{":party:"}}, codeInvalidParams)
	kept := react(t, b, "kept", id, ":party:")
	if emojis := kept["reactions"].([]any)[0].(map[string]any)["emojis"]; !reflect.DeepEqual(emojis, []any{":party:"}) {
		t.Fatalf("kept set: %#v", kept)
	}
}

// The catalog and its images survive a restart, at the same URLs.
func TestCustomEmojiSurvivesRestart(t *testing.T) {
	kept, uploads := store.NewMemory(), t.TempDir()
	_, httpServer, stop := startWithStore(t, kept, uploads)
	c := dialTestClient(t, httpServer)
	if status := addEmoji(t, c, ":party:", testPNG(t), "image/png"); status != http.StatusCreated {
		t.Fatalf("emoji upload: %d", status)
	}
	url, catalog := catalogOf(t, c.notification(t, "server"))
	stop()

	app, httpServer, _ := startWithStore(t, kept, uploads)
	_, frame := dialRaw(t, httpServer)
	restartedURL, restored := catalogOf(t, frame["params"].(map[string]any))
	// Restored emoji images stay out of the uploads that
	// MaxUploadStorageBytes bounds.
	app.mu.RLock()
	uploadCount, uploadBytes := app.uploads.Len(), app.uploadBytes
	app.mu.RUnlock()
	if uploadCount != 0 || uploadBytes != 0 {
		t.Fatalf("restored uploads: %d, %d bytes", uploadCount, uploadBytes)
	}
	// The restarted test server listens on another port.
	if path.Base(restartedURL) != path.Base(url) || !reflect.DeepEqual(restored, catalog) {
		t.Fatalf("catalog %s %#v after a restart, want %s %#v", restartedURL, restored, url, catalog)
	}
	image := restored[":party:"].(map[string]any)["url"].(string)
	if status, _, _ := httpDo(t, http.MethodGet, httpServer.URL+image[strings.Index(image, filePath):], nil, ""); status != http.StatusOK {
		t.Fatalf("restored image: %d", status)
	}
}

// Custom emoji images have a size cap and a storage quota of their own
// (MaxEmojiBytes, MaxEmojiStorageBytes): they do not count toward
// MaxUploadStorageBytes, so they never push message uploads out, and an
// image past the quota is refused rather than making room.
func TestCustomEmojiHaveTheirOwnLimits(t *testing.T) {
	image := testPNG(t)
	size := int64(len(image))
	dir := t.TempDir()
	config := DefaultConfig()
	config.UploadDir = dir
	config.MaxEmojiBytes = size
	config.MaxEmojiStorageBytes = 2 * size
	config.MaxUploadBytes = size
	config.MaxUploadStorageBytes = 2 * size
	_, httpServer := newTestServer(t, config)
	a := dialTestClient(t, httpServer)

	// One image is at most MaxEmojiBytes, though avatars may be larger.
	if status := addEmoji(t, a, ":big:", append(bytes.Clone(image), make([]byte, 1)...), "image/png"); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("an emoji over MaxEmojiBytes: %d", status)
	}
	for _, key := range []string{":a:", ":b:"} {
		if status := addEmoji(t, a, key, image, "image/png"); status != http.StatusCreated {
			t.Fatalf("emoji %s: %d", key, status)
		}
		a.notification(t, "server")
	}

	// The quota is full: a new key is denied, while a replacement fits in
	// the room its old image leaves.
	a.expectError(t, "command", "full", map[string]any{"body": map[string]any{"text": "/emoji add :c:", "embeds": []any{map[string]any{"kind": "upload"}}}}, codeDenied)
	if status := addEmoji(t, a, ":a:", image, "image/png"); status != http.StatusCreated {
		t.Fatalf("replacing an emoji in a full quota: %d", status)
	}
	_, catalog := catalogOf(t, a.notification(t, "server"))
	if len(catalog) != 2 || uploadFiles(t, dir) != 2 {
		t.Fatalf("catalog %#v with %d files", catalog, uploadFiles(t, dir))
	}

	// Message uploads use all of MaxUploadStorageBytes, so none is evicted.
	var files []string
	for i := range 2 {
		result := postEmbeds(t, a, fmt.Sprint("post-", i), map[string]any{"room_id": "general", "body": map[string]any{"embeds": []any{map[string]any{"kind": "upload"}}}})
		writeURL := result["embeds"].([]any)[0].(map[string]any)["write_url"].(string)
		if status, _, _ := httpDo(t, http.MethodPut, writeURL, bytes.NewReader(image), "image/png"); status != http.StatusCreated {
			t.Fatalf("message upload %d: %d", i, status)
		}
		files = append(files, embedsOf(t, a.notification(t, "message"))[0]["url"].(string))
	}
	a.expectQuiet(t)
	for _, file := range files {
		if status, _, _ := httpDo(t, http.MethodGet, file, nil, ""); status != http.StatusOK {
			t.Fatalf("message upload %s after emoji: %d", file, status)
		}
	}

	// Writes that each fit when their command ran are checked again when
	// they finish: the one that would pass the quota is refused, and its
	// file is removed.
	raceDir := t.TempDir()
	config.UploadDir = raceDir
	_, raceServer := newTestServer(t, config)
	c := dialTestClient(t, raceServer)
	if status := addEmoji(t, c, ":a:", image, "image/png"); status != http.StatusCreated {
		t.Fatalf("emoji :a:: %d", status)
	}
	c.notification(t, "server")
	first, second := emojiWriteURL(t, c, ":b:"), emojiWriteURL(t, c, ":c:")
	if status := putEmoji(t, first, image, "image/png"); status != http.StatusCreated {
		t.Fatalf("first pending emoji: %d", status)
	}
	c.notification(t, "server")
	if status := putEmoji(t, second, image, "image/png"); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("second pending emoji past the quota: %d", status)
	}
	c.expectQuiet(t)
	if files := uploadFiles(t, raceDir); files != 2 {
		t.Fatalf("upload files after the refused write: %d", files)
	}
}

// A write for a key that someone else took meanwhile, who the writer may not
// replace, is dropped when it finishes rather than replacing their image: the
// ownership check of /emoji add holds at the write as well.
func TestCustomEmojiWriteRechecksOwnership(t *testing.T) {
	dir := t.TempDir()
	_, mailbox, httpServer := emailTestServer(t, func(config *Config) {
		config.UploadDir = dir
		config.Roles = map[string][]string{"admin": {"ada@example.com"}}
	})
	b := dialTestClient(t, httpServer)
	admin, _ := dialRaw(t, httpServer)
	propose(t, admin, "ada@example.com")
	signInByEmail(t, admin, mailbox.receive(t).Code, nil)
	admin.drain(t)
	b.drain(t)

	// b asks for :x: while it is free, and keeps the write URL.
	stale := emojiWriteURL(t, b, ":x:")
	if status := addEmoji(t, admin, ":x:", testPNG(t), "image/png"); status != http.StatusCreated {
		t.Fatalf("admin's emoji: %d", status)
	}
	_, catalog := catalogOf(t, admin.notification(t, "server"))
	b.notification(t, "server")
	image := catalog[":x:"].(map[string]any)["url"].(string)

	if status := putEmoji(t, stale, testPNG(t), "image/png"); status != http.StatusConflict {
		t.Fatalf("b's stale write: %d", status)
	}
	admin.expectQuiet(t)
	b.expectQuiet(t)
	if status, _, _ := httpDo(t, http.MethodGet, image, nil, ""); status != http.StatusOK {
		t.Fatalf("admin's image after b's write: %d", status)
	}
	if files := uploadFiles(t, dir); files != 1 {
		t.Fatalf("upload files after the dropped write: %d", files)
	}
	// :x: is still the admin's: b may not replace it.
	b.expectError(t, "command", "replace", map[string]any{"body": map[string]any{"text": "/emoji add :x:", "embeds": []any{map[string]any{"kind": "upload"}}}}, codeDenied)

	// An admin's stale write still replaces another user's key.
	adminWrite := emojiWriteURL(t, admin, ":y:")
	if status := addEmoji(t, b, ":y:", testPNG(t), "image/png"); status != http.StatusCreated {
		t.Fatalf("b's emoji: %d", status)
	}
	admin.notification(t, "server")
	b.notification(t, "server")
	if status := putEmoji(t, adminWrite, testPNG(t), "image/png"); status != http.StatusCreated {
		t.Fatalf("admin's write over b's key: %d", status)
	}
	admin.notification(t, "server")
	b.notification(t, "server")
}

// dialHost connects to httpServer as if through a reverse proxy for host,
// over plain HTTP, and returns the server frame's params.
func dialHost(t *testing.T, httpServer *httptest.Server, host string) (*testClient, map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	ws, _, err := websocket.Dial(ctx, wsURL(httpServer), &websocket.DialOptions{Host: host})
	cancel()
	if err != nil {
		t.Fatalf("dial as %s: %v", host, err)
	}
	c := &testClient{ws: ws}
	t.Cleanup(func() { _ = ws.Close(websocket.StatusNormalClosure, "test finished") })
	return c, notificationParams(t, c.read(t), "server")
}

// Clients ignore a catalog_url that is http: on another host than a
// loopback one (ext:emoji), so a connection whose URLs would be such, as
// behind a plain-HTTP reverse proxy without PublicURL, is not offered
// ext:emoji, nor sent server frames when the catalog changes.
func TestCustomEmojiNeedAUsableCatalogURL(t *testing.T) {
	offered := func(params map[string]any) bool {
		ext, _ := params["ext"].(map[string]any)
		_, has := ext["emoji"]
		if slices.Contains(params["capabilities"].([]any), "ext:emoji") != has {
			t.Fatalf("ext:emoji and ext.emoji disagree: %#v", params)
		}
		return has
	}
	_, httpServer := newTestServer(t, DefaultConfig())
	proxied, params := dialHost(t, httpServer, "chat.example")
	if offered(params) {
		t.Fatalf("ext:emoji offered over plain HTTP to chat.example: %#v", params)
	}
	if _, has := params["ext"]; has {
		t.Fatalf("an empty ext: %#v", params)
	}
	for _, host := range []string{"localhost:8080", "app.localhost", "127.0.0.2:8080", "[::1]:8080"} {
		if _, params := dialHost(t, httpServer, host); !offered(params) {
			t.Fatalf("ext:emoji not offered to loopback %s: %#v", host, params)
		}
	}
	c := dialTestClient(t, httpServer)
	if status := addEmoji(t, c, ":party:", testPNG(t), "image/png"); status != http.StatusCreated {
		t.Fatalf("emoji upload: %d", status)
	}
	c.notification(t, "server")
	proxied.expectQuiet(t)

	// With an https: PublicURL, every connection is offered it.
	config := DefaultConfig()
	config.PublicURL = "https://chat.example"
	_, publicServer := newTestServer(t, config)
	_, params = dialHost(t, publicServer, "chat.example")
	if !offered(params) || !strings.HasPrefix(params["ext"].(map[string]any)["emoji"].(map[string]any)["catalog_url"].(string), "https://chat.example"+emojiPath) {
		t.Fatalf("ext:emoji with an https: PublicURL: %#v", params)
	}
}
