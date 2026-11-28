package aprond

// Benchmarks of the server's costs under conditions worth profiling: fan-out
// to one large room and to many small ones, storms of small notifications,
// history paging under writes, sign-in storms, slow consumers, and an
// extension that follows every event. They drive the server through package
// apron/server as clients do, over in-memory transports (benchTransport)
// that count what they are sent and drop it, so that what they measure is
// the server and the engine, not a network or WebSocket framing.
//
// Run them with, for example,
//
//	go test ./aprond -run '^$' -bench . -benchmem
//	go test ./aprond -run '^$' -bench 'RoomFanout/members=1000' -benchmem -cpuprofile cpu.out -memprofile mem.out
//
// Each reports allocs/op and B/op per operation, and where an operation
// fans out, frames/op and allocs per delivered frame.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/aprond/internal/presence"
	"github.com/apron-chat/apron-server-go/internal/auth/email"
	"github.com/apron-chat/apron-server-go/internal/authn"
	"github.com/apron-chat/apron-server-go/store"
)

// benchTransport is a server.Transport in memory: Read returns the frames a
// benchmark sends, and Write counts the frames the server writes, passes
// replies to the benchmark, and drops the rest.
type benchTransport struct {
	in   chan []byte
	done chan struct{}
	once sync.Once
	// replies receives the frames that answer a request (with an id), nil
	// to drop them too.
	replies chan []byte
	// counted counts the frames whose start is countPrefix, such as message
	// notifications, in a counter shared by many transports.
	counted     *atomic.Int64
	countPrefix []byte
	// stall, when closed... a stalled transport blocks every Write until
	// done, as a client that stopped reading.
	stall atomic.Bool
}

func newBenchTransport(replies bool) *benchTransport {
	t := &benchTransport{in: make(chan []byte, 16), done: make(chan struct{})}
	if replies {
		t.replies = make(chan []byte, 64)
	}
	return t
}

func (t *benchTransport) Read(ctx context.Context) ([]byte, error) {
	select {
	case frame := <-t.in:
		return frame, nil
	case <-t.done:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

var (
	replyPrefix = []byte(`{"id":`)
	errorPrefix = []byte(`{"error":`)
)

func (t *benchTransport) Write(ctx context.Context, frame []byte) error {
	if t.stall.Load() {
		select {
		case <-t.done:
			return io.EOF
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if t.counted != nil && bytes.HasPrefix(frame, t.countPrefix) {
		t.counted.Add(1)
	}
	if t.replies != nil && (bytes.HasPrefix(frame, replyPrefix) || bytes.HasPrefix(frame, errorPrefix)) {
		select {
		case t.replies <- frame:
		case <-t.done:
			return io.EOF
		}
	}
	return nil
}

func (*benchTransport) Ping(context.Context) error { return nil }

func (t *benchTransport) Close() error {
	t.once.Do(func() { close(t.done) })
	return nil
}

// benchClient is one connection of a benchmark, which sends one request at
// a time and waits for its reply.
type benchClient struct {
	t        *benchTransport
	requests int
	userID   string
	token    string
}

// benchServer is a server for a benchmark, with its connections.
type benchServer struct {
	tb      testing.TB
	s       *Server
	ctx     context.Context
	clients []*benchClient
}

func newBenchServer(tb testing.TB, config Config) *benchServer {
	tb.Helper()
	if config.Logger == nil {
		// Warnings, such as of the slow consumers dropped, would break up
		// the benchmarks' lines.
		config.Logger = slog.New(slog.DiscardHandler)
	}
	s, err := Open(config)
	if err != nil {
		tb.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	bs := &benchServer{tb: tb, s: s, ctx: ctx}
	tb.Cleanup(func() {
		cancel()
		shutdown, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if err := s.Shutdown(shutdown); err != nil {
			tb.Errorf("shutdown: %v", err)
		}
	})
	return bs
}

// connect opens a connection that is not signed in; with replies false, it
// reads none of its replies, and only counts frames.
func (bs *benchServer) connect(replies bool) *benchClient {
	t := newBenchTransport(replies)
	go bs.s.srv.ServeTransport(bs.ctx, t, nil, nil)
	c := &benchClient{t: t}
	bs.clients = append(bs.clients, c)
	return c
}

// guest opens a connection signed in as a new guest, who joins general.
func (bs *benchServer) guest() *benchClient {
	c := bs.connect(true)
	result := c.call(bs.tb, "auth", map[string]any{"scheme": "guest"})
	c.userID = stringField(result, "user_id")
	c.token = stringField(result, "token")
	return c
}

// member makes an account, who joins general and the rooms, and returns
// its user_id and a session token, without a connection.
func (bs *benchServer) member(n int, rooms ...string) (string, string) {
	id, token, err := bs.s.member(bs.ctx, InvitationOptions{Rooms: rooms}, fmt.Sprintf("member%d@example.com", n))
	if err != nil {
		bs.tb.Fatal(err)
	}
	return id, token
}

// quietMembers makes n invisible accounts, as member makes each, but in
// one change, so that the room's entry, which lists every member, is
// stored once rather than for each, and returns their session tokens.
func (bs *benchServer) quietMembers(n int) []string {
	s := bs.s
	var tokens []string
	// At most so many invitations are outstanding at once.
	for len(tokens) < n {
		invited := make([]Invitation, min(500, n-len(tokens)))
		for i := range invited {
			var err error
			if invited[i], err = s.invite(bs.ctx, "", InvitationOptions{}); err != nil {
				bs.tb.Fatal(err)
			}
		}
		now := time.Now()
		s.mu.Lock()
		for _, inv := range invited {
			presented := s.invitations.Presented(inv.Token, now)
			id := authn.Identity{Scheme: email.Name, Subject: fmt.Sprintf("quiet%d@example.com", len(tokens))}
			u := s.enrollAccountLocked(presented, inv.Token, id, nil, authn.Proof{}, now)
			u.Presence.Chosen = presence.Invisible
			s.joinDefaultRoomLocked(u)
			token, _ := s.sessions.Issue(u, "", true, now)
			tokens = append(tokens, token)
		}
		f := s.newFenceLocked()
		s.unlock()
		if err := f.Wait(bs.ctx); err != nil {
			bs.tb.Fatal(err)
		}
	}
	return tokens
}

// resume opens a connection signed in with a session token.
func (bs *benchServer) resume(token string, replies bool) *benchClient {
	c := bs.connect(true)
	result := c.call(bs.tb, "auth", map[string]any{"scheme": "token", "token": token})
	c.userID = stringField(result, "user_id")
	if !replies {
		// From now on the connection reads nothing but counts frames: a
		// recipient.
		c.t.replies = nil
	}
	return c
}

// stringField finds the first "name":"value" in a frame.
func stringField(frame []byte, name string) string {
	key := []byte(`"` + name + `":"`)
	i := bytes.Index(frame, key)
	if i < 0 {
		return ""
	}
	rest := frame[i+len(key):]
	end := bytes.IndexByte(rest, '"')
	if end < 0 {
		return ""
	}
	return string(rest[:end])
}

// send sends a request without waiting for its reply, and returns its id.
func (c *benchClient) send(method string, params any) string {
	c.requests++
	id := strconv.Itoa(c.requests)
	c.t.in <- encodeJSON(map[string]any{"id": id, "method": method, "params": params})
	return id
}

// notify sends a notification (no id).
func (c *benchClient) notify(method string, params any) {
	c.t.in <- encodeJSON(map[string]any{"method": method, "params": params})
}

// wait waits for the reply to the request id, failing on an error.
func (c *benchClient) wait(tb testing.TB, id string) []byte {
	timeout := time.NewTimer(30 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case frame := <-c.t.replies:
			if stringField(frame, "id") != id {
				continue
			}
			if bytes.HasPrefix(frame, errorPrefix) {
				tb.Fatalf("request %s: %s", id, frame)
			}
			return frame
		case <-c.t.done:
			tb.Fatalf("request %s: the connection closed", id)
		case <-timeout.C:
			tb.Fatalf("request %s: no reply", id)
		}
	}
}

func (c *benchClient) call(tb testing.TB, method string, params any) []byte {
	return c.wait(tb, c.send(method, params))
}

// waitCount waits until counter reaches want.
func waitCount(tb testing.TB, counter *atomic.Int64, want int64) {
	deadline := time.Now().Add(60 * time.Second)
	for counter.Load() < want {
		if time.Now().After(deadline) {
			tb.Fatalf("delivered %d frames of %d", counter.Load(), want)
		}
		time.Sleep(50 * time.Microsecond)
	}
}

// countFrames makes the clients count the frames that start with prefix in
// one counter.
func countFrames(prefix string, clients ...*benchClient) *atomic.Int64 {
	counter := new(atomic.Int64)
	for _, c := range clients {
		c.t.countPrefix = []byte(prefix)
		c.t.counted = counter
	}
	return counter
}

// benchStores are the stores a benchmark runs on.
func benchStores(b *testing.B, run func(b *testing.B, st store.Store)) {
	b.Run("store=memory", func(b *testing.B) { run(b, store.NewMemory()) })
	b.Run("store=sqlite", func(b *testing.B) {
		st, err := store.OpenSQLite(filepath.Join(b.TempDir(), "bench.db"))
		if err != nil {
			b.Fatal(err)
		}
		run(b, st)
	})
}

// bigRoom makes a server with a room of n members connected as recipients,
// besides a poster who has joined it too, and returns the poster and the
// recipients. The members are accounts made while nobody is connected, so
// that setting up costs O(n) rather than every join told to every member.
func bigRoom(b *testing.B, config Config, n int) (*benchServer, *benchClient, []*benchClient) {
	return room(b, config, n, false)
}

// quietRoom is bigRoom with members who are invisible, so that their
// sign-ins tell nobody of them and they are told of nobody's status: a
// room of thousands set up in O(n) frames, for what does not concern
// status.
func quietRoom(b *testing.B, config Config, n int) (*benchServer, *benchClient, []*benchClient) {
	return room(b, config, n, true)
}

func room(b *testing.B, config Config, n int, quiet bool) (*benchServer, *benchClient, []*benchClient) {
	bs := newBenchServer(b, config)
	var tokens []string
	if quiet {
		tokens = bs.quietMembers(n)
	} else {
		tokens = make([]string, n)
		for i := range n {
			_, tokens[i] = bs.member(i)
		}
	}
	_, posterToken := bs.member(n)
	recipients := make([]*benchClient, n)
	for i, token := range tokens {
		recipients[i] = bs.resume(token, false)
	}
	poster := bs.resume(posterToken, true)
	// Let the sign-ins' status announcements drain.
	if err := bs.s.Sync(bs.ctx); err != nil {
		b.Fatal(err)
	}
	return bs, poster, recipients
}

// BenchmarkRoomFanout posts to general, one request in flight, with members
// connected: the cost of one message as the room grows, from saving it to
// its frame written to every member.
func BenchmarkRoomFanout(b *testing.B) {
	for _, n := range []int{10, 100, 1000, 5000} {
		b.Run(fmt.Sprintf("members=%d", n), func(b *testing.B) {
			benchStores(b, func(b *testing.B, st store.Store) {
				if n == 5000 && testing.Short() {
					b.Skip("short")
				}
				bs, poster, recipients := quietRoom(b, Config{Store: st, OutgoingQueue: 1024}, n)
				delivered := countFrames(`{"method":"message"`, recipients...)
				params := map[string]any{"room_id": defaultRoomID, "body": map[string]any{"text": "hello, everyone in the room"}}
				// Each batch waits for its frames, so that no member falls
				// a queue behind.
				const window = 64
				runtime.GC()
				b.ReportAllocs()
				b.ResetTimer()
				for i := range b.N {
					poster.call(b, "message", params)
					if i%window == window-1 {
						waitCount(b, delivered, int64(i+1)*int64(n))
					}
				}
				waitCount(b, delivered, int64(b.N)*int64(n))
				b.StopTimer()
				b.ReportMetric(float64(n), "frames/op")
				_ = bs
			})
		})
	}
}

// BenchmarkRoomFanoutPush is RoomFanout with some of the members
// registered for push, with relays that answer every push at once: each
// message is checked against what wakes whom (§4.9.4), and moves the
// unread counts of those who push.
func BenchmarkRoomFanoutPush(b *testing.B) {
	const n = 1000
	for _, pushers := range []int{10, 100} {
		b.Run(fmt.Sprintf("members=%d/pushers=%d", n, pushers), func(b *testing.B) {
			relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
			}))
			defer relay.Close()
			bs, poster, recipients := quietRoom(b, Config{OutgoingQueue: 1024, AllowInsecurePush: true}, n)
			for i, r := range recipients[:pushers] {
				r.t.replies = make(chan []byte, 64)
				r.call(b, "push_register", map[string]any{"kind": "relay", "url": fmt.Sprintf("%s/%d", relay.URL, i), "token": "t", "wake": []string{"joined"}})
			}
			delivered := countFrames(`{"method":"message"`, recipients...)
			params := map[string]any{"room_id": defaultRoomID, "body": map[string]any{"text": "hello, everyone in the room"}}
			const window = 64
			runtime.GC()
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				poster.call(b, "message", params)
				if i%window == window-1 {
					waitCount(b, delivered, int64(i+1)*n)
				}
			}
			waitCount(b, delivered, int64(b.N)*n)
			b.StopTimer()
			_ = bs
		})
	}
}

// BenchmarkRoomManySmall posts to many small rooms at once, a poster and
// members in each, one request in flight per poster: what one lock costs
// rooms that share nothing.
func BenchmarkRoomManySmall(b *testing.B) {
	const rooms, membersPerRoom = 32, 8
	benchStores(b, func(b *testing.B, st store.Store) {
		bs := newBenchServer(b, Config{Store: st, OutgoingQueue: 1024})
		admin := bs.guest()
		ids := make([]string, rooms)
		for i := range ids {
			result := admin.call(b, "room_set", map[string]any{"title": fmt.Sprintf("room %d", i)})
			ids[i] = stringField(result, "room_id")
		}
		type roomLoad struct {
			poster *benchClient
			params map[string]any
		}
		loads := make([]roomLoad, rooms)
		var recipients []*benchClient
		n := 0
		for i, id := range ids {
			for range membersPerRoom {
				_, token := bs.member(n, id)
				n++
				recipients = append(recipients, bs.resume(token, false))
			}
			_, token := bs.member(n, id)
			n++
			loads[i] = roomLoad{poster: bs.resume(token, true), params: map[string]any{"room_id": id, "body": map[string]any{"text": "hello, room"}}}
		}
		delivered := countFrames(`{"method":"message"`, recipients...)
		_ = delivered
		var next atomic.Int64
		runtime.GC()
		b.ReportAllocs()
		b.ResetTimer()
		var wg sync.WaitGroup
		for _, load := range loads {
			wg.Go(func() {
				for next.Add(1) <= int64(b.N) {
					load.poster.call(b, "message", load.params)
				}
			})
		}
		wg.Wait()
		b.StopTimer()
	})
}

// countingStore counts what a server hands its store.
type countingStore struct {
	store.Store
	batches, entries, bytes atomic.Int64
}

func (c *countingStore) Apply(batch []store.Entry) error {
	c.batches.Add(1)
	c.entries.Add(int64(len(batch)))
	for _, e := range batch {
		c.bytes.Add(int64(len(e.Kind) + len(e.ID) + len(e.Value)))
	}
	return c.Store.Apply(batch)
}

// reportStore reports what the store was handed per operation since the
// counts were reset.
func (c *countingStore) report(b *testing.B) {
	b.ReportMetric(float64(c.bytes.Load())/float64(b.N), "storeB/op")
	b.ReportMetric(float64(c.batches.Load())/float64(b.N), "batches/op")
}

func (c *countingStore) reset() {
	c.batches.Store(0)
	c.entries.Store(0)
	c.bytes.Store(0)
}

// BenchmarkActivityReadStorm advances read cursors in a large room with activity
// read_message_id, the notification every client sends as it reads: each is
// a change to the room the store keeps.
func BenchmarkActivityReadStorm(b *testing.B) {
	for _, n := range []int{100, 1000} {
		b.Run(fmt.Sprintf("members=%d", n), func(b *testing.B) {
			st := &countingStore{Store: store.NewMemory()}
			bs, poster, recipients := bigRoom(b, Config{Store: st, OutgoingQueue: 1024}, n)
			// Readers read the poster's latest message, one after another.
			readers := recipients[:min(len(recipients), 50)]
			for _, r := range readers {
				r.t.replies = make(chan []byte, 64)
			}
			var ids []string
			for range b.N/len(readers) + 2 {
				ids = append(ids, stringField(poster.call(b, "message", map[string]any{"body": map[string]any{"text": "read me"}}), "message_id"))
			}
			if err := bs.s.Sync(bs.ctx); err != nil {
				b.Fatal(err)
			}
			st.reset()
			runtime.GC()
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				r := readers[i%len(readers)]
				r.notify("activity", map[string]any{"room_id": defaultRoomID, "read_message_id": ids[i/len(readers)]})
				if i%len(readers) == len(readers)-1 {
					// A request behind the notifications waits for them.
					r.call(b, "history", map[string]any{"room_id": defaultRoomID, "limit": 1})
				}
			}
			if err := bs.s.Sync(bs.ctx); err != nil {
				b.Fatal(err)
			}
			b.StopTimer()
			st.report(b)
		})
	}
}

// BenchmarkActivityTypingStorm sends typing activity from every member of a room,
// each a notification that goes to every other member and is never logged.
func BenchmarkActivityTypingStorm(b *testing.B) {
	const n = 200
	bs, poster, recipients := bigRoom(b, Config{OutgoingQueue: 4096}, n)
	_ = bs
	delivered := countFrames(`{"method":"activity"`, append(recipients, poster)...)
	// Every member types in turn, each within its limit on changes.
	typers := recipients
	params := map[string]any{"room_id": defaultRoomID, "typing": 5}
	runtime.GC()
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		typers[i%len(typers)].notify("activity", params)
		if i%256 == 255 {
			waitCount(b, delivered, int64(i+1)*n)
		}
	}
	waitCount(b, delivered, int64(b.N)*n)
	b.StopTimer()
	b.ReportMetric(n, "frames/op")
}

// BenchmarkStatusFlap flips one member's status between dnd and online with
// me, each change published to everyone who shares a room.
func BenchmarkStatusFlap(b *testing.B) {
	for _, n := range []int{100, 1000} {
		b.Run(fmt.Sprintf("members=%d", n), func(b *testing.B) {
			_, poster, recipients := bigRoom(b, Config{OutgoingQueue: 4096}, n)
			// Every member flaps in turn, each within its limit on status
			// changes.
			flappers := append(recipients, poster)
			for _, f := range flappers {
				f.t.replies = make(chan []byte, 64)
			}
			if b.N > len(flappers)*statusRequestBurst/2 {
				b.Skipf("b.N %d is beyond the members' limits on status changes", b.N)
			}
			statuses := []string{"dnd", "online"}
			runtime.GC()
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				flappers[i%len(flappers)].call(b, "me", map[string]any{"status": statuses[(i/len(flappers))%2]})
			}
			b.StopTimer()
		})
	}
}

// BenchmarkStatusFlapBystanders flips the status of members of a small
// room while many others are connected who share no room with them: what a
// change costs that nobody else is told of.
func BenchmarkStatusFlapBystanders(b *testing.B) {
	for _, n := range []int{0, 1000} {
		b.Run(fmt.Sprintf("bystanders=%d", n), func(b *testing.B) {
			bs, poster, _ := bigRoom(b, Config{OutgoingQueue: 4096}, n)
			room := stringField(poster.call(b, "room_set", map[string]any{"title": "small"}), "room_id")
			flappers := make([]*benchClient, 100)
			for i := range flappers {
				_, token := bs.member(n+1+i, room)
				flappers[i] = bs.resume(token, true)
				flappers[i].call(b, "room_leave", map[string]any{"room_id": defaultRoomID})
			}
			if b.N > len(flappers)*statusRequestBurst/2 {
				b.Skipf("b.N %d is beyond the members' limits on status changes", b.N)
			}
			statuses := []string{"dnd", "online"}
			runtime.GC()
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				flappers[i%len(flappers)].call(b, "me", map[string]any{"status": statuses[(i/len(flappers))%2]})
			}
			b.StopTimer()
		})
	}
}

// BenchmarkReactionStorm has every member of a room react to one hot
// message, one after another, each changing their set: each reaction
// record carries every reaction of the message to every member.
func BenchmarkReactionStorm(b *testing.B) {
	const n = 300
	st := &countingStore{Store: store.NewMemory()}
	bs, poster, recipients := bigRoom(b, Config{Store: st, OutgoingQueue: 4096}, n)
	for _, r := range recipients {
		r.t.replies = make(chan []byte, 64)
	}
	hot := stringField(poster.call(b, "message", map[string]any{"body": map[string]any{"text": "react to me"}}), "message_id")
	// Everyone has reacted already: the message is hot.
	for _, r := range recipients {
		r.call(b, "reactions", map[string]any{"message_id": hot, "emojis": []string{"👍", "🎉"}})
	}
	delivered := countFrames(`{"method":"reactions"`, recipients...)
	emojis := [][]string{{"👍"}, {"👍", "🎉"}}
	if err := bs.s.Sync(bs.ctx); err != nil {
		b.Fatal(err)
	}
	st.reset()
	runtime.GC()
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		// Each member changes their set once per round, so a round of n
		// reactions stays within the members' limits on changes.
		r := recipients[i%n]
		r.call(b, "reactions", map[string]any{"message_id": hot, "emojis": emojis[(i/n)%2]})
	}
	waitCount(b, delivered, int64(b.N)*n)
	if err := bs.s.Sync(bs.ctx); err != nil {
		b.Fatal(err)
	}
	b.StopTimer()
	b.ReportMetric(n, "frames/op")
	st.report(b)
}

// BenchmarkRoomListMembers lists the rooms a member joined, with their members,
// as clients do as they sign in: rooms × members.
func BenchmarkRoomListMembers(b *testing.B) {
	for _, shape := range []struct{ rooms, members int }{{32, 30}, {10, 1000}} {
		b.Run(fmt.Sprintf("rooms=%d/members=%d", shape.rooms, shape.members), func(b *testing.B) {
			bs := newBenchServer(b, Config{OutgoingQueue: 1024})
			admin := bs.guest()
			ids := make([]string, shape.rooms)
			for i := range ids {
				ids[i] = stringField(admin.call(b, "room_set", map[string]any{"title": fmt.Sprintf("room %d", i)}), "room_id")
			}
			for i := range shape.members {
				bs.member(i, ids...)
			}
			_, token := bs.member(shape.members, ids...)
			lister := bs.resume(token, true)
			params := map[string]any{"filter": "joined", "members": true}
			runtime.GC()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				lister.call(b, "room_list", params)
			}
		})
	}
}

// BenchmarkRoomJoinLeave has members join and leave a room of n members, each
// change a logged membership told to every member.
func BenchmarkRoomJoinLeave(b *testing.B) {
	for _, n := range []int{100, 1000} {
		b.Run(fmt.Sprintf("members=%d", n), func(b *testing.B) {
			st := &countingStore{Store: store.NewMemory()}
			bs, poster, _ := bigRoom(b, Config{Store: st, OutgoingQueue: 4096}, n)
			// Churners are separate members, each joining and leaving at
			// most within the limit on changes.
			churners := make([]*benchClient, 32)
			for i := range churners {
				_, token := bs.member(n + 1 + i)
				churners[i] = bs.resume(token, true)
				churners[i].call(b, "room_leave", map[string]any{"room_id": defaultRoomID})
			}
			_ = poster
			if err := bs.s.Sync(bs.ctx); err != nil {
				b.Fatal(err)
			}
			st.reset()
			runtime.GC()
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				c := churners[i%len(churners)]
				method := "room_join"
				if (i/len(churners))%2 == 1 {
					method = "room_leave"
				}
				c.call(b, method, map[string]any{"room_id": defaultRoomID})
			}
			if err := bs.s.Sync(bs.ctx); err != nil {
				b.Fatal(err)
			}
			b.StopTimer()
			st.report(b)
		})
	}
}

// BenchmarkHistoryPageUnderWrites pages a room's history of 20,000 messages while
// writers post to it.
func BenchmarkHistoryPageUnderWrites(b *testing.B) {
	benchStores(b, func(b *testing.B, st store.Store) {
		bs := newBenchServer(b, Config{Store: st, OutgoingQueue: 1024})
		writer := bs.guest()
		for range 20000 / 100 {
			// Pipelined, a hundred at a time.
			ids := make([]string, 100)
			for i := range ids {
				ids[i] = writer.send("message", map[string]any{"body": map[string]any{"text": "a message in the history of this room"}})
			}
			for _, id := range ids {
				writer.wait(b, id)
			}
		}
		readers := make([]*benchClient, 4)
		for i := range readers {
			readers[i] = bs.guest()
		}
		writers := make([]*benchClient, 2)
		for i := range writers {
			writers[i] = bs.guest()
		}
		var next atomic.Int64
		underWrites(b, writers, func() {
			var readersDone sync.WaitGroup
			for _, r := range readers {
				readersDone.Go(func() {
					for i := next.Add(1); i <= int64(b.N); i = next.Add(1) {
						limit := 100
						if i%2 == 0 {
							limit = 1000
						}
						r.call(b, "history", map[string]any{"room_id": defaultRoomID, "limit": limit})
					}
				})
			}
			readersDone.Wait()
		})
	})
}

// underWrites runs the measured part of a benchmark while writers post to
// general, one request in flight each, and reports how long a post took
// meanwhile (writer-ns/post): what the benchmark's requests cost others
// by the time they hold the server's lock.
func underWrites(b *testing.B, writers []*benchClient, run func()) {
	stop := make(chan struct{})
	var measuring atomic.Bool
	var posts, took atomic.Int64
	var wg sync.WaitGroup
	for _, w := range writers {
		wg.Go(func() {
			params := map[string]any{"body": map[string]any{"text": "written while others read"}}
			for {
				select {
				case <-stop:
					return
				default:
				}
				start := time.Now()
				w.call(b, "message", params)
				if measuring.Load() {
					posts.Add(1)
					took.Add(int64(time.Since(start)))
				}
			}
		})
	}
	runtime.GC()
	b.ReportAllocs()
	b.ResetTimer()
	measuring.Store(true)
	run()
	measuring.Store(false)
	b.StopTimer()
	close(stop)
	wg.Wait()
	b.ReportMetric(float64(took.Load())/float64(max(1, posts.Load())), "writer-ns/post")
}

// BenchmarkRoomListUnderWrites lists rooms with their members, the joined
// rooms of a member of rooms of many members, as clients do as they sign
// in, while others post.
func BenchmarkRoomListUnderWrites(b *testing.B) {
	const rooms, members = 4, 500
	bs := newBenchServer(b, Config{OutgoingQueue: 4096})
	admin := bs.guest()
	ids := make([]string, rooms)
	for i := range ids {
		ids[i] = stringField(admin.call(b, "room_set", map[string]any{"title": fmt.Sprintf("room %d", i)}), "room_id")
	}
	for i := range members {
		bs.member(i, ids...)
	}
	listers := make([]*benchClient, 2)
	for i := range listers {
		_, token := bs.member(members+i, ids...)
		listers[i] = bs.resume(token, true)
	}
	writers := make([]*benchClient, 2)
	for i := range writers {
		writers[i] = bs.guest()
	}
	params := map[string]any{"filter": "joined", "members": true}
	var next atomic.Int64
	underWrites(b, writers, func() {
		var done sync.WaitGroup
		for _, l := range listers {
			done.Go(func() {
				for next.Add(1) <= int64(b.N) {
					l.call(b, "room_list", params)
				}
			})
		}
		done.Wait()
	})
}

// BenchmarkSignInToken signs members in with session tokens and out again, in a
// room of n members who stay connected: each sign-in is told to them, and
// each is sent their statuses.
func BenchmarkSignInToken(b *testing.B) {
	for _, n := range []int{100, 1000} {
		b.Run(fmt.Sprintf("members=%d", n), func(b *testing.B) {
			bs, _, _ := bigRoom(b, Config{OutgoingQueue: 4096}, n)
			tokens := make([]string, 16)
			for i := range tokens {
				_, tokens[i] = bs.member(n + 1 + i)
			}
			runtime.GC()
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				c := bs.resume(tokens[i%len(tokens)], true)
				c.t.Close()
				waitGone(b, bs.s, c)
			}
		})
	}
}

// waitGone waits until the server has forgotten a closed connection.
func waitGone(tb testing.TB, s *Server, c *benchClient) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		s.mu.RLock()
		u := s.users.Get(c.userID)
		connected := u != nil && len(u.clients) > 0
		s.mu.RUnlock()
		if !connected {
			return
		}
		if time.Now().After(deadline) {
			tb.Fatal("the connection was not forgotten")
		}
		time.Sleep(20 * time.Microsecond)
	}
}

// BenchmarkRoomSlowConsumers posts to a room of n members, a tenth of whom
// stopped reading: they are disconnected as their queues fill, and nobody
// else waits for them.
func BenchmarkRoomSlowConsumers(b *testing.B) {
	const n = 1000
	_, poster, recipients := bigRoom(b, Config{OutgoingQueue: 256}, n)
	for i, r := range recipients {
		if i%10 == 0 {
			r.t.stall.Store(true)
		}
	}
	params := map[string]any{"body": map[string]any{"text": "keep up"}}
	runtime.GC()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		poster.call(b, "message", params)
	}
	b.StopTimer()
}

// indexer is an extension that follows every event, as examples/search
// does: it decodes each message and indexes its words.
type indexer struct {
	mu       sync.Mutex
	postings map[string]map[string]bool
	events   atomic.Int64
}

func (*indexer) Name() string { return "indexer" }

func (x *indexer) Init(r *Registrar) error {
	x.postings = make(map[string]map[string]bool)
	r.OnEvent(func(_ context.Context, e Event) {
		x.events.Add(1)
		m, ok := DecodeMessage(e)
		if !ok {
			return
		}
		x.mu.Lock()
		defer x.mu.Unlock()
		for word := range bytes.FieldsSeq([]byte(m.Text)) {
			key := string(word)
			if x.postings[key] == nil {
				x.postings[key] = make(map[string]bool)
			}
			x.postings[key][m.ID] = true
		}
	})
	return nil
}

// BenchmarkEventFollowers posts with extensions that follow every event, and
// waits for them to handle each.
func BenchmarkEventFollowers(b *testing.B) {
	for _, followers := range []int{0, 1, 4} {
		b.Run(fmt.Sprintf("followers=%d", followers), func(b *testing.B) {
			var extensions []Extension
			for i := range followers {
				extensions = append(extensions, &namedIndexer{name: fmt.Sprintf("indexer%d", i)})
			}
			bs := newBenchServer(b, Config{Extensions: extensions, OutgoingQueue: 1024})
			poster := bs.guest()
			params := map[string]any{"body": map[string]any{"text": "the quick brown fox jumps over the lazy dog"}}
			runtime.GC()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				poster.call(b, "message", params)
			}
			if err := bs.s.settle(bs.ctx); err != nil {
				b.Fatal(err)
			}
			b.StopTimer()
		})
	}
}

type namedIndexer struct {
	indexer
	name string
}

func (x *namedIndexer) Name() string { return x.name }

// BenchmarkEventDecodeMessage decodes a message record as each follower of the
// log that reads messages does (DecodeMessage).
func BenchmarkEventDecodeMessage(b *testing.B) {
	record := Event{Kind: EventMessage, LogID: 1, Record: []byte(`{"body":{"format":"markdown","mentions":["bob"],"text":"the quick brown fox jumps over the lazy dog"},"ext":{"x":1},"from":{"name":"Ada","user_id":"ada"},"log_id":"1795817355191","message_id":"1795817355191","room_id":"general"}`)}
	b.ReportAllocs()
	for range b.N {
		if _, ok := DecodeMessage(record); !ok {
			b.Fatal("not a message")
		}
	}
}

// BenchmarkLogRetained posts messages, pipelined, and reports what the
// server keeps of each once they are posted (retainedB/op): the log, and
// what it keeps of each message, which it never lets go of. Run it with a
// fixed count, such as -benchtime=20000x.
func BenchmarkLogRetained(b *testing.B) {
	// A store that keeps nothing, so that what is measured is the server's.
	bs := newBenchServer(b, Config{Store: discardStore{}, OutgoingQueue: 4096})
	poster := bs.guest()
	params := map[string]any{"body": map[string]any{"text": text200}}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	b.ReportAllocs()
	b.ResetTimer()
	for done := 0; done < b.N; {
		batch := min(100, b.N-done)
		ids := make([]string, batch)
		for i := range ids {
			ids[i] = poster.send("message", params)
		}
		for _, id := range ids {
			poster.wait(b, id)
		}
		done += batch
	}
	b.StopTimer()
	if err := bs.s.Sync(bs.ctx); err != nil {
		b.Fatal(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	b.ReportMetric(float64(int64(after.HeapAlloc)-int64(before.HeapAlloc))/float64(b.N), "retainedB/op")
}

// text200 is a message text of 200 bytes, as apron-hammer's flood posts.
var text200 = strings.Repeat("lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor ", 3)[:200]

// discardStore is a store that keeps nothing.
type discardStore struct{}

func (discardStore) Load(func(store.Entry) error) error { return nil }
func (discardStore) Apply([]store.Entry) error          { return nil }
func (discardStore) Close() error                       { return nil }
