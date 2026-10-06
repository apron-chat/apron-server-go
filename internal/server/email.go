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
	// emailCodeLifetime is how long a proposal's tokens stay valid.
	emailCodeLifetime = 10 * time.Minute
	// maxEmailAttempts wrong tokens on a connection invalidate its proposal.
	maxEmailAttempts = 5
	// emailResendInterval is the least time between emails to one address,
	// and emailWindow the window of maxEmailSendsPerWindow emails to it.
	emailResendInterval    = 30 * time.Second
	emailWindow            = time.Hour
	maxEmailSendsPerWindow = 6
	// Per client address (IP, or IPv6 /64): a burst of clientSendBurst
	// proposals, refilled one per clientSendRefill.
	clientSendBurst  = 10
	clientSendRefill = 3 * time.Minute
	// The tables of addresses and clients, and the outstanding link tokens,
	// are bounded.
	maxTrackedEmailState = 10_000
	maxTrackedClients    = 100_000
	maxEmailLinks        = 10_000
	// maxConcurrentEmailSends bounds deliveries in progress; beyond it a
	// proposal is retry_after.
	maxConcurrentEmailSends = 16
	// emailSendsPerConnection proposals may be made on one connection at
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
	// which works on any connection not signed in; empty for an addition
	// and when no link is configured.
	Link string
	// Expires is when the code stops working.
	Expires time.Time
	// Add reports a proposal to add the address to a signed-in account,
	// which signs nobody in and carries no link.
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

// emailProposal is a connection's pending proposal (§4.11): to sign in
// with an address, or to add it to the proposing account. Its short code
// works only on that connection; a sign-in's link token, unguessable, works
// on any connection not signed in. Guarded by s.mu.
type emailProposal struct {
	email string
	// accountID is the proposing account, for an addition.
	accountID string
	add       bool
	code      string
	link      string
	expires   time.Time
	attempts  int
	// owner is the proposing connection; name and userID are what it
	// requested for a new account.
	owner        *client
	name, userID string
}

// emailAddress is what the server keeps about one address, in memory only:
// the proposals accepted for it in the last emailWindow, emailed or not, so
// its limit reads the same whether or not it has an account. Entries are kept in least
// recently used order.
type emailAddress struct {
	email   string
	sends   []time.Time
	element *list.Element
}

// prune forgets sends older than emailWindow, and reports whether nothing is
// left to keep.
func (a *emailAddress) prune(now time.Time) bool {
	for len(a.sends) > 0 && !now.Before(a.sends[0].Add(emailWindow)) {
		a.sends = a.sends[1:]
	}
	return len(a.sends) == 0
}

// emailClient is the budget of one client address, kept like emailAddress.
type emailClient struct {
	key     string
	sends   *rate.Limiter
	element *list.Element
}

// emailState is the email sign-in state of a Server. Guarded by s.mu.
type emailState struct {
	addresses  map[string]*emailAddress
	addressLRU *list.List
	clients    map[string]*emailClient
	clientLRU  *list.List
	// links maps each outstanding link token to its proposal.
	links map[string]*emailProposal
	// slots holds a token for each delivery in progress.
	slots chan struct{}
}

func newEmailState() emailState {
	return emailState{
		addresses: make(map[string]*emailAddress), addressLRU: list.New(),
		clients: make(map[string]*emailClient), clientLRU: list.New(),
		links: make(map[string]*emailProposal),
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
// table is full it forgets the least recently used entry with no email in
// the window, and returns nil when it finds none, so a proposal is refused
// rather than erasing another address's limit.
func (e *emailState) createAddress(email string, now time.Time) *emailAddress {
	if a := e.address(email, now); a != nil {
		return a
	}
	if len(e.addresses) >= maxTrackedEmailState && !evict(e.addressLRU, evictionScan, func(value any) bool {
		a := value.(*emailAddress)
		if !a.prune(now) {
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
// table is full it forgets the least recently used client whose budget is
// full again, and returns nil when it finds none.
func (e *emailState) client(key string, now time.Time) *emailClient {
	c := e.clients[key]
	if c == nil {
		if len(e.clients) >= maxTrackedClients && !evict(e.clientLRU, evictionScan, func(value any) bool {
			old := value.(*emailClient)
			if old.sends.TokensAt(now) < clientSendBurst {
				return false
			}
			delete(e.clients, old.key)
			return true
		}) {
			return nil
		}
		c = &emailClient{key: key, sends: rate.NewLimiter(rate.Every(clientSendRefill), clientSendBurst)}
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

// dropProposalLocked forgets a proposal: its connection's, and its link.
func (s *Server) dropProposalLocked(p *emailProposal) {
	if p.owner != nil && p.owner.proposal == p {
		p.owner.proposal = nil
	}
	if p.link != "" && s.email.links[p.link] == p {
		delete(s.email.links, p.link)
	}
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

// signInLink builds a sign-in link from the configured page, never from
// request fields, with the token in the fragment so it stays out of server
// logs (§4.11): #token=<token>, and &server=<URL> with this server's public
// WebSocket URL when it is known, so a client that speaks to several
// servers presents it to the right one. Values are form-encoded.
func signInLink(page, token, server string) string {
	if page == "" {
		return ""
	}
	page, _, _ = strings.Cut(page, "#")
	link := page + "#token=" + url.QueryEscape(token)
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

// authenticateEmail runs the email scheme under s.mu (§4.11). A request
// with email proposes: on a connection signed in, guests included, adding
// the address to that account, else signing in with it. It returns {},
// whether or not the address has an account, and changes no
// authentication. A request with token approves the connection's proposal
// with its code, or a sign-in proposal with its link token.
func (s *Server) authenticateEmail(c *client, req request) (any, *rpcError) {
	// A notification has no request ID to answer and changes nothing.
	if !req.hasID {
		return nil, nil
	}
	token, err := parseString(req.params, "token", false)
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
	if _, has := req.params["token"]; has {
		return s.approveEmailLocked(c, req, token, name, requested, now)
	}
	raw, err := parseString(req.params, "email", true)
	if err != nil {
		return nil, err
	}
	email := normalizeEmail(raw)
	if email == "" {
		return nil, invalidParams("email must be an email address at a domain name")
	}
	return s.proposeEmailLocked(c, req, email, name, requested, now)
}

// proposeEmailLocked replaces the connection's proposal with a new one and
// emails its tokens in the background: a code to type and, for a sign-in, a
// link. An addition is proposed by code only, so only the proposing
// connection can approve it, and is emailed only when the address could be
// added: no account holds it, and the proposing one has none.
//
// Emails to one address are at least emailResendInterval apart and at most
// maxEmailSendsPerWindow an hour; a client address and a connection may
// propose a few; and at most maxConcurrentEmailSends deliveries run at once.
// Beyond any of these the request is retry_after, whether or not the
// address has an account.
func (s *Server) proposeEmailLocked(c *client, req request, email, name, requested string, now time.Time) (any, *rpcError) {
	busy := retryAfter("Too many sign-ins in progress; try again shortly", emailResendInterval)
	state := s.email.address(email, now)
	if state != nil {
		n := len(state.sends)
		if wait := state.sends[n-1].Add(emailResendInterval).Sub(now); wait > 0 {
			return nil, retryAfter("An email was just sent to this address; try again shortly", wait)
		}
		if n >= maxEmailSendsPerWindow {
			return nil, retryAfter("Too many emails were sent to this address; try again later", state.sends[0].Add(emailWindow).Sub(now))
		}
	}
	budget := s.email.client(c.clientKey, now)
	if budget == nil {
		return nil, busy
	}
	if delay := wait(budget.sends, now); delay > 0 {
		return nil, retryAfter("Too many sign-ins proposed; try again later", delay)
	}
	if c.emailSends == nil {
		c.emailSends = rate.NewLimiter(rate.Every(emailSendRefill), emailSendsPerConnection)
	}
	if delay := wait(c.emailSends, now); delay > 0 {
		return nil, retryAfter("Too many sign-ins proposed; try again later", delay)
	}
	add := c.user != nil
	if !add && len(s.email.links) >= maxEmailLinks {
		for token, p := range s.email.links {
			if !now.Before(p.expires) {
				s.dropProposalLocked(p)
				delete(s.email.links, token)
			}
		}
		if len(s.email.links) >= maxEmailLinks {
			return nil, busy
		}
	}
	if state == nil {
		// The entry is made only for a proposal that is accepted, emailed
		// or not: an addition sends nothing for an address with an
		// account, and must count the same as one that does.
		if state = s.email.createAddress(email, now); state == nil {
			return nil, busy
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
	if c.proposal != nil {
		s.dropProposalLocked(c.proposal)
	}
	p := &emailProposal{
		email: email, add: add, code: fmt.Sprintf("%06d", number), expires: now.Add(emailCodeLifetime),
		owner: c, name: name, userID: requested,
	}
	message := SignInEmail{To: email, Code: p.code, Expires: p.expires, Add: add}
	deliver := true
	if add {
		p.accountID = c.user.id
		deliver = s.emails[email] == nil && c.user.email == ""
	} else {
		p.link = rand.Text()
		s.email.links[p.link] = p
		message.Link = signInLink(s.config.EmailLinkURL, p.link, s.publicWebSocketURL())
	}
	c.proposal = p
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

// approveEmailLocked approves a proposal with a token (§4.11): the code of
// the connection's own proposal, or the link token of a sign-in proposal
// made anywhere. Approving a sign-in authenticates this connection, which
// must not be signed in, to the address's account or a new one, taking a
// requested user_id as guests do, or user_<n>, and returns {you, token}.
// Approving an addition adds the address to the proposing account and
// returns {}. A wrong token counts against the connection's proposal, which
// a few wrong tokens invalidate.
func (s *Server) approveEmailLocked(c *client, req request, token, name, requested string, now time.Time) (any, *rpcError) {
	denied := &rpcError{Code: codeDenied, Message: "The code is invalid or expired; ask for a new one"}
	if own := c.proposal; own != nil && !now.Before(own.expires) {
		s.dropProposalLocked(own)
	}
	var p *emailProposal
	switch own := c.proposal; {
	case token != "" && own != nil && subtle.ConstantTimeCompare([]byte(token), []byte(own.code)) == 1:
		p = own
	case token != "" && s.email.links[token] != nil:
		if p = s.email.links[token]; !now.Before(p.expires) {
			s.dropProposalLocked(p)
			return nil, denied
		}
	default:
		if own != nil {
			if own.attempts++; own.attempts >= maxEmailAttempts {
				s.dropProposalLocked(own)
			}
		}
		return nil, denied
	}

	if p.add {
		account := s.users[p.accountID]
		s.dropProposalLocked(p)
		switch {
		case account == nil:
			return nil, denied
		case s.emails[p.email] != nil:
			return nil, &rpcError{Code: codeDenied, Message: "This address belongs to an account"}
		case account.email != "":
			return nil, &rpcError{Code: codeDenied, Message: "This account has an email address already"}
		}
		account.email = p.email
		s.emails[p.email] = account
		s.touchUser(account.id)
		if s.grantRolesLocked(account) {
			// The account's roles changed with its address.
			s.notifyProfileLocked(account, nil, nil)
		}
		result := map[string]any{}
		c.sendResult(req, result)
		return result, nil
	}

	// A sign-in authenticates a connection that is not signed in; the
	// proposal stays for one that is not.
	if c.user != nil {
		return nil, &rpcError{Code: codeDenied, Message: "Sign-in codes work on a connection that is not signed in"}
	}
	s.dropProposalLocked(p)
	user := s.emails[p.email]
	if user == nil {
		// A proposal's own name and user_id apply only on its connection: a
		// link opened elsewhere is someone reading the email, whose account
		// the proposer must not name.
		if c == p.owner {
			if name == "" {
				name = p.name
			}
			if requested == "" {
				requested = p.userID
			}
		}
		user = newUserState(s.assignAccountIDLocked(requested), normalizeName(name))
		user.email = p.email
		s.users[user.id] = user
		s.emails[p.email] = user
		s.touchUser(user.id)
		s.grantRolesLocked(user)
		// A new account joins the default room, as a new guest does, after
		// the result (§3.2).
		return s.signInLocked(c, req, user, now, true, true)
	}
	return s.signInLocked(c, req, user, now, true, false)
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
