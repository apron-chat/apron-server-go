package server

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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
		got, _, _, _ := b.read(-1)
		if !bytes.Equal(got, want) {
			t.Fatalf("after %q a new reader got %q, want %q", chunk, got, want)
		}
		if text := b.text(); text != strings.ToValidUTF8(string(want), "\uFFFD") {
			t.Fatalf("after %q the output is %q, want %q", chunk, text, want)
		}
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
	got, _, _, ended := b.read(-1)
	if string(got) != "abcd" || !ended {
		t.Fatalf("reader got %q, ended %v", got, ended)
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

func TestAppendGrowsTheBodyThenMovesIntoIt(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 2)
	a, b := clients[0], clients[1]
	result, writeURL, live := postLive(t, a, "append", map[string]any{
		"text": "Here is the plan:\n\n", "format": "markdown", "mentions": []any{"guest_2"},
		"embeds": []any{map[string]any{"kind": "append", "url": "https://forged.example/a"}},
	})
	pending := b.notification(t, "message")
	liveURL, _ := live["url"].(string)
	if !strings.HasPrefix(liveURL, httpServer.URL+livePath) || !reflect.DeepEqual(embedsOf(t, pending)[0], live) {
		t.Fatalf("live embed: %#v, broadcast %#v", live, pending)
	}
	reader, err := http.Get(liveURL)
	if err != nil || reader.StatusCode != http.StatusOK {
		t.Fatalf("live reader: %v %v", reader, err)
	}
	defer reader.Body.Close()
	if reader.Header.Get("Content-Type") != "text/plain; charset=utf-8" || reader.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("live text headers: %#v", reader.Header)
	}
	text := "1. Roll back.\n2. Renew the cert, @guest_1.\n"
	if status, _, _ := httpDo(t, http.MethodPut, writeURL, strings.NewReader(text), "text/plain"); status != http.StatusNoContent {
		t.Fatalf("append write: %d", status)
	}
	if got, _ := io.ReadAll(reader.Body); string(got) != text {
		t.Fatalf("reader got %q", got)
	}
	// The finished snapshot has the text in the body, and no embed. The
	// appended text does not change mentions.
	for _, client := range clients {
		finished := client.notification(t, "message")
		want := map[string]any{"text": "Here is the plan:\n\n" + text, "format": "markdown", "mentions": []any{"guest_2"}}
		if !reflect.DeepEqual(finished["body"], any(want)) || finished["message_id"] != result["message_id"] || finished["prev_log_id"] != pending["log_id"] {
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
	if text := a.notification(t, "message")["body"].(map[string]any)["text"]; text != "> abcdef" {
		t.Fatalf("limited text %q", text)
	}
	// Invalid UTF-8 becomes replacement characters, within the limit.
	_, writeURL, _ = postLive(t, a, "invalid", map[string]any{"embeds": []any{map[string]any{"kind": "append"}}})
	if status, _, _ := httpDo(t, http.MethodPut, writeURL, strings.NewReader("ab\xffcdef"), ""); status != http.StatusNoContent {
		t.Fatalf("invalid append: %d", status)
	}
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
	if text := a.notification(t, "message")["body"].(map[string]any)["text"].(string); len(text) != 4<<10 {
		t.Fatalf("text of %d bytes", len(text))
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
		want := map[string]any{"text": "", "embeds": []any{map[string]any{"embed_id": write["embed_id"], "kind": "terminal", "title": "make test", "url": liveURL}}}
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
		want := []any{map[string]any{"embed_id": write["embed_id"], "kind": "terminal", "title": "make test", "output": "\x1b[32mok\x1b[0m 12 tests\r\n"}}
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
	if write["kind"] != "append" || !strings.Contains(notice, "YOUR_COMMAND | curl -sT - '"+writeURL+"'") || !strings.Contains(notice, "64 KiB") {
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
		want := map[string]any{"text": "Here is **the plan**:", "format": "markdown", "embeds": []any{map[string]any{"embed_id": write["embed_id"], "kind": "append", "url": embedsOf(t, posted)[0]["url"]}}}
		if !reflect.DeepEqual(posted["body"], any(want)) || posted["room_id"] != ops || posted["reply_to"] != nil {
			t.Fatalf("posted: %#v", posted)
		}
	}
	_, _ = pipe.Write([]byte("\n\n1. Roll back.\n"))
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

// A writer that disconnects may PUT again within the grace period and
// continue the text; otherwise the text ends with what it has.
func TestLiveTextWaitsForItsWriterToReturn(t *testing.T) {
	config := DefaultConfig()
	config.LiveTextGrace = 300 * time.Millisecond
	app, httpServer := newTestServer(t, config)
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
		// Wait until the server has seen the writer go.
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			app.mu.RLock()
			e := app.writes[token]
			gone := e == nil || !e.writing
			app.mu.RUnlock()
			if gone {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the writer never left")
			}
		}
	}

	_, writeURL, _ := postLive(t, a, "returns", map[string]any{"embeds": []any{map[string]any{"kind": "terminal"}}})
	disconnect(writeURL, "first ")
	if status, _, _ := httpDo(t, http.MethodPut, writeURL, strings.NewReader("second\n"), ""); status != http.StatusNoContent {
		t.Fatalf("returning writer: %d", status)
	}
	if output := embedsOf(t, a.notification(t, "message"))[0]["output"]; output != "first second\n" {
		t.Fatalf("output %q", output)
	}

	_, writeURL, _ = postLive(t, a, "gone", map[string]any{"text": "> ", "embeds": []any{map[string]any{"kind": "append"}}})
	disconnect(writeURL, "only this")
	if text := a.notification(t, "message")["body"].(map[string]any)["text"]; text != "> only this" {
		t.Fatalf("text %q", text)
	}
	if status, _, _ := httpDo(t, http.MethodPut, writeURL, strings.NewReader("late"), ""); status != http.StatusNotFound {
		t.Fatalf("write after the grace: %d", status)
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
	if status := <-writerDone; status != http.StatusNoContent {
		t.Fatalf("writer status %d", status)
	}
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
	_, writeURL, terminal := postLive(t, a, "terminal", map[string]any{"embeds": []any{map[string]any{"kind": "terminal"}}})
	if status, _, _ := httpDo(t, http.MethodPut, writeURL, strings.NewReader("done\n"), ""); status != http.StatusNoContent {
		t.Fatalf("terminal write: %d", status)
	}
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
	liveURL := httpServer.URL + terminal["url"].(string)[strings.Index(terminal["url"].(string), livePath):]
	if status, _, got := httpDo(t, http.MethodGet, liveURL, nil, ""); status != http.StatusOK || string(got) != "done\n" {
		t.Fatalf("finished terminal URL: %d %q", status, got)
	}
}
