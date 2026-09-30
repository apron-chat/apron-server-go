package main

import (
	"context"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"

	"github.com/apron-chat/apron-server-go/internal/server"
)

// smtpSender sends email sign-in codes through an SMTP relay, upgrading to
// TLS when the relay offers STARTTLS. Authentication, when a user is given,
// is PLAIN, which net/smtp sends only over TLS or to localhost.
type smtpSender struct {
	addr     string
	from     string
	username string
	password string
}

func newSMTPSender(addr, from, username, password string) (*smtpSender, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return nil, fmt.Errorf("invalid --email.smtp-addr %q: use host:port, such as smtp.example.com:587", addr)
	}
	if parsed, err := mail.ParseAddress(from); err != nil || from == "" {
		return nil, fmt.Errorf("invalid --email.from %q: an email address is required", from)
	} else {
		from = parsed.String()
	}
	return &smtpSender{addr: addr, from: from, username: username, password: password}, nil
}

func (s *smtpSender) SendSignInCode(ctx context.Context, m server.SignInEmail) error {
	sender, _ := mail.ParseAddress(s.from)
	var message strings.Builder
	fmt.Fprintf(&message, "From: %s\r\n", s.from)
	fmt.Fprintf(&message, "To: %s\r\n", m.To)
	fmt.Fprintf(&message, "Subject: Your sign-in code is %s\r\n", m.Code)
	fmt.Fprintf(&message, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	message.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	message.WriteString(strings.ReplaceAll(m.Text(), "\n", "\r\n"))
	var auth smtp.Auth
	if s.username != "" {
		host, _, _ := net.SplitHostPort(s.addr)
		auth = smtp.PlainAuth("", s.username, s.password, host)
	}
	done := make(chan error, 1)
	go func() { done <- smtp.SendMail(s.addr, auth, sender.Address, []string{m.To}, []byte(message.String())) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
