package aprond

import (
	"testing"

	"github.com/apron-chat/apron-server-go/apron/conformance"
)

// TestConformance runs the protocol's core checks against a server with
// its state in memory, in process, as guests. Email sign-in is on, as
// for aprond --email.enable, so that the server also offers token.
func TestConformance(t *testing.T) {
	app, _ := newTestServer(t, Config{EmailSender: LogEmailSender{}})
	conformance.Run(t, conformance.Config{Server: app.srv})
}
