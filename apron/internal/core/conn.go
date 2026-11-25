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
// nothing between them, or a fence.
type batch struct {
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
	out     chan batch
	done    chan struct{}
	stop    sync.Once
	// closing is set once the last batch is queued; nothing more is queued,
	// and Serve's worker drops the frames still waiting.
	closing atomic.Bool
	// pinged is set by the first liveness ping (§1); lastFrame is when the
	// latest frame arrived, in Unix nanoseconds.
	pinged    atomic.Bool
	lastFrame atomic.Int64
}

// NewConn returns the runtime of a connection on t. Start starts it.
func NewConn(t Writer, options Options) *Conn {
	c := &Conn{t: t, options: options, out: make(chan batch, max(options.Queue, 1)), done: make(chan struct{})}
	c.lastFrame.Store(time.Now().UnixNano())
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
func (c *Conn) Queued() int { return len(c.out) }

// Send queues frames as one batch: written one after another, with no
// other frame between them, in one place in the queue. A full queue stops
// the connection. It reports false when the connection is closing or
// stopped, or fell behind.
func (c *Conn) Send(frames ...[]byte) bool {
	if len(frames) == 0 {
		return true
	}
	return c.enqueue(batch{frames: frames})
}

// Fence holds the writer, and so every frame queued after it, until f is
// done; see Fence.
func (c *Conn) Fence(f Fence) bool { return c.enqueue(batch{fence: f}) }

func (c *Conn) enqueue(b batch) bool {
	if c.closing.Load() {
		return false
	}
	// A stopped connection queues nothing, even when its queue has room: a
	// select with both cases ready picks either.
	select {
	case <-c.done:
		return false
	default:
	}
	select {
	case c.out <- b:
		return true
	default:
		// A slow client holds up neither its sender nor anyone else's.
		if c.options.OnFellBehind != nil {
			c.options.OnFellBehind()
		}
		c.Stop()
		return false
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
	select {
	case c.out <- batch{frames: [][]byte{frame}, last: true}:
		return true
	default:
		c.Stop()
		return false
	}
}

// Stop ends the connection at once.
func (c *Conn) Stop() {
	c.stop.Do(func() {
		close(c.done)
		if c.options.OnStop != nil {
			c.options.OnStop()
		}
		_ = c.t.Close()
	})
}

// Pong answers a liveness ping (§1), before sign-in too.
func (c *Conn) Pong() {
	c.pinged.Store(true)
	c.Send(PongFrame)
}

func (c *Conn) write(frame []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), c.options.WriteTimeout)
	defer cancel()
	return c.t.Write(ctx, frame)
}

func (c *Conn) writeLoop() {
	for {
		select {
		case <-c.done:
			return
		case b := <-c.out:
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
	kind, payload, err := t.Conn.Read(ctx)
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
