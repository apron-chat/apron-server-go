package apron

import (
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/apron-chat/apron-server-go/apron/internal/livetext"
	"github.com/apron-chat/apron-server-go/apron/internal/roomlog"
	"github.com/apron-chat/apron-server-go/wire"
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
	return e.BaseURL + livePath + e.ID + "/" + e.Secret
}

// terminalDescription is a terminal's og description (§4.8.1), for a
// client that doesn't render terminal embeds and shows the card that og
// describes, which opens its url.
const terminalDescription = "Terminal output. Open it to watch."

// terminalOG is a terminal embed's og, titled as the embed is.
func (e *embedState) terminalOG() map[string]any {
	title := e.Title
	if title == "" {
		title = "Terminal"
	}
	return map[string]any{"title": title, "description": terminalDescription}
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

// startLiveLocked gives a new terminal or append embed its live text. Its
// url waits for the write to start (startWriterLocked). An append keeps at most AppendMaxBytes of edited text, and less
// after a long opening text, so that the finished body.text stays within
// the frames the server reads (ReadLimit).
func (s *Server) startLiveLocked(e *embedState, opening string) {
	if e.Kind == kindTerminal {
		e.live = livetext.New(true, s.config.TerminalKeepBytes, s.config.LiveTextMaxBytes)
	} else {
		keep := max(0, min(s.config.AppendMaxBytes, int(s.config.ReadLimit)-len(opening)))
		e.live = livetext.New(false, keep, s.config.LiveTextMaxBytes)
	}
	e.Owned = map[string]any{}
	if e.Kind == kindTerminal {
		e.Owned["og"] = e.terminalOG()
	}
}

// startWriterLocked admits live text's one writer and starts the duration
// limit. The embed gets its url now (§4.8.5): a command's pending message is
// posted with it, and a message's is republished with it. It returns the
// refusal's status and message, if any.
func (s *Server) startWriterLocked(e *embedState) (int, string) {
	if e.started {
		return http.StatusConflict, "This live text already has a writer"
	}
	if !s.writeAllowedLocked(e) {
		e.revoked = true
		s.failWriteLocked(e)
		return http.StatusForbidden, writeRevokedMessage
	}
	e.Owned = map[string]any{"url": e.liveURL()}
	if e.Kind == kindTerminal {
		e.Owned["og"] = e.terminalOG()
	}
	if e.post != nil {
		if !s.postPendingLocked(e) {
			s.removeEmbedLocked(e.ID)
			return http.StatusGone, "The room or the message this was to reply to is no longer available"
		}
	} else if m := s.messages[e.MessageID]; m != nil {
		s.embeds.Touch(e.ID)
		s.republishLocked(m, func(body *roomlog.Body) bool {
			return replaceEmbed(body, e.ID, func(embed map[string]any) { maps.Copy(embed, cloneObject(e.Owned)) })
		})
	}
	e.started = true
	e.timer.Stop()
	e.timer = time.AfterFunc(s.config.LiveTextMaxDuration, func() {
		s.mu.Lock()
		defer s.unlock()
		e.timedOut = true
		e.endLive()
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
	embed := map[string]any{"embed_id": e.ID, "kind": e.Kind}
	maps.Copy(embed, cloneObject(e.Owned))
	body := map[string]any{"text": "", "embeds": []any{embed}}
	if e.Kind == kindTerminal {
		if e.Title != "" {
			embed["title"] = e.Title
		}
	} else {
		body["text"], body["format"] = p.text, "markdown"
	}
	logID := s.log.NextID()
	messageID := formatID(logID)
	from := u.from()
	snapshot := &roomlog.Message{MessageID: messageID, LogID: messageID, RoomID: r.ID(), From: from, Body: storedBody(body)}
	if target := s.visibleMessageLocked(u, p.replyTo); target != nil && !r.RevealsTo(s.rooms.Get(target.RoomID())) {
		snapshot.ReplyTo = &roomlog.Ref{MessageID: p.replyTo}
	}
	m := s.newMessageLocked(messageID, from, u.id)
	e.MessageID = messageID
	e.post = nil
	s.embeds.Touch(e.ID)
	s.commitSnapshotLocked(m, snapshot, logID)
	s.messageUnreadLocked(m, nil, nil)
	s.unreadChangedLocked(u, r.ID())
	s.wakeNewLocked(m, m.Current())
	return true
}

// wakeNewLocked pushes a new message (§4.9), except one with live append
// text: its push waits for the text to end (§4.8.5), so that it carries
// the whole text.
func (s *Server) wakeNewLocked(m *messageState, snapshot *roomlog.Message) {
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
	if snapshot := m.Current(); !snapshot.Deleted {
		s.wakeLocked(m, snapshot, nil)
	}
}

// hasLiveAppend reports whether a snapshot has an append embed (§4.8.5).
func hasLiveAppend(snapshot *roomlog.Message) bool {
	if snapshot.Body == nil {
		return false
	}
	for _, raw := range snapshot.Body.Embeds {
		if embedOf(raw).Kind == kindAppend {
			return true
		}
	}
	return false
}

// writeLive copies the request body into live, e's live text, until the
// text ends: the body ends, the writer disconnects, a limit is reached, or
// the embed is removed. The text keeps what arrived (§4.8.3); a writer
// cannot continue it. The caller reads live under s.mu, as a sign-out may
// finish e (endWritesLocked), which lets go of e.live, once it unlocks.
func (s *Server) writeLive(w http.ResponseWriter, r *http.Request, e *embedState, live *livetext.Text) {
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
				case <-live.Done():
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
			open = ok && live.Write(chunk)
		case <-live.Done():
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
	if e.revoked {
		// The author lost access (endWritesLocked): what arrived until then
		// is kept.
		s.finishLiveLocked(e)
		http.Error(w, writeRevokedMessage, http.StatusForbidden)
		return
	}
	if e.removed {
		// A save without the embed ended the text: the writer's text was taken,
		// up to the save, so it succeeded (§4.8.3).
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.finishLiveLocked(e)
	if live.Limited() {
		http.Error(w, fmt.Sprintf("This live text is limited to %s; it ended", formatBytes(live.Limit())), http.StatusRequestEntityTooLarge)
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
// body.text and the embed removed (§4.8.5). The embed lets go of its live
// text, which only readers still reading it hold on to.
func (s *Server) finishLiveLocked(e *embedState) {
	if e.Finished || e.removed {
		return
	}
	e.Finished = true
	e.doneWriting()
	e.live.End()
	e.timer.Stop()
	s.embeds.EndWrite(e.token)
	s.embeds.Touch(e.ID)
	m := s.messages[e.MessageID]
	text := e.live.String()
	e.live = nil
	if e.Kind == kindTerminal {
		e.Owned = map[string]any{"output": text, "og": e.terminalOG()}
		if m != nil {
			s.republishLocked(m, func(body *roomlog.Body) bool {
				return replaceEmbed(body, e.ID, func(embed map[string]any) {
					delete(embed, "url")
					maps.Copy(embed, cloneObject(e.Owned))
				})
			})
		}
		return
	}
	if m != nil {
		s.republishLocked(m, func(body *roomlog.Body) bool {
			if !replaceEmbed(body, e.ID, nil) {
				return false
			}
			// A save may have lengthened body.text since the text started:
			// the text keeps its first part that fits in a request (ReadLimit).
			if room := max(0, int(s.config.ReadLimit)-len(body.Text)); len(text) > room {
				text = string(livetext.WholeRunes([]byte(text[:room])))
			}
			body.Text += text
			return true
		})
	}
	s.removeEmbedLocked(e.ID)
	if m != nil {
		s.releasePushLocked(m)
	}
}

// handleLive serves live text (§4.8.3): the kept text from its start, then
// more as it arrives, until the text ends. An append's reader gets its
// edited text so far, then the decoded text that follows with its edits,
// which clients apply (§4.8.5). A reader that falls behind what the live
// text holds fails, so that it reconnects and reads the kept text afresh,
// rather than skip what it missed. A finished terminal's url serves its
// output. A browser that opens a terminal's url as a page goes to the
// terminal viewer, if the server has one (TerminalViewerURL).
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != http.MethodGet {
		http.Error(w, "Read with GET", http.StatusMethodNotAllowed)
		return
	}
	s.mu.RLock()
	e := s.lookupEmbedLocked(strings.TrimPrefix(r.URL.Path, livePath))
	if e == nil || !liveKind(e.Kind) || (e.live == nil && !e.Finished) {
		s.mu.RUnlock()
		http.NotFound(w, r)
		return
	}
	live := e.live
	output, _ := e.Owned["output"].(string)
	finished := e.Finished
	liveURL := e.liveURL()
	terminal := e.Kind == kindTerminal
	s.mu.RUnlock()
	header := w.Header()
	// The viewer reads only https, so an http url (one with no TLS in
	// front) is served as text, unless the viewer is http too.
	viewer := s.config.TerminalViewerURL
	if viewer != "" && terminal && (strings.HasPrefix(liveURL, "https:") || strings.HasPrefix(viewer, "http:")) {
		// Readers that fetch it are served the text; a browser opening it as a page, the viewer.
		header.Set("Vary", "Sec-Fetch-Mode")
		if r.Header.Get("Sec-Fetch-Mode") == "navigate" {
			// The url goes in the fragment, which a browser never sends to the viewer's host (§4.8.5).
			header.Set("Location", viewer+"#"+url.Values{"stream": {liveURL}}.Encode())
			header.Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusFound)
			return
		}
	}
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
		chunk, next, changed, ended, behind := live.Read(position)
		if behind {
			// Close the connection without ending the response, which
			// would read as the end of the text.
			panic(http.ErrAbortHandler)
		}
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
func (s *Server) embedTerminalCommand(c *client, r *roomState, body map[string]any, args, replyTo string) (wire.CommandResult, *Error) {
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
func (s *Server) embedAppendCommand(c *client, r *roomState, body map[string]any, args, replyTo string) (wire.CommandResult, *Error) {
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
func (s *Server) liveCommand(c *client, r *roomState, body map[string]any, kind, text, replyTo string) (wire.CommandResult, *Error) {
	if len(asList(body["embeds"])) > 0 {
		return wire.CommandResult{}, invalidParams("/embed-%s takes no embeds: it makes its own", kind)
	}
	if replyTo != "" {
		if target := s.visibleMessageLocked(c.user, replyTo); target != nil && r.RevealsTo(s.rooms.Get(target.RoomID())) {
			return wire.CommandResult{}, &Error{Code: CodeDenied, Message: "A reply cannot quote a message that fewer people can see"}
		}
	}
	if err := s.admitPostLocked(c.user); err != nil {
		return wire.CommandResult{}, err
	}
	s.embedNumber++
	e := s.newWriteLocked(c, fmt.Sprintf("embed_%d", s.embedNumber), kind, "")
	e.post = &pendingPost{user: c.user, roomID: r.ID(), replyTo: replyTo, text: text}
	if kind == kindTerminal {
		e.Title = text
		text = ""
	}
	s.startLiveLocked(e, text)
	c.enqueue(privateNotice(r.ID(), s.liveNotice(e)))
	return e.writeResult(), nil
}

// liveNotice tells the sender of /embed-terminal or /embed-append how to
// write to its URL, and the limits that apply.
func (s *Server) liveNotice(e *embedState) string {
	url := e.writeURL()
	within := formatDuration(s.config.UploadStartTimeout)
	duration := formatDuration(s.config.LiveTextMaxDuration)
	if e.Kind == kindTerminal {
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
		"A backspace (`\\b`) removes the character before it, and a form feed (`\\f`) all the text before it. "+
		"The message appears when the stream starts. At a limit the stream ends, and so does the command feeding it. "+
		"Anyone with this URL can write to it, so keep it to yourself.",
		within, formatBytes(int64(e.live.Keep())), duration, url)
}
