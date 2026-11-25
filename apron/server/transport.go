package server

import (
	"bufio"
	"context"
	"io"
	"slices"
	"sync"
)

// Transport carries whole frames both ways (§1): a WebSocket, a line of
// newline-delimited JSON on a byte stream, or anything else.
type Transport interface {
	// Read returns the next frame from the client.
	Read(ctx context.Context) ([]byte, error)
	// Write sends one frame to the client.
	Write(ctx context.Context, frame []byte) error
	// Ping checks that the client is still there; a transport that cannot
	// tell returns nil.
	Ping(ctx context.Context) error
	// Close ends the transport at once.
	Close() error
}

// LineTransport carries newline-delimited JSON frames on a byte stream, such
// as a TCP connection, a pipe, or stdin and stdout (§1).
func LineTransport(rw io.ReadWriteCloser, maxFrame int) Transport {
	scanner := bufio.NewScanner(rw)
	// The scanner's limit is the larger of maxFrame and the buffer's
	// capacity.
	scanner.Buffer(make([]byte, 0, min(4096, maxFrame)), maxFrame)
	return &lineTransport{rw: rw, lines: scanner}
}

type lineTransport struct {
	rw    io.ReadWriteCloser
	lines *bufio.Scanner
	mu    sync.Mutex
}

func (t *lineTransport) Read(context.Context) ([]byte, error) {
	for t.lines.Scan() {
		if line := t.lines.Bytes(); len(line) > 0 {
			return slices.Clone(line), nil
		}
	}
	if err := t.lines.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

func (t *lineTransport) Write(_ context.Context, frame []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, err := t.rw.Write(append(slices.Clip(frame), '\n'))
	return err
}

func (t *lineTransport) Ping(context.Context) error { return nil }
func (t *lineTransport) Close() error               { return t.rw.Close() }
