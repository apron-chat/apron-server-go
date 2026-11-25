// Package core is the protocol plumbing (PROTOCOL.md §1) of packages apron
// and apron/server, which only they import: what another package needs of
// it, they export, such as apron.Marshal and server.MaxRequestIDBytes. It
// holds parsing a frame into a request, the deterministic JSON encoding,
// reading a log_id and cloning raw values, the liveness ping and pong,
// deduplication of retried requests, and a connection's runtime: its
// bounded write queue, writer, pinger, and in-order frame worker.
//
// It depends on the standard library and github.com/coder/websocket only,
// as the apron module does, since programs that embed it import it too.
package core
