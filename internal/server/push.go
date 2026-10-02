package server

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	maxPushTextRunes = 1000
	maxPushesPerUser = 10
	maxPushURLBytes  = 2048
	maxPushTokenLen  = 4096
	// maxPushPayloadBytes bounds the JSON payload every kind delivers (§4.7).
	maxPushPayloadBytes = 3072
	// maxWakeScopes and maxWakeScopeBytes bound a registration's wake list.
	maxWakeScopes     = 16
	maxWakeScopeBytes = 64
	// pushExpiry is how long a registration lasts unless registered again
	// (§4.7); clients register on each connection.
	pushExpiry  = 30 * 24 * time.Hour
	pushTimeout = 10 * time.Second
	// Deliveries run in lanes per push host, at most maxPushPerHost at once
	// to one host, and at most maxConcurrentPushPOST in all. A user has at
	// most maxPushQueuedPerUser deliveries waiting or running, and the server
	// at most maxPushQueued; beyond them a delivery is dropped, as pushes are
	// best effort.
	maxConcurrentPushPOST = 32
	maxPushPerHost        = 8
	maxPushQueuedPerUser  = 20
	maxPushQueued         = 1024
	// maxUnread caps the unread count a push carries, and maxUnreadScan the
	// records one room's count looks through, newest first, so counting
	// stays cheap for a user who never reads a busy room.
	maxUnread     = 999
	maxUnreadScan = 5000
)

// wakeScope is a set of wake scopes (§4.7).
type wakeScope uint8

const (
	wakeMentions wakeScope = 1 << iota
	wakeReplies
	wakePrivate
	wakeJoined
	wakeBadge
	// defaultWake is the scopes of a registration without wake.
	defaultWake = wakeMentions | wakeReplies
	// messageScopes are the scopes that select new messages.
	messageScopes = wakeMentions | wakeReplies | wakePrivate | wakeJoined
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
// implement (§4.7).
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

// pushIDPattern is the shape of a client's push_id (§4.7).
var pushIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// pushRegistration is one push endpoint of a user (§4.7), identified by the
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
	// lastUnread is the unread count the endpoint last received, -1 when
	// none was sent since the server started; a badge push is sent only when
	// the count differs (§4.7).
	lastUnread int
}

// live reports whether the registration has not expired at now.
func (p *pushRegistration) live(now time.Time) bool {
	return now.Before(p.renewed.Add(pushExpiry))
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
func (s *Server) removePushLocked(p *pushRegistration) {
	key := pushKey(p.userID, p.url)
	if s.pushes[key] != p {
		return
	}
	delete(s.pushes, key)
	s.touchPush(key)
	if u := s.users[p.userID]; u != nil && u.pushes[p.url] == p {
		delete(u.pushes, p.url)
	}
}

// livePush reports whether the user has a registration that has not
// expired, which can notify them (§4.11).
func (u *userState) livePush(now time.Time) bool {
	for _, p := range u.pushes {
		if p.live(now) {
			return true
		}
	}
	return false
}

// registerPush records a push endpoint for the caller (§4.7): kind relay,
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
	if len(endpoint) > maxPushURLBytes || len(token) > maxPushTokenLen {
		return nil, false, invalidParams("url is at most %d bytes and token at most %d", maxPushURLBytes, maxPushTokenLen)
	}
	if problem := s.checkPushURL(endpoint); problem != "" {
		return nil, false, invalidParams("%s", problem)
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
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &names) != nil || names == nil ||
			len(names) > maxWakeScopes || slices.ContainsFunc(names, func(name string) bool { return len(name) > maxWakeScopeBytes }) {
			return nil, false, invalidParams("wake is an array of at most %d scope names of at most %d bytes", maxWakeScopes, maxWakeScopeBytes)
		}
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
	// A user without an attended connection can now be notified (§4.11).
	s.statusChangedLocked(u, nil, false)
	return map[string]any{}, false, nil
}

// parseKeysParam reads a registration's optional keys (§4.7).
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
// is already unregistered (§4.7).
func (s *Server) unregisterPush(c *client, req request) (any, bool, *rpcError) {
	endpoint, err := parseString(req.params, "url", true)
	if err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.unlock()
	if registration := c.user.pushes[endpoint]; registration != nil {
		s.removePushLocked(registration)
		s.statusChangedLocked(c.user, nil, false)
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

// checkPushURL requires an absolute https URL (http too with
// AllowInsecurePush) without credentials or whitespace. Internal addresses
// are refused when dialing.
func (s *Server) checkPushURL(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || parsed.User != nil || strings.ContainsFunc(endpoint, func(r rune) bool { return r <= ' ' }) {
		return "url must be an absolute URL"
	}
	if parsed.Scheme != "https" && !(s.config.AllowInsecurePush && parsed.Scheme == "http") {
		return "url must use https"
	}
	return ""
}

// wakeLocked pushes a message to the users it concerns (§4.7). previous is
// the snapshot the save replaced, nil for a new message.
//
// A new message concerns the users its body.mentions lists (scope
// mentions) and the author of the message it replies to (replies), in any
// room they can see, and the room's members (joined, and private in a room
// only some can see). An edit concerns only the users it adds to
// body.mentions. The author is never woken by their own message.
//
// Each concerned user is woken only when no connection of theirs is
// attended (§4.11) and they are not muted; a room they muted wakes them only
// for mentions. Each live registration whose wake scopes include a reason
// of theirs gets the payload with its push_id and unread count.
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
		member := wakeJoined
		if everyone, _ := r.audience(); !everyone {
			member |= wakePrivate
		}
		for id, u := range r.members {
			if len(u.pushes) > 0 {
				reasons[id] |= member
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
	messageID, _ := strconv.ParseInt(m.id, 10, 64)
	const urgent = wakeMentions | wakeReplies | wakePrivate
	for _, id := range slices.Sorted(maps.Keys(reasons)) {
		u := s.users[id]
		if u == nil || len(u.pushes) == 0 || !r.visibleTo(u) {
			continue
		}
		if u.joined[r.id] == nil && reasons[id]&(wakeMentions|wakeReplies) != 0 {
			// A room the user has not joined counts toward unread from the
			// first message there that mentioned or replied to them.
			if since, ok := u.pings[r.id]; !ok || since > messageID {
				u.pings[r.id] = messageID
				s.touchUser(u.id)
			}
		}
		if u.attended() || u.mute.active(now) {
			continue
		}
		roomMuted := u.roomMuted(r.id, now)
		s.expirePushesLocked(u, now)
		for _, p := range sortedPushes(u) {
			scopes := reasons[id] & p.wake
			if roomMuted {
				scopes &= wakeMentions
			}
			if scopes == 0 {
				continue
			}
			unread := s.unreadLocked(u, p.wake, now)
			p.lastUnread = unread
			urgency := "normal"
			if scopes&urgent != 0 {
				urgency = "high"
			}
			s.deliverPushLocked(p, pushPayload(p.pushID, unread, snapshot), urgency)
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

// deliverPushLocked hands a payload to the deliverer, forgetting the
// registration if its endpoint answers that it is gone.
func (s *Server) deliverPushLocked(p *pushRegistration, payload []byte, urgency string) {
	s.push.deliver(*p, payload, urgency, func(gone bool) {
		if gone {
			s.mu.Lock()
			s.removePushLocked(p)
			if u := s.users[p.userID]; u != nil {
				s.statusChangedLocked(u, nil, false)
			}
			s.unlock()
		}
	})
}

// badgeLocked sends a badge push (§4.7), {push_id, unread} without a
// message, to each of the user's registrations with scope badge whose
// unread count changed since it last received one: after the user reads,
// posts, leaves a room, or changes a mute, or after a message they counted
// is deleted. It goes to attended users too, whose other devices show the
// count, but not to muted ones. webpush registrations get none: a browser
// shows a notification for every push.
func (s *Server) badgeLocked(u *userState, now time.Time) {
	if s.config.DisablePush || len(u.pushes) == 0 || u.mute.active(now) {
		return
	}
	for _, p := range sortedPushes(u) {
		if p.wake&wakeBadge == 0 || p.kind == "webpush" || !p.live(now) {
			continue
		}
		unread := s.unreadLocked(u, p.wake, now)
		if unread == p.lastUnread {
			continue
		}
		p.lastUnread = unread
		payload := map[string]any{"unread": unread}
		if p.pushID != "" {
			payload["push_id"] = p.pushID
		}
		s.deliverPushLocked(p, encodeJSON(payload), "low")
	}
}

// badgeRoomLocked sends badge pushes after a message in r is deleted, to
// the users it may have counted for: the room's members and those its last
// snapshot, previous, mentioned or replied to.
func (s *Server) badgeRoomLocked(r *roomState, previous map[string]any) {
	users := maps.Clone(r.members)
	body, _ := previous["body"].(map[string]any)
	for _, id := range mentions(body) {
		if u := s.users[id]; u != nil {
			users[id] = u
		}
	}
	if ref, ok := previous["reply_to"].(map[string]any); ok {
		if target := s.messages[ref["message_id"].(string)]; target != nil && s.users[target.owner] != nil {
			users[target.owner] = s.users[target.owner]
		}
	}
	now := time.Now()
	for _, id := range slices.Sorted(maps.Keys(users)) {
		s.badgeLocked(users[id], now)
	}
}

// unreadLocked counts the user's unread messages for a registration's
// scopes (§4.7): messages by others, not deleted, in rooms the user can
// see, after the user's read position in each room, that the scopes
// select, counting only mentions in rooms the user muted. A registration
// with only badge counts by the default scopes. The read position in a
// joined room is the latest of the user's read cursor (§4.4), their join,
// and their latest message there; in a room they have not joined, it is
// their read cursor, and only rooms where they were mentioned or replied to
// count, from the first such message. The count stops at maxUnread, and
// each room's at the newest maxUnreadScan records.
func (s *Server) unreadLocked(u *userState, wake wakeScope, now time.Time) int {
	scopes := wake & messageScopes
	if scopes == 0 {
		scopes = defaultWake
	}
	rooms := maps.Clone(u.joined)
	for id := range u.pings {
		if r := s.rooms[id]; r != nil && rooms[id] == nil {
			rooms[id] = r
		}
	}
	count := 0
	for _, id := range slices.Sorted(maps.Keys(rooms)) {
		r := rooms[id]
		if !r.visibleTo(u) {
			continue
		}
		joined := u.joined[r.id] != nil
		since := r.reads[u.id].id
		var member wakeScope
		if joined {
			since = max(since, r.active[u.id])
			member = wakeJoined
			if everyone, _ := r.audience(); !everyone {
				member |= wakePrivate
			}
		} else {
			since = max(since, u.pings[r.id]-1)
		}
		muted := u.roomMuted(r.id, now)
		start, _ := slices.BinarySearchFunc(r.log, since+1, compareLogID)
		start = max(start, len(r.log)-maxUnreadScan)
		for _, record := range r.log[start:] {
			if record.kind != kindMessage {
				continue
			}
			// A message's first snapshot is logged at its message_id; an
			// edit's log_id names no message.
			m := s.messages[formatID(record.id)]
			if m == nil || m.roomID != r.id || m.owner == u.id {
				continue
			}
			info := m.info()
			if info.deleted {
				continue
			}
			why := member
			if slices.Contains(info.mentions, u.id) {
				why |= wakeMentions
			}
			if target := s.messages[info.replyTo]; target != nil && target.owner == u.id {
				why |= wakeReplies
			}
			if muted {
				why &= wakeMentions
			}
			if why&scopes != 0 {
				if count++; count >= maxUnread {
					return count
				}
			}
		}
	}
	return count
}

// pushPayload is the payload every kind delivers (§4.7): push_id, unread,
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
// deliver messages. Each push host has its own lane of bounded concurrency,
// so a slow or stalling host delays only pushes to itself. Unless insecure
// pushes are allowed, it refuses to connect to loopback, private,
// link-local, and other non-public addresses, checked on the dialed address
// so DNS cannot redirect it.
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
}

// pushLane bounds concurrent deliveries to one push host; queued counts the
// deliveries waiting for it or running, so an unused lane is forgotten.
type pushLane struct {
	slots  chan struct{}
	queued int
}

// nonPublicPrefixes are special-purpose ranges that netip's classification
// does not already exclude: shared address space (CGNAT), IETF protocol
// assignments, benchmarking, the reserved class E range, and IPv6
// translation and tunneling prefixes that embed IPv4 addresses.
var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2002::/16"),
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
	transport := &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: pushTimeout}
	return &pushDeliverer{
		client: &http.Client{
			Timeout:   pushTimeout,
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		slots: make(chan struct{}, maxConcurrentPushPOST),
		users: make(map[string]int),
		hosts: make(map[string]*pushLane),
	}
}

// deliver POSTs payload to a registration's url. done reports whether the
// endpoint said it is gone (404 or 410). A delivery beyond the user's or
// the server's queue bound is dropped.
func (p *pushDeliverer) deliver(registration pushRegistration, payload []byte, urgency string, done func(gone bool)) {
	parsed, err := url.Parse(registration.url)
	if err != nil {
		return
	}
	host := parsed.Host
	p.mu.Lock()
	if p.queued >= maxPushQueued || p.users[registration.userID] >= maxPushQueuedPerUser {
		p.mu.Unlock()
		return
	}
	p.queued++
	p.users[registration.userID]++
	lane := p.hosts[host]
	if lane == nil {
		lane = &pushLane{slots: make(chan struct{}, maxPushPerHost)}
		p.hosts[host] = lane
	}
	lane.queued++
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
		ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
		defer cancel()
		request, err := p.request(ctx, registration, payload, urgency)
		if err != nil {
			return
		}
		response, err := p.client.Do(request)
		if err != nil {
			return
		}
		response.Body.Close()
		done(response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone)
	}()
}

// request builds a delivery (§4.7). relay: the payload as JSON, or, with
// keys, encrypted as for webpush, with the token as bearer. webpush
// (RFC 8030): the payload encrypted for the subscription (RFC 8291), with
// a VAPID Authorization (RFC 8292), TTL, and Urgency.
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
		request.Header.Set("TTL", pushTTL)
		request.Header.Set("Urgency", urgency)
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
