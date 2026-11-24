package apron

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/apron-chat/apron-server-go/apron/internal/roomlog"
	"github.com/apron-chat/apron-server-go/wire"
)

// EventKind is the kind of record an Event carries (PROTOCOL.md §2.3).
type EventKind string

const (
	// EventRoom is a room's record: its creation or a room_set (§4.2.4).
	EventRoom EventKind = "room"
	// EventMessage is a message snapshot (§3.5): new, edited, moved, or a
	// deletion's tombstone (§4.4). DecodeMessage reads it.
	EventMessage EventKind = "message"
	// EventReactions is a message's reactions (§4.7).
	EventReactions EventKind = "reactions"
	// EventMembership is a join or a leave (§4.2.2).
	EventMembership EventKind = "membership"
)

var eventKinds = map[roomlog.Kind]EventKind{
	roomlog.KindRoom:       EventRoom,
	roomlog.KindMessage:    EventMessage,
	roomlog.KindReactions:  EventReactions,
	roomlog.KindMembership: EventMembership,
}

// Event is one logged record: the server's log is the stream of what
// changes in its rooms, which extensions follow with OnEvent, OnEventFrom,
// and Follow.
type Event struct {
	// Kind says what the record is.
	Kind EventKind
	// LogID orders the events (§2.1): each is greater than the last.
	LogID int64
	// Rooms are the rooms whose logs hold the record: one, or both rooms
	// of a move (§4.3.1).
	Rooms []string
	// Record is the record as clients receive it, in history and as it is
	// broadcast, which PROTOCOL.md defines. A deletion redacts the
	// message's earlier records (§4.4): an event read after it carries the
	// redacted record.
	Record jsontext.Value
}

func eventOf(r *roomlog.Record) Event {
	return Event{Kind: eventKinds[r.Kind], LogID: r.ID, Rooms: slices.Clone(r.Rooms), Record: slices.Clone(r.Raw)}
}

// LastLogID is the log_id of the latest record, 0 for none.
func (h *Host) LastLogID() int64 {
	h.s.mu.RLock()
	defer h.s.mu.RUnlock()
	return h.s.log.Last()
}

// OnEvent calls fn with each record logged from now on, in order, on one
// goroutine of its own. fn delays the events after it, not the server; a
// slow fn falls behind and catches up. Events logged before a restart are
// not sent again: an extension that must see every event uses Follow.
//
// Events carry every room's records, private rooms' included: an extension
// that shows them to people checks Host.Visible or Host.Public first.
//
// A test waits for the followers of every kind with aprontest's
// Server.Settle, which returns once they have handled every event logged.
func (r *Registrar) OnEvent(fn func(ctx context.Context, e Event)) {
	r.follow("OnEvent", r.LastLogID(), func(ctx context.Context, e Event) error {
		fn(ctx, e)
		return nil
	}, nil)
}

// OnEventFrom calls fn with each record logged after the log_id after, in
// order, then each one logged later, on one goroutine of its own: 0 sends
// the whole log first, as an index kept in memory needs at each start. When
// fn returns an error, it is logged, and fn is called with the same event
// again until it succeeds, returns an error from SkipEvent, or the server
// shuts down: the first retry comes a second later, and each delay after
// it doubles, up to a minute. The events after it wait meanwhile.
func (r *Registrar) OnEventFrom(after int64, fn func(ctx context.Context, e Event) error) {
	r.follow("OnEventFrom", after, fn, nil)
}

// Follow calls fn with each record logged, in order, on one goroutine of
// its own, and keeps the log_id of the last one it handled in the store, so
// that it sees every event at least once across restarts, as a bridge
// needs. It starts from the latest record the first time the extension
// follows the log. Errors are retried as with OnEventFrom. An extension
// follows the log at most once.
func (r *Registrar) Follow(fn func(ctx context.Context, e Event) error) {
	if r.following {
		r.errs = append(r.errs, errors.New("an extension follows the log once"))
		return
	}
	r.following = true
	kv := r.Store()
	var cursor int64
	if found, err := kv.Get(followCursorKey, &cursor); err != nil || !found {
		cursor = r.LastLogID()
		if err := kv.put(followCursorKey, cursor); err != nil {
			r.errs = append(r.errs, err)
			return
		}
	}
	r.follow("Follow", cursor, fn, func(e Event) error { return kv.put(followCursorKey, e.LogID) })
}

// followCursorKey keeps Follow's cursor in the extension's KV.
const followCursorKey = reservedKeyPrefix + "follow"

// SkipEvent wraps an error that handling an event can never get past, such
// as a record the extension cannot use: OnEventFrom and Follow log it and
// go on to the next event instead of retrying.
func SkipEvent(err error) error { return skipEvent{err} }

type skipEvent struct{ error }

func (e skipEvent) Unwrap() error { return e.error }

// eventBatch bounds the events a follower takes from the log at once.
const eventBatch = 256

// follow delivers the log after a log_id on a goroutine, retrying failures;
// handled, if set, records each event handled.
func (r *Registrar) follow(what string, after int64, deliver func(ctx context.Context, e Event) error, handled func(Event) error) {
	if deliver == nil {
		r.errs = append(r.errs, fmt.Errorf("%s needs a function", what))
		return
	}
	if r.done.Load() {
		panic(fmt.Sprintf("apron: extension %s called %s after Init", r.name, what))
	}
	h, s := r.Host, r.s
	position := s.followers.add(fmt.Sprintf("extension %s's %s", r.name, what), after)
	h.Go(func(ctx context.Context) {
		cursor := after
		for {
			s.mu.RLock()
			records, changed := s.log.After(cursor, eventBatch)
			batch := make([]Event, 0, len(records))
			for _, r := range records {
				batch = append(batch, eventOf(r))
			}
			s.mu.RUnlock()
			if len(batch) == 0 {
				select {
				case <-changed:
					continue
				case <-ctx.Done():
					return
				}
			}
			for _, e := range batch {
				for delay := time.Second; ; delay = min(2*delay, time.Minute) {
					err := deliver(ctx, e)
					if err == nil {
						break
					}
					if _, skip := errors.AsType[skipEvent](err); skip {
						h.logger.Warn("skipping an event", "log_id", e.LogID, "error", err)
						break
					}
					h.logger.Error("handling an event failed; retrying", "log_id", e.LogID, "error", err, "retry_in", delay)
					select {
					case <-time.After(delay):
					case <-ctx.Done():
						return
					}
				}
				cursor = e.LogID
				if handled != nil {
					if err := handled(e); err != nil {
						return // the store is closed
					}
				}
				s.followers.advance(position, cursor)
			}
		}
	})
}

// followers tracks how far each follower of the log (follow) has handled
// it, for settle. It has its own lock, which no other is taken under.
type followers struct {
	mu   sync.Mutex
	list []*follower
	// changed is closed, and replaced, when a follower moves on.
	changed chan struct{}
}

// follower is a follower's place: the log_id of the last event it handled
// or skipped, or that it started after. Guarded by followers.mu.
type follower struct {
	name string
	at   int64
}

func (f *followers) add(name string, after int64) *follower {
	f.mu.Lock()
	defer f.mu.Unlock()
	position := &follower{name: name, at: after}
	f.list = append(f.list, position)
	return position
}

func (f *followers) advance(position *follower, logID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	position.at = logID
	if f.changed != nil {
		close(f.changed)
		f.changed = nil
	}
}

// behind returns a follower that has not handled logID yet, if any, and a
// channel that is closed when a follower moves on.
func (f *followers) behind(logID int64) (*follower, <-chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, position := range f.list {
		if position.at < logID {
			if f.changed == nil {
				f.changed = make(chan struct{})
			}
			return position, f.changed
		}
	}
	return nil, nil
}

// settle waits until every follower of the log has handled every event
// logged before it returns: those logged when it is called, and those
// logged meanwhile, such as by a follower's bot as it handled one, until
// the log is quiet. A follower that is retrying an event holds it up; what
// a follower does on goroutines of its own, it does not wait for.
func (s *Server) settle(ctx context.Context) error {
	last := func() int64 {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.log.Last()
	}
	for target := last(); ; {
		for {
			position, changed := s.followers.behind(target)
			if position == nil {
				break
			}
			select {
			case <-changed:
			case <-ctx.Done():
				s.followers.mu.Lock()
				at := position.at
				s.followers.mu.Unlock()
				return fmt.Errorf("apron: %s has handled the log up to log_id %d of %d: %w", position.name, at, target, context.Cause(ctx))
			}
		}
		now := last()
		if now == target {
			return nil
		}
		target = now
	}
}

// Message is a message record, decoded (PROTOCOL.md §3.5), as extensions
// see messages: one snapshot of a message, with its message_id a string,
// as Draft, CommandCall, and ReplyTo take one, and its body's text,
// format, and mentions in fields of their own. Code in package wire's
// terms converts with MessageOf and Wire.
type Message struct {
	ID     string
	LogID  int64
	RoomID string
	// PrevLogID is the message's previous snapshot, 0 for a new message;
	// PrevRoomID is set when the snapshot moved it from that room.
	PrevLogID  int64
	PrevRoomID string
	// From is the author: a user, or a system identity such as ~room.
	From Sender
	// Deleted marks a deletion's tombstone, which has no body.
	Deleted bool
	// Text and Format are the body's: Format is "plain" or "markdown".
	Text, Format string
	// ReplyTo is the message_id it replies to, if any.
	ReplyTo string
	// Mentions are the user_ids its body mentions (§3.5.5).
	Mentions []string
	// Body is the whole body, and Ext the message's ext (§4.12).
	Body map[string]any
	Ext  map[string]jsontext.Value
}

// Sender is who a record is from.
type Sender struct {
	UserID string
	Name   string
}

// New reports a message's first snapshot.
func (m Message) New() bool { return m.PrevLogID == 0 }

// MentionsUser reports whether the message mentions userID.
func (m Message) MentionsUser(userID string) bool { return slices.Contains(m.Mentions, userID) }

// DecodeMessage decodes the record of an EventMessage.
func DecodeMessage(e Event) (Message, bool) {
	if e.Kind != EventMessage {
		return Message{}, false
	}
	return decodeMessageRecord(e.Record)
}

// decodeMessageRecord decodes a message record, as history and broadcasts
// carry it.
func decodeMessageRecord(raw []byte) (Message, bool) {
	snapshot := roomlog.DecodeMessage(raw)
	if snapshot == nil {
		return Message{}, false
	}
	id := func(value string) int64 {
		n, _ := strconv.ParseInt(value, 10, 64)
		return n
	}
	m := Message{
		ID: snapshot.MessageID, LogID: id(snapshot.LogID), RoomID: snapshot.RoomID,
		PrevLogID: id(snapshot.PrevLogID), PrevRoomID: snapshot.PrevRoomID,
		From:    Sender{UserID: snapshot.From.UserID, Name: snapshot.From.Name},
		Deleted: snapshot.Deleted, ReplyTo: snapshot.ReplyID(), Ext: snapshot.Ext,
	}
	if body := snapshot.Body; body != nil {
		m.Body = bodyObject(&body.Body)
		m.Text, m.Format = body.Text, body.Format
		if m.Format == "" {
			m.Format = wire.Plain
		}
		m.Mentions = body.Mentions
	}
	return m, true
}
