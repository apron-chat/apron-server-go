package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/client"
)

// history prints a room's latest messages (§4.3).
func history(ctx context.Context, c *cli, args []string) error {
	flags := c.flags("history")
	room := flags.String("room", "", "the room_id to read")
	limit := flags.Int("limit", 20, "how many of the latest messages to print")
	page := flags.Int("page", 100, "how many changes of the log to ask for in each request")
	asJSON := flags.Bool("json", false, "print each message's snapshot as a line of JSON")
	if err := parse(flags, args, 0, 0); err != nil {
		return err
	}
	if *room == "" {
		flags.Usage()
		return errUsage
	}
	conn, err := c.connect(ctx, true)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := needs(conn, "history"); err != nil {
		return err
	}
	messages, _, err := latest(ctx, conn, *room, *limit, *page)
	if err != nil {
		return err
	}
	p := printer{out: c.stdout, json: *asJSON, room: *room}
	for _, m := range messages {
		if err := p.print(m); err != nil {
			return err
		}
	}
	return nil
}

// latest reads the room's log back from the newest change, a page of
// pageSize at a time, until it holds limit messages posted in what it has
// read, or the whole log, and returns them by replay (§4.3.5), oldest
// first, with H, the room's latest_log_id as the first page read it.
func latest(ctx context.Context, conn *client.Conn, room string, limit, pageSize int) ([]apron.Message, apron.LogID, error) {
	var state apron.Replay
	var h, oldest apron.LogID
	posted := func() []apron.Message {
		// The messages posted at or after the oldest change read: an older
		// one's later snapshot is not its place in the room.
		return slices.DeleteFunc(state.Messages(room), func(m apron.Message) bool { return m.MessageID < oldest })
	}
	first := true
	for page, err := range conn.HistoryPages(ctx, apron.HistoryParams{RoomID: room, Limit: pageSize}) {
		if err != nil {
			return nil, 0, fmt.Errorf("reading the history of %s: %s", roomName(room), describe(err))
		}
		if first {
			h, first = page.LatestLogID, false
		}
		state.ApplyHistory(page)
		if page.FirstLogID != 0 {
			oldest = page.FirstLogID
		}
		if len(posted()) >= limit {
			break
		}
	}
	messages := posted()
	if len(messages) > limit {
		messages = messages[len(messages)-limit:]
	}
	return messages, h, nil
}

// tail prints the latest messages, then the new ones as they come, until
// the connection ends or ctx is done, recovering as §4.3.5 says: the live
// messages that arrive while it reads the history wait, and those the
// history holds, at or below its latest_log_id, are not printed again.
func tail(ctx context.Context, c *cli, args []string) error {
	flags := c.flags("tail")
	room := flags.String("room", "", "the room_id to follow; empty is every room the connection receives")
	limit := flags.Int("limit", 10, "how many of the latest messages to print first")
	asJSON := flags.Bool("json", false, "print each message's snapshot as a line of JSON")
	if err := parse(flags, args, 0, 0); err != nil {
		return err
	}
	live := make(chan apron.Message, 4096)
	stopped := make(chan struct{})
	defer close(stopped)
	conn, err := c.connect(ctx, true, client.OnFrame(func(f apron.Frame) {
		var frame struct {
			Method string        `json:"method"`
			Params apron.Message `json:"params"`
		}
		if json.Unmarshal(f, &frame) != nil || frame.Method != "message" {
			return
		}
		select {
		case live <- frame.Params:
		case <-stopped:
		}
	}))
	if err != nil {
		return err
	}
	defer conn.Close()
	if *room != "" && conn.Hello().Has("rooms") {
		listed, err := conn.RoomList(ctx, apron.RoomListParams{RoomID: *room})
		if err == nil && len(listed.Joined) == 0 {
			fmt.Fprintf(c.stderr, "aproncli: you have not joined %s, so its new messages do not come to you: run aproncli join %s\n", *room, *room)
		}
	}
	p := printer{out: c.stdout, json: *asJSON, room: *room}
	// Without history, nothing has been missed: every message comes live.
	var h apron.LogID
	if conn.Hello().Has("history") && *limit > 0 {
		messages, latestLogID, err := latest(ctx, conn, *room, *limit, min(*limit*4, 100))
		if err != nil {
			return err
		}
		for _, m := range messages {
			if err := p.print(m); err != nil {
				return err
			}
		}
		h = latestLogID
	}
	for {
		select {
		case m := <-live:
			// A notice has no log_id, and is never in history.
			if m.MessageID != 0 && m.LogID <= h || *room != "" && m.RoomID != *room {
				continue
			}
			if err := p.print(m); err != nil {
				return err
			}
		case <-conn.Done():
			return fmt.Errorf("the connection ended: %w", conn.Err())
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// printer prints messages: as lines for people, or as JSON lines.
type printer struct {
	out  io.Writer
	json bool
	// room is the room printed, or "" for any, which each line then names.
	room string
}

// print prints a message snapshot or notice (§3.5): a new message, an
// edit, or a tombstone.
func (p printer) print(m apron.Message) error {
	if p.json {
		raw, err := apron.Marshal(m)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(p.out, "%s\n", raw)
		return err
	}
	var line strings.Builder
	if m.MessageID != 0 {
		line.WriteString(time.UnixMilli(int64(m.MessageID)).Format("2006-01-02 15:04 "))
	} else {
		line.WriteString("* ")
	}
	if p.room == "" && m.RoomID != "" {
		line.WriteString("#" + m.RoomID + " ")
	}
	name := m.From.Name
	if name == "" {
		name = m.From.UserID
	}
	line.WriteString(name)
	if m.ReplyTo != nil {
		fmt.Fprintf(&line, " (reply to %s)", m.ReplyTo.MessageID)
	}
	line.WriteString(": ")
	switch {
	case m.Deleted:
		line.WriteString("(deleted)")
	case !m.Body.Empty():
		line.WriteString(m.Body.Text)
		for _, embed := range m.Body.Embeds {
			var e struct {
				Kind  string `json:"kind"`
				Title string `json:"title"`
				URL   string `json:"url"`
			}
			if json.Unmarshal(embed, &e) == nil {
				line.WriteString(" [" + strings.Join(slices.DeleteFunc([]string{e.Kind, e.Title, e.URL}, func(s string) bool { return s == "" }), " ") + "]")
			}
		}
		if m.LogID != m.MessageID && m.MessageID != 0 {
			line.WriteString(" (edited)")
		}
	}
	_, err := fmt.Fprintln(p.out, line.String())
	return err
}

// roomName names a room for an error, or every room for "".
func roomName(room string) string {
	if room == "" {
		return "every room"
	}
	return room
}
