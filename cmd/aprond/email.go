package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"

	"github.com/apron-chat/apron-server-go/internal/server"
)

// smtpSender sends email sign-in codes through an SMTP relay. On port 465 it
// speaks TLS from the start; otherwise it requires STARTTLS unless insecure
// is set. It authenticates with PLAIN when a user is given. Every delivery is bounded by its context's deadline, the
// connection included.
type smtpSender struct {
	addr     string
	host     string
	from     string
	username string
	password string
	insecure bool
}

func newSMTPSender(addr, from, username, password string, insecure bool) (*smtpSender, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return nil, fmt.Errorf("invalid --email.smtp-addr %q: use host:port, such as smtp.example.com:587", addr)
	}
	parsed, err := mail.ParseAddress(from)
	if err != nil || from == "" {
		return nil, fmt.Errorf("invalid --email.from %q: an email address is required", from)
	}
	return &smtpSender{addr: addr, host: host, from: parsed.String(), username: username, password: password, insecure: insecure}, nil
}

// messageDomain is the domain of the sender address, for Message-ID.
func (s *smtpSender) messageDomain() string {
	parsed, err := mail.ParseAddress(s.from)
	if err != nil {
		return s.host
	}
	_, domain, _ := strings.Cut(parsed.Address, "@")
	return domain
}

// message renders a sign-in email.
func (s *smtpSender) message(m server.SignInEmail) []byte {
	var message strings.Builder
	fmt.Fprintf(&message, "From: %s\r\n", s.from)
	fmt.Fprintf(&message, "To: %s\r\n", m.To)
	subject := "Your sign-in code is %s"
	if m.Add {
		subject = "Your code to add this address is %s"
	}
	fmt.Fprintf(&message, "Subject: "+subject+"\r\n", m.Code)
	fmt.Fprintf(&message, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&message, "Message-ID: <%s@%s>\r\n", rand.Text(), s.messageDomain())
	message.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	message.WriteString(strings.ReplaceAll(m.Text(), "\n", "\r\n"))
	return []byte(message.String())
}

func (s *smtpSender) SendSignInCode(ctx context.Context, m server.SignInEmail) error {
	var conn net.Conn
	var err error
	_, port, _ := net.SplitHostPort(s.addr)
	implicitTLS := port == "465"
	if implicitTLS {
		conn, err = (&tls.Dialer{Config: &tls.Config{ServerName: s.host}}).DialContext(ctx, "tcp", s.addr)
	} else {
		conn, err = (&net.Dialer{}).DialContext(ctx, "tcp", s.addr)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	// Closing the connection when the context ends interrupts any exchange
	// still waiting on the relay.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	client, err := smtp.NewClient(conn, s.host)
	if err != nil {
		return err
	}
	defer client.Close()
	if ok, _ := client.Extension("STARTTLS"); !implicitTLS && ok {
		if err := client.StartTLS(&tls.Config{ServerName: s.host}); err != nil {
			return err
		}
	} else if !implicitTLS && !s.insecure {
		return errors.New("the SMTP relay does not offer STARTTLS; set --email.smtp-insecure to send in cleartext")
	}
	if s.username != "" {
		// net/smtp sends PLAIN credentials only over TLS or to localhost.
		if err := client.Auth(smtp.PlainAuth("", s.username, s.password, s.host)); err != nil {
			return err
		}
	}
	sender, _ := mail.ParseAddress(s.from)
	if err := client.Mail(sender.Address); err != nil {
		return err
	}
	if err := client.Rcpt(m.To); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(s.message(m)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return client.Quit()
}
