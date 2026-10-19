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
	// emailCodeLifetime is how long a proposal and its tokens, emailed as
	// it is made, stay valid.
	emailCodeLifetime = 10 * time.Minute
	// maxEmailAttempts wrong tokens, on any connection, cancel a proposal.
	maxEmailAttempts = 5
	// linkHandleBytes starts each link token with a handle naming its
	// proposal, so a wrong secret behind it counts against that proposal
	// from any connection.
	linkHandleBytes = 10
	// maxStateBytes bounds the state a proposal carries (§4.11.1).
	maxStateBytes = 256
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

// emailProposal is a connection's pending proposal (§4.11.1): to sign in
// with an address, or to add it to the proposing account. Its short code
// works only on that connection; its link token, a handle and an
// unguessable secret, works on any connection that may approve it
// (§4.11.3). It outlives its connection's disconnect, until it expires or
// is approved, and holds neither the connection nor the account, so a
// closed connection, and a retired guest, are not kept for it: the
// connection holds the proposal instead. Guarded by s.mu.
type emailProposal struct {
	email string
	// accountID is the user_id of the proposing account, for an addition.
	// User_ids are never reissued, so only a connection signed in to that
	// account has it.
	accountID string
	add       bool
	code      string
	// handle and secret make up the link token; both are empty when no
	// link is sent.
	handle, secret string
	// state is the client's, returned in the link.
	state    string
	expires  time.Time
	attempts int
	// dropped reports that the proposal was forgotten, which its
	// connection, still holding it, learns from this.
	dropped bool
	// name and userID are what the proposing connection requested for a
	// new account.
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
	// links maps the handle of each outstanding link token to its
	// proposal.
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
	p.dropped = true
	if p.handle != "" && s.email.links[p.handle] == p {
		delete(s.email.links, p.handle)
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

// signInLink builds a link from the configured page, never from request
// fields other than state, with the token in the fragment so it stays out
// of server logs (§4.11.2): #token=<token>, &state=<state> with the
// proposal's state, unchanged, when it carried one, and &server=<URL> with
// this server's public WebSocket URL when it is known, so a client that
// speaks to several servers presents it to the right one. Values are
// form-encoded.
func signInLink(page, token, state, server string) string {
	page, _, _ = strings.Cut(page, "#")
	link := page + "#token=" + url.QueryEscape(token)
	if state != "" {
		link += "&state=" + url.QueryEscape(state)
	}
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
// without token proposes, by email; one with token approves a proposal
// (§4.11.3), which takes either scheme the server offers.
func (s *Server) authenticateEmail(c *client, req request) (any, *rpcError) {
	if _, has := req.params["token"]; has {
		return s.approveLocked(c, req)
	}
	return s.proposeEmailLocked(c, req)
}

// parseRequested parses the name and user_id that a proposal or an
// approval may request for a new account.
func parseRequested(req request) (name, requested string, err *rpcError) {
	if name, err = parseString(req.params, "name", false); err != nil {
		return "", "", err
	}
	if requested, err = parseString(req.params, "user_id", false); err != nil {
		return "", "", err
	}
	return name, requested, nil
}

// proposeEmailLocked replaces the connection's proposal with a new one
// (§4.11.1) and emails its tokens in the background: a code to type and,
// when a link page is configured, a link with the proposal's state. On a
// connection signed in, guests included, it proposes adding the address to
// that account, else signing in with it. It returns {}, whether or not the
// address has an account, and changes no authentication. An addition is
// emailed only when the address could be added: no account holds it, and
// the proposing one has none.
//
// Emails to one address are at least emailResendInterval apart and at most
// maxEmailSendsPerWindow an hour; a client address and a connection may
// propose a few; and at most maxConcurrentEmailSends deliveries run at once.
// Beyond any of these the request is retry_after, whether or not the
// address has an account.
func (s *Server) proposeEmailLocked(c *client, req request) (any, *rpcError) {
	name, requested, err := parseRequested(req)
	if err != nil {
		return nil, err
	}
	state, err := parseString(req.params, "state", false)
	if err != nil {
		return nil, err
	}
	if len(state) > maxStateBytes {
		return nil, &rpcError{Code: codeTooLarge, Message: fmt.Sprintf("state is at most %d bytes", maxStateBytes)}
	}
	raw, err := parseString(req.params, "email", true)
	if err != nil {
		return nil, err
	}
	email := normalizeEmail(raw)
	if email == "" {
		return nil, invalidParams("email must be an email address at a domain name")
	}
	now := time.Now()
	busy := retryAfter("Too many sign-ins in progress; try again shortly", emailResendInterval)
	address := s.email.address(email, now)
	if address != nil {
		n := len(address.sends)
		if wait := address.sends[n-1].Add(emailResendInterval).Sub(now); wait > 0 {
			return nil, retryAfter("An email was just sent to this address; try again shortly", wait)
		}
		if n >= maxEmailSendsPerWindow {
			return nil, retryAfter("Too many emails were sent to this address; try again later", address.sends[0].Add(emailWindow).Sub(now))
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
	deliver := !add || (s.emails[email] == nil && c.user.email == "")
	linked := deliver && s.config.EmailLinkURL != ""
	if linked && len(s.email.links) >= maxEmailLinks {
		for _, p := range s.email.links {
			if !now.Before(p.expires) {
				s.dropProposalLocked(p)
			}
		}
		if len(s.email.links) >= maxEmailLinks {
			return nil, busy
		}
	}
	if address == nil {
		// The entry is made only for a proposal that is accepted, emailed
		// or not: an addition sends nothing for an address with an
		// account, and must count the same as one that does.
		if address = s.email.createAddress(email, now); address == nil {
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
	address.sends = append(address.sends, now)
	if c.proposal != nil {
		s.dropProposalLocked(c.proposal)
	}
	p := &emailProposal{
		email: email, add: add, code: fmt.Sprintf("%06d", number), state: state, expires: now.Add(emailCodeLifetime),
		name: name, userID: requested,
	}
	if add {
		p.accountID = c.user.id
	}
	message := SignInEmail{To: email, Code: p.code, Expires: p.expires, Add: add}
	if linked {
		p.handle, p.secret = s.newLinkHandleLocked(), rand.Text()
		s.email.links[p.handle] = p
		message.Link = signInLink(s.config.EmailLinkURL, p.handle+p.secret, state, s.publicWebSocketURL())
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

// newLinkHandleLocked returns a link handle no outstanding link has.
func (s *Server) newLinkHandleLocked() string {
	for {
		if handle := rand.Text()[:linkHandleBytes]; s.email.links[handle] == nil {
			return handle
		}
	}
}

// approveLocked approves a proposal with its temporary token (§4.11.3),
// under either scheme the server offers: the code of the connection's own
// proposal, or the link token of a proposal made on any connection.
// Approving a sign-in authenticates this connection, which must not be
// signed in, to the address's account or a new one, taking a requested
// user_id as guests do, or user_<n>, and returns {you, token}. Approving an
// addition, only on a connection signed in to the proposing account, adds
// the address to that account and returns {}. A token presented on a
// connection that may not approve it is denied, and its proposal stays for
// one that may.
func (s *Server) approveLocked(c *client, req request) (any, *rpcError) {
	token, err := parseString(req.params, "token", true)
	if err != nil {
		return nil, err
	}
	name, requested, err := parseRequested(req)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	p := s.proposalLocked(c, token, now)
	if p == nil {
		return nil, &rpcError{Code: codeDenied, Message: "The code is invalid or expired; ask for a new one"}
	}

	if p.add {
		// Only the proposing account approves an addition: otherwise
		// someone could lead a victim to add the victim's address to the
		// other person's account.
		account := c.user
		if account == nil || account.id != p.accountID {
			return nil, &rpcError{Code: codeDenied, Message: "Open this link where the account that asked for it is signed in"}
		}
		s.dropProposalLocked(p)
		switch {
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
	own := c.proposal == p
	s.dropProposalLocked(p)
	user := s.emails[p.email]
	if user == nil {
		// A proposal's own name and user_id apply only on its connection: a
		// link opened elsewhere is someone reading the email, whose account
		// the proposer must not name.
		if own {
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
		// the result (§3.2.2).
		return s.signInLocked(c, req, user, now, true, true)
	}
	return s.signInLocked(c, req, user, now, true, false)
}

// proposalLocked returns the live proposal that token approves: the
// connection's own, by its code, or any, by its link token. Otherwise it
// returns nil, and counts a failed attempt against the proposal that token
// names by its link handle, else against the connection's own, whichever
// connection presents it (§4.11.1): maxEmailAttempts of them cancel it.
// Expired proposals are dropped.
func (s *Server) proposalLocked(c *client, token string, now time.Time) *emailProposal {
	own := c.proposal
	if own != nil && (own.dropped || !now.Before(own.expires)) {
		s.dropProposalLocked(own)
		c.proposal, own = nil, nil
	}
	if own != nil && subtle.ConstantTimeCompare([]byte(token), []byte(own.code)) == 1 {
		return own
	}
	target := own
	if len(token) > linkHandleBytes {
		if p := s.email.links[token[:linkHandleBytes]]; p != nil {
			if !now.Before(p.expires) {
				s.dropProposalLocked(p)
				return nil
			}
			if subtle.ConstantTimeCompare([]byte(token[linkHandleBytes:]), []byte(p.secret)) == 1 {
				return p
			}
			target = p
		}
	}
	if target != nil {
		if target.attempts++; target.attempts >= maxEmailAttempts {
			s.dropProposalLocked(target)
		}
	}
	return nil
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
