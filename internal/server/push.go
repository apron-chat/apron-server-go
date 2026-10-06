package server

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	maxPushTextRunes = 1000
	maxPushesPerUser = 10
	maxPushURLBytes  = 512
	maxPushTokenLen  = 4096
	// maxPushPayloadBytes bounds the JSON payload every kind delivers (§4.9).
	maxPushPayloadBytes = 2048
	// maxWakeScopes and maxWakeScopeBytes bound the wake names a server
	// reads: those beyond the first maxWakeScopes, and longer ones, are
	// ignored.
	maxWakeScopes     = 16
	maxWakeScopeBytes = 64
	// pushExpiry is how long a registration lasts unless registered again
	// (§4.9); clients register on each connection. pushSweepInterval is how
	// often expired registrations are looked for.
	pushExpiry        = 30 * 24 * time.Hour
	pushSweepInterval = time.Hour
	pushTimeout       = 10 * time.Second
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
	// maxPushesPerUserDay bounds the pushes delivered to one user in a UTC
	// day, counting only those the endpoint accepted (2xx).
	maxPushesPerUserDay = 1000
	// maxPushResponseDrain is how much of a push service's answer is read so
	// that its connection can be reused.
	maxPushResponseDrain = 64 << 10
)

// wakeScope is a set of wake scopes (§4.9).
type wakeScope uint8

const (
	wakeMentions wakeScope = 1 << iota
	wakeReplies
	wakePrivate
	wakeJoined
	wakeBadge
	// defaultWake is the scopes of a registration without wake.
	defaultWake = wakeMentions | wakeReplies
	// urgentScopes push with Urgency high; joined is normal, badge low.
	urgentScopes = wakeMentions | wakeReplies | wakePrivate
)

// wakeScopeNames lists the scopes this server implements, in the order
// server.push.wake advertises them.
var wakeScopeNames = []struct {
	name  string
	scope wakeScope
}{{"mentions", wakeMentions}, {"replies", wakeReplies}, {"private", wakePrivate}, {"joined", wakeJoined}, {"badge", wakeBadge}}

// names lists the scopes in a set, in advertised order.
func (w wakeScope) names() []string {
	names := []string{}
	for _, scope := range wakeScopeNames {
		if w&scope.scope != 0 {
			names = append(names, scope.name)
		}
	}
	return names
}

// parseWakeNames reads a wake list, ignoring scopes this server does not
// implement (§4.9).
func parseWakeNames(names []string) wakeScope {
	var wake wakeScope
	for _, name := range names {
		for _, scope := range wakeScopeNames {
			if scope.name == name {
				wake |= scope.scope
			}
		}
	}
	return wake
}

// pushIDPattern is the shape of a client's push_id (§4.9).
var pushIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// pushRegistration is one push endpoint of a user (§4.9), identified by the
// user and its url.
type pushRegistration struct {
	userID string
	kind   string
	url    string
	// token is a relay's bearer token.
	token  string
	pushID string
	// keys, for webpush and optionally relay, encrypt the payload.
	keys *pushKeys
	wake wakeScope
	// renewed is when the client last registered it; it expires pushExpiry
	// later.
	renewed time.Time
	// lastUnread is the unread count the endpoint last accepted, -1 when
	// none was since the server started; a badge push is sent only when the
	// count differs (§4.9). sent numbers the pushes handed to the deliverer
	// and accepted the latest of them the endpoint accepted, so a push
	// accepted late does not record an older count.
	lastUnread int
	sent       uint64
	accepted   uint64
	// pendingUnread is the count of the latest push sent, while inFlight:
	// a badge push for the same count waits on its outcome.
	pendingUnread int
	inFlight      bool
}

// live reports whether the registration has not expired at now.
func (p *pushRegistration) live(now time.Time) bool {
	return now.Before(p.renewed.Add(pushExpiry))
}

// takesBadges reports whether the registration gets badge pushes: it has
// scope badge, which webpush ignores (§4.9).
func (p *pushRegistration) takesBadges() bool {
	return p.wake&wakeBadge != 0 && p.kind != "webpush"
}

// pushKey identifies a registration in Server.pushes and the store: its
// user and url, which holds no space.
func pushKey(userID, url string) string {
	return userID + " " + url
}

// addPushLocked records a registration, replacing the user's registration
// of the same url.
func (s *Server) addPushLocked(u *userState, p *pushRegistration) {
	u.pushes[p.url] = p
	key := pushKey(u.id, p.url)
	s.pushes[key] = p
	s.touchPush(key)
}

// removePushLocked forgets a registration, if it is still the one recorded.
// A user left without registrations has no unread counts kept.
func (s *Server) removePushLocked(p *pushRegistration) {
	key := pushKey(p.userID, p.url)
	if s.pushes[key] != p {
		return
	}
	delete(s.pushes, key)
	s.touchPush(key)
	if u := s.users[p.userID]; u != nil && u.pushes[p.url] == p {
		delete(u.pushes, p.url)
		if len(u.pushes) == 0 {
			u.unread = nil
			s.stopBadgeLocked(u)
		}
	}
}

// registerPush records a push endpoint for the caller (§4.9): kind relay,
// with an optional bearer token, or webpush, with the subscription's keys;
// a relay with keys gets the payload encrypted as for webpush. Registering
// a url again replaces the caller's registration of it and renews it; other
// users' registrations of the same url are their own. A user holds at most
// maxPushesPerUser: another replaces the one least recently registered.
func (s *Server) registerPush(c *client, req request) (any, bool, *rpcError) {
	kind, err := parseString(req.params, "kind", true)
	if err != nil {
		return nil, false, err
	}
	if kind != "relay" && kind != "webpush" {
		return nil, false, invalidParams("Unknown push kind %q; this server supports relay and webpush", kind)
	}
	endpoint, err := parseString(req.params, "url", true)
	if err != nil {
		return nil, false, err
	}
	token, err := parseString(req.params, "token", false)
	if err != nil {
		return nil, false, err
	}
	endpoint, problem := s.checkPushURL(endpoint)
	if problem != "" {
		return nil, false, invalidParams("%s", problem)
	}
	if len(endpoint) > maxPushURLBytes || len(token) > maxPushTokenLen {
		return nil, false, &rpcError{Code: codeTooLarge, Message: fmt.Sprintf("url is at most %d bytes and token at most %d", maxPushURLBytes, maxPushTokenLen)}
	}
	pushID, err := parseString(req.params, "push_id", false)
	if err != nil {
		return nil, false, err
	}
	if _, has := req.params["push_id"]; has && !pushIDPattern.MatchString(pushID) {
		return nil, false, invalidParams("push_id is 1 to 64 letters, digits, _ or -")
	}
	keys, err := parseKeysParam(req.params)
	if err != nil {
		return nil, false, err
	}
	if kind == "webpush" {
		if keys == nil {
			return nil, false, invalidParams("A webpush registration needs keys with p256dh and auth")
		}
		// A webpush subscription has no bearer token.
		token = ""
	}
	wake := defaultWake
	if raw, has := req.params["wake"]; has {
		var names []string
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &names) != nil || names == nil {
			return nil, false, invalidParams("wake must be an array of scope names")
		}
		// Past the bounds, names are ignored like unknown scopes.
		names = names[:min(len(names), maxWakeScopes)]
		names = slices.DeleteFunc(names, func(name string) bool { return len(name) > maxWakeScopeBytes })
		wake = parseWakeNames(names)
	}

	s.mu.Lock()
	defer s.unlock()
	u := c.user
	now := time.Now()
	s.expirePushesLocked(u, now)
	if _, replacing := u.pushes[endpoint]; !replacing && len(u.pushes) >= maxPushesPerUser {
		oldest := slices.MinFunc(slices.Collect(maps.Values(u.pushes)), func(a, b *pushRegistration) int {
			return a.renewed.Compare(b.renewed)
		})
		s.removePushLocked(oldest)
	}
	s.addPushLocked(u, &pushRegistration{
		userID: u.id, kind: kind, url: endpoint, token: token, pushID: pushID,
		keys: keys, wake: wake, renewed: now, lastUnread: -1,
	})
	return map[string]any{}, false, nil
}

// parseKeysParam reads a registration's optional keys (§4.9).
func parseKeysParam(params map[string]jsontext.Value) (*pushKeys, *rpcError) {
	if _, has := params["keys"]; !has {
		return nil, nil
	}
	var raw map[string]jsontext.Value
	if json.Unmarshal(params["keys"], &raw) != nil || raw == nil {
		return nil, invalidParams("keys must be an object with p256dh and auth")
	}
	p256dh, err := parseString(raw, "p256dh", true)
	if err != nil {
		return nil, invalidParams("keys.p256dh must be a string")
	}
	auth, err := parseString(raw, "auth", true)
	if err != nil {
		return nil, invalidParams("keys.auth must be a string")
	}
	keys, problem := parsePushKeys(p256dh, auth)
	if problem != nil {
		return nil, invalidParams("%s", problem.Error())
	}
	return &keys, nil
}

// unregisterPush removes the caller's registration of a url; an unknown url
// is already unregistered (§4.9).
func (s *Server) unregisterPush(c *client, req request) (any, bool, *rpcError) {
	endpoint, err := parseString(req.params, "url", true)
	if err != nil {
		return nil, false, err
	}
	if normalized, problem := normalizePushURL(endpoint); problem == "" {
		endpoint = normalized
	}
	s.mu.Lock()
	defer s.unlock()
	if registration := c.user.pushes[endpoint]; registration != nil {
		s.removePushLocked(registration)
	}
	return map[string]any{}, false, nil
}

// expirePushesLocked forgets the user's registrations that were not
// registered again within pushExpiry.
func (s *Server) expirePushesLocked(u *userState, now time.Time) {
	for _, p := range u.pushes {
		if !p.live(now) {
			s.removePushLocked(p)
		}
	}
}

// sweepPushes forgets expired registrations every pushSweepInterval until
// the server shuts down, so the store drops them though their users stay
// away.
func (s *Server) sweepPushes() {
	ticker := time.NewTicker(pushSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopped:
			return
		case <-ticker.C:
			s.mu.Lock()
			now := time.Now()
			for _, id := range slices.Sorted(maps.Keys(s.users)) {
				s.expirePushesLocked(s.users[id], now)
			}
			s.unlock()
		}
	}
}

// normalizePushURL is an endpoint in the one form a registration is kept
// under: an absolute URL without credentials, whitespace, or a host ending
// in a dot, with its scheme and host lowercased and no default port. A
// problem says why it is not one.
func normalizePushURL(endpoint string) (string, string) {
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

// checkPushURL normalizes an endpoint (normalizePushURL) and requires https
// (http too with AllowInsecurePush) and a host that is not an internal
// address literal or localhost. Names that resolve to internal addresses
// are refused when dialing.
func (s *Server) checkPushURL(endpoint string) (string, string) {
	endpoint, problem := normalizePushURL(endpoint)
	if problem != "" {
		return "", problem
	}
	parsed, _ := url.Parse(endpoint)
	if parsed.Scheme != "https" && !(s.config.AllowInsecurePush && parsed.Scheme == "http") {
		return "", "url must use https"
	}
	if s.config.AllowInsecurePush {
		return endpoint, ""
	}
	host := parsed.Hostname()
	if _, err := netip.ParseAddr(host); err == nil && !publicAddress(host) || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return "", "url must name a public host"
	}
	return endpoint, ""
}

// wakeLocked pushes a message to the users it concerns (§4.9). previous is
// the snapshot the save replaced, nil for a new message.
//
// A new message concerns the users its body.mentions lists (scope
// mentions) and the author of the message it replies to (replies), in any
// room they can see, the room's members (joined), and, in a private room
// or a thread of one, the users who joined every private room on the way
// up, members of the thread or not (private). An edit concerns only the
// users it adds to body.mentions. Nobody is woken by their own message.
//
// Each concerned user is woken only when no connection of theirs is
// attended (§4.5). Their unscoped mute or a dnd status silences every
// scope, and so does their mute of the room, or of a room it is a thread
// of; what is silenced reaches them only as a badge push (badgeLocked).
// Each live registration whose wake scopes select the message gets the
// payload with its push_id and the user's unread count, with the most
// urgent Urgency of the scopes that select it.
func (s *Server) wakeLocked(m *messageState, snapshot, previous map[string]any) {
	if s.config.DisablePush || len(s.pushes) == 0 || snapshot["deleted"] == true {
		return
	}
	r := s.rooms[m.roomID]
	reasons := make(map[string]wakeScope)
	body, _ := snapshot["body"].(map[string]any)
	for _, id := range mentions(body) {
		reasons[id] |= wakeMentions
	}
	if previous == nil {
		if ref, ok := snapshot["reply_to"].(map[string]any); ok {
			if target := s.messages[ref["message_id"].(string)]; target != nil {
				reasons[target.owner] |= wakeReplies
			}
		}
		for id, u := range r.members {
			if len(u.pushes) > 0 {
				reasons[id] |= wakeJoined
			}
		}
		// private selects messages in the private rooms the user joined and
		// in their threads (§4.9): those who can see a private room's
		// thread joined every private room above it.
		if everyone, audience := r.audience(); !everyone {
			for id, u := range audience {
				if len(u.pushes) > 0 {
					reasons[id] |= wakePrivate
				}
			}
		}
	} else {
		previousBody, _ := previous["body"].(map[string]any)
		for _, id := range mentions(previousBody) {
			delete(reasons, id)
		}
	}
	delete(reasons, m.owner)
	now := time.Now()
	for _, id := range slices.Sorted(maps.Keys(reasons)) {
		u := s.users[id]
		if u == nil || len(u.pushes) == 0 || !r.visibleTo(u) || u.attended() || u.silenced(now) || u.roomMuted(r, now) {
			continue
		}
		why := reasons[id]
		s.expirePushesLocked(u, now)
		unread := -1
		for _, p := range sortedPushes(u) {
			scopes := why & p.wake
			if scopes == 0 {
				continue
			}
			if unread < 0 {
				unread = s.unreadLocked(u, now)
			}
			urgency := "normal"
			if scopes&urgentScopes != 0 {
				urgency = "high"
			}
			s.deliverPushLocked(p, unread, pushPayload(p.pushID, unread, snapshot), urgency, false)
		}
	}
}

// sortedPushes lists the user's registrations by url, so deliveries go out
// in a stable order.
func sortedPushes(u *userState) []*pushRegistration {
	return slices.SortedFunc(maps.Values(u.pushes), func(a, b *pushRegistration) int {
		return cmp.Compare(a.url, b.url)
	})
}

// deliverPushLocked hands a payload carrying the count unread to the
// deliverer, unless the user has had maxPushesPerUserDay pushes today. A
// push the endpoint accepts, with a 2xx, counts toward them and records
// unread as the count the endpoint has; one answered 404 or 410 forgets the
// registration. A badge push replaces one still waiting for the same
// registration.
func (s *Server) deliverPushLocked(p *pushRegistration, unread int, payload []byte, urgency string, badge bool) {
	if u := s.users[p.userID]; u == nil || u.pushesTodayLocked(time.Now()) >= maxPushesPerUserDay {
		return
	}
	p.sent++
	seq := p.sent
	p.pendingUnread, p.inFlight = unread, true
	done := func(status int) {
		accepted := status >= 200 && status < 300
		gone := status == http.StatusNotFound || status == http.StatusGone
		s.mu.Lock()
		defer s.unlock()
		if seq == p.sent {
			p.inFlight = false
		}
		if !accepted && !gone {
			return
		}
		u := s.users[p.userID]
		switch {
		case u == nil:
		case accepted:
			u.pushesTodayLocked(time.Now())
			u.pushesToday++
			if seq > p.accepted {
				p.accepted, p.lastUnread = seq, unread
			}
		default:
			s.removePushLocked(p)
		}
	}
	latest := ""
	if badge {
		latest = pushKey(p.userID, p.url)
	}
	s.push.deliver(*p, payload, urgency, latest, done)
}

// pushesTodayLocked is how many pushes the user's endpoints accepted on
// now's UTC day.
func (u *userState) pushesTodayLocked(now time.Time) int {
	if day := now.UTC().Format(time.DateOnly); day != u.pushDay {
		u.pushDay, u.pushesToday = day, 0
	}
	return u.pushesToday
}

// pushPayload is the payload every kind delivers (§4.9): push_id, unread,
// and the message without log_id, format, embeds, or ext, its text
// truncated to maxPushTextRunes. When the JSON is longer than
// maxPushPayloadBytes the message is cut to fit: mentions go first, then
// text is shortened, and then body is dropped, from keeps only user_id, and
// reply_to goes, in turn.
func pushPayload(pushID string, unread int, snapshot map[string]any) []byte {
	message := map[string]any{
		"message_id": snapshot["message_id"],
		"room_id":    snapshot["room_id"],
		"from":       snapshot["from"],
	}
	if reply, ok := snapshot["reply_to"].(map[string]any); ok {
		message["reply_to"] = map[string]any{"message_id": reply["message_id"]}
	}
	text := ""
	var mentioned []string
	if body, ok := snapshot["body"].(map[string]any); ok {
		text, _ = body["text"].(string)
		mentioned = mentions(body)
	}
	if utf8.RuneCountInString(text) > maxPushTextRunes {
		text = string([]rune(text)[:maxPushTextRunes-1]) + "…"
	}
	envelope := map[string]any{"unread": unread, "message": message}
	if pushID != "" {
		envelope["push_id"] = pushID
	}
	setBody := func(text string, mentioned []string) {
		body := map[string]any{}
		if text != "" {
			body["text"] = text
		}
		if len(mentioned) > 0 {
			body["mentions"] = mentioned
		}
		if len(body) > 0 {
			message["body"] = body
		} else {
			delete(message, "body")
		}
	}
	var payload []byte
	fits := func() bool {
		payload = encodeJSON(envelope)
		return len(payload) <= maxPushPayloadBytes
	}
	setBody(text, mentioned)
	if fits() {
		return payload
	}
	setBody(text, nil)
	if fits() {
		return payload
	}
	// The longest prefix of the text that fits, found by bisection.
	runes := []rune(text)
	prefix := func(n int) bool {
		setBody(string(runes[:n])+"…", nil)
		return fits()
	}
	low, high := 0, len(runes)
	for low < high {
		if middle := (low + high + 1) / 2; prefix(middle) {
			low = middle
		} else {
			high = middle - 1
		}
	}
	if low > 0 && prefix(low) {
		return payload
	}
	for _, cut := range []func(){
		func() { setBody("", nil) },
		func() {
			if from, ok := message["from"].(map[string]any); ok {
				message["from"] = map[string]any{"user_id": from["user_id"]}
			}
		},
		func() { delete(message, "reply_to") },
	} {
		if cut(); fits() {
			break
		}
	}
	return payload
}

// pushDeliverer POSTs push payloads in the background, off the paths that
// deliver messages. Each push host has its own lane of bounded concurrency
// and queue, so a slow or stalling host delays only pushes to itself.
// Unless insecure pushes are allowed, it refuses to connect to loopback,
// private, link-local, and other non-public addresses, checked on the
// dialed address so DNS cannot redirect it.
type pushDeliverer struct {
	client  *http.Client
	slots   chan struct{}
	pending sync.WaitGroup
	// vapid signs webpush deliveries, with subject as their contact.
	vapid   *vapidKey
	subject string

	mu     sync.Mutex
	queued int
	users  map[string]int
	hosts  map[string]*pushLane
	// latest holds each badge delivery that has not started, by
	// registration, so a newer count replaces its payload and its done.
	latest map[string]*queuedPush
}

// queuedPush is a delivery's registration, as it was when the delivery
// was handed over, its payload, and what it reports to.
type queuedPush struct {
	registration pushRegistration
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

// publicAddress reports whether a dialed address is on the public internet.
func publicAddress(host string) bool {
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

func newPushDeliverer(allowInternal bool) *pushDeliverer {
	dialer := &net.Dialer{Timeout: pushTimeout}
	if !allowInternal {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if !publicAddress(host) {
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
	}
	return &pushDeliverer{
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

// deliver POSTs payload to a registration's url. With latest, the key of a
// badge delivery, a delivery for the same key that has not started takes
// the registration, as registered now, payload, and done in place of its
// own, and the replaced done is never called. done receives the endpoint's status code, or 0 for a delivery
// dropped beyond the user's, the host's, or the server's queue bound, or
// one that gets no answer.
func (p *pushDeliverer) deliver(registration pushRegistration, payload []byte, urgency, latest string, done func(status int)) {
	parsed, err := url.Parse(registration.url)
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
	if p.queued >= maxPushQueued || p.users[registration.userID] >= maxPushQueuedPerUser || lane != nil && lane.queued >= maxPushQueuedPerHost {
		p.mu.Unlock()
		p.dropped(done)
		return
	}
	p.queued++
	p.users[registration.userID]++
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
		defer p.release(registration.userID, host, lane)
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
func (p *pushDeliverer) dropped(done func(status int)) {
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
func (p *pushDeliverer) request(ctx context.Context, registration pushRegistration, payload []byte, urgency string) (*http.Request, error) {
	body, contentType := payload, "application/json"
	if registration.keys != nil {
		encrypted, err := encryptPush(payload, *registration.keys)
		if err != nil {
			return nil, err
		}
		body, contentType = encrypted, "application/octet-stream"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, registration.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", contentType)
	if registration.keys != nil {
		request.Header.Set("Content-Encoding", "aes128gcm")
	}
	request.Header.Set("TTL", pushTTL)
	request.Header.Set("Urgency", urgency)
	switch registration.kind {
	case "webpush":
		if p.vapid == nil || registration.keys == nil {
			return nil, errors.New("webpush needs a VAPID key and subscription keys")
		}
		authorization, err := p.vapid.authorization(registration.url, p.subject, time.Now())
		if err != nil {
			return nil, err
		}
		request.Header.Set("Authorization", authorization)
	default:
		if registration.token != "" {
			request.Header.Set("Authorization", "Bearer "+registration.token)
		}
	}
	return request, nil
}

// release ends a delivery's accounting, forgetting an unused host lane.
func (p *pushDeliverer) release(userID, host string, lane *pushLane) {
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

func (p *pushDeliverer) wait() {
	p.pending.Wait()
}
