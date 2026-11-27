package core

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Writer is the side of a transport (§1) a Conn writes to.
type Writer interface {
	// Write sends one frame to the client.
	Write(ctx context.Context, frame []byte) error
	// Ping checks that the client is still there; a transport that cannot
	// tell returns nil.
	Ping(ctx context.Context) error
	// Close ends the transport at once.
	Close() error
}

// Fence holds a connection's writer until Done is closed. If Err is then
// not nil, the frames queued after the fence are never written: the
// connection writes Options.FenceFailed instead, and closes.
type Fence interface {
	Done() <-chan struct{}
	Err() error
}

// Options configure a Conn.
type Options struct {
	// Queue bounds the batches waiting to be written; a client that lets
	// more pile up is too slow to keep up, and is disconnected.
	Queue int
	// WriteTimeout bounds writing one frame.
	WriteTimeout time.Duration
	// PingInterval and PingTimeout pace the transport's pings (Start), and
	// how long a client that sent liveness pings may fall silent.
	PingInterval time.Duration
	PingTimeout  time.Duration
	// FenceFailed is the frame written in place of the frames behind a
	// fence that failed, before the connection closes.
	FenceFailed []byte
	// OnStop, if set, runs once as the connection stops, before the
	// transport closes.
	OnStop func()
	// OnFellBehind, if set, runs when a batch or fence finds the queue
	// full, before the connection stops.
	OnFellBehind func()
}

// batch is one place in the queue: frames written one after another, with
// nothing between them, or a fence. A batch of one frame, the most of
// them, is that frame, one, without a slice of frames to allocate.
type batch struct {
	one    []byte
	frames [][]byte
	// last closes the connection once the frames are written.
	last  bool
	fence Fence
}

// Conn is a connection's runtime: its bounded write queue, the writer and
// pinger goroutines that drain it and check the transport, and the reader
// and in-order worker of Serve. Its methods are safe for concurrent use.
type Conn struct {
	t       Writer
	options Options
	// queue is the batches waiting to be written, a ring of
	// Options.Queue, from head, of n of them, under qmu. idle is set while
	// the writer waits for a batch, which whoever queues the next wakes
	// it for, with a token on wake.
	qmu   sync.Mutex
	queue []batch
	head  int
	n     int
	idle  bool
	wake  chan struct{}
	done  chan struct{}
	stop  sync.Once
	// stopped is set as done is closed, for enqueue to tell without a
	// channel operation.
	stopped atomic.Bool
	// closing is set once the last batch is queued; nothing more is queued,
	// and Serve's worker drops the frames still waiting.
	closing atomic.Bool
	// pinged is set by the first liveness ping (§1); lastFrame is when the
	// latest frame arrived, in Unix nanoseconds.
	pinged    atomic.Bool
	lastFrame atomic.Int64
	// ctx is the writer's context, canceled as the connection stops.
	ctx    context.Context
	cancel context.CancelFunc
	// timer bounds each write (Options.WriteTimeout), once for the
	// connection rather than a context with a deadline for each frame:
	// deadline is when the write under way must be done, in Unix
	// nanoseconds, or 0 between writes, and armed is set while timer is.
	timer    *time.Timer
	deadline atomic.Int64
	armed    atomic.Bool
}

// NewConn returns the runtime of a connection on t. Start starts it.
func NewConn(t Writer, options Options) *Conn {
	c := &Conn{t: t, options: options, queue: make([]batch, max(options.Queue, 1)), wake: make(chan struct{}, 1), done: make(chan struct{})}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.lastFrame.Store(time.Now().UnixNano())
	if options.WriteTimeout > 0 {
		c.timer = time.AfterFunc(options.WriteTimeout, c.writeExpired)
		c.timer.Stop()
	}
	return c
}

// Start starts the writer and, with ping, the pinger.
func (c *Conn) Start(ping bool) {
	go c.writeLoop()
	if ping && c.options.PingInterval > 0 {
		go c.pingLoop()
	}
}

// Done is closed once the connection stops.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Closing reports whether the connection's last batch is queued, or it
// stopped.
func (c *Conn) Closing() bool { return c.closing.Load() }

// Queued is the number of batches waiting to be written.
func (c *Conn) Queued() int {
	c.qmu.Lock()
	defer c.qmu.Unlock()
	return c.n
}

// Send queues frames as one batch: written one after another, with no
// other frame between them, in one place in the queue. A full queue stops
// the connection. It reports false when the connection is closing or
// stopped, or fell behind.
func (c *Conn) Send(frames ...[]byte) bool {
	switch len(frames) {
	case 0:
		return true
	case 1:
		return c.enqueue(batch{one: frames[0]})
	}
	return c.enqueue(batch{frames: frames})
}

// SendFrame queues one frame, as Send does, without a slice of frames.
func (c *Conn) SendFrame(frame []byte) bool { return c.enqueue(batch{one: frame}) }

// Fence holds the writer, and so every frame queued after it, until f is
// done; see Fence.
func (c *Conn) Fence(f Fence) bool { return c.enqueue(batch{fence: f}) }

func (c *Conn) enqueue(b batch) bool {
	queued, wake := c.enqueueDeferred(b)
	if wake {
		c.Wake()
	}
	return queued
}

// SendFrameDeferred queues one frame as SendFrame does, but leaves the
// writer waiting for it, if it is: it reports whether the frame was
// queued, and whether the writer is to be woken for it with Wake, which
// the caller does once it has queued what else it queues, such as a
// broadcast's frame to every connection: so that waking the writers is
// not part of what it does while it holds a lock.
func (c *Conn) SendFrameDeferred(frame []byte) (queued, wake bool) {
	return c.enqueueDeferred(batch{one: frame})
}

// Wake wakes the writer for what SendFrameDeferred queued.
func (c *Conn) Wake() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Conn) enqueueDeferred(b batch) (queued, wake bool) {
	// A connection closing or stopped queues nothing, even when its
	// queue has room.
	if c.closing.Load() || c.stopped.Load() {
		return false, false
	}
	return c.push(b, c.options.OnFellBehind)
}

// push queues a batch, and reports whether the writer waits for it; a
// full queue stops the connection, once fellBehind, if set, has run.
func (c *Conn) push(b batch, fellBehind func()) (queued, wake bool) {
	c.qmu.Lock()
	if c.n == len(c.queue) {
		c.qmu.Unlock()
		// A slow client holds up neither its sender nor anyone else's.
		if fellBehind != nil {
			fellBehind()
		}
		c.Stop()
		return false, false
	}
	c.queue[(c.head+c.n)%len(c.queue)] = b
	c.n++
	wake = c.idle
	c.idle = false
	c.qmu.Unlock()
	return true, wake
}

// next returns the next batch to write, waiting for one, or reports false
// once the connection stopped.
func (c *Conn) next() (batch, bool) {
	for {
		if c.stopped.Load() {
			return batch{}, false
		}
		c.qmu.Lock()
		if c.n > 0 {
			b := c.queue[c.head]
			c.queue[c.head] = batch{}
			c.head = (c.head + 1) % len(c.queue)
			c.n--
			c.qmu.Unlock()
			return b, true
		}
		c.idle = true
		c.qmu.Unlock()
		select {
		case <-c.done:
			return batch{}, false
		case <-c.wake:
		}
	}
}

// SendLast queues the connection's last frame, such as an error about the
// connection as a whole (§1.1), after the frames already queued; the
// connection closes once it is written, and nothing more is queued. A full
// queue stops the connection at once. It reports whether the frame was
// queued; only the first call queues one.
func (c *Conn) SendLast(frame []byte) bool {
	if !c.closing.CompareAndSwap(false, true) {
		return false
	}
	if c.stopped.Load() {
		return false
	}
	queued, wake := c.push(batch{one: frame, last: true}, nil)
	if wake {
		c.Wake()
	}
	return queued
}

// Stop ends the connection at once.
func (c *Conn) Stop() {
	c.stop.Do(func() {
		c.stopped.Store(true)
		close(c.done)
		c.cancel()
		if c.timer != nil {
			c.timer.Stop()
		}
		if c.options.OnStop != nil {
			c.options.OnStop()
		}
		_ = c.t.Close()
	})
}

// Pong answers a liveness ping (§1), before sign-in too.
func (c *Conn) Pong() {
	c.pinged.Store(true)
	c.SendFrame(PongFrame)
}

// write writes one frame, within Options.WriteTimeout: a write still under
// way then stops the connection, which cancels the context the writer
// has, and closes the transport.
func (c *Conn) write(frame []byte) error {
	if c.timer == nil {
		return c.t.Write(c.ctx, frame)
	}
	c.deadline.Store(time.Now().Add(c.options.WriteTimeout).UnixNano())
	// The timer, once armed, stays armed while writes follow each other,
	// and writeExpired arms it again for the write under way.
	if c.armed.CompareAndSwap(false, true) {
		c.timer.Reset(c.options.WriteTimeout)
	}
	err := c.t.Write(c.ctx, frame)
	c.deadline.Store(0)
	return err
}

// writeExpired runs when the write timer fires: it stops the connection
// if the write under way is past its deadline, arms the timer again for
// one that is not, and else leaves it unarmed until the next write.
func (c *Conn) writeExpired() {
	for {
		if deadline := c.deadline.Load(); deadline != 0 {
			if left := time.Until(time.Unix(0, deadline)); left > 0 {
				c.timer.Reset(left)
				return
			}
			c.Stop()
			return
		}
		c.armed.Store(false)
		// A write that began before armed was cleared found the timer
		// armed, and left it to this.
		if c.deadline.Load() == 0 || !c.armed.CompareAndSwap(false, true) {
			return
		}
	}
}

func (c *Conn) writeLoop() {
	for {
		b, ok := c.next()
		if !ok {
			return
		}
		if b.fence != nil {
			select {
			case <-c.done:
				return
			case <-b.fence.Done():
			}
			if b.fence.Err() != nil {
				// What the frames behind it acknowledge did not happen:
				// the connection closes rather than send them.
				_ = c.write(c.options.FenceFailed)
				c.Stop()
				return
			}
			continue
		}
		if b.frames == nil {
			if err := c.write(b.one); err != nil {
				c.Stop()
				return
			}
		}
		for _, frame := range b.frames {
			select {
			case <-c.done:
				return
			default:
			}
			if err := c.write(frame); err != nil {
				c.Stop()
				return
			}
		}
		if b.last {
			// The last frame explains the close; a close handshake
			// would wait on a peer that may not be reading.
			c.Stop()
			return
		}
	}
}

// pingLoop pings the transport, which finds dead ones, and closes a
// connection whose client sent liveness pings (§1) and then fell silent
// for three intervals: its page is frozen or gone even if the transport is
// not. Three intervals leave room for background timer throttling.
func (c *Conn) pingLoop() {
	interval, timeout := c.options.PingInterval, c.options.PingTimeout
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	silence := 3*interval + timeout
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			if c.pinged.Load() && time.Since(time.Unix(0, c.lastFrame.Load())) > silence {
				c.Stop()
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			err := c.t.Ping(ctx)
			cancel()
			if err != nil {
				c.Stop()
				return
			}
		}
	}
}

// InboundQueue bounds the frames read from a connection and waiting for
// its worker; a client that sends more waits for them.
const InboundQueue = 64

// Serve reads frames with read until it fails or the connection stops.
// The exact liveness ping is answered at once, without waiting for the
// frames before it (§1). Every other frame goes to process, in order, each
// to completion before the next, on a worker goroutine of the
// connection's own, which makes auth a barrier (§3.2.2); the reader goes
// on reading meanwhile, which keeps the transport's pings answered while
// process works. Frames read before reading ended are still processed,
// unless the connection is closing. Once reading ends, readDone runs, if
// set, and Serve returns when the worker has finished.
func (c *Conn) Serve(read func() ([]byte, error), process func(payload []byte), readDone func()) {
	frames := make(chan []byte, InboundQueue)
	worked := make(chan struct{})
	go func() {
		defer close(worked)
		for payload := range frames {
			if !c.closing.Load() {
				process(payload)
			}
		}
	}()
	defer func() {
		if readDone != nil {
			readDone()
		}
		close(frames)
		<-worked
	}()
	for {
		payload, err := read()
		if err != nil {
			return
		}
		c.lastFrame.Store(time.Now().UnixNano())
		if bytes.Equal(payload, PingFrame) {
			c.Pong()
			continue
		}
		select {
		case frames <- payload:
		case <-c.done:
			return
		}
	}
}

// WebSocket is a WebSocket transport (§1), which carries text frames.
type WebSocket struct{ Conn *websocket.Conn }

var errBinaryFrame = errors.New("text frames required")

// Read returns the next frame. A binary frame closes the WebSocket.
func (t WebSocket) Read(ctx context.Context) ([]byte, error) {
	kind, payload, err := ReadMessage(ctx, t.Conn)
	if err != nil {
		return nil, err
	}
	if kind != websocket.MessageText {
		_ = t.Conn.Close(websocket.StatusUnsupportedData, "text frames required")
		return nil, errBinaryFrame
	}
	return payload, nil
}

// Write sends one text frame.
func (t WebSocket) Write(ctx context.Context, frame []byte) error {
	return t.Conn.Write(ctx, websocket.MessageText, frame)
}

// Ping pings at the WebSocket level.
func (t WebSocket) Ping(ctx context.Context) error { return t.Conn.Ping(ctx) }

// Close closes the WebSocket without a handshake.
func (t WebSocket) Close() error { return t.Conn.CloseNow() }

// readBuffers are the buffers ReadMessage reads into.
var readBuffers = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// ReadMessage reads the next message of a WebSocket, as Conn.Read does, into
// a buffer of a pool, and returns a copy of the size it has: Conn.Read
// grows a new buffer for each message, which for one of tens of kilobytes,
// such as a page of history, allocates it several times over.
func ReadMessage(ctx context.Context, ws *websocket.Conn) (websocket.MessageType, []byte, error) {
	kind, r, err := ws.Reader(ctx)
	if err != nil {
		return 0, nil, err
	}
	buf := readBuffers.Get().(*bytes.Buffer)
	defer func() {
		// A buffer grown for a large message is not kept.
		if buf.Cap() <= 1<<20 {
			buf.Reset()
			readBuffers.Put(buf)
		}
	}()
	if _, err := buf.ReadFrom(r); err != nil {
		return 0, nil, err
	}
	return kind, bytes.Clone(buf.Bytes()), nil
}
