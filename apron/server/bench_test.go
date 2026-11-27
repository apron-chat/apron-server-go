package server_test

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/internal/benchchat"
	"github.com/apron-chat/apron-server-go/apron/server"
)

// The benchmarks of the engine run package benchchat's backend, which
// keeps its state under one lock and sends while it holds it, as
// examples/minimal does, on connections in memory, so that what they
// measure is the Server's: its queues, encoding, dispatch, and locks.
//
//	go test ./server -run '^$' -bench . -benchmem
//
// Besides ns/op, allocs/op, and B/op, the fan-out benchmarks report what
// each recipient costs (per-recipient metrics), which tells a cost that
// grows with the connections from one that does not.

// perRecipient reports the time, allocations, and bytes of each of the
// b.N*n frames a fan-out benchmark delivered, from mallocs and bytes
// counted since before.
func perRecipient(b *testing.B, n int, before runtime.MemStats) {
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	frames := float64(b.N) * float64(n)
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/frames, "ns/recipient")
	b.ReportMetric(float64(after.Mallocs-before.Mallocs)/frames, "allocs/recipient")
	b.ReportMetric(float64(after.TotalAlloc-before.TotalAlloc)/frames, "B/recipient")
}

func memStats() runtime.MemStats {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m
}

// message is an encoded message notification of about the size of a
// line of chat.
func message(b *testing.B, text string) apron.Frame {
	frame, err := apron.Encode(apron.Message{
		MessageID: 1_700_000_000_000, LogID: 1_700_000_000_000, RoomID: "general",
		From: apron.User{UserID: "guest_1", Name: "Ada"}, Body: &apron.Body{Text: text},
	})
	if err != nil {
		b.Fatal(err)
	}
	return frame
}

// BenchmarkFanoutBroadcast sends one encoded message to every connection
// (Server.Broadcast), and waits until each was written it. send-ns is
// the time Broadcast itself takes for each recipient: what a backend that
// sends while it holds its lock holds it for.
func BenchmarkFanoutBroadcast(b *testing.B) {
	for _, n := range []int{10, 100, 1000, 10000} {
		b.Run("conns="+strconv.Itoa(n), func(b *testing.B) {
			chat := benchchat.New(server.Config{})
			sink := benchchat.NewSink()
			ts := chat.Open(n, sink, nil)
			defer chat.CloseAll(ts)
			frame := message(b, "Hello, everyone: this is a line of chat of a usual length.")
			target := sink.Total()
			before := memStats()
			b.ResetTimer()
			var sending time.Duration
			for b.Loop() {
				target += int64(n)
				start := time.Now()
				chat.Server.Broadcast(frame, nil)
				sending += time.Since(start)
				sink.Wait(target)
			}
			b.StopTimer()
			perRecipient(b, n, before)
			b.ReportMetric(float64(sending.Nanoseconds())/float64(b.N*n), "send-ns/recipient")
		})
	}
}

// BenchmarkFanoutSend sends one encoded message to every connection
// from the backend's own list, as a backend that keeps each room's
// members does: with Conn.Send to each (send), or with SendAll (sendall),
// which wakes their writers once it has queued them all. send-ns is the
// time the sending itself takes for each recipient, which a backend
// holds its lock for.
func BenchmarkFanoutSend(b *testing.B) {
	for _, run := range []struct {
		n   int
		all bool
	}{{10, false}, {10, true}, {1000, false}, {1000, true}, {10000, false}, {10000, true}} {
		n := run.n
		how := "send"
		if run.all {
			how = "sendall"
		}
		b.Run(fmt.Sprintf("conns=%d/%s", n, how), func(b *testing.B) {
			chat := benchchat.New(server.Config{})
			sink := benchchat.NewSink()
			ts := chat.Open(n, sink, nil)
			defer chat.CloseAll(ts)
			conns := chat.Server.Conns()
			frame := message(b, "Hello, everyone: this is a line of chat of a usual length.")
			target := sink.Total()
			before := memStats()
			b.ResetTimer()
			var sending time.Duration
			for b.Loop() {
				target += int64(n)
				start := time.Now()
				if run.all {
					server.SendAll(frame, slices.Values(conns))
				} else {
					for _, c := range conns {
						c.Send(frame)
					}
				}
				sending += time.Since(start)
				sink.Wait(target)
			}
			b.StopTimer()
			perRecipient(b, n, before)
			b.ReportMetric(float64(sending.Nanoseconds())/float64(b.N*n), "send-ns/recipient")
		})
	}
}

// BenchmarkFanoutNotify sends one message to every connection with Conn.Notify,
// which encodes it for each: what encoding once saves.
func BenchmarkFanoutNotify(b *testing.B) {
	const n = 1000
	chat := benchchat.New(server.Config{})
	sink := benchchat.NewSink()
	ts := chat.Open(n, sink, nil)
	defer chat.CloseAll(ts)
	conns := chat.Server.Conns()
	m := apron.Message{MessageID: 1, LogID: 1, RoomID: "general", From: apron.User{UserID: "guest_1", Name: "Ada"}, Body: &apron.Body{Text: "Hello, everyone: this is a line of chat of a usual length."}}
	target := sink.Total()
	before := memStats()
	b.ResetTimer()
	for b.Loop() {
		target += n
		for _, c := range conns {
			c.Notify(m)
		}
		sink.Wait(target)
	}
	b.StopTimer()
	perRecipient(b, n, before)
}

// roundTrip sends a request on t, and waits for writes frames in all on
// its sink, from the total before.
type roundTrip struct {
	t      *benchchat.Transport
	sink   *benchchat.Sink
	target int64
	next   int
}

func newRoundTrip(chat *benchchat.Chat, room string) *roundTrip {
	sink := benchchat.NewSink()
	ts := chat.Open(1, sink, func(int) string { return room })
	return &roundTrip{t: ts[0], sink: sink, target: sink.Total()}
}

// call sends a request whose params are raw JSON, with a new id, and waits
// for writes frames.
func (r *roundTrip) call(method, params string, writes int) {
	r.next++
	frame := make([]byte, 0, len(method)+len(params)+48)
	frame = append(frame, `{"id":"`...)
	frame = strconv.AppendInt(frame, int64(r.next), 10)
	frame = append(frame, `","method":"`...)
	frame = append(frame, method...)
	frame = append(frame, `","params":`...)
	frame = append(frame, params...)
	frame = append(frame, '}')
	r.target += int64(writes)
	r.t.Send(frame)
	r.sink.Wait(r.target)
}

// BenchmarkRequestRoundTrip makes one request at a time on one connection, alone
// in its room: message (its notification and its result), history (a page
// of 100 of 1000 messages), history-raw (the same page, of records the
// backend keeps encoded, HistoryResult.Raw), me (a rename), status (idle, which others see
// as away), reactions, and a request that fails (unsupported).
func BenchmarkRequestRoundTrip(b *testing.B) {
	b.Run("message", func(b *testing.B) {
		chat := benchchat.New(server.Config{})
		r := newRoundTrip(chat, "solo")
		defer chat.CloseAll([]*benchchat.Transport{r.t})
		for b.Loop() {
			r.call("message", `{"body":{"text":"Hello, everyone: this is a line of chat of a usual length."}}`, 2)
		}
	})
	b.Run("history", func(b *testing.B) {
		chat := benchchat.New(server.Config{})
		r := newRoundTrip(chat, "solo")
		defer chat.CloseAll([]*benchchat.Transport{r.t})
		for range 1000 {
			r.call("message", `{"body":{"text":"Hello, everyone: this is a line of chat of a usual length."}}`, 2)
		}
		for b.Loop() {
			r.call("history", `{"limit":100}`, 1)
		}
	})
	b.Run("history-raw", func(b *testing.B) {
		chat := benchchat.New(server.Config{})
		chat.RawHistory = true
		r := newRoundTrip(chat, "solo")
		defer chat.CloseAll([]*benchchat.Transport{r.t})
		for range 1000 {
			r.call("message", `{"body":{"text":"Hello, everyone: this is a line of chat of a usual length."}}`, 2)
		}
		for b.Loop() {
			r.call("history", `{"limit":100}`, 1)
		}
	})
	b.Run("me", func(b *testing.B) {
		chat := benchchat.New(server.Config{})
		r := newRoundTrip(chat, "solo")
		defer chat.CloseAll([]*benchchat.Transport{r.t})
		names := []string{`{"name":"Ada"}`, `{"name":"Grace"}`}
		i := 0
		for b.Loop() {
			r.call("me", names[i%2], 1)
			i++
		}
	})
	b.Run("reactions", func(b *testing.B) {
		chat := benchchat.New(server.Config{})
		r := newRoundTrip(chat, "solo")
		defer chat.CloseAll([]*benchchat.Transport{r.t})
		for b.Loop() {
			r.call("reactions", `{"message_id":"1700000000000","emojis":["👍"]}`, 2)
		}
	})
	b.Run("unsupported", func(b *testing.B) {
		chat := benchchat.New(server.Config{})
		r := newRoundTrip(chat, "solo")
		defer chat.CloseAll([]*benchchat.Transport{r.t})
		for b.Loop() {
			r.call("nonesuch", `{}`, 1)
		}
	})
}

// BenchmarkRequestParallel makes requests on many connections at once,
// each alone in its room, so that what they share is the Server and the
// backend's lock: run with -cpu=1,2,4 to see how it scales. ext:noop is a
// method whose handler takes no lock of its own, so that what its
// requests share is the Server's alone, deduplication and all.
func BenchmarkRequestParallel(b *testing.B) {
	for _, method := range []string{"message", "history", "ext:noop", "ext:noop/nodedup"} {
		b.Run(method, func(b *testing.B) {
			config := server.Config{}
			if strings.HasSuffix(method, "/nodedup") {
				config.DedupSize = -1
				method = strings.TrimSuffix(method, "/nodedup")
			}
			chat := benchchat.New(config)
			chat.MemberLists = true
			chat.Server.Handle("ext:noop", server.Handler{Serve: func(_ context.Context, _ *server.Conn, _ *server.Request, reply server.Reply[any]) error {
				return reply.Send(apron.OK{})
			}})
			var mu sync.Mutex
			var opened []*roundTrip
			defer func() {
				for _, r := range opened {
					_ = r.t.Close()
				}
				_ = chat.Server.Shutdown(context.Background())
			}()
			var rooms atomic.Int64
			b.RunParallel(func(pb *testing.PB) {
				r := newRoundTrip(chat, "room"+strconv.FormatInt(rooms.Add(1), 10))
				mu.Lock()
				opened = append(opened, r)
				mu.Unlock()
				for pb.Next() {
					switch method {
					case "message":
						r.call("message", `{"body":{"text":"Hello, everyone: this is a line of chat of a usual length."}}`, 2)
					case "history":
						r.call("history", `{"limit":10}`, 1)
					default:
						r.call(method, `{"n":1}`, 1)
					}
				}
			})
		})
	}
}

// BenchmarkRequestLocal makes one request at a time on a Local connection,
// as a bot does: Local.Call encodes the request, and the reply goes back
// to it.
func BenchmarkRequestLocal(b *testing.B) {
	chat := benchchat.New(server.Config{})
	// The connection's OnFrame gets each message's notification; the
	// benchmark lets it catch up now and then, as a client that reads what
	// it is sent does, rather than fall behind its queue of 1024 on a busy
	// machine.
	var got atomic.Int64
	caught := make(chan struct{}, 1)
	l, err := chat.Server.Local(server.LocalOptions{User: apron.User{UserID: "bot", Name: "Bot"}, OnFrame: func(apron.Frame) {
		got.Add(1)
		select {
		case caught <- struct{}{}:
		default:
		}
	}})
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()
	p := apron.MessageParams{Body: &apron.Body{Text: "Hello, everyone: this is a line of chat of a usual length."}}
	ctx := context.Background()
	sent := int64(1) // the server frame
	for b.Loop() {
		if _, err := l.Call(ctx, "message", p); err != nil {
			b.Fatal(err)
		}
		if sent++; sent%256 == 0 {
			for got.Load() < sent {
				<-caught
			}
		}
	}
}

// BenchmarkFanoutRooms posts a message to one room of many: 1000
// connections in rooms of size members. With Broadcast and a filter, every
// message walks every connection; with the backend's member lists and
// Conn.Send, only the room's. status changes the sender's shown status
// instead, which PublishUser tells those who share the room, walking every
// connection, and PublishUserTo (status-memberlists) the room's members
// from the backend's list.
func BenchmarkFanoutRooms(b *testing.B) {
	const conns = 1000
	for _, members := range []int{10, 1000} {
		for _, how := range []string{"broadcast", "memberlists", "status", "status-memberlists"} {
			b.Run(fmt.Sprintf("members=%d/%s", members, how), func(b *testing.B) {
				chat := benchchat.New(server.Config{})
				chat.MemberLists = strings.HasSuffix(how, "memberlists")
				sink := benchchat.NewSink()
				rooms := conns / members
				ts := chat.Open(conns-1, sink, func(i int) string { return "room" + strconv.Itoa(i%rooms) })
				r := newRoundTrip(chat, "room0")
				defer chat.CloseAll(append(ts, r.t))
				// The sender's room has members connections, the sender's
				// among them, which share its sink's count but for the
				// sender's own: wait on the shared sink for the others.
				idle := []string{`{"idle":60}`, `{"idle":false}`}
				for b.Loop() {
					others := sink.Total() + int64(members-1)
					if strings.HasPrefix(how, "status") {
						r.call("status", idle[r.next%2], 1)
					} else {
						r.call("message", `{"body":{"text":"hi"}}`, 2)
					}
					sink.Wait(others)
				}
			})
		}
	}
}

// BenchmarkRequestLarge posts a message of 200 KB, near the 256 KiB a
// frame may have, to a room of 100: what the request path copies of it,
// and that its notification is encoded once for all of them.
func BenchmarkRequestLarge(b *testing.B) {
	const members = 100
	chat := benchchat.New(server.Config{})
	chat.MemberLists = true
	sink := benchchat.NewSink()
	ts := chat.Open(members-1, sink, func(int) string { return "big" })
	r := newRoundTrip(chat, "big")
	defer chat.CloseAll(append(ts, r.t))
	params := string(benchchat.Text(strings.Repeat("All work and no play makes Jack a dull boy. ", 200<<10/44)))
	b.SetBytes(int64(len(params)))
	for b.Loop() {
		others := sink.Total() + members - 1
		r.call("message", params, 2)
		sink.Wait(others)
	}
}

// BenchmarkFanoutStorm sends the tiny frames of busy rooms, each fanned out to
// a room of 100: reactions (a request and a reactions record), activity
// (typing, a notification, relayed to everyone else), and status (idle
// and back, which others see as away and online, PublishUser's patches).
func BenchmarkFanoutStorm(b *testing.B) {
	const members = 100
	for _, kind := range []string{"reactions", "activity", "status"} {
		b.Run(kind, func(b *testing.B) {
			chat := benchchat.New(server.Config{})
			chat.MemberLists = true
			sink := benchchat.NewSink()
			ts := chat.Open(members-1, sink, func(int) string { return "busy" })
			r := newRoundTrip(chat, "busy")
			defer chat.CloseAll(append(ts, r.t))
			before := memStats()
			i := 0
			for b.Loop() {
				others := sink.Total() + members - 1
				switch kind {
				case "reactions":
					r.call("reactions", `{"message_id":"1700000000000","emojis":["👍"]}`, 2)
				case "activity":
					r.next++
					r.t.Send([]byte(`{"method":"activity","params":{"typing":5}}`))
				case "status":
					idle := `{"idle":false}`
					if i%2 == 0 {
						idle = `{"idle":60}`
					}
					r.call("status", idle, 1)
				}
				sink.Wait(others)
				i++
			}
			b.StopTimer()
			perRecipient(b, members, before)
		})
	}
}

// BenchmarkFanoutSlowConsumer broadcasts to 100 connections, one of which stops
// reading: its queue fills, and it is disconnected, without holding up
// the others, or the broadcaster.
func BenchmarkFanoutSlowConsumer(b *testing.B) {
	for _, stalled := range []int{0, 1, 10} {
		b.Run("stalled="+strconv.Itoa(stalled), func(b *testing.B) {
			const fast = 100
			chat := benchchat.New(server.Config{})
			sink := benchchat.NewSink()
			ts := chat.Open(fast, sink, nil)
			stall := chat.Open(stalled, benchchat.NewSink(), nil)
			for _, t := range stall {
				t.Stall.Store(true)
			}
			defer chat.CloseAll(append(ts, stall...))
			frame := message(b, "Hello, everyone: this is a line of chat of a usual length.")
			target := sink.Total()
			for b.Loop() {
				target += fast
				chat.Server.Broadcast(frame, nil)
				sink.Wait(target)
			}
			b.StopTimer()
			dropped := 0
			for _, t := range stall {
				select {
				case <-t.Closed():
					dropped++
				case <-time.After(time.Second):
				}
			}
			if b.N > 200 && dropped != stalled {
				b.Errorf("%d of %d stalled connections were dropped", dropped, stalled)
			}
		})
	}
}

// BenchmarkConnChurn opens a connection, signs it in, and closes it: what a
// connection costs the Server for its life.
func BenchmarkConnChurn(b *testing.B) {
	chat := benchchat.New(server.Config{})
	sink := benchchat.NewSink()
	// Others stay connected, so that what churns is not the whole server.
	ts := chat.Open(100, sink, nil)
	defer chat.CloseAll(ts)
	for b.Loop() {
		t := chat.Open(1, sink, nil)[0]
		_ = t.Close()
	}
}

// BenchmarkConnUser reads a connection's identity, as a handler does for
// every message's sender: a copy (Conn.User), or as it is kept
// (Conn.Identity).
func BenchmarkConnUser(b *testing.B) {
	chat := benchchat.New(server.Config{})
	l, err := chat.Server.Local(server.LocalOptions{User: apron.User{UserID: "ada", Name: "Ada", Roles: []string{"admin"}, Ext: apron.Ext{"tz": []byte(`"UTC"`)}}})
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()
	c := l.Conn()
	b.Run("user", func(b *testing.B) {
		for b.Loop() {
			_ = *c.User()
		}
	})
	b.Run("identity", func(b *testing.B) {
		for b.Loop() {
			_, _ = c.Identity()
		}
	})
}
