package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/apron-chat/apron-server-go/apron"
)

// A write that outlives the write timeout, to a server that stopped
// reading, ends the connection; writes that do not leave it open.
func TestWriteTimeout(t *testing.T) {
	defer func(d time.Duration) { writeTimeout = d }(writeTimeout)
	writeTimeout = 200 * time.Millisecond
	stop := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		_ = ws.Write(r.Context(), websocket.MessageText, []byte(`{"method":"server","params":{"apron":10,"auth":["guest"]}}`))
		<-stop // and never reads
	}))
	defer ts.Close()
	defer close(stop)
	ctx := context.Background()
	c, err := Connect(ctx, "ws"+strings.TrimPrefix(ts.URL, "http"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Notify(ctx, "activity", map[string]any{"typing": 1}); err != nil {
		t.Fatalf("a write the server has room for: %v", err)
	}
	time.Sleep(2 * writeTimeout)
	if err := c.Err(); err != nil {
		t.Fatalf("an idle connection ended: %v", err)
	}
	big := map[string]string{"x": strings.Repeat("x", 4<<20)}
	start := time.Now()
	for c.Err() == nil && time.Since(start) < 10*time.Second {
		_ = c.Notify(ctx, "ext:big", big)
	}
	if err := c.Err(); !errors.Is(err, apron.ErrClosed) {
		t.Fatalf("after a stuck write: %v", err)
	}
}
