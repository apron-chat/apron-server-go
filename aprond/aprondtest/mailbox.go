package aprondtest

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/apron-chat/apron-server-go/aprond"
)

// Mailbox is an aprond.EmailSender, for Config.EmailSender, that keeps the
// sign-in codes the server sends, with which Server.Account signs in, and
// Code reads. Its zero value is ready to use.
type Mailbox struct {
	mu      sync.Mutex
	sent    []aprond.SignInEmail
	changed chan struct{}
}

// SendSignInCode keeps the message.
func (m *Mailbox) SendSignInCode(_ context.Context, message aprond.SignInEmail) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, message)
	if m.changed != nil {
		close(m.changed)
		m.changed = nil
	}
	return nil
}

// Code takes the code of the first message to address it has not taken,
// waiting for one at most wait, and returns it, or "" if none came, as
// for an address that may not sign in.
func (m *Mailbox) Code(address string, wait time.Duration) string {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		m.mu.Lock()
		if i := slices.IndexFunc(m.sent, func(s aprond.SignInEmail) bool { return strings.EqualFold(s.To, address) }); i >= 0 {
			code := m.sent[i].Code
			m.sent = slices.Delete(m.sent, i, i+1)
			m.mu.Unlock()
			return code
		}
		if m.changed == nil {
			m.changed = make(chan struct{})
		}
		changed := m.changed
		m.mu.Unlock()
		select {
		case <-changed:
		case <-deadline.C:
			return ""
		}
	}
}
