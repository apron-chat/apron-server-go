package aprond

import (
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron/conformance"
)

// TestConformance runs the protocol's core checks against a server with
// its state in memory, in process, as guests. Email sign-in is on, as
// for aprond --email.enable, so that the server also offers token.
func TestConformance(t *testing.T) {
	app, _ := newTestServer(t, Config{EmailSender: LogEmailSender{}})
	conformance.Run(t, conformance.Config{Server: app.srv})
}

// TestConformanceRetaining runs the checks against a server that retains
// a room's newest 50 records, sweeping as often as it can, so that
// history's bounds (§4.3.3) move under them.
func TestConformanceRetaining(t *testing.T) {
	interval := retentionInterval
	retentionInterval = 10 * time.Millisecond
	t.Cleanup(func() { retentionInterval = interval })
	app, _ := newTestServer(t, Config{EmailSender: LogEmailSender{}, HistoryRecords: 50})
	conformance.Run(t, conformance.Config{Server: app.srv})
}
