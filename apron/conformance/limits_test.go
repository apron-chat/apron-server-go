package conformance

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"testing"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/server"
)

// TestLimits runs the checks that a server's limits keep from running
// whole against servers with those limits: each is not failed, but skips
// with a warning, after it checked what it could.
func TestLimits(t *testing.T) {
	if testing.Short() {
		t.Skip("posts a burst of 8 MiB")
	}
	reported := []string{"history.shape.page", "core.delivery.slow-reader", "history.bounds.inclusive", "history.bounds.limit"}
	for _, test := range []struct {
		name          string
		maxText, keep int
		warned        []string
	}{
		{name: "small messages", maxText: 4096, warned: []string{"core.delivery.slow-reader"}},
		{name: "short history", keep: 300, warned: []string{"core.delivery.slow-reader", "history.bounds.inclusive"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			chat := &limited{maxText: test.maxText, keep: test.keep}
			chat.server = server.New(chat, server.Config{Messages: chat, History: chat, Logger: slog.New(slog.DiscardHandler)})
			r := &run{config: Config{Server: chat.server}, order: map[orderKey]int64{}, violations: map[string]map[string]bool{}}
			for _, item := range checks {
				if !slices.Contains(reported, item.id) {
					if item.setup {
						r.quietly(t, item)
					}
					continue
				}
				var skipped bool
				t.Run(item.id, func(t *testing.T) {
					defer func() { skipped = t.Skipped() }()
					r.check(t, item)
				})
				if want := slices.Contains(test.warned, item.id); skipped != want {
					t.Errorf("%s: skipped %v, want %v", item.id, skipped, want)
				}
			}
		})
	}
}

// limited is a one-room chat of guests that refuses a text longer than
// maxText, unless 0, and keeps the last keep messages of its log, or all
// of them for 0.
type limited struct {
	server        *server.Server
	maxText, keep int
	clock         server.Clock

	mu     sync.Mutex
	guests int
	log    []apron.Record
	lower  apron.LogID
	latest apron.LogID
}

func (*limited) Hello(context.Context, *server.Conn) apron.ServerInfo {
	return apron.ServerInfo{Auth: []string{"guest"}}
}

func (l *limited) Auth(_ context.Context, _ *server.Conn, p apron.AuthParams, reply server.Reply[apron.AuthResult]) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.guests++
	return reply.Send(apron.AuthResult{You: &apron.CurrentUser{UserID: fmt.Sprintf("guest_%d", l.guests), Name: p.Name}})
}

func (l *limited) Message(_ context.Context, c *server.Conn, p apron.MessageParams, reply server.Reply[apron.MessageResult]) error {
	if p.RoomID != "" && p.RoomID != "general" {
		return apron.InvalidParams("There is only one room, general")
	}
	if l.maxText > 0 && len(p.Body.Text) > l.maxText {
		return apron.TooLarge(fmt.Sprintf("A message is at most %d bytes", l.maxText))
	}
	if p.Body.Empty() {
		return reply.Send(apron.MessageResult{})
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if p.ReplyTo != nil && !slices.ContainsFunc(l.log, func(r apron.Record) bool { return r.LogID == p.ReplyTo.MessageID }) {
		return apron.InvalidParams("reply_to names no message")
	}
	id := l.clock.Next()
	m := apron.Message{MessageID: id, LogID: id, RoomID: "general", From: *c.User(), Body: p.Body, ReplyTo: p.ReplyTo}
	frame, err := apron.Encode(m)
	if err != nil {
		return err
	}
	l.log = append(l.log, apron.Record{LogID: id, Message: &m})
	if l.lower == 0 {
		l.lower = id
	}
	if l.keep > 0 && len(l.log) > l.keep {
		l.log = slices.Delete(l.log, 0, len(l.log)-l.keep)
		l.lower = l.log[0].LogID
	}
	l.latest = id
	l.server.Broadcast(frame, nil)
	return reply.Send(apron.MessageResult{MessageID: id})
}

func (l *limited) History(_ context.Context, _ *server.Conn, p apron.HistoryParams, reply server.Reply[apron.HistoryResult]) error {
	if p.RoomID != "" && p.RoomID != "general" {
		return apron.InvalidParams("There is only one room, general")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return reply.Send(server.Page(server.Log{Records: l.log, Lower: l.lower, Latest: l.latest}, p, 200))
}
