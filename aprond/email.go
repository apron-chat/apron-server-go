package aprond

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/apron-chat/apron-server-go/internal/auth/email"
)

// EmailSender delivers the codes of email sign-in (PROTOCOL.md §4.11). The
// server calls it on its own goroutine, outside any lock; an error is logged,
// and the person asks for another code.
type EmailSender interface {
	SendSignInCode(ctx context.Context, message SignInEmail) error
}

// SignInEmail is one email sign-in code to deliver.
type SignInEmail struct {
	// To is the address, as the server normalized it.
	To string
	// Code is the short token to type, six digits, which works on the
	// connection that made the proposal, or on another with the proposal's
	// state.
	Code string
	// Link opens Config.EmailLinkURL with the long token in its fragment,
	// which works on any connection: for a sign-in, one not signed in, and
	// for an addition, one signed in to the proposing account. Empty when
	// no link is configured.
	Link string
	// Server is this server's WebSocket URL that the proposal came in on,
	// as the link's server gives it, such as wss://chat.example/ws; empty
	// when it is not known. The email gives it with the code, and tells the
	// person to type the code only in an app connected to it (§4.11.2), so
	// that another server that proposed with their address cannot get the
	// code from them.
	Server string
	// Expires is when the code and the link stop working.
	Expires time.Time
	// Add reports a proposal to add the address to a signed-in account,
	// which signs nobody in.
	Add bool
}

// Text renders the message as plain text, for senders without templates.
func (m SignInEmail) Text() string {
	var b strings.Builder
	switch {
	case m.Server == "" && m.Add:
		fmt.Fprintf(&b, "Your code to add this address to your account is %s.\n", m.Code)
	case m.Server == "":
		fmt.Fprintf(&b, "Your sign-in code is %s.\n", m.Code)
	case m.Add:
		fmt.Fprintf(&b, "Your code to add this address to your account on %s is %s.\n", m.Server, m.Code)
	default:
		fmt.Fprintf(&b, "Your sign-in code for %s is %s.\n", m.Server, m.Code)
	}
	if m.Server != "" {
		fmt.Fprintf(&b, "Type it only in an app that is connected to %s.\n", m.Server)
	}
	switch {
	case m.Link == "":
	case m.Add:
		fmt.Fprintf(&b, "\nOr open this link where you are signed in to add it:\n%s\n", m.Link)
	default:
		fmt.Fprintf(&b, "\nOr open this link to sign in:\n%s\n", m.Link)
	}
	fmt.Fprintf(&b, "\nThe code expires in %d minutes. If you did not ask for it, ignore this email.\n", int(time.Until(m.Expires).Round(time.Minute)/time.Minute))
	return b.String()
}

// LogEmailSender logs sign-in codes instead of sending them, for
// development: whoever reads the server's log can sign in as any address.
type LogEmailSender struct {
	// Logger receives the codes; nil uses slog.Default().
	Logger *slog.Logger
}

// SendSignInCode logs the code and link.
func (l LogEmailSender) SendSignInCode(_ context.Context, m SignInEmail) error {
	logger := l.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("email sign-in code (not sent; development sender)", "to", m.To, "server", m.Server, "code", m.Code, "link", m.Link)
	return nil
}

// emailScheme is email sign-in delivering with sender.
func emailScheme(sender EmailSender, linkPage string) *email.Scheme {
	return email.New(func(ctx context.Context, m email.Message) error {
		return sender.SendSignInCode(ctx, SignInEmail{To: m.To, Code: m.Code, Link: m.Link, Server: m.Server, Expires: m.Expires, Add: m.Add})
	}, linkPage)
}
