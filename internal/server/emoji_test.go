package server

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"path"
	"reflect"
	"strings"
	"testing"

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
	embed := map[string]any{"kind": "upload", "og": map[string]any{"image": map[string]any{"alt": "A party"}}}
	result := c.result(t, "command", c.nextID("emoji"), map[string]any{"body": map[string]any{"text": "/emoji add " + key, "embeds": []any{embed}}})
	writeURL := result["embeds"].([]any)[0].(map[string]any)["write_url"].(string)
	status, _, _ := httpDo(t, http.MethodPut, writeURL, bytes.NewReader(content), contentType)
	return status
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

	_, httpServer, _ = startWithStore(t, kept, uploads)
	_, frame := dialRaw(t, httpServer)
	restartedURL, restored := catalogOf(t, frame["params"].(map[string]any))
	// The restarted test server listens on another port.
	if path.Base(restartedURL) != path.Base(url) || !reflect.DeepEqual(restored, catalog) {
		t.Fatalf("catalog %s %#v after a restart, want %s %#v", restartedURL, restored, url, catalog)
	}
	image := restored[":party:"].(map[string]any)["url"].(string)
	if status, _, _ := httpDo(t, http.MethodGet, httpServer.URL+image[strings.Index(image, filePath):], nil, ""); status != http.StatusOK {
		t.Fatalf("restored image: %d", status)
	}
}
