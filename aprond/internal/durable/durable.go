// Package durable writes a server's changes to a store.Store in the
// background, in order, and lets an answer wait until a change is stored.
//
// The server collects what it changed while it holds its lock and hands the
// Writer one batch of entries as it releases the lock (Writer.Queue). A
// single goroutine applies the batches in order, off the lock: those that
// wait while it applies one are applied together next, as one, so that a
// store that applies a batch as a transaction keeps up with changes that
// come faster than it commits. A Fence waits for the batch that holds a
// change, and every batch before it. A store that fails stays failed:
// nothing more is written, so the store keeps a consistent state as of the
// batches it applied before the failure, and every fence after them fails.
//
// The Writer knows nothing of what the entries hold. Its methods but Err are
// called holding the server's lock, which orders the batches.
package durable

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"

	"github.com/apron-chat/apron-server-go/store"
)

// queueLength bounds the batches waiting for the writer; beyond it, Queue
// waits for the store.
const queueLength = 1024

// mergeEntries bounds the entries of the batches waiting that the writer
// applies as one.
const mergeEntries = 4096

// batch is what one Queue hands the writer: the entries to apply as one,
// and the fences that wait for them.
type batch struct {
	entries []store.Entry
	fences  []*Fence
}

// Fence is released once the batch it waits for, and every batch before
// it, are applied, or could not be: Err then says why. It is a connection's
// fence (server.Conn.HoldUntil).
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
	// failedErr is wrapped by Err, and by the errors fences report, once
	// the store failed to apply a batch.
	failedErr error
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
// goroutine of its own, with the store's first failure, wrapping
// failedErr; closedErr is what fences report once the writer is stopped.
func Start(st store.Store, onError func(error), failedErr, closedErr error) *Writer {
	w := &Writer{
		store: st, onError: onError, failedErr: failedErr, closedErr: closedErr,
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

// Fenced reports whether a fence waits for the next Queue. Called holding
// the server's lock.
func (w *Writer) Fenced() bool { return len(w.fences) > 0 }

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

// Err is the store's failure, wrapping the Writer's failedErr, or nil while it has not
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
		b = w.merge(b)
		err := w.Err()
		if err == nil && len(b.entries) > 0 {
			if applyErr := w.store.Apply(b.entries); applyErr != nil {
				err = fmt.Errorf("%w: %w", w.failedErr, applyErr)
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

// merge appends the batches already waiting after b to it, in order, up to
// mergeEntries entries, so that they are applied as one, with all their
// fences released after it.
func (w *Writer) merge(b batch) batch {
	merged := false
	for len(b.entries) < mergeEntries {
		var next batch
		var ok bool
		select {
		case next, ok = <-w.batches:
		default:
		}
		if !ok {
			// None waits, or the queue is closed.
			break
		}
		if !merged {
			// The first batch's slice is the server's; a copy is appended to.
			b.entries = slices.Clip(b.entries)
			merged = true
		}
		b.entries = append(b.entries, next.entries...)
		b.fences = append(b.fences, next.fences...)
	}
	if merged {
		b.entries = latest(b.entries)
	}
	return b
}

// latest keeps the last of the entries with each kind and ID, in order: a
// merged batch applies as one, so an entry that a later one replaces, such
// as a room's for each message posted in it, need not be written.
func latest(entries []store.Entry) []store.Entry {
	type key struct{ kind, id string }
	seen := make(map[key]bool, len(entries))
	kept := entries
	n := len(entries)
	for i := len(entries) - 1; i >= 0; i-- {
		k := key{entries[i].Kind, entries[i].ID}
		if !seen[k] {
			seen[k] = true
			n--
			kept[n] = entries[i]
		}
	}
	return kept[n:]
}
