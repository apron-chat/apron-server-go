// Package wstest runs an apron server with extensions in a test, and
// connects to it as a WebSocket client, for the examples' tests.
package wstest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/apron-chat/apron-server-go/apron"
)

// Serve opens a server with extensions and an in-memory store, served by
// an httptest server, both closed when the test ends.
func Serve(t *testing.T, config apron.Config, extensions ...apron.Extension) (*apron.Server, *httptest.Server) {
	t.Helper()
	config.Extensions = extensions
	app, err := apron.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app.Handler())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := app.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		server.Close()
	})
	return app, server
}

// Client is a connection to a test server.
type Client struct {
	t      *testing.T
	ws     *websocket.Conn
	n      int
	UserID string
}

// Frame is a decoded frame.
type Frame map[string]any

// Params are a notification's params.
func (f Frame) Params() map[string]any {
	params, _ := f["params"].(map[string]any)
	return params
}

// Dial connects without signing in, and returns the server frame.
func Dial(t *testing.T, server *httptest.Server) (*Client, Frame) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	ws.SetReadLimit(1 << 24)
	t.Cleanup(func() { _ = ws.CloseNow() })
	c := &Client{t: t, ws: ws}
	return c, c.Read()
}

// Guest connects and signs in as a guest named name.
func Guest(t *testing.T, server *httptest.Server, name string) *Client {
	t.Helper()
	c, _ := Dial(t, server)
	result, err := c.Call("auth", map[string]any{"scheme": "guest", "name": name})
	if err != nil {
		t.Fatal(err)
	}
	c.UserID = result["you"].(map[string]any)["user_id"].(string)
	return c
}

// Call sends a request and returns its result, skipping the notifications
// before its reply, or its error.
func (c *Client) Call(method string, params map[string]any) (map[string]any, error) {
	c.t.Helper()
	c.n++
	id := fmt.Sprintf("r%d", c.n)
	payload, _ := json.Marshal(map[string]any{"method": method, "id": id, "params": params})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, payload); err != nil {
		c.t.Fatal(err)
	}
	for {
		frame := c.Read()
		if frame["id"] != id {
			continue
		}
		if failure, ok := frame["error"].(map[string]any); ok {
			return nil, fmt.Errorf("%v: %v", failure["code"], failure["message"])
		}
		result, _ := frame["result"].(map[string]any)
		return result, nil
	}
}

// Post posts text to a room and returns the message_id.
func (c *Client) Post(roomID, text string) (string, error) {
	c.t.Helper()
	result, err := c.Call("message", map[string]any{"room_id": roomID, "body": map[string]any{"text": text}})
	if err != nil {
		return "", err
	}
	return result["message_id"].(string), nil
}

// Command runs a command line in a room and returns the text of the
// notices it sent, then its error.
func (c *Client) Command(roomID, line string) ([]string, error) {
	c.t.Helper()
	c.n++
	id := fmt.Sprintf("r%d", c.n)
	payload, _ := json.Marshal(map[string]any{"method": "command", "id": id, "params": map[string]any{"room_id": roomID, "body": map[string]any{"text": line}}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, payload); err != nil {
		c.t.Fatal(err)
	}
	var notices []string
	for {
		frame := c.Read()
		if frame["id"] == id {
			if failure, ok := frame["error"].(map[string]any); ok {
				return notices, fmt.Errorf("%v: %v", failure["code"], failure["message"])
			}
			return notices, nil
		}
		if from, _ := frame.Params()["from"].(map[string]any); frame["method"] == "message" && from["user_id"] == "~private" {
			notices = append(notices, frame.Params()["body"].(map[string]any)["text"].(string))
		}
	}
}

// Closed reports whether the server closes the connection within a few
// seconds, reading what it sends until then.
func (c *Client) Closed() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := c.ws.Read(ctx); err != nil {
			return ctx.Err() == nil
		}
	}
}

// Read returns the next frame.
func (c *Client) Read() Frame {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, payload, err := c.ws.Read(ctx)
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	var frame Frame
	if err := json.Unmarshal(payload, &frame); err != nil {
		c.t.Fatal(err)
	}
	return frame
}

// Message reads frames until a message snapshot for which match is true,
// and returns its params.
func (c *Client) Message(match func(params map[string]any) bool) map[string]any {
	c.t.Helper()
	for {
		frame := c.Read()
		if frame["method"] == "message" && match(frame.Params()) {
			return frame.Params()
		}
	}
}

// Text is a message snapshot's body.text.
func Text(params map[string]any) string {
	body, _ := params["body"].(map[string]any)
	text, _ := body["text"].(string)
	return text
}

// From is a message snapshot's from.user_id.
func From(params map[string]any) string {
	from, _ := params["from"].(map[string]any)
	id, _ := from["user_id"].(string)
	return id
}
