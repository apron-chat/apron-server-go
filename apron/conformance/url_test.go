package conformance

import (
	"os"
	"testing"
)

// TestURL checks the server at $APRON_CONFORMANCE_URL, such as
// ws://127.0.0.1:8080/ws, over a WebSocket, when it is set.
func TestURL(t *testing.T) {
	url := os.Getenv("APRON_CONFORMANCE_URL")
	if url == "" {
		t.Skip("APRON_CONFORMANCE_URL is not set")
	}
	Run(t, Config{URL: url})
}
