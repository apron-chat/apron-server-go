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
	// emailWindow is the rolling window of the per-address budgets: at most
	// maxEmailSendsPerWindow codes sent to an address, and at most
	// maxEmailFailuresPerWindow wrong codes for it across its codes, after
	// which its sign-in is refused until the window passes.
	emailWindow               = time.Hour
	maxEmailSendsPerWindow    = 6
	maxEmailFailuresPerWindow = 10
	// maxOutstandingEmailCodes bounds the addresses with a code outstanding.
	maxOutstandingEmailCodes = 10_000
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

// emailAddress is what the server keeps about one address's sign-in, in
// memory only: its outstanding code, if any, and the sends and failures in
// the last emailWindow.
type emailAddress struct {
	code     string
	expires  time.Time
	attempts int
	// requestedBy is the user_id signed in on the connection that asked
	// for the code, or empty.
	requestedBy string
	sends       []time.Time
	failures    []time.Time
}

// prune forgets an expired code and budget entries older than emailWindow,
// and reports whether nothing is left to keep.
func (a *emailAddress) prune(now time.Time) bool {
	if a.code != "" && !now.Before(a.expires) {
		a.code = ""
	}
	recent := func(times []time.Time) []time.Time {
		for len(times) > 0 && !now.Before(times[0].Add(emailWindow)) {
			times = times[1:]
		}
		return times
	}
	a.sends, a.failures = recent(a.sends), recent(a.failures)
	return a.code == "" && len(a.sends) == 0 && len(a.failures) == 0
}

// lockedFor is how long the address's sign-in stays refused after it used
// its budget of wrong codes, or 0.
func (a *emailAddress) lockedFor(now time.Time) time.Duration {
	if len(a.failures) < maxEmailFailuresPerWindow {
		return 0
	}
	return a.failures[0].Add(emailWindow).Sub(now)
}

// normalizeEmail returns a bare address, lowercased, or "" if value is not
// one. The domain must be a DNS name with a dot: address literals such as
// a@[10.0.0.5] and single labels such as a@localhost are refused, so codes
// go only to public mail domains.
func normalizeEmail(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || len(value) > maxEmailBytes {
		return ""
	}
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Name != "" || parsed.Address != value {
		return ""
	}
	_, domain, _ := strings.Cut(value, "@")
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return ""
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' ||
			strings.Trim(label, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
			return ""
		}
	}
	if strings.Trim(labels[len(labels)-1], "0123456789") == "" {
		return "" // an IPv4 address, not a domain
	}
	return value
}

// signInLink builds the sign-in link from the configured page, never from
// request fields, with the address and code in the fragment so they stay out
// of server logs (§4.10): #email=<address>&token=<code>, and &server=<URL>
// with this server's public WebSocket URL when it is known, so a client that
// speaks to several servers presents the code to the right one. Values are
// form-encoded.
func signInLink(page, email, code, server string) string {
	if page == "" {
		return ""
	}
	page, _, _ = strings.Cut(page, "#")
	link := page + "#email=" + url.QueryEscape(email) + "&token=" + url.QueryEscape(code)
	if server != "" {
		link += "&server=" + url.QueryEscape(server)
	}
	return link
}

// publicWebSocketURL is the WebSocket endpoint under Config.PublicURL, such
// as wss://chat.example/ws, or "" when no public URL is configured.
func (s *Server) publicWebSocketURL() string {
	parsed, err := url.Parse(s.config.PublicURL)
	if s.config.PublicURL == "" || err != nil {
		return ""
	}
	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	default:
		return ""
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/ws"
	parsed.RawQuery, parsed.Fragment = "", ""
	return parsed.String()
}

// authenticateEmail runs the email scheme under s.mu (§4.10). Without token
// it sends a code to the address and returns {}, whether or not the address
// has an account, and authenticates nothing. With token it signs in to the
// address's account, or to a new one for an address new to the server,
// taking a requested user_id as guests do, or user_<n>, and returns `you`
// with a bearer token for the token scheme.
//
// On a connection already signed in, a code adds the address to that
// identity, which a guest becomes an account by, but only when the same
// identity requested the code: a code proves only that its presenter reads
// the address's mail, not that the address belongs to whoever is signed in,
// so a code requested by anyone else is denied there, as is an address
// another account holds.
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
		return nil, invalidParams("email must be an email address at a domain name")
	}
	code, err := parseString(req.params, "token", false)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for address, state := range s.emailAddresses {
		if state.prune(now) {
			delete(s.emailAddresses, address)
		}
	}
	state := s.emailAddresses[email]
	if state == nil {
		state = &emailAddress{}
	}
	if _, has := req.params["token"]; !has {
		return s.sendEmailCodeLocked(c, req, email, state, now)
	}

	if wait := state.lockedFor(now); wait > 0 {
		return nil, &rpcError{Code: codeDenied, Message: "Too many wrong codes for this address; try again later", Data: map[string]any{"retry_after": seconds(wait)}}
	}
	if state.code == "" || subtle.ConstantTimeCompare([]byte(code), []byte(state.code)) != 1 {
		if state.code != "" {
			// Only a guess at an outstanding code counts toward the budget.
			state.failures = append(state.failures, now)
			if state.attempts++; state.attempts >= maxEmailAttempts {
				state.code = ""
			}
			s.emailAddresses[email] = state
		}
		return nil, &rpcError{Code: codeDenied, Message: "The code is invalid or expired; ask for a new one"}
	}
	owner := s.emails[email]
	if c.user != nil && owner != c.user {
		// On a signed-in connection a code adds the address to that
		// account (§4.10), but only when that account asked for it: a code
		// from anyone else, such as one in a link an attacker sent, would
		// let its requester sign in as this account later. An address is
		// never moved from another account, and an account has one.
		switch {
		case owner != nil:
			return nil, &rpcError{Code: codeDenied, Message: "This address belongs to another account; sign out to sign in with it"}
		case state.requestedBy != c.user.id:
			return nil, &rpcError{Code: codeDenied, Message: "This code was not requested by this account; sign out to sign in with it"}
		case c.user.email != "":
			return nil, &rpcError{Code: codeDenied, Message: "This account already has an email address"}
		}
		state.code = ""
		c.user.email = email
		s.emails[email] = c.user
		s.touchUser(c.user.id)
		if s.grantRolesLocked(c.user) {
			// The connection's identity became an account holding roles.
			s.notifyProfileLocked(c.user, c)
		}
		return s.signInLocked(c, req, c.user, now)
	}
	state.code = ""

	user := owner
	if user == nil {
		name, err := parseString(req.params, "name", false)
		if err != nil {
			return nil, err
		}
		requested, err := parseString(req.params, "user_id", false)
		if err != nil {
			return nil, err
		}
		user = newUserState(s.assignAccountIDLocked(requested), normalizeName(name))
		user.email = email
		s.users[user.id] = user
		s.emails[email] = user
		s.touchUser(user.id)
		s.grantRolesLocked(user)
		// A new account joins the default room, as a new guest does, and
		// the join reaches this connection before the result.
		s.attachLocked(c, user)
		s.addMemberLocked(user, s.rooms[defaultRoomID])
	}
	return s.signInLocked(c, req, user, now)
}

// sendEmailCodeLocked replaces the address's code with a new one and sends
// it in the background. Codes to one address are at least
// emailResendInterval apart and at most maxEmailSendsPerWindow an hour, none
// while its sign-in is refused, and each connection may ask for a few; the
// server keeps at most maxOutstandingEmailCodes. Beyond any of these the
// request is retry_after, whether or not the address has an account.
func (s *Server) sendEmailCodeLocked(c *client, req request, email string, state *emailAddress, now time.Time) (any, *rpcError) {
	if n := len(state.sends); n > 0 {
		if wait := state.sends[n-1].Add(emailResendInterval).Sub(now); wait > 0 {
			return nil, retryAfter("A code was just sent to this address; try again shortly", wait)
		}
		if n >= maxEmailSendsPerWindow {
			return nil, retryAfter("Too many codes were sent to this address; try again later", state.sends[0].Add(emailWindow).Sub(now))
		}
	}
	if wait := state.lockedFor(now); wait > 0 {
		return nil, retryAfter("Too many wrong codes for this address; try again later", wait)
	}
	if s.emailAddresses[email] == nil && len(s.emailAddresses) >= maxOutstandingEmailCodes {
		return nil, retryAfter("Too many sign-ins in progress; try again shortly", emailResendInterval)
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
	state.code, state.expires, state.attempts = fmt.Sprintf("%06d", number), now.Add(emailCodeLifetime), 0
	state.requestedBy = ""
	if c.user != nil {
		state.requestedBy = c.user.id
	}
	state.sends = append(state.sends, now)
	s.emailAddresses[email] = state
	message := SignInEmail{To: email, Code: state.code, Link: signInLink(s.config.EmailLinkURL, email, state.code, s.publicWebSocketURL()), Expires: state.expires}
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
	return &rpcError{Code: codeRetryAfter, Message: message, Data: map[string]any{"retry_after": seconds(wait)}}
}

// seconds rounds a wait up to whole seconds, at least one.
func seconds(wait time.Duration) int64 {
	return max(1, int64((wait+time.Second-1)/time.Second))
}
