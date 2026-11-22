package apron

import (
	"context"

	"github.com/apron-chat/apron-server-go/apron/internal/durable"
)

// Durability. The server answers most requests once their changes are made
// in memory, and writes them to the store in the background, in order, a
// batch per lock (persist.go), through a durable.Writer. Where an answer
// must mean that a change is stored, as for a new account or a revoked
// credential, the server holds the answer behind a fence: the batch that
// holds the change, and every batch before it, are applied first. A store
// that fails stays failed: the server writes nothing more, so the store
// keeps a consistent state as of the batch before the failure, and every
// fence after it fails.

// ErrStoreFailed is wrapped by the error Sync returns, and that
// Config.OnStoreError is given, once the store failed to apply a batch.
// The server writes nothing more after it.
var ErrStoreFailed = durable.ErrFailed

// newFenceLocked returns a fence that waits for the changes made so far,
// those the current lock made included, to be stored. Called holding s.mu,
// whose release (unlock) queues them.
func (s *Server) newFenceLocked() *durable.Fence {
	return s.store.Fence()
}

// fenceLocked holds the frames the connection is sent from now on until the
// changes made so far are stored. If they cannot be, the connection closes
// with an internal error instead of sending them, so that a reply queued
// after the fence, such as a new account's result, means its change is
// stored. Called holding s.mu.
func (c *client) fenceLocked() {
	c.conn.Fence(c.server.newFenceLocked())
}

// StoreErr is the store's failure, wrapping ErrStoreFailed, or nil while it
// has not failed.
func (s *Server) StoreErr() error {
	return s.store.Err()
}

// Sync waits until every change the server made before the call, such as
// an extension's Host.Store().Put, is in the store. It returns the store's
// failure (ErrStoreFailed), ErrServerClosed once the store is closed, or
// ctx's error if ctx ends first, when the changes may still be stored.
func (s *Server) Sync(ctx context.Context) error {
	s.mu.Lock()
	f := s.newFenceLocked()
	s.unlock()
	return f.Wait(ctx)
}

// Sync waits until every change the server made before the call, the
// extension's own Store().Put and Delete included, is in the store; see
// Server.Sync.
func (h *Host) Sync(ctx context.Context) error { return h.s.Sync(ctx) }
