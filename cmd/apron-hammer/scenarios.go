package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const generalRoom = "general"

var httpClient = &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 256}}

// run calls f on n goroutines and waits for them.
func run(n int, f func(i int)) {
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { f(i) })
	}
	wg.Wait()
}

// connect signs in n guests, at most 32 at a time, and returns those that
// signed in. options, when set, makes each one's dial options.
func connect(h *hammer, n int, label string, options func(i int) dialOptions) []*peer {
	return compact(connectIndexed(h, n, label, options))
}

// connectIndexed is connect, with nil in place of each guest that failed to
// sign in, so that peers[i] was dialed with options(i).
func connectIndexed(h *hammer, n int, label string, options func(i int) dialOptions) []*peer {
	peers := make([]*peer, n)
	slots := make(chan struct{}, 32)
	run(n, func(i int) {
		slots <- struct{}{}
		defer func() { <-slots }()
		var o dialOptions
		if options != nil {
			o = options(i)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		p, err := h.dialGuest(ctx, fmt.Sprintf("%s %d", label, i), o)
		if err != nil {
			fmt.Printf("   !! connect %s %d: %v\n", label, i, err)
			return
		}
		peers[i] = p
	})
	return peers
}

func compact(peers []*peer) []*peer {
	out := peers[:0]
	for _, p := range peers {
		if p != nil {
			out = append(out, p)
		}
	}
	return out
}

func closeAll(peers []*peer) {
	for _, p := range peers {
		p.close()
	}
}

// measure records f under op unless the measured window ended first, which
// is how every loop stops.
func measure(ctx context.Context, st *stats, op string, f func() error) error {
	start := time.Now()
	err := f()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, errDisconnected) {
		// Loops stop on a closed connection, which the frame counters report.
		return err
	}
	st.observe(op, time.Since(start), err)
	return err
}

func text(n int) string {
	const words = "lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor "
	return strings.Repeat(words, n/len(words)+1)[:n]
}

func postMessage(ctx context.Context, p *peer, roomID, body string) (string, error) {
	raw, err := p.call(ctx, "message", map[string]any{"room_id": roomID, "body": map[string]any{"text": body}})
	if err != nil {
		return "", err
	}
	var result struct {
		MessageID string `json:"message_id"`
	}
	err = json.Unmarshal(raw, &result)
	return result.MessageID, err
}

// createRoom creates a room or thread with room_set, which joins its creator.
func createRoom(ctx context.Context, p *peer, params map[string]any) (string, error) {
	raw, err := p.call(ctx, "room_set", params)
	if err != nil {
		return "", err
	}
	var result struct {
		RoomID string `json:"room_id"`
	}
	err = json.Unmarshal(raw, &result)
	return result.RoomID, err
}

// orderCheck verifies that a connection receives each room's message
// snapshots in ascending log_id, the one order a live connection is promised
// (§3.5), and counts the message creations it receives. A transient notice,
// which has no log_id, is neither checked nor counted.
type orderCheck struct {
	h        *hammer
	last     map[string]int64
	messages atomic.Int64
}

func newOrderCheck(h *hammer) *orderCheck {
	return &orderCheck{h: h, last: make(map[string]int64)}
}

// notify runs on the peer's read goroutine.
func (o *orderCheck) notify(method string, frame []byte) {
	if method != "message" {
		return
	}
	var f struct {
		Params struct {
			RoomID    string `json:"room_id"`
			MessageID string `json:"message_id"`
			LogID     string `json:"log_id"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frame, &f); err != nil {
		o.h.protocolViolation("message notification: %v", err)
		return
	}
	if f.Params.LogID == "" {
		return
	}
	id, err := strconv.ParseInt(f.Params.LogID, 10, 64)
	if err != nil {
		o.h.protocolViolation("message notification log_id %q", f.Params.LogID)
		return
	}
	if id <= o.last[f.Params.RoomID] {
		o.h.protocolViolation("room %s snapshot log_id %d after %d", f.Params.RoomID, id, o.last[f.Params.RoomID])
	}
	o.last[f.Params.RoomID] = id
	if f.Params.MessageID == f.Params.LogID {
		o.messages.Add(1)
	}
}

func runChurn(h *hammer, st *stats) {
	ctx, cancel := st.window()
	defer cancel()
	defer st.finish()
	run(h.clients, func(i int) {
		for n := 0; ctx.Err() == nil; n++ {
			var p *peer
			err := measure(ctx, st, "connect+auth", func() (err error) {
				p, err = h.dialGuest(ctx, "churn", dialOptions{})
				return err
			})
			if err != nil {
				// The window may end after a successful sign-in.
				if p != nil {
					p.close()
				}
				continue
			}
			// Alternate the close handshake with dropped connections.
			if n%2 == 0 {
				p.closeGracefully()
			} else {
				p.close()
			}
		}
	})
}

func runFlood(h *hammer, st *stats) {
	checks := make([]*orderCheck, h.clients)
	peers := connect(h, h.clients, "flood", func(i int) dialOptions {
		checks[i] = newOrderCheck(h)
		return dialOptions{notify: checks[i].notify}
	})
	defer closeAll(peers)
	body := text(h.bodySize)
	var acked atomic.Int64
	ctx, cancel := st.window()
	run(len(peers), func(i int) {
		for ctx.Err() == nil && peers[i].alive() {
			if measure(ctx, st, "message", func() error {
				_, err := postMessage(ctx, peers[i], generalRoom, body)
				return err
			}) == nil {
				acked.Add(1)
			}
		}
	})
	cancel()
	st.finish()
	// Let deliveries of the last posts arrive before counting.
	time.Sleep(500 * time.Millisecond)
	var received int64
	alive := 0
	for i, p := range peers {
		received += checks[i].messages.Load()
		if p.alive() {
			alive++
		}
	}
	st.note("%d posts acknowledged; each of %d live clients received %.1f%% of them on average",
		acked.Load(), alive, 100*float64(received)/float64(max(1, acked.Load()*int64(len(peers)))))
}

// activityPace is how often each client sends activity.
const activityPace = 250 * time.Millisecond

// activityProbe measures one client's activity: the time from each
// notification it sends to the broadcast of it reaching its own connection,
// keyed by the typing value, and the broadcasts it receives from others.
type activityProbe struct {
	st     *stats
	ctx    context.Context
	self   atomic.Pointer[string]
	mu     sync.Mutex
	sent   map[int]time.Time
	echoes atomic.Int64
	others atomic.Int64
}

func (a *activityProbe) notify(method string, frame []byte) {
	if method != "activity" {
		return
	}
	var f struct {
		Params struct {
			From struct {
				UserID string `json:"user_id"`
			} `json:"from"`
			Typing *int `json:"typing"`
		} `json:"params"`
	}
	self := a.self.Load()
	if json.Unmarshal(frame, &f) != nil || self == nil || f.Params.Typing == nil {
		return
	}
	if f.Params.From.UserID != *self {
		a.others.Add(1)
		return
	}
	a.echoes.Add(1)
	a.mu.Lock()
	start, ok := a.sent[*f.Params.Typing]
	delete(a.sent, *f.Params.Typing)
	ctx := a.ctx
	a.mu.Unlock()
	if ok && ctx != nil && ctx.Err() == nil {
		a.st.observe("activity echo", time.Since(start), nil)
	}
}

// runActivity has every client send typing activity to general as a
// notification, without an id, every activityPace (§4.6), and measures its
// delivery through the activity broadcasts the clients receive. No reply
// is expected; one would be a violation.
func runActivity(h *hammer, st *stats) {
	all := make([]*activityProbe, h.clients)
	indexed := connectIndexed(h, h.clients, "typist", func(i int) dialOptions {
		all[i] = &activityProbe{st: st, sent: make(map[int]time.Time)}
		return dialOptions{notify: all[i].notify}
	})
	var connected []*peer
	var probes []*activityProbe
	for i, p := range indexed {
		if p != nil {
			connected = append(connected, p)
			probes = append(probes, all[i])
		}
	}
	defer closeAll(connected)
	ctx, cancel := st.window()
	for _, probe := range probes {
		probe.mu.Lock()
		probe.ctx = ctx
		probe.mu.Unlock()
	}
	var sent atomic.Int64
	run(len(connected), func(i int) {
		p, probe := connected[i], probes[i]
		self := p.userID
		probe.self.Store(&self)
		ticker := time.NewTicker(activityPace)
		defer ticker.Stop()
		for seq := 0; ctx.Err() == nil && p.alive(); seq++ {
			typing := seq%30 + 1
			probe.mu.Lock()
			probe.sent[typing] = time.Now()
			probe.mu.Unlock()
			// Not the window's context: the WebSocket library closes the
			// connection when a write's context ends during the write.
			write, cancelWrite := context.WithTimeout(context.Background(), 5*time.Second)
			if p.send(write, "", "activity", map[string]any{"room_id": generalRoom, "typing": typing}) == nil {
				sent.Add(1)
			}
			cancelWrite()
			select {
			case <-ticker.C:
			case <-ctx.Done():
			}
		}
	})
	cancel()
	st.finish()
	// Let the last broadcasts arrive before counting.
	time.Sleep(500 * time.Millisecond)
	var echoes, others int64
	for _, probe := range probes {
		echoes += probe.echoes.Load()
		others += probe.others.Load()
	}
	clients := int64(len(connected))
	st.note("%d activity notifications sent; %.1f%% echoed to their senders, and others received %.1f%% of the broadcasts to them",
		sent.Load(), 100*float64(echoes)/float64(max(1, sent.Load())), 100*float64(others)/float64(max(1, sent.Load()*(clients-1))))
}

// runSlow checks that clients which stop reading are dropped once their
// queue fills, without slowing everyone else down.
func runSlow(h *hammer, st *stats) {
	n := max(1, h.clients/2)
	senders := connect(h, n, "sender", nil)
	defer closeAll(senders)
	slow := connect(h, n, "slow", func(int) dialOptions { return dialOptions{noRead: true} })
	defer closeAll(slow)
	body := text(4 << 10)
	ctx, cancel := st.window()
	run(len(senders), func(i int) {
		for ctx.Err() == nil && senders[i].alive() {
			_ = measure(ctx, st, "message 4KiB", func() error {
				_, err := postMessage(ctx, senders[i], generalRoom, body)
				return err
			})
		}
	})
	cancel()
	st.finish()
	// A slow client the server dropped reads to the end of what was sent and
	// then sees the connection close; one still held open just times out.
	var dropped, held atomic.Int64
	run(len(slow), func(i int) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		for {
			if _, _, err := slow[i].ws.Read(ctx); err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					held.Add(1)
				} else {
					dropped.Add(1)
				}
				return
			}
		}
	})
	st.note("slow clients: %d dropped by the server, %d still connected", dropped.Load(), held.Load())
}

const historySeed = 20000

func runHistory(h *hammer, st *stats) {
	setup, cancelSetup := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelSetup()
	writer := connect(h, 1, "historian", nil)
	if len(writer) == 0 {
		return
	}
	defer closeAll(writer)
	roomID, err := createRoom(setup, writer[0], map[string]any{"title": "History bench"})
	if err != nil {
		st.note("create room: %v", err)
		return
	}
	started := time.Now()
	ids := make([]int64, historySeed)
	var next atomic.Int64
	body := text(h.bodySize)
	run(32, func(int) {
		for i := int(next.Add(1) - 1); i < historySeed; i = int(next.Add(1) - 1) {
			id, err := postMessage(setup, writer[0], roomID, body)
			if err != nil {
				return
			}
			ids[i], _ = strconv.ParseInt(id, 10, 64)
		}
	})
	st.note("seeded %d messages in %s", historySeed, time.Since(started).Round(time.Millisecond))

	readers := connect(h, h.clients, "reader", nil)
	defer closeAll(readers)
	ctx, cancel := st.window()
	defer cancel()
	defer st.finish()
	run(len(readers), func(i int) {
		random := rand.New(rand.NewPCG(uint64(i), 1))
		for ctx.Err() == nil && readers[i].alive() {
			if random.IntN(2) == 0 {
				_ = measure(ctx, st, "history latest 100", func() error {
					_, err := readers[i].call(ctx, "history", map[string]any{"room_id": roomID, "limit": 100})
					return err
				})
				continue
			}
			before := strconv.FormatInt(ids[random.IntN(len(ids))], 10)
			_ = measure(ctx, st, "history page 1000", func() error {
				_, err := readers[i].call(ctx, "history", map[string]any{"room_id": roomID, "before": before, "limit": 1000})
				return err
			})
		}
	})
}

const threadCount = 1000

func runThreads(h *hammer, st *stats) {
	setup, cancelSetup := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelSetup()
	creator := connect(h, 1, "weaver", nil)
	if len(creator) == 0 {
		return
	}
	defer closeAll(creator)
	started := time.Now()
	var next atomic.Int64
	threads := make([]string, threadCount)
	run(16, func(int) {
		for i := next.Add(1); i <= threadCount; i = next.Add(1) {
			id, err := createRoom(setup, creator[0], map[string]any{"parent_room_id": generalRoom, "title": fmt.Sprintf("Thread %d", i)})
			if err != nil {
				return
			}
			threads[i-1] = id
		}
	})
	threads = slices.DeleteFunc(threads, func(id string) bool { return id == "" })
	if len(threads) == 0 {
		st.note("created no threads")
		return
	}
	st.note("created %d threads in %s", len(threads), time.Since(started).Round(time.Millisecond))

	listers := connect(h, max(1, h.clients/2), "lister", nil)
	defer closeAll(listers)
	ctx, cancel := st.window()
	defer cancel()
	defer st.finish()
	run(h.clients, func(i int) {
		for n := 0; ctx.Err() == nil; n++ {
			if i < len(listers) {
				// Alternate the threads a client could join with the joined
				// rooms and their members.
				if n%2 == 0 {
					_ = measure(ctx, st, "room_list not_joined", func() error {
						_, err := listers[i].call(ctx, "room_list", map[string]any{"parent_room_id": generalRoom, "filter": "not_joined"})
						return err
					})
				} else {
					_ = measure(ctx, st, "room_list members", func() error {
						_, err := listers[i].call(ctx, "room_list", map[string]any{"filter": "joined", "members": true})
						return err
					})
				}
				continue
			}
			// A client signs in and lists its joined rooms with their
			// members in one round trip, then joins and leaves a thread;
			// each join and leave is a logged membership.
			var p *peer
			if measure(ctx, st, "auth+room_list", func() (err error) {
				p, err = h.dialGuest(ctx, "joiner", dialOptions{list: true})
				return err
			}) != nil {
				// The window may end after a successful sign-in.
				if p != nil {
					p.close()
				}
				continue
			}
			thread := threads[(i+n)%len(threads)]
			_ = measure(ctx, st, "room_join", func() error {
				_, err := p.call(ctx, "room_join", map[string]any{"room_id": thread})
				return err
			})
			_ = measure(ctx, st, "room_leave", func() error {
				_, err := p.call(ctx, "room_leave", map[string]any{"room_id": thread})
				return err
			})
			p.close()
		}
	})
}

func runEdits(h *hammer, st *stats) {
	peers := connect(h, h.clients, "editor", nil)
	defer closeAll(peers)
	ctx, cancel := st.window()
	defer cancel()
	defer st.finish()
	run(len(peers), func(i int) {
		p := peers[i]
		for ctx.Err() == nil && p.alive() {
			var id string
			if measure(ctx, st, "post", func() (err error) {
				id, err = postMessage(ctx, p, generalRoom, "draft")
				return err
			}) != nil {
				continue
			}
			for edit := range 3 {
				_ = measure(ctx, st, "edit", func() error {
					_, err := p.call(ctx, "message", map[string]any{"room_id": generalRoom, "message_id": id, "body": map[string]any{"text": fmt.Sprintf("edit %d", edit)}})
					return err
				})
			}
			for _, emojis := range [][]string{{"👍", "🎉"}, {}} {
				_ = measure(ctx, st, "reactions", func() error {
					_, err := p.call(ctx, "reactions", map[string]any{"message_id": id, "emojis": emojis})
					return err
				})
			}
			_ = measure(ctx, st, "delete", func() error {
				_, err := p.call(ctx, "message", map[string]any{"room_id": generalRoom, "message_id": id, "deleted": true})
				return err
			})
		}
	})
}

const uploadBytes = 64 << 10

func runEmbeds(h *hammer, st *stats) {
	streamers := max(1, h.clients/4)
	messages := make([]chan []byte, streamers)
	peers := connect(h, h.clients, "sharer", func(i int) dialOptions {
		if i >= streamers {
			return dialOptions{}
		}
		// Streamers learn their stream URL from the message broadcast.
		ch := make(chan []byte, 1024)
		messages[i] = ch
		return dialOptions{notify: func(method string, frame []byte) {
			if method == "message" {
				select {
				case ch <- frame:
				default:
				}
			}
		}}
	})
	defer closeAll(peers)
	// Idle keep-alive connections would otherwise count as server goroutines.
	defer httpClient.CloseIdleConnections()
	payload := bytes.Repeat([]byte{0xA5}, uploadBytes)
	ctx, cancel := st.window()
	defer cancel()
	defer st.finish()
	run(len(peers), func(i int) {
		for ctx.Err() == nil && peers[i].alive() {
			if i < streamers && messages[i] != nil {
				_ = measure(ctx, st, "stream 2s", func() error { return stream(ctx, peers[i], messages[i]) })
			} else {
				_ = measure(ctx, st, "upload+get 64KiB", func() error { return upload(ctx, peers[i], payload) })
			}
		}
	})
}

type writtenEmbed struct {
	Embeds []struct {
		WriteURL string `json:"write_url"`
	} `json:"embeds"`
	MessageID string `json:"message_id"`
}

func postEmbed(ctx context.Context, p *peer, kind string) (writtenEmbed, error) {
	var result writtenEmbed
	raw, err := p.call(ctx, "message", map[string]any{"room_id": generalRoom, "body": map[string]any{
		"text": kind, "embeds": []any{map[string]any{"kind": kind, "title": kind + ".bin"}},
	}})
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return result, err
	}
	if len(result.Embeds) != 1 {
		return result, fmt.Errorf("message result has %d write URLs", len(result.Embeds))
	}
	return result, nil
}

func upload(ctx context.Context, p *peer, payload []byte) error {
	written, err := postEmbed(ctx, p, "upload")
	if err != nil {
		return err
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodPut, written.Embeds[0].WriteURL, bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err := httpClient.Do(request)
	if err != nil {
		return err
	}
	var put struct {
		URL string `json:"url"`
	}
	err = json.UnmarshalRead(response.Body, &put)
	response.Body.Close()
	if response.StatusCode != http.StatusCreated || err != nil {
		return fmt.Errorf("PUT: %s", response.Status)
	}
	request, _ = http.NewRequestWithContext(ctx, http.MethodGet, put.URL, nil)
	response, err = httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	got, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, payload) {
		return fmt.Errorf("GET returned %d bytes, want %d", len(got), len(payload))
	}
	return nil
}

// stream writes 1 KiB chunks to a stream for two seconds while reading it
// back from its stream URL.
func stream(ctx context.Context, p *peer, messages chan []byte) error {
	// Discard broadcasts that arrived since the last stream.
	for len(messages) > 0 {
		<-messages
	}
	written, err := postEmbed(ctx, p, "stream")
	if err != nil {
		return err
	}
	streamURL, err := awaitStreamURL(ctx, messages, written.MessageID)
	if err != nil {
		return err
	}
	body, writer := io.Pipe()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPut, written.Embeds[0].WriteURL, body)
	request.Header.Set("Content-Type", "text/plain")
	posted := make(chan error, 1)
	go func() {
		response, err := httpClient.Do(request)
		if err == nil {
			response.Body.Close()
			if response.StatusCode != http.StatusNoContent {
				err = fmt.Errorf("stream PUT: %s", response.Status)
			}
		}
		posted <- err
	}()
	chunk := []byte(text(1 << 10))
	sent := 0
	var readErr error
	var got int64
	readDone := make(chan struct{})
	readStart := time.Now()
	var readTook time.Duration
	go func() {
		defer close(readDone)
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
		response, err := httpClient.Do(request)
		if err != nil {
			readErr = err
			return
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			readErr = fmt.Errorf("stream GET: %s", response.Status)
			return
		}
		got, readErr = io.Copy(io.Discard, response.Body)
		readTook = time.Since(readStart)
	}()
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end) && ctx.Err() == nil; {
		if _, err := writer.Write(chunk); err != nil {
			break
		}
		sent += len(chunk)
		time.Sleep(10 * time.Millisecond)
	}
	writer.Close()
	if err := <-posted; err != nil {
		return err
	}
	<-readDone
	if readErr != nil {
		return readErr
	}
	if got == 0 {
		return fmt.Errorf("stream reader got nothing of %d bytes; its response ended after %s", sent, readTook.Round(time.Millisecond))
	}
	return nil
}

func awaitStreamURL(ctx context.Context, messages chan []byte, messageID string) (string, error) {
	timeout := time.After(5 * time.Second)
	for {
		select {
		case frame := <-messages:
			var f struct {
				Params struct {
					MessageID string `json:"message_id"`
					Body      struct {
						Embeds []struct {
							URL string `json:"url"`
						} `json:"embeds"`
					} `json:"body"`
				} `json:"params"`
			}
			if json.Unmarshal(frame, &f) == nil && f.Params.MessageID == messageID && len(f.Params.Body.Embeds) == 1 {
				return f.Params.Body.Embeds[0].URL, nil
			}
		case <-timeout:
			return "", errors.New("no broadcast carried the stream URL")
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// bigExtNumber is above 2^53: a server that decodes ext into float64 loses
// it, and one that merges raw JSON keeps it byte for byte.
const bigExtNumber = "12345678901234567891"

// snapshotLog keeps the raw frame of the latest snapshot of each message a
// connection receives.
type snapshotLog struct {
	mu     sync.Mutex
	latest map[string][]byte
	logIDs map[string]int64
}

func (l *snapshotLog) notify(method string, frame []byte) {
	if method != "message" {
		return
	}
	var f struct {
		Params struct {
			MessageID string `json:"message_id"`
			LogID     string `json:"log_id"`
		} `json:"params"`
	}
	if json.Unmarshal(frame, &f) != nil || f.Params.LogID == "" {
		return
	}
	id, _ := strconv.ParseInt(f.Params.LogID, 10, 64)
	l.mu.Lock()
	defer l.mu.Unlock()
	if id > l.logIDs[f.Params.MessageID] {
		l.logIDs[f.Params.MessageID] = id
		l.latest[f.Params.MessageID] = frame
	}
}

// ext returns the ext of the latest snapshot of a message, with each value
// as the JSON it arrived as, and whether the snapshot carries ext.
func (l *snapshotLog) ext(messageID string) (map[string]string, bool) {
	l.mu.Lock()
	frame := l.latest[messageID]
	l.mu.Unlock()
	var f struct {
		Params struct {
			Ext map[string]jsontext.Value `json:"ext"`
		} `json:"params"`
	}
	if frame == nil || json.Unmarshal(frame, &f) != nil || f.Params.Ext == nil {
		return nil, false
	}
	ext := make(map[string]string, len(f.Params.Ext))
	for key, value := range f.Params.Ext {
		value = slices.Clone(value)
		_ = value.Compact()
		ext[key] = string(value)
	}
	return ext, true
}

// runExt saves messages through the ext merge's edge cases (§4.12, §4.4) and
// checks each resulting snapshot: a creation drops empty values and keeps
// integers beyond 2^53 exactly; two saves of different keys pipelined
// without waiting both survive, as concurrent saves must; "ext": {} and a
// save without ext change nothing; null is an ordinary value; empty values
// remove keys; and a tombstone carries no ext.
func runExt(h *hammer, st *stats) {
	logs := make([]*snapshotLog, h.clients)
	indexed := connectIndexed(h, h.clients, "extender", func(i int) dialOptions {
		logs[i] = &snapshotLog{latest: make(map[string][]byte), logIDs: make(map[string]int64)}
		return dialOptions{notify: logs[i].notify}
	})
	defer closeAll(compact(slices.Clone(indexed)))
	ctx, cancel := st.window()
	defer cancel()
	defer st.finish()
	run(len(indexed), func(i int) {
		p, log := indexed[i], logs[i]
		if p == nil {
			return
		}
		check := func(step, messageID string, want map[string]string) {
			got, has := log.ext(messageID)
			if want == nil && has || want != nil && !maps.Equal(got, want) {
				h.protocolViolation("ext %s: message %s has ext %v, want %v", step, messageID, got, want)
			}
		}
		save := func(op string, params map[string]any) error {
			return measure(ctx, st, op, func() error {
				_, err := p.call(ctx, "message", params)
				return err
			})
		}
		body := map[string]any{"text": "ext"}
		for n := 0; ctx.Err() == nil && p.alive(); n++ {
			var id string
			if measure(ctx, st, "ext create", func() error {
				raw, err := p.call(ctx, "message", map[string]any{"room_id": generalRoom, "body": body,
					"ext": map[string]any{"keep": 1, "big": jsontext.Value(bigExtNumber), "gone": ""}})
				if err != nil {
					return err
				}
				var result struct {
					MessageID string `json:"message_id"`
				}
				err = json.Unmarshal(raw, &result)
				id = result.MessageID
				return err
			}) != nil {
				continue
			}
			check("create", id, map[string]string{"keep": "1", "big": bigExtNumber})

			// Two saves of different keys, the second sent before the first
			// is answered.
			edit := func(ext map[string]any) map[string]any {
				params := map[string]any{"room_id": generalRoom, "message_id": id, "body": body}
				if ext != nil {
					params["ext"] = ext
				}
				return params
			}
			if measure(ctx, st, "ext pipelined saves", func() error {
				left, right := fmt.Sprintf("ext-left-%d", n), fmt.Sprintf("ext-right-%d", n)
				leftCh, err := p.expect(left)
				if err != nil {
					return err
				}
				rightCh, err := p.expect(right)
				if err != nil {
					return err
				}
				if err := p.send(ctx, left, "message", edit(map[string]any{"left": n})); err != nil {
					return err
				}
				if err := p.send(ctx, right, "message", edit(map[string]any{"right": n})); err != nil {
					return err
				}
				if _, err := p.wait(ctx, left, leftCh); err != nil {
					return err
				}
				_, err = p.wait(ctx, right, rightCh)
				return err
			}) != nil {
				continue
			}
			both := map[string]string{"keep": "1", "big": bigExtNumber, "left": strconv.Itoa(n), "right": strconv.Itoa(n)}
			check("pipelined saves", id, both)
			if save("ext save", edit(nil)) != nil {
				continue
			}
			check("save without ext", id, both)
			if save("ext save", edit(map[string]any{})) != nil {
				continue
			}
			check("ext {}", id, both)
			if save("ext save", edit(map[string]any{"keep": nil})) != nil {
				continue
			}
			check("null", id, map[string]string{"keep": "null", "big": bigExtNumber, "left": strconv.Itoa(n), "right": strconv.Itoa(n)})
			if save("ext save", edit(map[string]any{"keep": map[string]any{}, "left": "", "right": []any{}, "never": ""})) != nil {
				continue
			}
			check("empty values", id, map[string]string{"big": bigExtNumber})
			if save("ext delete", map[string]any{"room_id": generalRoom, "message_id": id, "deleted": true, "ext": map[string]any{"late": 1}}) != nil {
				continue
			}
			check("tombstone", id, nil)
		}
	})
}

// runSignIn signs guests in over and over and checks that nothing a
// sign-in causes reaches its connection before the auth result (§3.2):
// before it, only the server frame and transient notices may arrive.
func runSignIn(h *hammer, st *stats) {
	ctx, cancel := st.window()
	defer cancel()
	defer st.finish()
	run(h.clients, func(int) {
		for ctx.Err() == nil {
			var mu sync.Mutex
			answered := false
			var early []string
			options := dialOptions{
				notify: func(method string, frame []byte) {
					mu.Lock()
					defer mu.Unlock()
					if answered || method == "server" {
						return
					}
					var f struct {
						Params struct {
							MessageID *string `json:"message_id"`
						} `json:"params"`
					}
					if method == "message" && json.Unmarshal(frame, &f) == nil && f.Params.MessageID == nil {
						return
					}
					early = append(early, method)
				},
				onReply: func(id string) {
					mu.Lock()
					defer mu.Unlock()
					answered = true
				},
			}
			var p *peer
			_ = measure(ctx, st, "sign-in", func() (err error) {
				p, err = h.dialGuest(ctx, "signer", options)
				return err
			})
			if p != nil {
				mu.Lock()
				if len(early) > 0 {
					h.protocolViolation("%s arrived before the auth result of %s", strings.Join(early, ", "), p.userID)
				}
				mu.Unlock()
				p.close()
			}
		}
	})
}
