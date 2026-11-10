package apron

import (
	"context"
	"errors"

	"github.com/apron-chat/apron-server-go/store"
)

// Durability. The server answers most requests once their changes are made
// in memory, and writes them to the store in the background, in order, a
// batch per lock (persist.go). Where an answer must mean that a change is
// stored, as for a new account or a revoked credential, the server holds the
// answer behind a fence: the batch that holds the change, and every batch
// before it, are applied first. A store that fails stays failed: the server
// writes nothing more, so the store keeps a consistent state as of the batch
// before the failure, and every fence after it fails.

// ErrStoreFailed is wrapped by the error Sync returns, and that
// Config.OnStoreError is given, once the store failed to apply a batch.
// The server writes nothing more after it.
var ErrStoreFailed = errors.New("apron: the store failed")

// storeBatch is what one flush queues for the store writer: the entries to
// apply as one, and the fences that wait for them.
type storeBatch struct {
	entries []store.Entry
	fences  []*storeFence
}

// storeFence is released once the batch it waits for, and every batch
// before it, are applied, or could not be: err is then the reason.
type storeFence struct {
	done chan struct{}
	err  error
}

func (f *storeFence) release(err error) {
	f.err = err
	close(f.done)
}

// newFenceLocked returns a fence that waits for the changes made so far,
// those the current lock made included, to be stored. Called holding s.mu,
// whose release (unlock) queues them.
func (s *Server) newFenceLocked() *storeFence {
	f := &storeFence{done: make(chan struct{})}
	s.fences = append(s.fences, f)
	return f
}

// fenceLocked holds the frames the connection is sent from now on until the
// changes made so far are stored. If they cannot be, the connection closes
// with an internal error instead of sending them, so that a reply queued
// after the fence, such as a new account's result, means its change is
// stored. Called holding s.mu.
func (c *client) fenceLocked() {
	f := c.server.newFenceLocked()
	if c.closing.Load() {
		return
	}
	select {
	case <-c.done:
	case c.out <- outboundBatch{fence: f}:
	default:
		c.stopConnection()
	}
}

// StoreErr is the store's failure, wrapping ErrStoreFailed, or nil while it
// has not failed.
func (s *Server) StoreErr() error {
	if err := s.storeErr.Load(); err != nil {
		return *err
	}
	return nil
}

// Sync waits until every change the server made before the call, such as
// an extension's Host.Store().Put, is in the store. It returns the store's
// failure (ErrStoreFailed), ErrServerClosed once the store is closed, or
// ctx's error if ctx ends first, when the changes may still be stored.
func (s *Server) Sync(ctx context.Context) error {
	s.mu.Lock()
	f := s.newFenceLocked()
	s.unlock()
	select {
	case <-f.done:
		return f.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Sync waits until every change the server made before the call, the
// extension's own Store().Put and Delete included, is in the store; see
// Server.Sync.
func (h *Host) Sync(ctx context.Context) error { return h.s.Sync(ctx) }
