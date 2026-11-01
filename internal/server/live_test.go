package server

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/unicode"

	"github.com/apron-chat/apron-server-go/internal/store"
)

// keepLastReference is §4.8.6's keep_last, transcribed from its Python.
func keepLastReference(output []byte, limit int) []byte {
	if len(output) <= limit {
		return output
	}
	tail := output[len(output)-limit:]
	for i, b := range tail {
		if b < 0x20 {
			return tail[i:]
		}
	}
	// tail.lstrip(bytes(range(0x80, 0xC0)))
	i := 0
	for i < len(tail) && tail[i] >= 0x80 && tail[i] < 0xC0 {
		i++
	}
	return tail[i:]
}

func TestKeepLastCutsAtAControlCharacterElseAWholeCharacter(t *testing.T) {
	for _, tc := range []struct {
		output string
		limit  int
		want   string
	}{
		{"short", 16, "short"},
		{"0123456789", 10, "0123456789"},
		{"line one\nline two", 12, "\nline two"},
		{"a\x1b[31mred\x1b[0m", 9, "\x1b[0m"},
		{"\r\n" + strings.Repeat("y", 12), 8, "yyyyyyyy"},
		{"ééé", 5, "éé"},
		{"ééé", 4, "éé"},
		{"ééé", 3, "é"},
		{"日本語", 7, "本語"},
		{"日本語", 8, "本語"},
		{"x日本語", 7, "本語"},
		{strings.Repeat("\x80", 6), 4, ""},
	} {
		got := keepLast([]byte(tc.output), tc.limit)
		if string(got) != tc.want {
			t.Errorf("keepLast(%q, %d) = %q, want %q", tc.output, tc.limit, got, tc.want)
		}
		if reference := keepLastReference([]byte(tc.output), tc.limit); !bytes.Equal(got, reference) {
			t.Errorf("keepLast(%q, %d) = %q, the reference %q", tc.output, tc.limit, got, reference)
		}
	}
}

func TestWholeRunesDropsAPartialCharacter(t *testing.T) {
	for in, want := range map[string]string{
		"":                   "",
		"abc":                "abc",
		"abé":                "abé",
		"ab\xc3":             "ab",
		"ab\xe6\x97":         "ab",
		"ab日":                "ab日",
		"ab\xf0\x9f\x98":     "ab",
		"ab\xf0\x9f\x98\x80": "ab\xf0\x9f\x98\x80",
	} {
		if got := string(wholeRunes([]byte(in))); got != want {
			t.Errorf("wholeRunes(%q) = %q, want %q", in, got, want)
		}
	}
}

// The live buffer serves what a terminal keeps from its start, both to a
// new reader and as the finished output.
func TestTerminalBufferServesKeepLast(t *testing.T) {
	b := newLiveText(true, 8, 1<<20)
	var all []byte
	for _, chunk := range []string{"build ok\r\n", "\x1b[32m", "passed", " 12 tests\n", "ééé", "x"} {
		b.write([]byte(chunk))
		all = append(all, chunk...)
		want := keepLast(all, 8)
		got, _, _, _, _ := b.read(-1)
		if !bytes.Equal(got, want) {
			t.Fatalf("after %q a new reader got %q, want %q", chunk, got, want)
		}
		if text := b.text(); text != strings.ToValidUTF8(string(want), "\uFFFD") {
			t.Fatalf("after %q the output is %q, want %q", chunk, text, want)
		}
	}
}

// A reader goes on from where it is while the terminal still holds the
// bytes after it, and is told when it fell behind them.
func TestTerminalBufferTellsAReaderThatFellBehind(t *testing.T) {
	b := newLiveText(true, 8, 1<<20)
	b.write([]byte("12345678"))
	_, next, _, _, _ := b.read(-1)
	b.write([]byte("abcdefgh"))
	if got, _, _, _, behind := b.read(next); string(got) != "abcdefgh" || behind {
		t.Fatalf("a reader still within the text got %q, behind %v", got, behind)
	}
	b.write([]byte("ij"))
	if got, _, _, _, behind := b.read(next); got != nil || !behind {
		t.Fatalf("a reader past what the terminal holds got %q, behind %v", got, behind)
	}
}

func TestAppendBufferKeepsTheFirstPartUpToAWholeCharacter(t *testing.T) {
	b := newLiveText(false, 5, 1<<20)
	if !b.write([]byte("abc")) {
		t.Fatal("ended early")
	}
	if b.write([]byte("dé")) || !b.limited() {
		t.Fatal("did not end at the limit")
	}
	if text := b.text(); text != "abcd" {
		t.Fatalf("kept %q", text)
	}
	got, _, _, ended, _ := b.read(-1)
	if string(got) != "abcd" || !ended {
		t.Fatalf("reader got %q, ended %v", got, ended)
	}
}

// editAppend is §4.8.5's edits, for reference: the whole text decoded,
// then each backspace removes the code point before it and each form feed
// all the text before it.
func editAppend(raw []byte) string {
	decoded, _ := unicode.UTF8.NewDecoder().Bytes(raw)
	var kept []rune
	for _, r := range string(decoded) {
		switch r {
		case '\b':
			kept = kept[:max(0, len(kept)-1)]
		case '\f':
			kept = kept[:0]
		default:
			kept = append(kept, r)
		}
	}
	return string(kept)
}

// An append applies backspaces and form feeds as the text arrives, after
// decoding, however the writer splits it (§4.8.5).
func TestAppendBufferAppliesEdits(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"hel\b\bello", "hello"},
		{"caf\u00e9\be", "cafe"},
		{"\U0001F600\b!", "!"},
		{"draft\fHello", "Hello"},
		{"\b\bstart", "start"},
		{"one\ftwo\fthree", "three"},
		{"ab\xff\bc", "abc"},
		{"ab\xE2\x82\bc", "abc"},
		{"end \xE2\x82", "end \uFFFD"},
		{"no edits\r\n\x1b[0m", "no edits\r\n\x1b[0m"},
	} {
		if want := editAppend([]byte(c.in)); want != c.want {
			t.Fatalf("%q: the reference edits it to %q, not %q", c.in, want, c.want)
		}
		for split := 0; split <= len(c.in); split++ {
			b := newLiveText(false, 1<<10, 1<<20)
			b.write([]byte(c.in[:split]))
			b.write([]byte(c.in[split:]))
			b.end()
			if got := b.text(); got != c.want {
				t.Errorf("%q split at %d: %q, want %q", c.in, split, got, c.want)
			}
			if got, _, _, _, _ := b.read(-1); string(got) != c.want {
				t.Errorf("%q split at %d: a new reader got %q, want %q", c.in, split, got, c.want)
			}
			// A reader from the start gets the decoded text with its edits.
			got, _, _, _, _ := b.read(0)
			if decoded, _ := unicode.UTF8.NewDecoder().Bytes([]byte(c.in)); string(got) != string(decoded) {
				t.Errorf("%q split at %d: a reader from the start got %q", c.in, split, got)
			}
		}
	}
}

// An append decodes invalid UTF-8 as a browser's TextDecoder('utf-8') does
// (WHATWG), independently of x/text: these outputs are TextDecoder's.
func TestAppendDecodesInvalidUTF8AsBrowsersDo(t *testing.T) {
	for _, c := range []struct{ in, decoded, want string }{
		{"a\xE2\x82A\b", "a\uFFFDA\b", "a\uFFFD"},
		{"a\xF0\x80\x80\b", "a\uFFFD\uFFFD\uFFFD\b", "a\uFFFD\uFFFD"},
		{"a\xED\xA0\x80\b", "a\uFFFD\uFFFD\uFFFD\b", "a\uFFFD\uFFFD"},
		{"a\xC0\xAF\b", "a\uFFFD\uFFFD\b", "a\uFFFD"},
		{"a\xFF\b", "a\uFFFD\b", "a"},
		{"a\x80\x80\b", "a\uFFFD\uFFFD\b", "a\uFFFD"},
		{"a\xF0\x9F\x9A\bb", "a\uFFFD\bb", "ab"},
		{"a\xF0\x9F\x9A", "a\uFFFD", "a\uFFFD"},
	} {
		if got := editAppend([]byte(c.in)); got != c.want {
			t.Errorf("%q: the reference edits it to %q, want %q", c.in, got, c.want)
		}
		for split := 0; split <= len(c.in); split++ {
			b := newLiveText(false, 1<<10, 1<<20)
			b.write([]byte(c.in[:split]))
			b.write([]byte(c.in[split:]))
			b.end()
			if got := b.text(); got != c.want {
				t.Errorf("%q split at %d: %q, want %q", c.in, split, got, c.want)
			}
			if got, _, _, _, _ := b.read(0); string(got) != c.decoded {
				t.Errorf("%q split at %d: a reader from the start got %q, want %q", c.in, split, got, c.decoded)
			}
		}
	}
}

// An append whose kept text is full ends cleanly when its writer ends
// partway through a character: the U+FFFD for it does not fit, and the
// writer stayed within the limit.
func TestAppendFullWithAnIncompleteCharacterEndsCleanly(t *testing.T) {
	b := newLiveText(false, 5, 1<<20)
	if !b.write([]byte("vwxyz\xE2\x82")) {
		t.Fatal("ended early")
	}
	b.end()
	if b.limited() || b.text() != "vwxyz" {
		t.Fatalf("kept %q, limited %v", b.text(), b.limited())
	}

	config := DefaultConfig()
	config.AppendMaxBytes = 5
	_, httpServer := newTestServer(t, config)
	a := dialTestClient(t, httpServer)
	_, writeURL, _ := postLive(t, a, "full", map[string]any{"text": "> ", "embeds": []any{map[string]any{"kind": "append"}}})
	if status, _, got := httpDo(t, http.MethodPut, writeURL, strings.NewReader("vwxyz\xE2\x82"), ""); status != http.StatusNoContent {
		t.Fatalf("append write: %d %q", status, got)
	}
	startedSnapshot(t, a)
	if got := a.notification(t, "message")["body"].(map[string]any)["text"]; got != "> vwxyz" {
		t.Fatalf("finished text %q", got)
	}
}

// A new reader of an append gets its edited text so far, then what follows;
// one already reading gets the edits to apply. The start of a character
// that has not all arrived waits for the rest.
func TestAppendBufferServesEditedTextToANewReader(t *testing.T) {
	b := newLiveText(false, 1<<10, 1<<20)
	b.write([]byte("secret draft\fcaf\xc3"))
	early, next, _, _, _ := b.read(-1)
	if string(early) != "caf" {
		t.Fatalf("a new reader got %q", early)
	}
	b.write([]byte("\xa9\b\be!"))
	late, _, _, _, _ := b.read(-1)
	more, _, _, _, _ := b.read(next)
	if string(late) != "cae!" || string(more) != "\u00e9\b\be!" {
		t.Fatalf("a new reader got %q, the reader before it %q", late, more)
	}
}

// An append's size limit counts its edited text, while its writer may send
// up to maxBytes; readers that fall behind the edits it holds fail.
func TestAppendBufferLimitsItsEditedText(t *testing.T) {
	b := newLiveText(false, 4, 1<<20)
	_, start, _, _, _ := b.read(-1)
	for range 4 << 10 {
		if !b.write([]byte("abcd\f")) {
			t.Fatal("ended while the edited text fit")
		}
	}
	if b.text() != "" || b.total != 20<<10 {
		t.Fatalf("kept %q of %d bytes", b.text(), b.total)
	}
	if _, _, _, _, behind := b.read(start); !behind {
		t.Fatal("a reader from the start is not behind")
	}
	if b.write([]byte("wxyz!")) || !b.limited() || b.limit() != 4 || b.text() != "wxyz" {
		t.Fatalf("kept %q, limited %v to %d", b.text(), b.limited(), b.limit())
	}

	b = newLiveText(false, 4, 8)
	if b.write([]byte("ab\b\b\bcd\xc3\xa9")) || !b.limited() || b.limit() != 8 || b.text() != "cd" {
		t.Fatalf("kept %q, limited %v to %d", b.text(), b.limited(), b.limit())
	}
}

// postLive posts live text embeds and returns the result, the first's write
// URL, and its embed in the pending snapshot.
func postLive(t *testing.T, c *testClient, id string, body map[string]any) (map[string]any, string, map[string]any) {
	t.Helper()
	result := postEmbeds(t, c, id, map[string]any{"room_id": "general", "body": body})
	writeURL := result["embeds"].([]any)[0].(map[string]any)["write_url"].(string)
	var live map[string]any
	for _, embed := range embedsOf(t, result["snapshot"].(map[string]any)) {
		if live == nil && liveKind(embed["kind"]) {
			live = embed
		}
	}
	return result, writeURL, live
}

// liveURLOf is live text's url, which the server publishes only once its
// write starts (§4.8.5): a test that reads before then asks the server.
func liveURLOf(t *testing.T, app *Server, embedID any) string {
	t.Helper()
	app.mu.RLock()
	defer app.mu.RUnlock()
	e := app.embeds[embedID.(string)]
	if e == nil || !liveKind(e.kind) {
		t.Fatalf("no live text %v", embedID)
	}
	return e.liveURL()
}

// startedSnapshot reads the snapshot that adds live text's url when its
// write starts (§4.8.5).
func startedSnapshot(t *testing.T, c *testClient) map[string]any {
	t.Helper()
	snapshot := c.notification(t, "message")
	for _, embed := range embedsOf(t, snapshot) {
		if liveKind(embed["kind"]) && embed["url"] != nil {
			return snapshot
		}
	}
	t.Fatalf("not the snapshot that starts live text: %#v", snapshot)
	return nil
}

func TestAppendGrowsTheBodyThenMovesIntoIt(t *testing.T) {
	app, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	result, writeURL, live := postLive(t, a, "append", map[string]any{
		"text": "Here is the plan:\n\n", "format": "markdown", "mentions": []any{"guest_2"},
		"embeds": []any{map[string]any{"kind": "append", "url": "https://forged.example/a"}},
	})
	pending := b.notification(t, "message")
	// Until its write starts the embed has no url, the forged one dropped.
	if _, has := live["url"]; has || !reflect.DeepEqual(embedsOf(t, pending)[0], live) {
		t.Fatalf("live embed: %#v, broadcast %#v", live, pending)
	}
	liveURL := liveURLOf(t, app, live["embed_id"])
	reader, err := http.Get(liveURL)
	if err != nil || reader.StatusCode != http.StatusOK {
		t.Fatalf("live reader: %v %v", reader, err)
	}
	defer reader.Body.Close()
	if reader.Header.Get("Content-Type") != "text/plain; charset=utf-8" || reader.Header.Get("X-Content-Type-Options") != "nosniff" || reader.Header.Get("Cache-Control") != "no-store, no-transform" {
		t.Fatalf("live text headers: %#v", reader.Header)
	}
	text := "1. Roll back.\n2. Renew the cert, @guest_1.\n"
	if status, _, _ := httpDo(t, http.MethodPut, writeURL, strings.NewReader(text), "text/plain"); status != http.StatusNoContent {
		t.Fatalf("append write: %d", status)
	}
	if got, _ := io.ReadAll(reader.Body); string(got) != text {
		t.Fatalf("reader got %q", got)
	}
	// The write's start publishes the url. The finished snapshot has the
	// text in the body, and no embed. The appended text does not change
	// mentions.
	for _, client := range clients {
		started := startedSnapshot(t, client)
		if embedsOf(t, started)[0]["url"] != liveURL || started["prev_log_id"] != pending["log_id"] {
			t.Fatalf("started snapshot: %#v", started)
		}
		finished := client.notification(t, "message")
		want := map[string]any{"text": "Here is the plan:\n\n" + text, "format": "markdown", "mentions": []any{"guest_2"}}
		if !reflect.DeepEqual(finished["body"], any(want)) || finished["message_id"] != result["message_id"] || finished["prev_log_id"] != started["log_id"] {
			t.Fatalf("finished snapshot: %#v", finished)
		}
	}
	if status, _, _ := httpDo(t, http.MethodGet, liveURL, nil, ""); status != http.StatusNotFound {
		t.Fatalf("finished append URL: %d", status)
	}
	page := historyPage(t, a, "general", map[string]any{})
	messages := records(t, page, "messages")
	if last := messages[len(messages)-1].(map[string]any); last["body"].(map[string]any)["text"] != "Here is the plan:\n\n"+text {
		t.Fatalf("history: %#v", last)
	}
}

// At its size limit an append keeps its first part, up to a whole
// character, and the writer learns that it ended (§4.8.5).
func TestAppendKeepsItsFirstPart(t *testing.T) {
	config := DefaultConfig()
	config.AppendMaxBytes = 7
	_, httpServer := newTestServer(t, config)
	a := dialTestClient(t, httpServer)
	_, writeURL, _ := postLive(t, a, "append", map[string]any{"text": "> ", "embeds": []any{map[string]any{"kind": "append"}}})
	if status, _, got := httpDo(t, http.MethodPut, writeURL, strings.NewReader("abcdefé and more"), ""); status != http.StatusRequestEntityTooLarge || !strings.Contains(string(got), "7 bytes") {
		t.Fatalf("oversized append: %d %q", status, got)
	}
	startedSnapshot(t, a)
	if text := a.notification(t, "message")["body"].(map[string]any)["text"]; text != "> abcdef" {
		t.Fatalf("limited text %q", text)
	}
	// Invalid UTF-8 becomes replacement characters, within the limit.
	_, writeURL, _ = postLive(t, a, "invalid", map[string]any{"embeds": []any{map[string]any{"kind": "append"}}})
	if status, _, _ := httpDo(t, http.MethodPut, writeURL, strings.NewReader("ab\xffcdef"), ""); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("invalid append: %d", status)
	}
	startedSnapshot(t, a)
	if text := a.notification(t, "message")["body"].(map[string]any)["text"]; text != "ab\uFFFDcd" {
		t.Fatalf("invalid text kept as %q", text)
	}
}

// An append keeps no more than the server reads in a frame, with its
// message's opening text.
func TestAppendFitsInARequest(t *testing.T) {
	config := DefaultConfig()
	config.ReadLimit = 4 << 10
	config.AppendMaxBytes = 64 << 10
	_, httpServer := newTestServer(t, config)
	a := dialTestClient(t, httpServer)
	opening := strings.Repeat("o", 1<<10)
	_, writeURL, _ := postLive(t, a, "append", map[string]any{"text": opening, "embeds": []any{map[string]any{"kind": "append"}}})
	if status, _, _ := httpDo(t, http.MethodPut, writeURL, strings.NewReader(strings.Repeat("x", 4<<10)), ""); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized append: %d", status)
	}
	startedSnapshot(t, a)
	if text := a.notification(t, "message")["body"].(map[string]any)["text"].(string); len(text) != 4<<10 {
		t.Fatalf("text of %d bytes", len(text))
	}
}

// An append that a save lengthened the body under still fits in a request:
// it keeps its first part, up to a whole character (§4.8.5).
func TestAppendFitsInARequestAfterASave(t *testing.T) {
	config := DefaultConfig()
	config.ReadLimit = 4 << 10
	config.AppendMaxBytes = 64 << 10
	app, httpServer := newTestServer(t, config)
	a := dialTestClient(t, httpServer)
	result, writeURL, live := postLive(t, a, "append", map[string]any{"text": "> ", "embeds": []any{map[string]any{"kind": "append"}}})
	pipe, writerDone := startLiveWrite(t, app, writeURL, strings.Repeat("é", 1<<10))
	startedSnapshot(t, a)
	edited := strings.Repeat("e", 3<<10+1)
	save(t, a, "edit", map[string]any{"message_id": result["message_id"], "body": map[string]any{"text": edited, "embeds": []any{live}}})
	_ = pipe.Close()
	if status := <-writerDone; status != http.StatusNoContent {
		t.Fatalf("writer status %d", status)
	}
	if text := a.notification(t, "message")["body"].(map[string]any)["text"].(string); text != edited+strings.Repeat("é", 511) {
		t.Fatalf("text of %d bytes, want %d", len(text), len(edited)+1022)
	}
}

// Backspace and form feed revise an append's text before it moves into
// body.text, never reaching the opening text (§4.8.5). A reader that is
// reading gets the edits to apply.
func TestAppendEditsReviseTheText(t *testing.T) {
	app, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	_, writeURL, live := postLive(t, a, "append", map[string]any{
		"text": "Here is the plan:\n\n", "format": "markdown", "embeds": []any{map[string]any{"kind": "append"}},
	})
	b.notification(t, "message")
	reader, err := http.Get(liveURLOf(t, app, live["embed_id"]))
	if err != nil || reader.StatusCode != http.StatusOK {
		t.Fatalf("live reader: %v %v", reader, err)
	}
	defer reader.Body.Close()
	text := "draft\fHello wrold\b\b\b\borld, caf\u00e9\be"
	if status, _, _ := httpDo(t, http.MethodPut, writeURL, strings.NewReader(text), ""); status != http.StatusNoContent {
		t.Fatalf("append write: %d", status)
	}
	if got, _ := io.ReadAll(reader.Body); string(got) != text {
		t.Fatalf("reader got %q", got)
	}
	for _, client := range clients {
		startedSnapshot(t, client)
		if got := client.notification(t, "message")["body"].(map[string]any)["text"]; got != "Here is the plan:\n\nHello world, cafe" {
			t.Fatalf("finished text %q", got)
		}
	}

	_, writeURL, _ = postLive(t, a, "opening", map[string]any{"text": "> ", "embeds": []any{map[string]any{"kind": "append"}}})
	b.notification(t, "message")
	if status, _, _ := httpDo(t, http.MethodPut, writeURL, strings.NewReader("\b\b\bok"), ""); status != http.StatusNoContent {
		t.Fatalf("append write: %d", status)
	}
	startedSnapshot(t, a)
	if got := a.notification(t, "message")["body"].(map[string]any)["text"]; got != "> ok" {
		t.Fatalf("finished text %q", got)
	}
}

// waitForLiveText waits until live text's kept text is want.
func waitForLiveText(t *testing.T, app *Server, embedID, want string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		app.mu.RLock()
		e := app.embeds[embedID]
		arrived := e != nil && e.live != nil && e.live.text() == want
		app.mu.RUnlock()
		if arrived {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the text never became %q", want)
		}
	}
}

// A reader that connects to an append later gets its edited text so far,
// not the drafts it erased, then what follows (§4.8.5).
func TestALateAppendReaderGetsTheEditedText(t *testing.T) {
	app, httpServer := newTestServer(t, DefaultConfig())
	a := dialTestClient(t, httpServer)
	_, writeURL, live := postLive(t, a, "append", map[string]any{"text": "> ", "embeds": []any{map[string]any{"kind": "append"}}})
	pipe, writerDone := startLiveWrite(t, app, writeURL, "secret draft\fThe pk\bl")
	waitForLiveText(t, app, live["embed_id"].(string), "The pl")
	reader, err := http.Get(embedsOf(t, startedSnapshot(t, a))[0]["url"].(string))
	if err != nil || reader.StatusCode != http.StatusOK {
		t.Fatalf("live reader: %v %v", reader, err)
	}
	defer reader.Body.Close()
	first := make([]byte, len("The pl"))
	if _, err := io.ReadFull(reader.Body, first); err != nil || string(first) != "The pl" {
		t.Fatalf("a late reader got %q, %v", first, err)
	}
	_, _ = pipe.Write([]byte("an\b\bans.\n"))
	_ = pipe.Close()
	if rest, _ := io.ReadAll(reader.Body); string(rest) != "an\b\bans.\n" {
		t.Fatalf("then %q", rest)
	}
	if status := <-writerDone; status != http.StatusNoContent {
		t.Fatalf("writer status %d", status)
	}
	if got := a.notification(t, "message")["body"].(map[string]any)["text"]; got != "> The plans.\n" {
		t.Fatalf("finished text %q", got)
	}
}

// An append's size limit counts its text after the edits (§4.8.5).
func TestAppendLimitCountsTheEditedText(t *testing.T) {
	config := DefaultConfig()
	config.AppendMaxBytes = 5
	_, httpServer := newTestServer(t, config)
	a := dialTestClient(t, httpServer)
	_, writeURL, _ := postLive(t, a, "fits", map[string]any{"text": "> ", "embeds": []any{map[string]any{"kind": "append"}}})
	if status, _, _ := httpDo(t, http.MethodPut, writeURL, strings.NewReader("abcd\b\b\b\bvwxyz"), ""); status != http.StatusNoContent {
		t.Fatalf("append write: %d", status)
	}
	startedSnapshot(t, a)
	if got := a.notification(t, "message")["body"].(map[string]any)["text"]; got != "> vwxyz" {
		t.Fatalf("finished text %q", got)
	}
	_, writeURL, _ = postLive(t, a, "full", map[string]any{"text": "> ", "embeds": []any{map[string]any{"kind": "append"}}})
	if status, _, got := httpDo(t, http.MethodPut, writeURL, strings.NewReader("vwxyz\b!?"), ""); status != http.StatusRequestEntityTooLarge || !strings.Contains(string(got), "5 bytes") {
		t.Fatalf("full append: %d %q", status, got)
	}
	startedSnapshot(t, a)
	if got := a.notification(t, "message")["body"].(map[string]any)["text"]; got != "> vwxy!" {
		t.Fatalf("finished text %q", got)
	}
}

// A terminal keeps backspace and form feed as terminal control functions
// (§4.8.6): only an append's text is edited.
func TestTerminalKeepsBackspaceAndFormFeed(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	a := dialTestClient(t, httpServer)
	_, writeURL, _ := postLive(t, a, "terminal", map[string]any{"embeds": []any{map[string]any{"kind": "terminal"}}})
	output := "ab\bc\fd\r\n"
	if status, _, _ := httpDo(t, http.MethodPut, writeURL, strings.NewReader(output), ""); status != http.StatusNoContent {
		t.Fatalf("terminal write: %d", status)
	}
	liveURL := embedsOf(t, startedSnapshot(t, a))[0]["url"].(string)
	if got := embedsOf(t, a.notification(t, "message"))[0]["output"]; got != output {
		t.Fatalf("output %q", got)
	}
	if status, _, got := httpDo(t, http.MethodGet, liveURL, nil, ""); status != http.StatusOK || string(got) != output {
		t.Fatalf("finished terminal URL: %d %q", status, got)
	}
}

func TestAMessageHasAtMostOneAppend(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	a := dialTestClient(t, httpServer)
	a.expectError(t, "message", "two", map[string]any{"room_id": "general", "body": map[string]any{
		"embeds": []any{map[string]any{"kind": "append"}, map[string]any{"kind": "append"}},
	}}, codeInvalidParams)
	result, _, live := postLive(t, a, "one", map[string]any{"embeds": []any{map[string]any{"kind": "append"}, map[string]any{"kind": "terminal"}}})
	// A save may keep the one append, but not add another.
	a.expectError(t, "message", "another", map[string]any{"room_id": "general", "message_id": result["message_id"], "body": map[string]any{
		"embeds": []any{live, map[string]any{"kind": "append"}},
	}}, codeInvalidParams)
	_, saved := save(t, a, "keep", map[string]any{"message_id": result["message_id"], "body": map[string]any{"text": "edited", "embeds": []any{live}}})
	if kept := embedsOf(t, saved); len(kept) != 1 || kept[0]["url"] != live["url"] {
		t.Fatalf("kept: %#v", kept)
	}
}

// Only /embed-terminal and /embed-append make live text; they make their
// own embed.
func TestLiveEmbedsInOtherCommandsAreInvalid(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	a := dialTestClient(t, httpServer)
	for i, text := range []string{"/avatar", "/help", "/emoji add :x:", "/embed-terminal", "/embed-append hi"} {
		for _, kind := range []string{"terminal", "append"} {
			a.expectError(t, "command", fmt.Sprint(kind, i), map[string]any{"body": map[string]any{"text": text, "embeds": []any{map[string]any{"kind": kind}}}}, codeInvalidParams)
		}
	}
	a.expectError(t, "command", "upload", map[string]any{"body": map[string]any{"text": "/embed-terminal", "embeds": []any{map[string]any{"kind": "upload"}}}}, codeInvalidParams)
	a.expectQuiet(t)
}

// liveCommand runs /embed-terminal or /embed-append and returns its write
// URL and the ~private notice that precedes the result.
func liveCommand(t *testing.T, c *testClient, id string, params map[string]any) (map[string]any, string, string) {
	t.Helper()
	before, result := c.request(t, "command", id, params)
	written := result["embeds"].([]any)
	if len(before) != 1 || len(result) != 1 || len(written) != 1 {
		t.Fatalf("command: %#v then %#v", before, result)
	}
	notice := notificationParams(t, before[0], "message")
	if notice["from"].(map[string]any)["user_id"] != privateNoticeID || notice["message_id"] != nil || notice["log_id"] != nil {
		t.Fatalf("notice: %#v", notice)
	}
	write := written[0].(map[string]any)
	writeURL := write["write_url"].(string)
	if !strings.HasPrefix(writeURL, "http://") || !strings.Contains(notice["body"].(map[string]any)["text"].(string), writeURL) {
		t.Fatalf("write %#v, notice %#v", write, notice)
	}
	return write, writeURL, notice["body"].(map[string]any)["text"].(string)
}

func TestEmbedTerminalCommandPostsWhenTheWriteStarts(t *testing.T) {
	app, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	replied, _ := save(t, b, "first", map[string]any{"body": map[string]any{"text": "run the tests?"}})
	a.notification(t, "message")

	write, writeURL, notice := liveCommand(t, a, "terminal", map[string]any{
		"room_id": "general", "reply_to": map[string]any{"message_id": replied},
		"body": map[string]any{"text": "/embed-terminal make test"},
	})
	if write["kind"] != "terminal" || write["embed_id"] == nil || !strings.HasPrefix(writeURL, httpServer.URL+writePath) {
		t.Fatalf("write: %#v", write)
	}
	for _, want := range []string{"within 5 minutes", "80×40", "64 KiB", "1 hour",
		"script -qfc 'stty cols 80 rows 40; YOUR_COMMAND' /dev/null | curl -sT - '" + writeURL + "'",
		"tmux new -d -s apron -x 80 -y 40 \\; set -w window-size manual \\; pipe-pane -o \"curl -sT - '" + writeURL + "'\""} {
		if !strings.Contains(notice, want) {
			t.Fatalf("notice lacks %q: %s", want, notice)
		}
	}
	// Nothing is posted until the write starts.
	a.expectQuiet(t)
	b.expectQuiet(t)

	pipe, writerDone := startLiveWrite(t, app, writeURL, "\x1b[32mok\x1b[0m")
	var liveURL, messageID string
	for _, client := range clients {
		posted := client.notification(t, "message")
		embeds := embedsOf(t, posted)
		liveURL, _ = embeds[0]["url"].(string)
		messageID, _ = posted["message_id"].(string)
		og := map[string]any{"title": "make test", "description": terminalLive}
		want := map[string]any{"text": "", "embeds": []any{map[string]any{"embed_id": write["embed_id"], "kind": "terminal", "title": "make test", "url": liveURL, "og": og}}}
		if !reflect.DeepEqual(posted["body"], any(want)) || posted["from"].(map[string]any)["user_id"] != "guest_1" ||
			posted["room_id"] != "general" || !reflect.DeepEqual(posted["reply_to"], any(map[string]any{"message_id": replied})) || posted["log_id"] != messageID {
			t.Fatalf("posted: %#v", posted)
		}
	}
	status, header, got := func() (int, http.Header, []byte) {
		// The reader gets the text so far; read it as it grows.
		response, err := http.Get(liveURL)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		first := make([]byte, len("\x1b[32mok\x1b[0m"))
		if _, err := io.ReadFull(response.Body, first); err != nil {
			t.Fatal(err)
		}
		_, _ = pipe.Write([]byte(" 12 tests\r\n"))
		_ = pipe.Close()
		rest, _ := io.ReadAll(response.Body)
		return response.StatusCode, response.Header, append(first, rest...)
	}()
	if status != http.StatusOK || string(got) != "\x1b[32mok\x1b[0m 12 tests\r\n" || header.Get("Content-Type") != "text/plain; charset=utf-8" || header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("live read: %d %q %#v", status, got, header)
	}
	if status := <-writerDone; status != http.StatusNoContent {
		t.Fatalf("writer status %d", status)
	}
	for _, client := range clients {
		finished := client.notification(t, "message")
		og := map[string]any{"title": "make test", "description": terminalFinished}
		want := []any{map[string]any{"embed_id": write["embed_id"], "kind": "terminal", "title": "make test", "output": "\x1b[32mok\x1b[0m 12 tests\r\n", "og": og}}
		if finished["message_id"] != messageID || !reflect.DeepEqual(finished["body"].(map[string]any)["embeds"], any(want)) {
			t.Fatalf("finished: %#v", finished)
		}
	}
	// The write URL works once.
	if status, _, _ := httpDo(t, http.MethodPut, writeURL, strings.NewReader("again"), ""); status != http.StatusNotFound {
		t.Fatalf("reused write URL: %d", status)
	}
}

func TestEmbedAppendCommandPostsItsOpeningText(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	ops, _ := saveRoom(t, a, "ops", map[string]any{"title": "Ops"})
	joinRoom(t, b, ops)
	expectMembership(t, a, ops, "guest_2", true)

	write, writeURL, notice := liveCommand(t, a, "append", map[string]any{
		"room_id": ops, "body": map[string]any{"text": "/embed-append Here is **the plan**:", "mentions": []any{"guest_2"}},
	})
	if write["kind"] != "append" || !strings.Contains(notice, "YOUR_COMMAND | curl -sT - '"+writeURL+"'") || !strings.Contains(notice, "64 KiB") || !strings.Contains(notice, "a form feed (`\\f`)") {
		t.Fatalf("write %#v, notice %s", write, notice)
	}
	a.expectQuiet(t)
	body, pipe := io.Pipe()
	writerDone := make(chan int, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodPut, writeURL, body)
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			response.Body.Close()
			writerDone <- response.StatusCode
		}
		close(writerDone)
	}()
	// The message is posted when the request arrives, before any text.
	var posted map[string]any
	for _, client := range clients {
		posted = client.notification(t, "message")
		want := map[string]any{"text": "Here is **the plan**:\n\n", "format": "markdown", "embeds": []any{map[string]any{"embed_id": write["embed_id"], "kind": "append", "url": embedsOf(t, posted)[0]["url"]}}}
		if !reflect.DeepEqual(posted["body"], any(want)) || posted["room_id"] != ops || posted["reply_to"] != nil {
			t.Fatalf("posted: %#v", posted)
		}
	}
	_, _ = pipe.Write([]byte("1. Roll back.\n"))
	_, _ = pipe.Write([]byte("2. Renew the cert.\n"))
	_ = pipe.Close()
	if status := <-writerDone; status != http.StatusNoContent {
		t.Fatalf("writer status %d", status)
	}
	for _, client := range clients {
		finished := client.notification(t, "message")
		want := map[string]any{"text": "Here is **the plan**:\n\n1. Roll back.\n2. Renew the cert.\n", "format": "markdown"}
		if finished["message_id"] != posted["message_id"] || !reflect.DeepEqual(finished["body"], any(want)) {
			t.Fatalf("finished: %#v", finished)
		}
	}
}

// A command's write URL that is not used in time expires, and nothing is
// posted.
func TestEmbedCommandWritesExpire(t *testing.T) {
	config := DefaultConfig()
	config.UploadStartTimeout = 100 * time.Millisecond
	app, httpServer := newTestServer(t, config)
	a := dialTestClient(t, httpServer)
	_, writeURL, notice := liveCommand(t, a, "terminal", map[string]any{"body": map[string]any{"text": "/embed-terminal"}})
	if !strings.Contains(notice, "within 1 second") {
		t.Fatalf("notice: %s", notice)
	}
	_, appendURL, _ := liveCommand(t, a, "append", map[string]any{"body": map[string]any{"text": "/embed-append"}})
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		app.mu.RLock()
		left := len(app.writes) + len(app.embeds)
		app.mu.RUnlock()
		if left == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the writes never expired")
		}
	}
	for _, url := range []string{writeURL, appendURL} {
		if status, _, _ := httpDo(t, http.MethodPut, url, strings.NewReader("late"), ""); status != http.StatusNotFound {
			t.Fatalf("expired write URL: %d", status)
		}
	}
	a.expectQuiet(t)
	if page := historyPage(t, a, "general", map[string]any{}); page["messages"] != nil {
		t.Fatalf("an expired command posted: %#v", page)
	}
}

// A writer that disconnects ends the text, which keeps what arrived (§4.8.3),
// and the write URL is used up: a writer cannot continue the text.
func TestLiveTextEndsWhenItsWriterDisconnects(t *testing.T) {
	app, httpServer := newTestServer(t, DefaultConfig())
	a := dialTestClient(t, httpServer)
	disconnect := func(writeURL, text string) {
		t.Helper()
		conn, err := net.Dial("tcp", strings.TrimPrefix(httpServer.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(conn, "PUT %s HTTP/1.1\r\nHost: test\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n", strings.TrimPrefix(writeURL, httpServer.URL), len(text), text)
		token := writeURL[strings.LastIndex(writeURL, "/")+1:]
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			app.mu.RLock()
			e := app.writes[token]
			arrived := e != nil && e.live.text() == text
			app.mu.RUnlock()
			if arrived {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the text never arrived")
			}
		}
		conn.Close()
	}

	_, writeURL, _ := postLive(t, a, "terminal", map[string]any{"embeds": []any{map[string]any{"kind": "terminal"}}})
	disconnect(writeURL, "first ")
	startedSnapshot(t, a)
	if output := embedsOf(t, a.notification(t, "message"))[0]["output"]; output != "first " {
		t.Fatalf("output %q", output)
	}
	if status, _, _ := httpDo(t, http.MethodPut, writeURL, strings.NewReader("second\n"), ""); status != http.StatusNotFound {
		t.Fatalf("write after the disconnect: %d", status)
	}

	_, writeURL, _ = postLive(t, a, "append", map[string]any{"text": "> ", "embeds": []any{map[string]any{"kind": "append"}}})
	disconnect(writeURL, "only this")
	startedSnapshot(t, a)
	if text := a.notification(t, "message")["body"].(map[string]any)["text"]; text != "> only this" {
		t.Fatalf("text %q", text)
	}
}

// stallingWriter is a reader's connection whose first write waits for
// resume, as a slow reader's does while the writer goes on.
type stallingWriter struct {
	header  http.Header
	got     []byte
	stalled chan struct{}
	resume  chan struct{}
}

func (w *stallingWriter) Header() http.Header { return w.header }
func (w *stallingWriter) WriteHeader(int)     {}
func (w *stallingWriter) Write(p []byte) (int, error) {
	if w.got == nil {
		close(w.stalled)
		<-w.resume
	}
	w.got = append(w.got, p...)
	return len(p), nil
}

// A reader that falls behind what a terminal keeps fails, rather than skip
// what it missed, so that it reconnects and reads the kept text afresh
// (§4.8.5).
func TestALiveReaderThatFallsBehindFails(t *testing.T) {
	config := DefaultConfig()
	config.TerminalKeepBytes = 8
	app, httpServer := newTestServer(t, config)
	a := dialTestClient(t, httpServer)
	_, writeURL, terminal := postLive(t, a, "terminal", map[string]any{"embeds": []any{map[string]any{"kind": "terminal"}}})
	pipe, writerDone := startLiveWrite(t, app, writeURL, "12345678")
	app.mu.RLock()
	live := app.embeds[terminal["embed_id"].(string)].live
	app.mu.RUnlock()
	waitFor := func(what string, done func(b *liveText) bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			live.mu.Lock()
			ok := done(live)
			live.mu.Unlock()
			if ok {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
		}
	}
	waitFor("the text never arrived", func(b *liveText) bool { return b.total == 8 })
	w := &stallingWriter{header: make(http.Header), stalled: make(chan struct{}), resume: make(chan struct{})}
	failed := make(chan any, 1)
	go func() {
		defer func() { failed <- recover() }()
		app.handleLive(w, httptest.NewRequest(http.MethodGet, liveURLOf(t, app, terminal["embed_id"]), nil))
	}()
	<-w.stalled
	_, _ = pipe.Write([]byte("abcdefghij"))
	waitFor("the terminal never let go of what the reader missed", func(b *liveText) bool { return b.start > 8 })
	close(w.resume)
	_ = pipe.Close()
	if status := <-writerDone; status != http.StatusNoContent {
		t.Fatalf("writer status %d", status)
	}
	if recovered := <-failed; recovered != http.ErrAbortHandler || string(w.got) != "12345678" {
		t.Fatalf("a reader that fell behind got %q, then %v", w.got, recovered)
	}
}

// A finished terminal lets go of its live text: its url serves the output
// from its snapshot.
func TestAFinishedTerminalLetsGoOfItsLiveText(t *testing.T) {
	app, httpServer := newTestServer(t, DefaultConfig())
	a := dialTestClient(t, httpServer)
	_, writeURL, terminal := postLive(t, a, "terminal", map[string]any{"embeds": []any{map[string]any{"kind": "terminal"}}})
	if status, _, _ := httpDo(t, http.MethodPut, writeURL, strings.NewReader("done\n"), ""); status != http.StatusNoContent {
		t.Fatalf("terminal write: %d", status)
	}
	liveURL := embedsOf(t, startedSnapshot(t, a))[0]["url"].(string)
	a.notification(t, "message")
	app.mu.RLock()
	live := app.embeds[terminal["embed_id"].(string)].live
	app.mu.RUnlock()
	if live != nil {
		t.Fatalf("a finished terminal holds its live text: %q", live.data)
	}
	if status, _, got := httpDo(t, http.MethodGet, liveURL, nil, ""); status != http.StatusOK || string(got) != "done\n" {
		t.Fatalf("finished terminal URL: %d %q", status, got)
	}
}

// The kept text is converted with WHATWG's UTF-8 decode without BOM
// (§4.8.3): one U+FFFD per maximal invalid subsequence, and for an
// incomplete sequence at the end; a BOM is kept.
func TestLiveTextDecodesAsWHATWG(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"ok \u00e9", "ok \u00e9"},
		{"a\xF1\x80\x80\xE1\x80\xC2b", "a\uFFFD\uFFFD\uFFFDb"},
		{"\xC0\xAF", "\uFFFD\uFFFD"},
		{"\xED\xA0\x80", "\uFFFD\uFFFD\uFFFD"},
		{"\xE1\x80\xE2\xF0\x91\x92\xF1\xBF\x41", "\uFFFD\uFFFD\uFFFD\uFFFDA"},
		{"end \xE2\x82", "end \uFFFD"},
		{"\uFEFFbom", "\uFEFFbom"},
	} {
		live := newLiveText(true, 1<<10, 1<<20)
		live.write([]byte(c.in))
		if got := live.text(); got != c.want {
			t.Errorf("%q: %q, want %q", c.in, got, c.want)
		}
	}
}

// Live text has one writer at a time.
func TestLiveTextHasOneWriter(t *testing.T) {
	app, httpServer := newTestServer(t, DefaultConfig())
	a := dialTestClient(t, httpServer)
	_, writeURL, _ := postLive(t, a, "terminal", map[string]any{"embeds": []any{map[string]any{"kind": "terminal"}}})
	pipe, writerDone := startLiveWrite(t, app, writeURL, "one")
	if status, _, _ := httpDo(t, http.MethodPut, writeURL, strings.NewReader("two"), ""); status != http.StatusConflict {
		t.Fatalf("second writer: %d", status)
	}
	_ = pipe.Close()
	if status := <-writerDone; status != http.StatusNoContent {
		t.Fatalf("writer status %d", status)
	}
	startedSnapshot(t, a)
	if output := embedsOf(t, a.notification(t, "message"))[0]["output"]; output != "one" {
		t.Fatalf("output %q", output)
	}
}

// Live text ends at its duration limit, with or without a writer.
func TestLiveTextEndsAtItsDurationLimit(t *testing.T) {
	config := DefaultConfig()
	config.LiveTextMaxDuration = 200 * time.Millisecond
	app, httpServer := newTestServer(t, config)
	a := dialTestClient(t, httpServer)
	_, writeURL, _ := postLive(t, a, "terminal", map[string]any{"embeds": []any{map[string]any{"kind": "terminal"}}})
	pipe, writerDone := startLiveWrite(t, app, writeURL, "still going")
	defer pipe.Close()
	// The writer, still sending, is told why the text ended.
	if status := <-writerDone; status != http.StatusOK {
		t.Fatalf("writer status %d", status)
	}
	startedSnapshot(t, a)
	if output := embedsOf(t, a.notification(t, "message"))[0]["output"]; output != "still going" {
		t.Fatalf("output %q", output)
	}
}

// The push of a new message with append text waits for the text to end, so
// that it carries the whole text (§4.8.5).
func TestAppendPushWaitsForTheText(t *testing.T) {
	relay := newTestRelay(t)
	app, httpServer := pushTestServer(t)
	a := dialTestClient(t, httpServer)
	alice := addAccount(t, app, "alice", a)
	addPush(app, alice, pushRegistration{url: relay.URL + "/alice", wake: defaultWake})
	messagePushes := func() []map[string]any {
		t.Helper()
		var bodies []map[string]any
		for _, request := range relay.pushes(t, app, a) {
			if message, ok := request.payload["message"].(map[string]any); ok {
				bodies = append(bodies, message["body"].(map[string]any))
			}
		}
		return bodies
	}
	_, writeURL, _ := postLive(t, a, "append", map[string]any{"text": "@alice: ", "mentions": []any{"alice"}, "embeds": []any{map[string]any{"kind": "append"}}})
	if got := messagePushes(); len(got) != 0 {
		t.Fatalf("pushed before the text ended: %#v", got)
	}
	if status, _, _ := httpDo(t, http.MethodPut, writeURL, strings.NewReader("the deploy is done"), ""); status != http.StatusNoContent {
		t.Fatalf("append write: %d", status)
	}
	a.notification(t, "message")
	if got := messagePushes(); len(got) != 1 || got[0]["text"] != "@alice: the deploy is done" {
		t.Fatalf("pushes: %#v", got)
	}

	// A save that removes the embed ends the text and sends the push.
	result, _, _ := postLive(t, a, "saved", map[string]any{"text": "@alice: ", "mentions": []any{"alice"}, "embeds": []any{map[string]any{"kind": "append"}}})
	if got := messagePushes(); len(got) != 0 {
		t.Fatalf("pushed before the save: %#v", got)
	}
	save(t, a, "stop", map[string]any{"message_id": result["message_id"], "body": map[string]any{"text": "@alice: never mind", "mentions": []any{"alice"}}})
	if got := messagePushes(); len(got) != 1 || got[0]["text"] != "@alice: never mind" {
		t.Fatalf("pushes after the save: %#v", got)
	}
}

// A finished terminal keeps its output across a restart, and its url
// serves it; live text that had not ended fails, as any unfinished write
// does (§4.8.3).
func TestLiveTextAcrossARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	uploads := t.TempDir()
	open := func() store.Store {
		s, err := store.OpenSQLite(path)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	_, httpServer, stop := startWithStore(t, open(), uploads)
	a := dialTestClient(t, httpServer)
	_, writeURL, _ := postLive(t, a, "terminal", map[string]any{"embeds": []any{map[string]any{"kind": "terminal"}}})
	if status, _, _ := httpDo(t, http.MethodPut, writeURL, strings.NewReader("done\n"), ""); status != http.StatusNoContent {
		t.Fatalf("terminal write: %d", status)
	}
	terminalURL := embedsOf(t, startedSnapshot(t, a))[0]["url"].(string)
	a.notification(t, "message")
	postLive(t, a, "append", map[string]any{"text": "> ", "embeds": []any{map[string]any{"kind": "append"}}})
	stop()

	_, httpServer, _ = startWithStore(t, open(), uploads)
	b := dialTestClient(t, httpServer)
	// The page holds each message's snapshots; the last is its current one.
	latest := make(map[any]map[string]any)
	var order []any
	for _, value := range records(t, historyPage(t, b, "general", map[string]any{}), "messages") {
		snapshot := value.(map[string]any)
		if latest[snapshot["message_id"]] == nil {
			order = append(order, snapshot["message_id"])
		}
		latest[snapshot["message_id"]] = snapshot
	}
	finished := embedsOf(t, latest[order[0]])
	if len(order) != 2 || len(finished) != 1 || finished[0]["output"] != "done\n" || finished[0]["url"] != nil {
		t.Fatalf("terminal after a restart: %#v", latest)
	}
	if appended := latest[order[1]]; appended["body"].(map[string]any)["text"] != "> " || len(embedsOf(t, appended)) != 0 {
		t.Fatalf("append after a restart: %#v", appended)
	}
	liveURL := httpServer.URL + terminalURL[strings.Index(terminalURL, livePath):]
	if status, _, got := httpDo(t, http.MethodGet, liveURL, nil, ""); status != http.StatusOK || string(got) != "done\n" {
		t.Fatalf("finished terminal URL: %d %q", status, got)
	}
}

// navigate opens url as a browser opens a page, without following a
// redirect, and returns the status and the headers.
func navigate(t *testing.T, target string) (int, http.Header) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Sec-Fetch-Mode", "navigate")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	return response.StatusCode, response.Header
}

// A terminal embed carries an og that says what it is (§4.8.6), waiting,
// live and finished, so a client that doesn't render terminals shows a card
// for it; and a browser that opens its url goes to the terminal viewer, with
// the url in the fragment (§4.8.5).
func TestTerminalCardAndViewer(t *testing.T) {
	app, httpServer := newTestServer(t, DefaultConfig())
	a := dialTestClient(t, httpServer)
	result := postEmbeds(t, a, "terminal", map[string]any{"room_id": "general", "body": map[string]any{
		"embeds": []any{map[string]any{"kind": "terminal", "title": "make test", "og": map[string]any{"title": "forged"}}},
	}})
	waiting := embedsOf(t, result["snapshot"].(map[string]any))[0]
	if og, _ := waiting["og"].(map[string]any); og["title"] != "make test" || og["description"] != terminalWaiting || len(og) != 2 {
		t.Fatalf("waiting og: %#v", waiting["og"])
	}
	liveURL := liveURLOf(t, app, waiting["embed_id"])

	body, pipe := io.Pipe()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		request, _ := http.NewRequest(http.MethodPut, result["embeds"].([]any)[0].(map[string]any)["write_url"].(string), body)
		if response, err := http.DefaultClient.Do(request); err == nil {
			response.Body.Close()
		}
	}()
	_, _ = pipe.Write([]byte("\x1b[32mok\x1b[0m\r\n"))
	live := embedsOf(t, startedSnapshot(t, a))[0]
	if og, _ := live["og"].(map[string]any); live["url"] != liveURL || og["title"] != "make test" || og["description"] != terminalLive {
		t.Fatalf("live embed: %#v", live)
	}

	// A save that sends the embed back with another og keeps the server's.
	_, saved := save(t, a, "edit", map[string]any{"message_id": result["message_id"], "body": map[string]any{
		"text": "edited", "embeds": []any{map[string]any{"embed_id": live["embed_id"], "kind": "terminal", "title": "make test", "og": map[string]any{"title": "forged"}}},
	}})
	if og, _ := embedsOf(t, saved)[0]["og"].(map[string]any); og["title"] != "make test" || og["description"] != terminalLive {
		t.Fatalf("saved og: %#v", og)
	}

	// A browser goes to the viewer, with the url and title in the fragment; a reader that fetches gets the text.
	status, header := navigate(t, liveURL)
	location, err := url.Parse(header.Get("Location"))
	if status != http.StatusFound || err != nil || header.Get("Vary") != "Sec-Fetch-Mode" || location.Scheme+"://"+location.Host+location.Path != DefaultTerminalViewerURL || location.RawQuery != "" {
		t.Fatalf("navigation: %d %#v", status, header)
	}
	if fragment, err := url.ParseQuery(location.Fragment); err != nil || fragment.Get("stream") != liveURL || fragment.Get("title") != "make test" {
		t.Fatalf("viewer fragment %q: %v", location.Fragment, err)
	}
	reader, err := http.Get(liveURL)
	if err != nil || reader.StatusCode != http.StatusOK || reader.Header.Get("Content-Type") != "text/plain; charset=utf-8" || reader.Header.Get("Vary") != "Sec-Fetch-Mode" {
		t.Fatalf("reader: %v %v", reader, err)
	}
	reader.Body.Close()

	_ = pipe.Close()
	<-writerDone
	finished := embedsOf(t, a.notification(t, "message"))[0]
	if og, _ := finished["og"].(map[string]any); finished["url"] != nil || finished["output"] != "\x1b[32mok\x1b[0m\r\n" || og["description"] != terminalFinished || og["title"] != "make test" {
		t.Fatalf("finished embed: %#v", finished)
	}
}

// Without a viewer, a browser that opens a terminal's url gets the text; an
// append's url never redirects, and an append carries no og.
func TestNoViewerAndAppendsServeText(t *testing.T) {
	config := DefaultConfig()
	config.TerminalViewerURL = ""
	app, httpServer := newTestServer(t, config)
	a := dialTestClient(t, httpServer)
	for _, kind := range []string{"terminal", "append"} {
		result := postEmbeds(t, a, kind, map[string]any{"room_id": "general", "body": map[string]any{"embeds": []any{map[string]any{"kind": kind}}}})
		embed := embedsOf(t, result["snapshot"].(map[string]any))[0]
		if (kind == "append") != (embed["og"] == nil) {
			t.Fatalf("%s og: %#v", kind, embed["og"])
		}
		writeURL := result["embeds"].([]any)[0].(map[string]any)["write_url"].(string)
		if status, _, _ := httpDo(t, http.MethodPut, writeURL, strings.NewReader("text"), ""); status != http.StatusNoContent {
			t.Fatalf("%s write: %d", kind, status)
		}
		startedSnapshot(t, a)
		a.notification(t, "message")
		if kind == "terminal" {
			if status, header := navigate(t, liveURLOf(t, app, embed["embed_id"])); status != http.StatusOK || header.Get("Content-Type") != "text/plain; charset=utf-8" || header.Get("Location") != "" {
				t.Fatalf("terminal without a viewer: %d %#v", status, header)
			}
		}
	}
}
