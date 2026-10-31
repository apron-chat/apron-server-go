package server

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/unicode"
)

// Live text (§4.8.3): terminal (§4.8.6) and append (§4.8.5) embeds, which
// a writer sends as a streaming PUT to the write URL and readers GET from
// the embed's url as it grows.

// livePath serves live text: /live/<embed_id>/<secret>.
const livePath = "/live/"

const (
	kindTerminal = "terminal"
	kindAppend   = "append"
)

// liveKind reports whether embeds of kind carry live text.
func liveKind(kind any) bool {
	return kind == kindTerminal || kind == kindAppend
}

func (e *embedState) liveURL() string {
	return e.baseURL + livePath + e.id + "/" + e.secret
}

// pendingPost is the message a /embed-terminal or /embed-append command
// posts once its write starts: from user, in roomID, replying to replyTo
// if set, with text as an append's opening body.text.
type pendingPost struct {
	user    *userState
	roomID  string
	replyTo string
	text    string
}

// startLiveLocked gives a new terminal or append embed its live text and
// url. An append keeps at most AppendMaxBytes, and less after a long
// opening text, so that the finished body.text stays within the frames
// the server reads (ReadLimit).
func (s *Server) startLiveLocked(e *embedState, opening string) {
	if e.kind == kindTerminal {
		e.live = newLiveText(true, s.config.TerminalKeepBytes, s.config.LiveTextMaxBytes)
	} else {
		keep := max(0, min(s.config.AppendMaxBytes, int(s.config.ReadLimit)-len(opening)))
		e.live = newLiveText(false, keep, int64(keep))
	}
	e.owned = map[string]any{"url": e.liveURL()}
}

// startWriterLocked admits live text's one writer, which posts a command's
// pending message and starts the duration limit. It returns the refusal's
// status and message, if any.
func (s *Server) startWriterLocked(e *embedState) (int, string) {
	if e.started {
		return http.StatusConflict, "This live text already has a writer"
	}
	if e.post != nil && !s.postPendingLocked(e) {
		s.removeEmbedLocked(e.id)
		return http.StatusGone, "The room or the message this was to reply to is no longer available"
	}
	e.started = true
	e.timer.Stop()
	e.timer = time.AfterFunc(s.config.LiveTextMaxDuration, func() {
		s.mu.Lock()
		defer s.unlock()
		e.timedOut = true
		e.live.end()
		if !e.writing {
			s.finishLiveLocked(e)
		}
	})
	e.writing = true
	return 0, ""
}

// postPendingLocked posts a command's message when its write starts
// (/embed-terminal, /embed-append): an ordinary new message from the
// command's user, in its room, carrying the embed with its url. It reports
// false when the user or the room is gone. A reply_to that the user can no
// longer quote there is left out.
func (s *Server) postPendingLocked(e *embedState) bool {
	p := e.post
	u := p.user
	if s.users[u.id] != u {
		return false
	}
	r := s.visibleRoomLocked(u, p.roomID)
	if r == nil {
		return false
	}
	embed := map[string]any{"embed_id": e.id, "kind": e.kind, "url": e.liveURL()}
	body := map[string]any{"text": "", "embeds": []any{embed}}
	if e.kind == kindTerminal {
		if e.title != "" {
			embed["title"] = e.title
		}
	} else {
		body["text"], body["format"] = p.text, "markdown"
	}
	logID := s.nextIDLocked()
	messageID := formatID(logID)
	from := u.from()
	snapshot := map[string]any{
		"message_id": messageID,
		"log_id":     messageID,
		"room_id":    r.id,
		"from":       from,
		"body":       body,
	}
	if target := s.visibleMessageLocked(u, p.replyTo); target != nil && !r.revealsTo(s.rooms[target.roomID]) {
		snapshot["reply_to"] = map[string]any{"message_id": p.replyTo}
	}
	m := &messageState{id: messageID, from: from, owner: u.id, reactions: make(map[string]reactionSet)}
	s.messages[messageID] = m
	e.messageID = messageID
	e.post = nil
	s.touchEmbed(e.id)
	s.commitSnapshotLocked(m, snapshot, logID)
	s.messageUnreadLocked(m, nil, nil)
	s.unreadChangedLocked(u, r.id)
	s.wakeNewLocked(m, snapshot)
	return true
}

// wakeNewLocked pushes a new message (§4.9), except one with live append
// text: its push waits for the text to end (§4.8.5), so that it carries
// the whole text.
func (s *Server) wakeNewLocked(m *messageState, snapshot map[string]any) {
	if hasLiveAppend(snapshot) {
		m.pushHeld = true
		return
	}
	s.wakeLocked(m, snapshot, nil)
}

// releasePushLocked sends the push a message's live append text held
// back, for its current snapshot, as for a new message.
func (s *Server) releasePushLocked(m *messageState) {
	if !m.pushHeld {
		return
	}
	m.pushHeld = false
	if snapshot := m.snapshot(); snapshot["deleted"] != true {
		s.wakeLocked(m, snapshot, nil)
	}
}

func hasLiveAppend(snapshot map[string]any) bool {
	body, _ := snapshot["body"].(map[string]any)
	for _, value := range asList(body["embeds"]) {
		if embed, ok := value.(map[string]any); ok && embed["kind"] == kindAppend {
			return true
		}
	}
	return false
}

// writeLive copies the request body into live text until the text ends:
// the body ends, the writer disconnects, a limit is reached, or the embed is
// removed. The text keeps what arrived (§4.8.3); a writer cannot continue it.
func (s *Server) writeLive(w http.ResponseWriter, r *http.Request, e *embedState) {
	live := e.live
	// The reply may come before the body ends, as when the embed is removed.
	controller := http.NewResponseController(w)
	_ = controller.EnableFullDuplex()
	chunks := make(chan []byte)
	go func() {
		defer close(chunks)
		for {
			buf := make([]byte, 32<<10)
			n, err := r.Body.Read(buf)
			if n > 0 {
				select {
				case chunks <- buf[:n]:
				case <-live.finished:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	bodyEnded := false
	for open := true; open; {
		select {
		case chunk, ok := <-chunks:
			bodyEnded = !ok
			open = ok && live.write(chunk)
		case <-live.finished:
			open = false
		}
	}
	if !bodyEnded {
		// Unblock a body read still waiting on the writer, and let it finish.
		// The expired deadline also cancels the connection's context, so the
		// connection must not serve another request.
		w.Header().Set("Connection", "close")
		_ = controller.SetReadDeadline(time.Now())
		for range chunks {
		}
	}
	s.mu.Lock()
	defer s.unlock()
	e.writing = false
	if e.removed {
		http.Error(w, "The live text was removed from its message", http.StatusGone)
		return
	}
	s.finishLiveLocked(e)
	if live.limited() {
		http.Error(w, fmt.Sprintf("This live text is limited to %s; it ended", formatBytes(live.limit())), http.StatusRequestEntityTooLarge)
		return
	}
	if e.timedOut {
		// The writer may still be sending: a reply before the close tells it why.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "The text reached its %s limit; what arrived is posted.\n", formatDuration(s.config.LiveTextMaxDuration))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// finishLiveLocked ends live text exactly once and publishes the snapshot
// its kind defines (§4.8.3): a terminal's embed carries the kept output in
// place of its url (§4.8.6), and an append's kept text is added to
// body.text and the embed removed (§4.8.5).
func (s *Server) finishLiveLocked(e *embedState) {
	if e.finished || e.removed {
		return
	}
	e.finished = true
	e.live.end()
	e.timer.Stop()
	delete(s.writes, e.token)
	s.touchEmbed(e.id)
	m := s.messages[e.messageID]
	if e.kind == kindTerminal {
		output := e.live.text()
		e.owned = map[string]any{"output": output}
		if m != nil {
			s.republishLocked(m, func(body map[string]any) bool {
				return replaceEmbed(body, e.id, func(embed map[string]any) {
					delete(embed, "url")
					embed["output"] = output
				})
			})
		}
		return
	}
	text := e.live.text()
	if m != nil {
		s.republishLocked(m, func(body map[string]any) bool {
			if !replaceEmbed(body, e.id, nil) {
				return false
			}
			current, _ := body["text"].(string)
			body["text"] = current + text
			return true
		})
	}
	s.removeEmbedLocked(e.id)
	if m != nil {
		s.releasePushLocked(m)
	}
}

// handleLive serves live text (§4.8.3): the kept text from its start, then
// more as it arrives, until the text ends. A finished terminal's url serves
// its output.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != http.MethodGet {
		http.Error(w, "Read with GET", http.StatusMethodNotAllowed)
		return
	}
	s.mu.RLock()
	e := s.lookupEmbedLocked(strings.TrimPrefix(r.URL.Path, livePath))
	if e == nil || !liveKind(e.kind) || (e.live == nil && !e.finished) {
		s.mu.RUnlock()
		http.NotFound(w, r)
		return
	}
	live := e.live
	output, _ := e.owned["output"].(string)
	finished := e.finished
	s.mu.RUnlock()
	header := w.Header()
	header.Set("Content-Type", "text/plain; charset=utf-8")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	// no-transform: a proxy that compresses it holds the stream back until it
	// ends (§4.8.3). X-Accel-Buffering asks nginx not to buffer it either.
	header.Set("Cache-Control", "no-store, no-transform")
	header.Set("X-Accel-Buffering", "no")
	if finished {
		_, _ = io.WriteString(w, output)
		return
	}
	controller := http.NewResponseController(w)
	position := int64(-1)
	for {
		chunk, next, changed, ended := live.read(position)
		if len(chunk) > 0 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
		_ = controller.Flush()
		position = next
		if ended {
			return
		}
		select {
		case <-changed:
		case <-r.Context().Done():
			return
		}
	}
}

// liveText holds live text as it arrives and wakes its readers as it grows.
// A terminal keeps its last keep bytes, cut by keepLast; an append keeps its
// first keep bytes, up to a whole character. Past maxBytes in all, the text
// ends.
type liveText struct {
	mu       sync.Mutex
	terminal bool
	keep     int
	maxBytes int64
	// data holds the bytes from absolute offset start on: for a terminal
	// at least its last keep bytes, and at most twice that.
	data    []byte
	start   int64
	total   int64
	changed chan struct{}
	ended   bool
	atLimit bool
	// finished closes when the text ends, for its writer.
	finished chan struct{}
}

func newLiveText(terminal bool, keep int, maxBytes int64) *liveText {
	if !terminal {
		maxBytes = int64(keep)
	}
	return &liveText{terminal: terminal, keep: keep, maxBytes: maxBytes, changed: make(chan struct{}), finished: make(chan struct{})}
}

// write adds to the text and reports whether it still accepts more. Past
// the size limit it keeps what fits and ends.
func (b *liveText) write(p []byte) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ended {
		return false
	}
	if room := b.maxBytes - b.total; int64(len(p)) > room {
		p = p[:room]
		b.atLimit = true
	}
	b.data = append(b.data, p...)
	b.total += int64(len(p))
	if b.terminal && len(b.data) > 2*b.keep {
		excess := len(b.data) - b.keep
		b.data = append([]byte(nil), b.data[excess:]...)
		b.start += int64(excess)
	}
	if !b.terminal && b.atLimit {
		b.data = wholeRunes(b.data)
	}
	b.signalLocked()
	if b.atLimit {
		b.endLocked()
	}
	return !b.ended
}

func (b *liveText) end() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.endLocked()
}

func (b *liveText) endLocked() {
	if !b.ended {
		b.ended = true
		close(b.finished)
		b.signalLocked()
	}
}

func (b *liveText) signalLocked() {
	close(b.changed)
	b.changed = make(chan struct{})
}

func (b *liveText) isEnded() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ended
}

func (b *liveText) limited() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.atLimit
}

// limit is the size limit the writer reached: what a terminal's writer may
// send, or what an append keeps.
func (b *liveText) limit() int64 {
	return b.maxBytes
}

// tailLocked returns the offset in data of the last keep bytes a terminal
// keeps, and whether it kept less than it received.
func (b *liveText) tailLocked() (int, bool) {
	if !b.terminal || b.total <= int64(b.keep) {
		return 0, false
	}
	return len(b.data) - b.keep, true
}

// keptStartLocked is the absolute offset of the kept text: for a terminal
// past its size, the start of keepLast.
func (b *liveText) keptStartLocked() int64 {
	tail, cut := b.tailLocked()
	if !cut {
		return b.start
	}
	return b.start + int64(tail+keepLastStart(b.data[tail:]))
}

// read returns the bytes from position on, the next position, a channel
// closed on the next change, and whether the text has ended with nothing
// more to read. A new reader (position -1), and one that fell behind what
// a terminal keeps, read from the start of the kept text.
func (b *liveText) read(position int64) ([]byte, int64, chan struct{}, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	tail, _ := b.tailLocked()
	if position < b.start+int64(tail) {
		position = b.keptStartLocked()
	}
	end := b.start + int64(len(b.data))
	position = min(position, end)
	chunk := append([]byte(nil), b.data[position-b.start:]...)
	return chunk, end, b.changed, b.ended
}

// text is the kept text, converted with WHATWG's UTF-8 decode without BOM
// (§4.8.3): each invalid sequence, and an incomplete one at the end, becomes
// one U+FFFD, as a browser's TextDecoder shows it.
func (b *liveText) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	kept := b.data[b.keptStartLocked()-b.start:]
	decoded, _ := unicode.UTF8.NewDecoder().Bytes(kept) // x/text's UTF8 never fails, and keeps a BOM
	text := string(decoded)
	if !b.terminal && len(text) > b.keep {
		// Replacement characters can be longer than the bytes they replace.
		text = string(wholeRunes([]byte(text[:b.keep])))
	}
	return text
}

// keepLast is the last part of output of at most limit bytes that a
// terminal keeps (§4.8.6): it starts at its first byte below 0x20, or
// without one, at its first byte outside 0x80–0xBF.
func keepLast(output []byte, limit int) []byte {
	if len(output) <= limit {
		return output
	}
	tail := output[len(output)-limit:]
	return tail[keepLastStart(tail):]
}

// keepLastStart is where keepLast starts in tail.
func keepLastStart(tail []byte) int {
	for i, c := range tail {
		if c < 0x20 {
			return i
		}
	}
	for i, c := range tail {
		if c < 0x80 || c > 0xBF {
			return i
		}
	}
	return len(tail)
}

// wholeRunes drops a character that p ends partway through, as when an
// append is cut at its size limit (§4.8.5).
func wholeRunes(p []byte) []byte {
	for i := len(p) - 1; i >= 0 && i >= len(p)-utf8.UTFMax; i-- {
		if utf8.RuneStart(p[i]) {
			if !utf8.FullRune(p[i:]) {
				return p[:i]
			}
			return p
		}
	}
	return p
}

// formatBytes describes a size for people, such as "64 KiB".
func formatBytes(n int64) string {
	switch {
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%d KiB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

// formatDuration describes a duration for people, such as "5 minutes".
func formatDuration(d time.Duration) string {
	unit := func(n int64, name string) string {
		if n == 1 {
			return "1 " + name
		}
		return fmt.Sprintf("%d %ss", n, name)
	}
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return unit(int64(d/time.Hour), "hour")
	case d >= time.Minute && d%time.Minute == 0:
		return unit(int64(d/time.Minute), "minute")
	}
	return unit(int64((d+time.Second-1)/time.Second), "second")
}

// maxLiveTitleRunes bounds the title /embed-terminal takes.
const maxLiveTitleRunes = 200

// embedTerminalCommand prepares a terminal embed titled by the rest of the
// line, if any: see liveCommand.
func (s *Server) embedTerminalCommand(c *client, r *roomState, body map[string]any, args, replyTo string) (map[string]any, *rpcError) {
	title, _, _ := strings.Cut(args, "\n")
	title = strings.TrimSpace(title)
	if runes := []rune(title); len(runes) > maxLiveTitleRunes {
		title = string(runes[:maxLiveTitleRunes])
	}
	return s.liveCommand(c, r, body, kindTerminal, title, replyTo)
}

// embedAppendCommand prepares an append embed whose message starts with the
// rest of the line, as Markdown: see liveCommand. A paragraph break follows
// opening text, so that the streamed text starts a paragraph of its own.
func (s *Server) embedAppendCommand(c *client, r *roomState, body map[string]any, args, replyTo string) (map[string]any, *rpcError) {
	if args != "" {
		args += "\n\n"
	}
	return s.liveCommand(c, r, body, kindAppend, args, replyTo)
}

// liveCommand prepares live text that posts a new message when its writer
// starts: from the sender, in the command's room, replying to the
// command's reply_to. Nothing is posted yet: the result carries the write
// URL (§4.8.3), and a ~private notice shows it with ways to use it. A write
// that does not start in time expires, and nothing is posted. It counts as
// a new message toward MessagesPerMinute.
func (s *Server) liveCommand(c *client, r *roomState, body map[string]any, kind, text, replyTo string) (map[string]any, *rpcError) {
	if len(asList(body["embeds"])) > 0 {
		return nil, invalidParams("/embed-%s takes no embeds: it makes its own", kind)
	}
	if replyTo != "" {
		if target := s.visibleMessageLocked(c.user, replyTo); target != nil && r.revealsTo(s.rooms[target.roomID]) {
			return nil, &rpcError{Code: codeDenied, Message: "A reply cannot quote a message that fewer people can see"}
		}
	}
	if err := s.admitPostLocked(c.user); err != nil {
		return nil, err
	}
	s.embedNumber++
	e := s.newWriteLocked(c, fmt.Sprintf("embed_%d", s.embedNumber), kind, "")
	e.post = &pendingPost{user: c.user, roomID: r.id, replyTo: replyTo, text: text}
	if kind == kindTerminal {
		e.title = text
		text = ""
	}
	s.startLiveLocked(e, text)
	c.enqueue(privateNotice(r, s.liveNotice(e)))
	return map[string]any{
		"embeds": []any{map[string]any{"embed_id": e.id, "kind": kind, "write_url": e.writeURL()}},
	}, nil
}

// liveNotice tells the sender of /embed-terminal or /embed-append how to
// write to its URL, and the limits that apply.
func (s *Server) liveNotice(e *embedState) string {
	url := e.writeURL()
	within := formatDuration(s.config.UploadStartTimeout)
	duration := formatDuration(s.config.LiveTextMaxDuration)
	if e.kind == kindTerminal {
		return fmt.Sprintf("Stream a terminal into this room within %s (80×40; it keeps the last %s, for up to %s):\n\n"+
			"```sh\nscript -qfc 'stty cols 80 rows 40; YOUR_COMMAND' /dev/null | curl -sT - '%s'\n```\n\n"+
			"Or stream a tmux session, until you `exit` its shell:\n\n"+
			"```sh\ntmux new -d -s apron -x 80 -y 40 \\; set -w window-size manual \\; pipe-pane -o \"curl -sT - '%s'\" && tmux attach -t apron\n```\n\n"+
			"The message appears when the stream starts. At a limit the stream ends, and so does the command feeding it. "+
			"Anyone with this URL can write to it, so keep it to yourself.",
			within, formatBytes(int64(s.config.TerminalKeepBytes)), duration, url, url)
	}
	return fmt.Sprintf("Stream text into this room within %s; it continues your message as Markdown (up to %s, for up to %s):\n\n"+
		"```sh\nYOUR_COMMAND | curl -sT - '%s'\n```\n\n"+
		"The message appears when the stream starts. At a limit the stream ends, and so does the command feeding it. "+
		"Anyone with this URL can write to it, so keep it to yourself.",
		within, formatBytes(e.live.limit()), duration, url)
}
