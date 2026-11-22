// Package wirecore is package wire's protocol plumbing (PROTOCOL.md §1),
// which only wire imports: what another package needs of it, wire exports,
// such as wire.Marshal and wire.MaxRequestIDBytes. It holds parsing a frame
// into a request, the deterministic JSON encoding, the liveness ping and
// pong, deduplication of retried requests, and a connection's runtime: its
// bounded write queue, writer, pinger, and in-order frame worker.
//
// It depends on the standard library and github.com/coder/websocket only,
// as package wire does, since programs that embed wire import it too.
package wirecore
