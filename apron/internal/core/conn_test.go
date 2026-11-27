package core

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// slowWriter takes delay to write each frame, or, with delay negative,
// until its context is done, as a transport whose client stopped reading.
type slowWriter struct {
	delay   time.Duration
	written atomic.Int64
}

func (w *slowWriter) Write(ctx context.Context, _ []byte) error {
	if w.delay < 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	select {
	case <-time.After(w.delay):
		w.written.Add(1)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (*slowWriter) Ping(context.Context) error { return nil }
func (*slowWriter) Close() error               { return nil }

// A write that takes longer than WriteTimeout stops the connection, and
// cancels the context its writer has; writes that each take less do not,
// however long they take together, nor does a connection that waits
// between them.
func TestWriteTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stuck := &slowWriter{delay: -1}
		c := NewConn(stuck, Options{Queue: 8, WriteTimeout: time.Second})
		c.Start(false)
		c.SendFrame([]byte(`{}`))
		start := time.Now()
		<-c.Done()
		if waited := time.Since(start); waited != time.Second {
			t.Fatalf("a stuck write stopped the connection after %v, want 1s", waited)
		}

		slow := &slowWriter{delay: 900 * time.Millisecond}
		c = NewConn(slow, Options{Queue: 8, WriteTimeout: time.Second})
		c.Start(false)
		for range 2 {
			for range 5 {
				c.SendFrame([]byte(`{}`))
			}
			time.Sleep(10 * time.Second)
			synctest.Wait()
		}
		select {
		case <-c.Done():
			t.Fatalf("writes within the timeout stopped the connection after %d frames", slow.written.Load())
		default:
		}
		if slow.written.Load() != 10 {
			t.Fatalf("%d frames written, want 10", slow.written.Load())
		}
		c.Stop()
	})
}

// countingWriter counts the frames written, and signals each.
type countingWriter struct {
	written atomic.Int64
	wrote   chan struct{}
}

func (w *countingWriter) Write(context.Context, []byte) error {
	w.written.Add(1)
	select {
	case w.wrote <- struct{}{}:
	default:
	}
	return nil
}

func (*countingWriter) Ping(context.Context) error { return nil }
func (*countingWriter) Close() error               { return nil }

// Every frame queued is written, whether its sender woke the writer at
// once or later (SendFrameDeferred), however senders interleave.
func TestQueueWakes(t *testing.T) {
	w := &countingWriter{wrote: make(chan struct{}, 1)}
	c := NewConn(w, Options{Queue: 1 << 16, WriteTimeout: time.Minute})
	c.Start(false)
	defer c.Stop()
	const senders, frames = 8, 2000
	done := make(chan struct{})
	for s := range senders {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := range frames {
				if (s+i)%3 == 0 {
					c.SendFrame([]byte(`{}`))
					continue
				}
				if _, wake := c.SendFrameDeferred([]byte(`{}`)); wake {
					if i%2 == 0 {
						go c.Wake()
					} else {
						c.Wake()
					}
				}
			}
		}()
	}
	for range senders {
		<-done
	}
	deadline := time.After(10 * time.Second)
	for w.written.Load() < senders*frames {
		select {
		case <-w.wrote:
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatalf("%d of %d frames written", w.written.Load(), senders*frames)
		}
	}
}
