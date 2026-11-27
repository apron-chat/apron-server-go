// Package benchchat is what this module's benchmarks run a Server with: a
// backend shaped as examples/minimal's, one that keeps its state under one
// lock and sends while it holds it, with rooms, history, profiles,
// status, reactions, and activity; and a Transport in memory that counts
// what the Server writes, for benchmarks of many connections without a
// network. It is for tests alone.
package benchchat

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/server"
)

// Chat is a backend of rooms that guests join as they sign in: auth's
// agent names the guest's room, "general" when it is empty.
type Chat struct {
	Server *server.Server
	// MemberLists, if set, has the backend send a room's frames to the
	// room's members from its own lists, with Conn.Send, rather than with
	// Server.Broadcast and a filter, which walks every connection.
	MemberLists bool
	// RawHistory, if set, has the backend keep each logged record encoded
	// too, and answer history with them as they are (HistoryResult.Raw),
	// as a backend that stores its log encoded does.
	RawHistory bool
	// ReserveHistory, if set, has the backend answer history in a place
	// it reserves under its lock (Reply.Reserve), encoding the page once
	// it lets go of it.
	ReserveHistory bool
	// LockHeld is how long, in nanoseconds, history requests held the
	// backend's lock, in all.
	LockHeld atomic.Int64

	clock server.Clock

	mu     sync.Mutex
	guests int
	rooms  map[string]*room
	log    []apron.Record
	// bare is log with only the log_ids of its records, and raw their
	// encodings, for RawHistory.
	bare  []apron.Record
	raw   []jsontext.Value
	lower apron.LogID
}

type room struct {
	members map[*server.Conn]struct{}
	list    []*server.Conn
}

// member is a connection's own state, its Data.
type member struct {
	room string
	user apron.User
}

// New returns a Chat and its Server, with config's limits, and every
// handler the Chat has.
func New(config server.Config) *Chat {
	c := &Chat{rooms: map[string]*room{}}
	config.Messages, config.Profiles, config.History = c, c, c
	config.Status, config.Reactions, config.Activity = c, c, c
	config.OnConnect, config.OnDisconnect = c.connect, c.disconnect
	config.Logger = slog.New(slog.DiscardHandler)
	c.Server = server.New(c, config)
	return c
}

// Hello is the server frame.
func (*Chat) Hello(context.Context, *server.Conn) apron.ServerInfo {
	return apron.ServerInfo{Auth: []string{"guest"}, Agent: "benchchat/1", Status: []string{"online", "away"}}
}

// connect adds a Local connection to the general room, as Auth does a
// network one to its own.
func (c *Chat) connect(_ context.Context, conn *server.Conn) error {
	if !conn.Local() {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.join(conn, "general", *conn.User())
	return nil
}

// Auth signs a guest in, into the room its agent names.
func (c *Chat) Auth(_ context.Context, conn *server.Conn, p apron.AuthParams, reply server.Reply[apron.AuthResult]) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.guests++
	user := apron.User{UserID: "guest_" + strconv.Itoa(c.guests), Name: p.Name}
	if user.Name == "" {
		user.Name = user.UserID
	}
	roomID := p.Agent
	if roomID == "" {
		roomID = "general"
	}
	c.join(conn, roomID, user)
	return reply.Send(apron.AuthResult{You: &apron.CurrentUser{User: user, Cleared: []string{"status"}}})
}

func (c *Chat) join(conn *server.Conn, roomID string, user apron.User) {
	r := c.rooms[roomID]
	if r == nil {
		r = &room{members: map[*server.Conn]struct{}{}}
		c.rooms[roomID] = r
	}
	r.members[conn] = struct{}{}
	r.list = append(r.list, conn)
	conn.SetData(&member{room: roomID, user: user})
}

func (c *Chat) disconnect(conn *server.Conn) {
	m, _ := conn.Data().(*member)
	if m == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.rooms[m.room]; r != nil {
		delete(r.members, conn)
		for i, other := range r.list {
			if other == conn {
				r.list = append(r.list[:i], r.list[i+1:]...)
				break
			}
		}
	}
}

// deliver sends a frame to a room's members, but except, with c.mu held.
func (c *Chat) deliver(roomID string, frame apron.Frame, except *server.Conn) {
	if c.MemberLists {
		if r := c.rooms[roomID]; r != nil {
			for _, conn := range r.list {
				if conn != except {
					conn.Send(frame)
				}
			}
		}
		return
	}
	c.Server.Broadcast(frame, func(conn *server.Conn) bool {
		m, _ := conn.Data().(*member)
		return conn != except && m != nil && m.room == roomID
	})
}

func roomOf(conn *server.Conn) *member {
	m, _ := conn.Data().(*member)
	return m
}

// Message posts a message to the sender's room.
func (c *Chat) Message(_ context.Context, conn *server.Conn, p apron.MessageParams, reply server.Reply[apron.MessageResult]) error {
	if p.Body.Empty() {
		return apron.InvalidParams("A new message needs text or embeds")
	}
	m := roomOf(conn)
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.clock.Next()
	message := apron.Message{MessageID: id, LogID: id, RoomID: m.room, From: m.user, Body: p.Body, ReplyTo: p.ReplyTo}
	frame, err := apron.Encode(message)
	if err != nil {
		return err
	}
	c.logMessage(message)
	c.deliver(m.room, frame, nil)
	return reply.Send(apron.MessageResult{MessageID: id})
}

// History pages through the log, every room's.
func (c *Chat) History(_ context.Context, _ *server.Conn, p apron.HistoryParams, reply server.Reply[apron.HistoryResult]) error {
	c.mu.Lock()
	locked := time.Now()
	held := func() { c.LockHeld.Add(int64(time.Since(locked))) }
	if c.ReserveHistory {
		// The page is read under the lock, and its place fixed, and it is
		// encoded and sent once the lock is let go.
		page := server.Page(server.Log{Records: c.log, Lower: c.lower, Latest: c.clock.Last()}, p, 100)
		err := reply.Reserve()
		held()
		c.mu.Unlock()
		if err != nil {
			return err
		}
		return reply.Send(page)
	}
	defer c.mu.Unlock()
	defer held()
	if !c.RawHistory {
		return reply.Send(server.Page(server.Log{Records: c.log, Lower: c.lower, Latest: c.clock.Last()}, p, 100))
	}
	// Page picks the window of the bare log, and its records are the raw
	// ones of that window.
	result := server.Page(server.Log{Records: c.bare, Lower: c.lower, Latest: c.clock.Last()}, p, 100)
	result.Raw = &apron.RawRecords{}
	if result.FirstLogID != 0 {
		start, _ := slices.BinarySearchFunc(c.bare, result.FirstLogID, func(r apron.Record, id apron.LogID) int { return cmp.Compare(r.LogID, id) })
		end, _ := slices.BinarySearchFunc(c.bare, result.LastLogID, func(r apron.Record, id apron.LogID) int { return cmp.Compare(r.LogID, id) })
		result.Raw.Messages = c.raw[start : end+1]
	}
	return reply.Send(result)
}

// Me renames the user, and tells the room.
func (c *Chat) Me(_ context.Context, conn *server.Conn, p apron.MeParams, reply server.Reply[apron.CurrentUser]) error {
	m := roomOf(conn)
	c.mu.Lock()
	defer c.mu.Unlock()
	old := m.user
	you := server.ApplyMe(old, p)
	m.user = you
	c.publish(server.UserChange{Old: old, New: you, Except: conn}, m.room)
	return reply.Send(you.Complete(old))
}

// publish tells a change of a user to those who share its room, with
// c.mu held: from the room's member list with MemberLists, and else with a
// filter of every connection. The user has one connection, whose own view
// a status change leaves as it is, and a rename's Except is.
func (c *Chat) publish(change server.UserChange, roomID string) {
	if c.MemberLists {
		var members []*server.Conn
		if r := c.rooms[roomID]; r != nil {
			members = r.list
		}
		c.Server.PublishUserTo(change, nil, slices.Values(members))
		return
	}
	c.Server.PublishUser(change, c.shares(roomID))
}

func (c *Chat) shares(roomID string) func(*server.Conn) bool {
	return func(conn *server.Conn) bool {
		m, _ := conn.Data().(*member)
		return m != nil && m.room == roomID
	}
}

// Status shows the user away while the connection is idle, and online
// when it is attended, as others see it (§4.5.2).
func (c *Chat) Status(_ context.Context, conn *server.Conn, p apron.StatusParams, _ server.Reply[apron.OK]) error {
	if p.Idle == nil {
		return nil
	}
	m := roomOf(conn)
	shown := "online"
	if !p.Idle.Attended && p.Idle.Seconds > 0 {
		shown = "away"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	old, cur := m.user, m.user
	old.Status = otherStatus(shown)
	cur.Status = shown
	c.publish(server.UserChange{Old: m.user, New: m.user, OldShown: old, NewShown: cur}, m.room)
	return nil
}

func otherStatus(s string) string {
	if s == "away" {
		return "online"
	}
	return "away"
}

// React sets the user's reactions, and tells the room.
func (c *Chat) React(_ context.Context, conn *server.Conn, p apron.ReactParams, _ server.Reply[apron.OK]) error {
	m := roomOf(conn)
	c.mu.Lock()
	defer c.mu.Unlock()
	rec := apron.Reactions{LogID: c.clock.Next(), MessageID: p.MessageID, RoomID: m.room, Reactions: []apron.Reaction{{From: m.user, Emojis: p.Emojis}}}
	frame, err := apron.Encode(rec)
	if err != nil {
		return err
	}
	c.deliver(m.room, frame, nil)
	return nil
}

// Activity relays typing to the room.
func (c *Chat) Activity(_ context.Context, conn *server.Conn, p apron.ActivityParams) {
	m := roomOf(conn)
	frame, err := apron.Encode(apron.Activity{From: m.user, RoomID: m.room, Typing: p.Typing})
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deliver(m.room, frame, conn)
}

// Sink counts the frames written to the connections that share it, and
// wakes a benchmark waiting for a count.
type Sink struct {
	total   atomic.Int64
	target  atomic.Int64
	reached chan struct{}
}

// NewSink returns a Sink with nothing counted.
func NewSink() *Sink { return &Sink{reached: make(chan struct{}, 1)} }

func (s *Sink) add() {
	if s.total.Add(1) >= s.target.Load() {
		select {
		case s.reached <- struct{}{}:
		default:
		}
	}
}

// Total is how many frames were written.
func (s *Sink) Total() int64 { return s.total.Load() }

// Wait waits until n frames in all were written.
func (s *Sink) Wait(n int64) {
	s.target.Store(n)
	for s.total.Load() < n {
		<-s.reached
	}
}

// Transport is a connection in memory: Read returns what the benchmark
// sends (Send), and Write counts each frame in its Sink, and keeps the
// last one when Keep is set.
type Transport struct {
	sink   *Sink
	in     chan []byte
	closed chan struct{}
	once   sync.Once
	// Stall, if set, makes every Write wait until the transport is
	// closed: a client that stopped reading.
	Stall atomic.Bool
	// OnWrite, if set, sees each frame written.
	OnWrite func(frame []byte)
}

// NewTransport returns a transport that counts in sink.
func NewTransport(sink *Sink) *Transport {
	return &Transport{sink: sink, in: make(chan []byte, 64), closed: make(chan struct{})}
}

// Send hands the Server a frame from the client.
func (t *Transport) Send(frame []byte) {
	select {
	case t.in <- frame:
	case <-t.closed:
	}
}

func (t *Transport) Read(ctx context.Context) ([]byte, error) {
	select {
	case frame := <-t.in:
		return frame, nil
	case <-t.closed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (t *Transport) Write(_ context.Context, frame []byte) error {
	if t.Stall.Load() {
		<-t.closed
		return io.ErrClosedPipe
	}
	if t.OnWrite != nil {
		t.OnWrite(frame)
	}
	t.sink.add()
	return nil
}

func (*Transport) Ping(context.Context) error { return nil }

func (t *Transport) Close() error {
	t.once.Do(func() { close(t.closed) })
	return nil
}

// Closed is closed once the Server closed the transport.
func (t *Transport) Closed() <-chan struct{} { return t.closed }

// Open opens n connections to the Chat's Server in memory, each signed in
// as a guest of the room roomOf names for its index, and waits until they
// are: each was written its server frame and auth result, which sink
// counts. stall, if set, says which are clients that stop reading once
// signed in.
func (c *Chat) Open(n int, sink *Sink, roomOf func(i int) string) []*Transport {
	ts := make([]*Transport, n)
	before := sink.Total()
	for i := range ts {
		t := NewTransport(sink)
		ts[i] = t
		go c.Server.ServeTransport(context.Background(), t, nil, nil)
		agent := ""
		if roomOf != nil {
			agent = roomOf(i)
		}
		auth, _ := apron.Marshal(map[string]any{"method": "auth", "id": "a", "params": apron.AuthParams{Scheme: "guest", Agent: agent}})
		t.Send(auth)
	}
	sink.Wait(before + 2*int64(n))
	return ts
}

// CloseAll closes the transports, and waits until the Server has
// forgotten their connections.
func (c *Chat) CloseAll(ts []*Transport) {
	for _, t := range ts {
		_ = t.Close()
	}
	_ = c.Server.Shutdown(context.Background())
}

// Request returns a request frame, such as a message's, with id.
func Request(method, id string, params any) []byte {
	frame := map[string]any{"method": method, "id": id}
	if params != nil {
		frame["params"] = params
	}
	raw, err := apron.Marshal(frame)
	if err != nil {
		panic(err)
	}
	return raw
}

// Text is a message body's params, raw, for a request frame built by hand.
func Text(s string) jsontext.Value {
	raw, _ := apron.Marshal(apron.MessageParams{Body: &apron.Body{Text: s}})
	return raw
}

// Seed logs n messages in roomID, as if posted, without sending them.
func (c *Chat) Seed(roomID string, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range n {
		id := c.clock.Next()
		if c.lower == 0 {
			c.lower = id
		}
		c.logMessage(Message(id, roomID, i))
	}
}

// logMessage logs a message, with c.mu held.
func (c *Chat) logMessage(m apron.Message) {
	if c.lower == 0 {
		c.lower = m.LogID
	}
	c.log = append(c.log, apron.Record{LogID: m.LogID, Message: &m})
	raw, err := apron.Marshal(m)
	if err != nil {
		panic(err)
	}
	c.bare = append(c.bare, apron.Record{LogID: m.LogID})
	c.raw = append(c.raw, raw)
}

// Message is the i-th message of a seeded log, with message_id and
// log_id id: a line of chat of a usual length, from one of 100 users.
func Message(id apron.LogID, roomID string, i int) apron.Message {
	user := "guest_" + strconv.Itoa(i%100)
	return apron.Message{
		MessageID: id, LogID: id, RoomID: roomID,
		From: apron.User{UserID: user, Name: user},
		Body: &apron.Body{Text: "Hello, everyone: this is line " + strconv.Itoa(i) + " of chat, of a usual length."},
	}
}
