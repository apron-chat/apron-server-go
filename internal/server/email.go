package server

import (
	"container/list"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

const (
	// emailCodeLifetime is how long an email code stays valid.
	emailCodeLifetime = 10 * time.Minute
	// maxEmailAttempts wrong codes invalidate a code.
	maxEmailAttempts = 5
	// emailResendInterval is the least time between codes sent to one address.
	emailResendInterval = 30 * time.Second
	// emailWindow is the rolling window of the per-address budgets: at most
	// maxEmailSendsPerWindow codes sent to an address, and at most
	// maxAddressFailuresPerWindow wrong codes for it, from anyone, after
	// which codes for it are refused until the window passes. The budget of
	// one guesser is tighter: see clientFailureBurst.
	emailWindow                 = time.Hour
	maxEmailSendsPerWindow      = 6
	maxAddressFailuresPerWindow = 30
	// Per client address (IP, or IPv6 /64): a burst of clientSendBurst
	// codes, refilled one per clientSendRefill, and of clientFailureBurst
	// wrong codes, refilled one per clientFailureRefill, across addresses.
	clientSendBurst      = 10
	clientSendRefill     = 3 * time.Minute
	clientFailureBurst   = 10
	clientFailureRefill  = 6 * time.Minute
	maxTrackedEmailState = 10_000
	maxTrackedClients    = 100_000
	// maxConcurrentEmailSends bounds deliveries in progress; beyond it a
	// code request is retry_after.
	maxConcurrentEmailSends = 16
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
	// Add reports a code asked for by a signed-in account to add the
	// address to it, which signs nobody in and carries no link.
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

// emailCode is one outstanding code.
type emailCode struct {
	code     string
	expires  time.Time
	attempts int
}

// matches reports whether value is the code, and counts a wrong guess,
// reporting whether the code is used up.
func (c *emailCode) guess(value string) (right, spent bool) {
	if subtle.ConstantTimeCompare([]byte(value), []byte(c.code)) == 1 {
		return true, true
	}
	c.attempts++
	return false, c.attempts >= maxEmailAttempts
}

// emailAddress is what the server keeps about one address, in memory only:
// its outstanding sign-in code, for connections not signed in, and the
// sends and wrong codes of the last emailWindow. Entries are kept in least
// recently used order, and the oldest is forgotten when there are
// maxTrackedEmailState, so a full table never refuses anyone.
type emailAddress struct {
	email    string
	signIn   *emailCode
	sends    []time.Time
	failures []time.Time
	element  *list.Element
}

// prune forgets an expired code and budget entries older than emailWindow,
// and reports whether nothing is left to keep.
func (a *emailAddress) prune(now time.Time) bool {
	if a.signIn != nil && !now.Before(a.signIn.expires) {
		a.signIn = nil
	}
	recent := func(times []time.Time) []time.Time {
		for len(times) > 0 && !now.Before(times[0].Add(emailWindow)) {
			times = times[1:]
		}
		return times
	}
	a.sends, a.failures = recent(a.sends), recent(a.failures)
	return a.signIn == nil && len(a.sends) == 0 && len(a.failures) == 0
}

// sendTimes is the address's sends in the window, none for no entry.
func (a *emailAddress) sendTimes() []time.Time {
	if a == nil {
		return nil
	}
	return a.sends
}

// lockedFor is how long codes for the address stay refused after its
// budget of wrong codes ran out, or 0.
func (a *emailAddress) lockedFor(now time.Time) time.Duration {
	if a == nil || len(a.failures) < maxAddressFailuresPerWindow {
		return 0
	}
	return a.failures[0].Add(emailWindow).Sub(now)
}

// emailClient is the budget of one client address, kept like emailAddress.
type emailClient struct {
	key      string
	sends    *rate.Limiter
	failures *rate.Limiter
	element  *list.Element
}

// pendingAdd is the code an account asked for, while signed in, to add an
// address to itself (§4.10), keyed by the account's user_id.
type pendingAdd struct {
	email string
	emailCode
}

// emailState is the email sign-in state of a Server. Guarded by s.mu.
type emailState struct {
	addresses  map[string]*emailAddress
	addressLRU *list.List
	clients    map[string]*emailClient
	clientLRU  *list.List
	adds       map[string]*pendingAdd
	// slots holds a token for each delivery in progress.
	slots chan struct{}
}

func newEmailState() emailState {
	return emailState{
		addresses: make(map[string]*emailAddress), addressLRU: list.New(),
		clients: make(map[string]*emailClient), clientLRU: list.New(),
		adds:  make(map[string]*pendingAdd),
		slots: make(chan struct{}, maxConcurrentEmailSends),
	}
}

// address returns the state of an address, pruned, or nil when there is
// none to keep. It creates nothing: a request that is refused leaves no
// trace in the table.
func (e *emailState) address(email string, now time.Time) *emailAddress {
	a := e.addresses[email]
	if a == nil {
		return nil
	}
	if a.prune(now) {
		e.addressLRU.Remove(a.element)
		delete(e.addresses, email)
		return nil
	}
	e.addressLRU.MoveToFront(a.element)
	return a
}

// createAddress returns the state of an address, creating it. When the
// table is full it forgets the least recently used entry that holds only
// send times, never one with a live code or wrong codes in the window, and
// returns nil when it finds none, so a request is refused rather than
// erasing anyone's code or lock.
func (e *emailState) createAddress(email string, now time.Time) *emailAddress {
	if a := e.address(email, now); a != nil {
		return a
	}
	if len(e.addresses) >= maxTrackedEmailState && !evict(e.addressLRU, evictionScan, func(value any) bool {
		a := value.(*emailAddress)
		if a.prune(now); a.signIn != nil || len(a.failures) > 0 {
			return false
		}
		delete(e.addresses, a.email)
		return true
	}) {
		return nil
	}
	a := &emailAddress{email: email}
	a.element = e.addressLRU.PushFront(a)
	e.addresses[email] = a
	return a
}

// client returns the budget of a client address, creating it. When the
// table is full it forgets the least recently used client whose budgets are
// full again, and returns nil when it finds none.
func (e *emailState) client(key string, now time.Time) *emailClient {
	c := e.clients[key]
	if c == nil {
		if len(e.clients) >= maxTrackedClients && !evict(e.clientLRU, evictionScan, func(value any) bool {
			old := value.(*emailClient)
			if old.sends.TokensAt(now) < clientSendBurst || old.failures.TokensAt(now) < clientFailureBurst {
				return false
			}
			delete(e.clients, old.key)
			return true
		}) {
			return nil
		}
		c = &emailClient{
			key:      key,
			sends:    rate.NewLimiter(rate.Every(clientSendRefill), clientSendBurst),
			failures: rate.NewLimiter(rate.Every(clientFailureRefill), clientFailureBurst),
		}
		c.element = e.clientLRU.PushFront(c)
		e.clients[key] = c
	}
	e.clientLRU.MoveToFront(c.element)
	return c
}

// evictionScan bounds how many of the least recently used entries a full
// table looks at for one it may forget.
const evictionScan = 256

// evict removes the least recently used element of lru, among the last
// scan, that forget accepts, and reports whether there was one.
func evict(lru *list.List, scan int, forget func(value any) bool) bool {
	for element := lru.Back(); element != nil && scan > 0; element, scan = element.Prev(), scan-1 {
		if forget(element.Value) {
			lru.Remove(element)
			return true
		}
	}
	return false
}

// wait is how long until limiter allows one more event, or 0.
func wait(limiter *rate.Limiter, now time.Time) time.Duration {
	reservation := limiter.ReserveN(now, 1)
	delay := reservation.DelayFrom(now)
	reservation.CancelAt(now)
	return delay
}

// clientKey identifies a connection's client for rate limits: its IP, or
// the /64 of an IPv6 address, which one household or host usually has.
func clientKey(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ip
	}
	if v4 := parsed.To4(); v4 != nil {
		return v4.String()
	}
	return parsed.Mask(net.CIDRMask(64, 128)).String() + "/64"
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

// authenticateEmail runs the email scheme under s.mu (§4.10). A request
// without token asks for a code and returns {}, whether or not the address
// has an account, and changes no authentication. On a connection not signed
// in, the code signs in: to the address's account, or to a new one, taking a
// requested user_id as guests do, or user_<n>. On a connection signed in,
// the code only adds the address to that account, which a guest becomes an
// account by, and it is its own code: the account's request, whose email has
// no link. A code proves only that its presenter reads the address's mail,
// so a code from anyone else never adds an address, and an add code never
// signs in: otherwise whoever requested a code for their own address and got
// someone signed in to present it would then sign in as that person.
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
	name, err := parseString(req.params, "name", false)
	if err != nil {
		return nil, err
	}
	requested, err := parseString(req.params, "user_id", false)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	busy := retryAfter("Too many sign-ins in progress; try again shortly", emailResendInterval)
	budget := s.email.client(c.clientKey, now)
	if budget == nil {
		return nil, busy
	}
	if _, has := req.params["token"]; !has {
		return s.sendEmailCodeLocked(c, req, email, budget, now)
	}

	state := s.email.address(email, now)
	if wait := state.lockedFor(now); wait > 0 {
		return nil, retryAfter("Too many wrong codes for this address; try again later", wait)
	}
	if delay := wait(budget.failures, now); delay > 0 {
		return nil, retryAfter("Too many wrong codes; try again later", delay)
	}
	var pending *emailCode
	if c.user != nil {
		if add := s.email.adds[c.user.id]; add != nil && add.email == email && now.Before(add.expires) {
			pending = &add.emailCode
		}
	} else if state != nil {
		pending = state.signIn
	}
	denied := &rpcError{Code: codeDenied, Message: "The code is invalid or expired; ask for a new one"}
	if pending == nil {
		return nil, denied
	}
	right, spent := pending.guess(code)
	if !right {
		budget.failures.AllowN(now, 1)
		if state == nil {
			// An add code's address may have no entry yet; if the table
			// has no room, the guesser's own budget still counted it.
			state = s.email.createAddress(email, now)
		}
		if state != nil {
			state.failures = append(state.failures, now)
		}
		if spent {
			if c.user != nil {
				delete(s.email.adds, c.user.id)
			} else {
				state.signIn = nil
			}
		}
		return nil, denied
	}

	if c.user != nil {
		delete(s.email.adds, c.user.id)
		if s.emails[email] != nil || c.user.email != "" {
			return nil, &rpcError{Code: codeDenied, Message: "This address belongs to an account, or this account has one already"}
		}
		c.user.email = email
		s.emails[email] = c.user
		s.touchUser(c.user.id)
		if s.grantRolesLocked(c.user) {
			// The connection's identity became an account holding roles.
			s.notifyProfileLocked(c.user, c)
		}
		if c.token == ([32]byte{}) {
			// A guest that became an account needs a token to come back.
			return s.signInLocked(c, req, c.user, now)
		}
		return s.switchUserLocked(c, req, c.user, nil), nil
	}
	state.signIn = nil
	user := s.emails[email]
	if user == nil {
		user = newUserState(s.assignAccountIDLocked(requested), normalizeName(name))
		user.email = email
		s.users[user.id] = user
		s.emails[email] = user
		s.touchUser(user.id)
		s.grantRolesLocked(user)
		// A new account joins the default room, as a new guest does, and
		// the join reaches this connection before the result.
		s.attachLocked(c, user)
		s.joinDefaultRoomLocked(user)
	}
	return s.signInLocked(c, req, user, now)
}

// sendEmailCodeLocked sends a code in the background: a sign-in code, with a
// link, replacing the address's earlier one, on a connection not signed in;
// on one signed in, the account's add code, without a link, replacing the
// account's earlier one. An add code is sent only when the address could be
// added, and the result is the same {} either way.
//
// Codes to one address are at least emailResendInterval apart and at most
// maxEmailSendsPerWindow an hour, none while its codes are refused; a client
// address and a connection may ask for a few; and at most
// maxConcurrentEmailSends deliveries run at once. Beyond any of these the
// request is retry_after, whether or not the address has an account.
func (s *Server) sendEmailCodeLocked(c *client, req request, email string, budget *emailClient, now time.Time) (any, *rpcError) {
	state := s.email.address(email, now)
	if n := len(state.sendTimes()); n > 0 {
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
	if delay := wait(budget.sends, now); delay > 0 {
		return nil, retryAfter("Too many codes requested; try again later", delay)
	}
	if c.emailSends == nil {
		c.emailSends = rate.NewLimiter(rate.Every(emailSendRefill), emailSendsPerConnection)
	}
	if delay := wait(c.emailSends, now); delay > 0 {
		return nil, retryAfter("Too many codes requested; try again later", delay)
	}
	if state == nil {
		// The entry is made only for a code that is sent.
		if state = s.email.createAddress(email, now); state == nil {
			return nil, retryAfter("Too many sign-ins in progress; try again shortly", emailResendInterval)
		}
	}
	select {
	case s.email.slots <- struct{}{}:
	default:
		return nil, retryAfter("Too many emails are being sent; try again shortly", emailResendInterval)
	}
	number, randErr := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if randErr != nil {
		<-s.email.slots
		return nil, &rpcError{Code: codeInternalError, Message: "Unable to create a code"}
	}
	budget.sends.AllowN(now, 1)
	c.emailSends.AllowN(now, 1)
	state.sends = append(state.sends, now)
	code := emailCode{code: fmt.Sprintf("%06d", number), expires: now.Add(emailCodeLifetime)}
	message := SignInEmail{To: email, Code: code.code, Expires: code.expires}
	deliver := true
	if c.user != nil {
		s.email.adds[c.user.id] = &pendingAdd{email: email, emailCode: code}
		message.Add = true
		deliver = s.emails[email] == nil && c.user.email == ""
		if len(s.email.adds) > maxTrackedEmailState {
			for id, add := range s.email.adds {
				if !now.Before(add.expires) {
					delete(s.email.adds, id)
				}
			}
		}
	} else {
		state.signIn = &code
		message.Link = signInLink(s.config.EmailLinkURL, email, code.code, s.publicWebSocketURL())
	}
	if !deliver {
		<-s.email.slots
	} else {
		sender := s.config.EmailSender
		s.mailing.Add(1)
		go func() {
			defer s.mailing.Done()
			defer func() { <-s.email.slots }()
			ctx, cancel := context.WithTimeout(context.Background(), emailSendTimeout)
			defer cancel()
			if err := sender.SendSignInCode(ctx, message); err != nil {
				slog.Error("cannot send an email code", "error", err)
			}
		}()
	}
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
