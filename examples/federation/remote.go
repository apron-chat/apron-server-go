package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/time/rate"
)

// rpcError is an error reply (PROTOCOL.md §1.1).
type rpcError struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    jsontext.Value `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("error %d: %s", e.Code, e.Message) }

// Error codes the bridge acts on (§1.1).
const (
	codeInvalidParams = -32602
	codeDenied        = -32001
	codeRetryAfter    = -32002
)

// retryAfter is the delay a retry_after error asks for, 0 for another
// error.
func retryAfter(err error) time.Duration {
	e, ok := errors.AsType[*rpcError](err)
	if !ok || e.Code != codeRetryAfter {
		return 0
	}
	var data struct {
		RetryAfter float64 `json:"retry_after"`
	}
	_ = json.Unmarshal(e.Data, &data)
	return max(time.Second, time.Duration(data.RetryAfter*float64(time.Second)))
}

// errDisconnected is the error of a request whose connection closed before
// its reply came.
var errDisconnected = errors.New("the connection to the remote server closed")

// frame is a frame the remote server sent: a notification (Method), or
// the reply to a request (ID).
type frame struct {
	Method string         `json:"method"`
	ID     *string        `json:"id"`
	Params jsontext.Value `json:"params"`
	Result jsontext.Value `json:"result"`
	Error  *rpcError      `json:"error"`
}

type reply struct {
	result jsontext.Value
	err    *rpcError
}

// remoteConn is one WebSocket connection to the remote server, as a client
// (PROTOCOL.md §1): requests wait for their reply by id, and notifications
// go to notes in order. Its methods are safe for concurrent use.
type remoteConn struct {
	ws *websocket.Conn
	// pace spaces the frames the bridge sends, and slots bounds its
	// requests waiting for a reply: servers limit both per connection
	// (the Cloudflare server closes one past 60 frames a minute, or 8
	// requests in flight).
	pace  *rate.Limiter
	slots chan struct{}

	mu      sync.Mutex
	pending map[string]chan reply // nil once closed

	// server receives the server frames (§3.1), and notes every other
	// notification. A connection whose notes are not read falls behind and
	// is closed.
	server chan jsontext.Value
	notes  chan frame
	done   chan struct{}
	// err is why the connection closed, once done is: a read error, or an
	// error frame without id (§1.1).
	err error
}

// Pacing of the frames sent: a burst of sendBurst, then one each
// sendEvery, pings included, which stays under 60 a minute; and at most
// maxInFlight requests waiting for a reply.
const (
	sendBurst   = 5
	sendEvery   = 1200 * time.Millisecond
	maxInFlight = 4
)

// noteQueue bounds the notifications waiting to be handled; a bridge that
// falls further behind reconnects and catches up with history.
const noteQueue = 4096

func dialRemote(ctx context.Context, address string) (*remoteConn, error) {
	ws, _, err := websocket.Dial(ctx, address, nil)
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(16 << 20)
	c := &remoteConn{
		ws: ws, pending: make(map[string]chan reply),
		pace: rate.NewLimiter(rate.Every(sendEvery), sendBurst), slots: make(chan struct{}, maxInFlight),
		server: make(chan jsontext.Value, 4), notes: make(chan frame, noteQueue), done: make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

func (c *remoteConn) readLoop() {
	err := c.read()
	c.mu.Lock()
	pending := c.pending
	c.pending = nil
	c.mu.Unlock()
	for _, ch := range pending {
		close(ch)
	}
	c.err = err
	close(c.done)
	_ = c.ws.CloseNow()
}

func (c *remoteConn) read() error {
	for {
		_, data, err := c.ws.Read(context.Background())
		if err != nil {
			return err
		}
		var f frame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		switch {
		case f.Method == "server":
			select {
			case c.server <- f.Params:
			default:
			}
		case f.Method == "pong":
		case f.Method != "":
			select {
			case c.notes <- f:
			default:
				return errors.New("fell behind the remote server's notifications")
			}
		case f.ID == nil && f.Error != nil:
			// An error that is no reply ends the connection (§1.1).
			return f.Error
		case f.ID != nil:
			c.mu.Lock()
			ch := c.pending[*f.ID]
			delete(c.pending, *f.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- reply{f.Result, f.Error}
			}
		}
	}
}

// call sends a request with this id and waits for its reply. The id
// identifies the operation (§1.2): a retry of the same operation sends the
// same id and params.
func (c *remoteConn) call(ctx context.Context, id, method string, params any) (jsontext.Value, error) {
	request := map[string]any{"method": method, "id": id}
	if params != nil {
		request["params"] = params
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-c.done:
		return nil, errDisconnected
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := c.pace.Wait(ctx); err != nil {
		return nil, err
	}
	ch := make(chan reply, 1)
	c.mu.Lock()
	if c.pending == nil {
		c.mu.Unlock()
		return nil, errDisconnected
	}
	if c.pending[id] != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("request %s is already waiting", id)
	}
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.ws.Write(ctx, websocket.MessageText, payload); err != nil {
		c.mu.Lock()
		if c.pending != nil {
			delete(c.pending, id)
		}
		c.mu.Unlock()
		return nil, errors.Join(errDisconnected, err)
	}
	select {
	case r, ok := <-ch:
		switch {
		case !ok:
			return nil, errDisconnected
		case r.err != nil:
			return nil, r.err
		}
		return r.result, nil
	case <-ctx.Done():
		c.mu.Lock()
		if c.pending != nil {
			delete(c.pending, id)
		}
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

// ping sends {"method":"ping"} every interval until the connection closes
// (§1).
func (c *remoteConn) ping(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), interval)
			err := c.pace.Wait(ctx)
			if err == nil {
				err = c.ws.Write(ctx, websocket.MessageText, []byte(`{"method":"ping"}`))
			}
			cancel()
			if err != nil {
				return
			}
		case <-c.done:
			return
		}
	}
}

func (c *remoteConn) close() { _ = c.ws.Close(websocket.StatusNormalClosure, "bye") }
