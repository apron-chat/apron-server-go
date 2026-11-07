// Package email is the email sign-in scheme (PROTOCOL.md §4.11.4): a
// proposal mails a code, and a link when a page is configured, whose token
// proves the address. The server's proposals (authn.Core) make and check
// the tokens; this package checks addresses, bounds how many emails go
// out, and delivers them.
package email

import (
	"container/list"
	"context"
	"encoding/json/jsontext"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/apron-chat/apron-server-go/internal/authn"
)

const (
	// resendInterval is the least time between emails to one address, and
	// window the window of maxSendsPerWindow emails to it.
	resendInterval    = 30 * time.Second
	window            = time.Hour
	maxSendsPerWindow = 6
	// Per client address (IP, or IPv6 /64): a burst of clientSendBurst
	// proposals, refilled one per clientSendRefill.
	clientSendBurst  = 10
	clientSendRefill = 3 * time.Minute
	// The tables of addresses and clients are bounded.
	maxTrackedAddresses = 10_000
	maxTrackedClients   = 100_000
	// maxConcurrentSends bounds deliveries in progress; beyond it a
	// proposal is retry_after.
	maxConcurrentSends = 16
	// sendsPerConnection proposals may be made on one connection at once,
	// refilled one per sendRefill.
	sendsPerConnection = 5
	sendRefill         = 2 * time.Minute
	// sendTimeout bounds one delivery.
	sendTimeout = 30 * time.Second
	maxBytes    = 254
	// maxStateBytes bounds the state a proposal carries (§4.11.1).
	maxStateBytes = 256
	// evictionScan bounds how many of the least recently used entries a
	// full table looks at for one it may forget.
	evictionScan = 256
)

// Limits, exported for the server's tests.
const (
	ResendInterval      = resendInterval
	MaxSendsPerWindow   = maxSendsPerWindow
	ClientSendBurst     = clientSendBurst
	MaxTrackedAddresses = maxTrackedAddresses
	MaxConcurrentSends  = maxConcurrentSends
	SendsPerConnection  = sendsPerConnection
)

// IdentityLabel names the scheme's identities in refusals.
const IdentityLabel = "email address"

// Name is the scheme's name, and its identities'.
const Name = "email"

// Message is one sign-in code to deliver.
type Message struct {
	To, Code, Link string
	Expires        time.Time
	Add            bool
}

// Scheme is email sign-in.
type Scheme struct {
	deliver func(ctx context.Context, m Message) error
	// linkPage is the page a link opens, empty for no link.
	linkPage string

	mu         sync.Mutex
	addresses  map[string]*address
	addressLRU *list.List
	clients    map[string]*clientBudget
	clientLRU  *list.List
	// slots holds a token for each delivery in progress.
	slots chan struct{}
}

// New is email sign-in that delivers with deliver, and links to linkPage
// when it is set.
func New(deliver func(ctx context.Context, m Message) error, linkPage string) *Scheme {
	return &Scheme{
		deliver: deliver, linkPage: linkPage,
		addresses: make(map[string]*address), addressLRU: list.New(),
		clients: make(map[string]*clientBudget), clientLRU: list.New(),
		slots: make(chan struct{}, maxConcurrentSends),
	}
}

func (*Scheme) Name() string { return Name }

// address is what the scheme keeps about one address, in memory only: the
// proposals accepted for it in the last window, emailed or not, so its
// limit reads the same whether or not it has an account. Entries are kept
// in least recently used order.
type address struct {
	email   string
	sends   []time.Time
	element *list.Element
}

// prune forgets sends older than window, and reports whether nothing is
// left to keep.
func (a *address) prune(now time.Time) bool {
	for len(a.sends) > 0 && !now.Before(a.sends[0].Add(window)) {
		a.sends = a.sends[1:]
	}
	return len(a.sends) == 0
}

// clientBudget is the budget of one client address, kept like address.
type clientBudget struct {
	key     string
	sends   *rate.Limiter
	element *list.Element
}

// Auth proposes, mailing a code for the address, or approves with a token
// (§4.11.3).
func (s *Scheme) Auth(_ context.Context, r *authn.Request) (authn.Result, error) {
	name, requested, err := parseRequested(r.Params)
	if err != nil {
		return nil, err
	}
	if _, has := r.Params["token"]; has {
		token, err := authn.String(r.Params, "token", true)
		if err != nil {
			return nil, err
		}
		return authn.Approve{Token: token, Name: name, UserID: requested}, nil
	}
	return s.propose(r, name, requested)
}

// propose makes a proposal (§4.11.1) and emails its tokens in the
// background: a code to type and, when a link page is configured, a link
// with the proposal's state. On a connection signed in, guests included, it
// proposes adding the address to that account, else signing in with it. It
// returns {}, whether or not the address has an account, and changes no
// authentication. A code is mailed only when it would work: an addition
// when no account holds the address and the proposing one has none, and a
// sign-in when the address may sign in.
//
// Emails to one address are at least resendInterval apart and at most
// maxSendsPerWindow an hour; a client address and a connection may propose
// a few; and at most maxConcurrentSends deliveries run at once. Beyond any
// of these the request is retry_after, whether or not the address has an
// account.
func (s *Scheme) propose(r *authn.Request, name, requested string) (authn.Result, error) {
	state, err := authn.String(r.Params, "state", false)
	if err != nil {
		return nil, err
	}
	if len(state) > maxStateBytes {
		return nil, authn.TooLarge("state is at most 256 bytes")
	}
	raw, err := authn.String(r.Params, "email", true)
	if err != nil {
		return nil, err
	}
	addr := Normalize(raw)
	if addr == "" {
		return nil, authn.InvalidParams("email must be an email address at a domain name")
	}
	now := r.Now
	busy := authn.RetryAfter("Too many sign-ins in progress; try again shortly", resendInterval)
	connection, _ := r.State.Load().(*rate.Limiter)
	if connection == nil {
		connection = rate.NewLimiter(rate.Every(sendRefill), sendsPerConnection)
		r.State.Store(connection)
	}
	id := authn.Identity{Scheme: Name, Subject: addr}
	deliver := r.Core.CanProve(r, id)

	s.mu.Lock()
	a := s.address(addr, now)
	if a != nil {
		n := len(a.sends)
		if wait := a.sends[n-1].Add(resendInterval).Sub(now); wait > 0 {
			s.mu.Unlock()
			return nil, authn.RetryAfter("An email was just sent to this address; try again shortly", wait)
		}
		if n >= maxSendsPerWindow {
			s.mu.Unlock()
			return nil, authn.RetryAfter("Too many emails were sent to this address; try again later", a.sends[0].Add(window).Sub(now))
		}
	}
	budget := s.client(r.ClientKey, now)
	if budget == nil {
		s.mu.Unlock()
		return nil, busy
	}
	if delay := wait(budget.sends, now); delay > 0 {
		s.mu.Unlock()
		return nil, authn.RetryAfter("Too many sign-ins proposed; try again later", delay)
	}
	if delay := wait(connection, now); delay > 0 {
		s.mu.Unlock()
		return nil, authn.RetryAfter("Too many sign-ins proposed; try again later", delay)
	}
	if a == nil {
		// The entry is made only for a proposal that is accepted, emailed
		// or not: one that sends nothing must count the same as one that
		// does.
		if a = s.createAddress(addr, now); a == nil {
			s.mu.Unlock()
			return nil, busy
		}
	}
	// A slot is taken whether or not the code is mailed, so that the reply
	// reads the same either way.
	select {
	case s.slots <- struct{}{}:
	default:
		s.mu.Unlock()
		return nil, authn.RetryAfter("Too many emails are being sent; try again shortly", resendInterval)
	}
	s.mu.Unlock()

	proposal := authn.Proposal{
		Proven: &authn.Proven{Identity: id, Create: true, Label: IdentityLabel},
		State:  state, Name: name, UserID: requested,
	}
	if deliver {
		proposal.Link = s.linkPage
	}
	tokens, proposeErr := r.Core.Propose(r, proposal)
	if proposeErr != nil {
		<-s.slots
		return nil, proposeErr
	}
	s.mu.Lock()
	budget.sends.AllowN(now, 1)
	connection.AllowN(now, 1)
	a.sends = append(a.sends, now)
	s.mu.Unlock()
	if !deliver {
		<-s.slots
		return authn.Reply{Result: map[string]any{}}, nil
	}
	message := Message{To: addr, Code: tokens.Code, Link: tokens.Link, Expires: tokens.Expires, Add: tokens.Add}
	logger := r.Core.Logger()
	r.Core.Go(func(context.Context) {
		defer func() { <-s.slots }()
		ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
		defer cancel()
		if err := s.deliver(ctx, message); err != nil {
			logger.Error("cannot send an email code", "error", err)
		}
	})
	return authn.Reply{Result: map[string]any{}}, nil
}

// address returns the state of an address, pruned, or nil when there is
// none to keep. It creates nothing: a request that is refused leaves no
// trace in the table. Called holding s.mu.
func (s *Scheme) address(addr string, now time.Time) *address {
	a := s.addresses[addr]
	if a == nil {
		return nil
	}
	if a.prune(now) {
		s.addressLRU.Remove(a.element)
		delete(s.addresses, addr)
		return nil
	}
	s.addressLRU.MoveToFront(a.element)
	return a
}

// createAddress returns the state of an address, creating it. When the
// table is full it forgets the least recently used entry with no email in
// the window, and returns nil when it finds none, so a proposal is refused
// rather than erasing another address's limit. Called holding s.mu.
func (s *Scheme) createAddress(addr string, now time.Time) *address {
	if a := s.address(addr, now); a != nil {
		return a
	}
	if len(s.addresses) >= maxTrackedAddresses && !evict(s.addressLRU, evictionScan, func(value any) bool {
		a := value.(*address)
		if !a.prune(now) {
			return false
		}
		delete(s.addresses, a.email)
		return true
	}) {
		return nil
	}
	a := &address{email: addr}
	a.element = s.addressLRU.PushFront(a)
	s.addresses[addr] = a
	return a
}

// client returns the budget of a client address, creating it. When the
// table is full it forgets the least recently used client whose budget is
// full again, and returns nil when it finds none. Called holding s.mu.
func (s *Scheme) client(key string, now time.Time) *clientBudget {
	c := s.clients[key]
	if c == nil {
		if len(s.clients) >= maxTrackedClients && !evict(s.clientLRU, evictionScan, func(value any) bool {
			old := value.(*clientBudget)
			if old.sends.TokensAt(now) < clientSendBurst {
				return false
			}
			delete(s.clients, old.key)
			return true
		}) {
			return nil
		}
		c = &clientBudget{key: key, sends: rate.NewLimiter(rate.Every(clientSendRefill), clientSendBurst)}
		c.element = s.clientLRU.PushFront(c)
		s.clients[key] = c
	}
	s.clientLRU.MoveToFront(c.element)
	return c
}

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

// Normalize returns a bare address, lowercased, or "" if value is not one.
// The domain must be a DNS name with a dot: address literals such as
// a@[10.0.0.5] and single labels such as a@localhost are refused, so codes
// go only to public mail domains.
func Normalize(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || len(value) > maxBytes {
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

// ClientKey identifies a connection's client for rate limits: its IP, or
// the /64 of an IPv6 address, which one household or host usually has.
func ClientKey(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ip
	}
	if v4 := parsed.To4(); v4 != nil {
		return v4.String()
	}
	return parsed.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// parseRequested parses the name and user_id that a proposal or an
// approval may request for a new account.
func parseRequested(params map[string]jsontext.Value) (name, requested string, err *authn.Error) {
	if name, err = authn.String(params, "name", false); err != nil {
		return "", "", err
	}
	if requested, err = authn.String(params, "user_id", false); err != nil {
		return "", "", err
	}
	return name, requested, nil
}

// AgeSends moves an address's emails back by d, for tests.
func (s *Scheme) AgeSends(addr string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.addresses[addr]; a != nil {
		for i := range a.sends {
			a.sends[i] = a.sends[i].Add(-d)
		}
	}
}

// Tracked counts the addresses the scheme keeps, and reports whether it
// keeps addr, for tests.
func (s *Scheme) Tracked(addr string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, kept := s.addresses[addr]
	return len(s.addresses), kept
}

// Fill fills the table of addresses with ones emailed now, for tests.
func (s *Scheme) Fill() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 0; len(s.addresses) < maxTrackedAddresses; i++ {
		s.createAddress("filler"+strconv.Itoa(i)+"@example.com", time.Now()).sends = []time.Time{time.Now()}
	}
}
