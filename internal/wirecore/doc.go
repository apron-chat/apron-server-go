// Package wirecore is the protocol plumbing that package wire and package
// apron share (PROTOCOL.md §1): parsing a frame into a request, the
// deterministic JSON encoding, the liveness ping and pong, deduplication of
// retried requests, and a connection's runtime: its bounded write queue,
// writer, pinger, and in-order frame worker.
//
// It depends on the standard library and github.com/coder/websocket only,
// as package wire does, since programs that embed wire import it too.
package wirecore
