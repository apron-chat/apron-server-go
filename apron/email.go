package apron

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
	// Code is the short token to type, six digits, which works only on the
	// connection that made the proposal.
	Code string
	// Link opens Config.EmailLinkURL with the long token in its fragment,
	// which works on any connection: for a sign-in, one not signed in, and
	// for an addition, one signed in to the proposing account. Empty when
	// no link is configured.
	Link string
	// Expires is when the code and the link stop working.
	Expires time.Time
	// Add reports a proposal to add the address to a signed-in account,
	// which signs nobody in.
	Add bool
}

// Text renders the message as plain text, for senders without templates.
func (m SignInEmail) Text() string {
	var b strings.Builder
	if m.Add {
		fmt.Fprintf(&b, "Your code to add this address to your account is %s.\n", m.Code)
	} else {
		fmt.Fprintf(&b, "Your sign-in code is %s.\n", m.Code)
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
	logger.Info("email sign-in code (not sent; development sender)", "to", m.To, "code", m.Code, "link", m.Link)
	return nil
}

// emailScheme is email sign-in delivering with sender.
func emailScheme(sender EmailSender, linkPage string) *email.Scheme {
	return email.New(func(ctx context.Context, m email.Message) error {
		return sender.SendSignInCode(ctx, SignInEmail{To: m.To, Code: m.Code, Link: m.Link, Expires: m.Expires, Add: m.Add})
	}, linkPage)
}
