// Package durable writes a server's changes to a store.Store in the
// background, in order, and lets an answer wait until a change is stored.
//
// The server collects what it changed while it holds its lock and hands the
// Writer one batch of entries as it releases the lock (Writer.Queue). A
// single goroutine applies the batches in order, off the lock. A Fence waits
// for the batch that holds a change, and every batch before it. A store that
// fails stays failed: nothing more is written, so the store keeps a
// consistent state as of the batch before the failure, and every fence after
// it fails.
//
// The Writer knows nothing of what the entries hold. Its methods but Err are
// called holding the server's lock, which orders the batches.
package durable

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/apron-chat/apron-server-go/store"
)

// ErrFailed is wrapped by Err, and by the errors fences report, once the
// store failed to apply a batch.
var ErrFailed = errors.New("apron: the store failed")

// queueLength bounds the batches waiting for the writer; beyond it, Queue
// waits for the store.
const queueLength = 1024

// batch is what one Queue hands the writer: the entries to apply as one,
// and the fences that wait for them.
type batch struct {
	entries []store.Entry
	fences  []*Fence
}

// Fence is released once the batch it waits for, and every batch before
// it, are applied, or could not be: Err then says why. It is a connection's
// fence (wire.Fence).
type Fence struct {
	done chan struct{}
	err  error
}

func (f *Fence) release(err error) {
	f.err = err
	close(f.done)
}

// Done is closed once the fence is released.
func (f *Fence) Done() <-chan struct{} { return f.done }

// Err is why the changes the fence waits for are not stored, once Done is
// closed: the store's failure, or the Writer's closed error.
func (f *Fence) Err() error { return f.err }

// Wait waits for the fence and returns its Err, or ctx's error if ctx ends
// first, when the changes may still be stored.
func (f *Fence) Wait(ctx context.Context) error {
	select {
	case <-f.done:
		return f.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Writer applies batches to a store in order on a goroutine of its own.
type Writer struct {
	store   store.Store
	onError func(error)
	// closedErr releases the fences that wait once the Writer is stopped.
	closedErr error

	batches chan batch
	done    chan struct{}
	// stopped and fences are guarded by the server's lock: fences wait
	// for the next batch queued.
	stopped bool
	fences  []*Fence
	// err is the store's first failure, after which nothing more is
	// written.
	err atomic.Pointer[error]
}

// Start starts a writer to st. onError, if set, is called once, on a
// goroutine of its own, with the store's first failure; closedErr is what
// fences report once the writer is stopped.
func Start(st store.Store, onError func(error), closedErr error) *Writer {
	w := &Writer{
		store: st, onError: onError, closedErr: closedErr,
		batches: make(chan batch, queueLength),
		done:    make(chan struct{}),
	}
	go w.run()
	return w
}

// Fence returns a fence that waits for the changes made so far, those of
// the next Queue included. Called holding the server's lock.
func (w *Writer) Fence() *Fence {
	f := &Fence{done: make(chan struct{})}
	w.fences = append(w.fences, f)
	return f
}

// Stopped reports whether Stop was called: nothing more is written.
// Called holding the server's lock.
func (w *Writer) Stopped() bool { return w.stopped }

// Queue hands the writer entries to apply as one batch, with the fences
// made since the last Queue; once stopped, it drops them and releases the
// fences with the closed error. Called holding the server's lock.
func (w *Writer) Queue(entries []store.Entry) {
	fences := w.fences
	w.fences = nil
	if w.stopped {
		for _, f := range fences {
			f.release(w.closedErr)
		}
		return
	}
	if len(entries) > 0 || len(fences) > 0 {
		w.batches <- batch{entries: entries, fences: fences}
	}
}

// Stop ends the writer once it applied what was queued, and waits for it.
// It does not close the store. Called holding the server's lock; the
// writer never takes it.
func (w *Writer) Stop() {
	if w.stopped {
		return
	}
	w.stopped = true
	close(w.batches)
	<-w.done
}

// Err is the store's failure, wrapping ErrFailed, or nil while it has not
// failed. It may be called without the server's lock.
func (w *Writer) Err() error {
	if err := w.err.Load(); err != nil {
		return *err
	}
	return nil
}

// run applies batches to the store in order until the queue closes, then
// releases the fences behind each. After the store's first failure it
// applies nothing more, and every later fence fails.
func (w *Writer) run() {
	defer close(w.done)
	for b := range w.batches {
		err := w.Err()
		if err == nil && len(b.entries) > 0 {
			if applyErr := w.store.Apply(b.entries); applyErr != nil {
				err = fmt.Errorf("%w: %w", ErrFailed, applyErr)
				w.err.Store(&err)
				slog.Error("cannot write to the store; the server writes nothing more", "error", applyErr)
				if w.onError != nil {
					// On its own goroutine, so that it may shut the server
					// down, which waits for this writer.
					go w.onError(err)
				}
			}
		}
		for _, f := range b.fences {
			f.release(err)
		}
	}
}
