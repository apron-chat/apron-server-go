package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"math/big"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

const (
	// emailCodeLifetime is how long an email sign-in code stays valid.
	emailCodeLifetime = 10 * time.Minute
	// maxEmailAttempts wrong codes invalidate an address's code.
	maxEmailAttempts = 5
	// emailResendInterval is the least time between codes sent to one address.
	emailResendInterval = 30 * time.Second
	// emailSendsPerConnection codes may be requested on one connection at
	// once, refilled one per emailSendRefill.
	emailSendsPerConnection = 5
	emailSendRefill         = 2 * time.Minute
	// emailSendTimeout bounds one delivery by the EmailSender.
	emailSendTimeout = 30 * time.Second
	maxEmailBytes    = 254
	// accountIDPrefix starts the user_ids assigned to new email accounts
	// that did not request one.
	accountIDPrefix = "user_"
)

// EmailSender delivers the codes of email sign-in (PROTOCOL.md §4.10). The
// server calls it on its own goroutine, outside any lock; an error is logged,
// and the person asks for another code.
type EmailSender interface {
	SendSignInCode(ctx context.Context, message SignInEmail) error
}

// SignInEmail is one email sign-in code to deliver.
type SignInEmail struct {
	// To is the address, as the server normalized it.
	To string
	// Code is the temporary token, six digits.
	Code string
	// Link opens Config.EmailLinkURL with the address and code in its
	// fragment, or is empty when no link is configured.
	Link string
	// Expires is when the code stops working.
	Expires time.Time
}

// Text renders the message as plain text, for senders without templates.
func (m SignInEmail) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Your sign-in code is %s.\n", m.Code)
	if m.Link != "" {
		fmt.Fprintf(&b, "\nOr open this link to sign in:\n%s\n", m.Link)
	}
	fmt.Fprintf(&b, "\nThe code expires in %d minutes. If you did not ask for it, ignore this email.\n", int(time.Until(m.Expires).Round(time.Minute)/time.Minute))
	return b.String()
}

// LogEmailSender logs sign-in codes instead of sending them, for
// development: whoever reads the server's log can sign in as any address.
type LogEmailSender struct {
	Logger *slog.Logger
}

func (l LogEmailSender) SendSignInCode(_ context.Context, m SignInEmail) error {
	logger := l.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("email sign-in code (not sent; development sender)", "to", m.To, "code", m.Code, "link", m.Link)
	return nil
}

// emailCode is the outstanding sign-in code of one address.
type emailCode struct {
	code     string
	expires  time.Time
	sent     time.Time
	attempts int
}

// normalizeEmail returns a bare address, lowercased, or "" if value is not
// one.
func normalizeEmail(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || len(value) > maxEmailBytes {
		return ""
	}
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Name != "" || parsed.Address != value {
		return ""
	}
	return value
}

// signInLink builds the sign-in link from the configured page, never from
// request fields, with the address and code in the fragment so they stay out
// of server logs (§4.10).
func signInLink(page, email, code string) string {
	if page == "" {
		return ""
	}
	page, _, _ = strings.Cut(page, "#")
	return page + "#" + url.Values{"email": {email}, "token": {code}}.Encode()
}

// authenticateEmail runs the email scheme under s.mu (§4.10). Without token
// it sends a code to the address and returns {}, whether or not the address
// has an account, and authenticates nothing. With token it signs in: the
// address's account, or else a new one, and returns `you` with a bearer
// token for the token scheme. An address new to the server is added to the
// connection's identity when it has none yet, so a guest keeps their
// user_id, rooms, and messages; otherwise a new account is created, taking a
// requested user_id as guests do, or user_<n>.
func (s *Server) authenticateEmail(c *client, req request) (any, *rpcError) {
	// A notification has no request ID to answer and changes nothing.
	if !req.hasID {
		return nil, nil
	}
	raw, err := parseString(req.params, "email", true)
	if err != nil {
		return nil, err
	}
	email := normalizeEmail(raw)
	if email == "" {
		return nil, invalidParams("email must be an email address")
	}
	code, err := parseString(req.params, "token", false)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for address, pending := range s.emailCodes {
		if !now.Before(pending.expires) {
			delete(s.emailCodes, address)
		}
	}
	if _, has := req.params["token"]; !has {
		return s.sendEmailCodeLocked(c, req, email, now)
	}

	pending := s.emailCodes[email]
	if pending == nil {
		return nil, &rpcError{Code: codeDenied, Message: "The code is invalid or expired; ask for a new one"}
	}
	if subtle.ConstantTimeCompare([]byte(code), []byte(pending.code)) != 1 {
		if pending.attempts++; pending.attempts >= maxEmailAttempts {
			delete(s.emailCodes, email)
		}
		return nil, &rpcError{Code: codeDenied, Message: "The code is invalid or expired; ask for a new one"}
	}
	delete(s.emailCodes, email)

	user := s.emails[email]
	if user == nil {
		if c.user != nil && c.user.email == "" {
			user = c.user
		} else {
			name, err := parseString(req.params, "name", false)
			if err != nil {
				return nil, err
			}
			requested, err := parseString(req.params, "user_id", false)
			if err != nil {
				return nil, err
			}
			user = newUserState(s.assignAccountIDLocked(requested), normalizeName(name))
			s.users[user.id] = user
		}
		user.email = email
		s.emails[email] = user
		s.touchUser(user.id)
		if s.grantRolesLocked(user) && user == c.user {
			// The connection's identity became an account holding roles.
			s.notifyProfileLocked(user, c)
		}
		if len(user.joined) == 0 && user != c.user {
			// A new account joins the default room, as a new guest does.
			// The join is delivered to this connection only once it is
			// the account's; it precedes the result either way.
			s.attachLocked(c, user)
			s.addMemberLocked(user, s.rooms[defaultRoomID])
		}
	}
	return s.signInLocked(c, req, user, now)
}

// sendEmailCodeLocked replaces the address's code with a new one and sends
// it in the background. Codes to one address are at least
// emailResendInterval apart, and each connection may ask for a few; beyond
// either the request is retry_after, whether or not the address has an
// account.
func (s *Server) sendEmailCodeLocked(c *client, req request, email string, now time.Time) (any, *rpcError) {
	if pending := s.emailCodes[email]; pending != nil {
		if wait := pending.sent.Add(emailResendInterval).Sub(now); wait > 0 {
			return nil, retryAfter("A code was just sent to this address; try again shortly", wait)
		}
	}
	if c.emailSends == nil {
		c.emailSends = rate.NewLimiter(rate.Every(emailSendRefill), emailSendsPerConnection)
	}
	reservation := c.emailSends.ReserveN(now, 1)
	if wait := reservation.DelayFrom(now); wait > 0 {
		reservation.CancelAt(now)
		return nil, retryAfter("Too many sign-in codes requested; try again later", wait)
	}
	number, randErr := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if randErr != nil {
		return nil, &rpcError{Code: codeInternalError, Message: "Unable to create a sign-in code"}
	}
	pending := &emailCode{code: fmt.Sprintf("%06d", number), expires: now.Add(emailCodeLifetime), sent: now}
	s.emailCodes[email] = pending
	message := SignInEmail{To: email, Code: pending.code, Link: signInLink(s.config.EmailLinkURL, email, pending.code), Expires: pending.expires}
	sender := s.config.EmailSender
	s.mailing.Add(1)
	go func() {
		defer s.mailing.Done()
		ctx, cancel := context.WithTimeout(context.Background(), emailSendTimeout)
		defer cancel()
		if err := sender.SendSignInCode(ctx, message); err != nil {
			slog.Error("cannot send an email sign-in code", "error", err)
		}
	}()
	result := map[string]any{}
	c.sendResult(req, result)
	return result, nil
}

// assignAccountIDLocked honors a requested user_id as assignUserIDLocked
// does, and otherwise assigns the next unused user_<n>.
func (s *Server) assignAccountIDLocked(requested string) string {
	if requestable(requested) && s.claimUserIDLocked(requested) {
		return requested
	}
	for {
		s.accountNumber++
		if id := fmt.Sprintf("%s%d", accountIDPrefix, s.accountNumber); s.claimUserIDLocked(id) {
			return id
		}
	}
}

// retryAfter is a retry_after error suggesting wait, in whole seconds.
func retryAfter(message string, wait time.Duration) *rpcError {
	seconds := int64((wait + time.Second - 1) / time.Second)
	return &rpcError{Code: codeRetryAfter, Message: message, Data: map[string]any{"retry_after": seconds}}
}
