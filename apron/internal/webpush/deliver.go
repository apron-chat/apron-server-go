package webpush

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	pushTimeout = 10 * time.Second
	// Deliveries run in lanes per push host, at most maxPushPerHost at once
	// to one host, and at most maxConcurrentPushPOST in all. A user has at
	// most maxPushQueuedPerUser deliveries waiting or running, a host
	// maxPushQueuedPerHost, and the server maxPushQueued; beyond them a
	// delivery is dropped, as pushes are best effort.
	maxConcurrentPushPOST = 32
	maxPushPerHost        = 8
	maxPushQueuedPerUser  = 20
	maxPushQueuedPerHost  = 256
	maxPushQueued         = 1024
	// maxPushResponseDrain is how much of a push service's answer is read so
	// that its connection can be reused.
	maxPushResponseDrain = 64 << 10
)

// Target is where a delivery goes: a registration of a user (§4.9), of
// kind relay, with an optional bearer token, or webpush; with Keys, the
// payload is encrypted for them.
type Target struct {
	UserID string
	Kind   string
	URL    string
	Token  string
	Keys   *Keys
}

// NormalizeURL is an endpoint in the one form a registration is kept
// under: an absolute URL without credentials, whitespace, or a host ending
// in a dot, with its scheme and host lowercased and no default port. A
// problem says why it is not one.
func NormalizeURL(endpoint string) (string, string) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" || strings.ContainsFunc(endpoint, func(r rune) bool { return r <= ' ' }) {
		return "", "url must be an absolute URL"
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	if host == "" || strings.HasSuffix(host, ".") {
		return "", "url must name a host without a trailing dot"
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := parsed.Port(); port != "" && !(parsed.Scheme == "https" && port == "443") && !(parsed.Scheme == "http" && port == "80") {
		host += ":" + port
	}
	parsed.Host = host
	return parsed.String(), ""
}

// CheckURL normalizes an endpoint (NormalizeURL) and requires https (http
// too when insecure) and, unless insecure, a host that is not an internal
// address literal or localhost. Names that resolve to internal addresses
// are refused when dialing.
func CheckURL(endpoint string, insecure bool) (string, string) {
	endpoint, problem := NormalizeURL(endpoint)
	if problem != "" {
		return "", problem
	}
	parsed, _ := url.Parse(endpoint)
	if parsed.Scheme != "https" && !(insecure && parsed.Scheme == "http") {
		return "", "url must use https"
	}
	if insecure {
		return endpoint, ""
	}
	host := parsed.Hostname()
	if _, err := netip.ParseAddr(host); err == nil && !PublicAddress(host) || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return "", "url must name a public host"
	}
	return endpoint, ""
}

// Deliverer POSTs push payloads in the background, off the paths that
// deliver messages. Each push host has its own lane of bounded concurrency
// and queue, so a slow or stalling host delays only pushes to itself.
// Unless insecure pushes are allowed, it refuses to connect to loopback,
// private, link-local, and other non-public addresses, checked on the
// dialed address so DNS cannot redirect it.
type Deliverer struct {
	client  *http.Client
	slots   chan struct{}
	pending sync.WaitGroup
	// VAPID signs webpush deliveries, with Subject as their contact. They
	// are set before the first delivery.
	VAPID   *VAPIDKey
	Subject string

	mu     sync.Mutex
	queued int
	users  map[string]int
	hosts  map[string]*pushLane
	// latest holds each badge delivery that has not started, by
	// registration, so a newer count replaces its payload and its done.
	latest map[string]*queuedPush
}

// queuedPush is a delivery's target, as it was when the delivery was
// handed over, its payload, and what it reports to.
type queuedPush struct {
	registration Target
	payload      []byte
	done         func(status int)
}

// pushLane bounds concurrent deliveries to one push host; queued counts the
// deliveries waiting for it or running, so an unused lane is forgotten.
type pushLane struct {
	slots  chan struct{}
	queued int
}

// nonPublicPrefixes are special-purpose ranges that netip's classification
// does not already exclude: this network, shared address space (CGNAT),
// IETF protocol assignments, documentation, the 6to4 relay anycast,
// benchmarking, and the reserved class E range; and in IPv6, IPv4-compatible
// addresses, discard-only, translation and tunneling prefixes that embed
// IPv4 addresses, benchmarking, ORCHID, documentation, deprecated
// site-local, and segment routing.
var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2001:2::/48"),
	netip.MustParsePrefix("2001:10::/28"),
	netip.MustParsePrefix("2001:20::/28"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
	netip.MustParsePrefix("fec0::/10"),
}

// PublicAddress reports whether a dialed address is on the public internet.
func PublicAddress(host string) bool {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return false
	}
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

// NewDeliverer returns a deliverer, which refuses to connect to internal
// addresses unless allowInternal.
func NewDeliverer(allowInternal bool) *Deliverer {
	dialer := &net.Dialer{Timeout: pushTimeout}
	if !allowInternal {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if !PublicAddress(host) {
				return errors.New("push endpoint resolves to an internal address")
			}
			return nil
		}
	}
	transport := &http.Transport{
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: pushTimeout,
		// A custom dialer turns HTTP/2 off unless asked; push services
		// serve many pushes over one HTTP/2 connection.
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: maxPushPerHost,
		// A Transport literal keeps idle connections forever and without a
		// total bound; push hosts are chosen by clients, so both are set.
		MaxIdleConns:    maxConcurrentPushPOST * 2,
		IdleConnTimeout: 90 * time.Second,
	}
	return &Deliverer{
		client: &http.Client{
			Timeout:   pushTimeout,
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		slots:  make(chan struct{}, maxConcurrentPushPOST),
		users:  make(map[string]int),
		hosts:  make(map[string]*pushLane),
		latest: make(map[string]*queuedPush),
	}
}

// Deliver POSTs payload to a target's url. With latest, the key of a
// badge delivery, a delivery for the same key that has not started takes
// the registration, as registered now, payload, and done in place of its
// own, and the replaced done is never called. done receives the endpoint's status code, or 0 for a delivery
// dropped beyond the user's, the host's, or the server's queue bound, or
// one that gets no answer.
func (p *Deliverer) Deliver(registration Target, payload []byte, urgency, latest string, done func(status int)) {
	parsed, err := url.Parse(registration.URL)
	if err != nil {
		p.dropped(done)
		return
	}
	host := strings.ToLower(parsed.Host)
	p.mu.Lock()
	if waiting := p.latest[latest]; latest != "" && waiting != nil {
		waiting.registration, waiting.payload, waiting.done = registration, payload, done
		p.mu.Unlock()
		return
	}
	lane := p.hosts[host]
	if p.queued >= maxPushQueued || p.users[registration.UserID] >= maxPushQueuedPerUser || lane != nil && lane.queued >= maxPushQueuedPerHost {
		p.mu.Unlock()
		p.dropped(done)
		return
	}
	p.queued++
	p.users[registration.UserID]++
	if lane == nil {
		lane = &pushLane{slots: make(chan struct{}, maxPushPerHost)}
		p.hosts[host] = lane
	}
	lane.queued++
	waiting := &queuedPush{registration: registration, payload: payload, done: done}
	if latest != "" {
		p.latest[latest] = waiting
	}
	p.mu.Unlock()
	p.pending.Add(1)
	go func() {
		defer p.pending.Done()
		defer p.release(registration.UserID, host, lane)
		// The host's lane first, then a shared slot: a stalled host holds at
		// most its lane's share of the slots.
		lane.slots <- struct{}{}
		defer func() { <-lane.slots }()
		p.slots <- struct{}{}
		defer func() { <-p.slots }()
		p.mu.Lock()
		registration, payload, done := waiting.registration, waiting.payload, waiting.done
		if latest != "" {
			delete(p.latest, latest)
		}
		p.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
		defer cancel()
		request, err := p.request(ctx, registration, payload, urgency)
		if err != nil {
			done(0)
			return
		}
		response, err := p.client.Do(request)
		if err != nil {
			done(0)
			return
		}
		// Reading the answer lets the connection be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxPushResponseDrain))
		response.Body.Close()
		done(response.StatusCode)
	}()
}

// dropped tells done of a delivery dropped before it started, apart, since
// the caller may hold the lock done takes.
func (p *Deliverer) dropped(done func(status int)) {
	p.pending.Add(1)
	go func() {
		defer p.pending.Done()
		done(0)
	}()
}

// request builds a delivery (§4.9), with TTL and Urgency for every kind.
// relay: the payload as JSON, or, with keys, encrypted as for webpush, with
// the token, if any, as bearer. webpush (RFC 8030): the payload encrypted
// for the subscription (RFC 8291), with a VAPID Authorization (RFC 8292).
func (p *Deliverer) request(ctx context.Context, registration Target, payload []byte, urgency string) (*http.Request, error) {
	body, contentType := payload, "application/json"
	if registration.Keys != nil {
		encrypted, err := Encrypt(payload, *registration.Keys)
		if err != nil {
			return nil, err
		}
		body, contentType = encrypted, "application/octet-stream"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, registration.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", contentType)
	if registration.Keys != nil {
		request.Header.Set("Content-Encoding", "aes128gcm")
	}
	request.Header.Set("TTL", pushTTL)
	request.Header.Set("Urgency", urgency)
	switch registration.Kind {
	case "webpush":
		if p.VAPID == nil || registration.Keys == nil {
			return nil, errors.New("webpush needs a VAPID key and subscription keys")
		}
		authorization, err := p.VAPID.authorization(registration.URL, p.Subject, time.Now())
		if err != nil {
			return nil, err
		}
		request.Header.Set("Authorization", authorization)
	default:
		if registration.Token != "" {
			request.Header.Set("Authorization", "Bearer "+registration.Token)
		}
	}
	return request, nil
}

// release ends a delivery's accounting, forgetting an unused host lane.
func (p *Deliverer) release(userID, host string, lane *pushLane) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queued--
	if p.users[userID]--; p.users[userID] == 0 {
		delete(p.users, userID)
	}
	if lane.queued--; lane.queued == 0 {
		delete(p.hosts, host)
	}
}

// Wait waits for the deliveries in progress, and the done of each.
func (p *Deliverer) Wait() {
	p.pending.Wait()
}
