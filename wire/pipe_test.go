package wire_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/wire"
)

// client is the other end of a LineTransport over net.Pipe, which reads a
// frame only when the test asks: what remains of the hand-rolled client
// that came before wiretest, for the tests still written for it. New tests
// use wiretest.Connect.
type client struct {
	t     *testing.T
	conn  net.Conn
	lines *bufio.Scanner
	done  chan struct{}
}

type frame map[string]any

func (f frame) errorCode() int {
	e, _ := f["error"].(map[string]any)
	code, _ := e["code"].(float64)
	return int(code)
}

func (f frame) result() map[string]any {
	r, _ := f["result"].(map[string]any)
	return r
}

// serve serves one connection of s over net.Pipe, and returns the client
// end, having read the server frame.
func serve(t *testing.T, s *wire.Server) (*client, frame) {
	t.Helper()
	server, conn := net.Pipe()
	c := &client{t: t, conn: conn, lines: bufio.NewScanner(conn), done: make(chan struct{})}
	c.lines.Buffer(nil, 1<<20)
	go func() {
		defer close(c.done)
		s.ServeTransport(context.Background(), wire.LineTransport(server, 1<<16), nil, nil)
	}()
	t.Cleanup(func() {
		_ = conn.Close()
		select {
		case <-c.done:
		case <-time.After(5 * time.Second):
			t.Error("ServeTransport did not return")
		}
	})
	hello := c.read()
	if hello["method"] != "server" {
		t.Fatalf("first frame = %v, want the server frame", hello)
	}
	return c, hello
}

func (c *client) send(raw string) {
	c.t.Helper()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c.conn, raw+"\n"); err != nil {
		c.t.Fatalf("send %s: %v", raw, err)
	}
}

func (c *client) read() frame {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if !c.lines.Scan() {
		c.t.Fatalf("read: %v", c.lines.Err())
	}
	var f frame
	if err := json.Unmarshal(c.lines.Bytes(), &f); err != nil {
		c.t.Fatalf("read %q: %v", c.lines.Bytes(), err)
	}
	return f
}
