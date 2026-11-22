package apron

import (
	"bytes"
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/apron-chat/apron-server-go/apron/internal/webpush"
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
	// maxPushesPerUserDay bounds the pushes delivered to one user in a UTC
	// day, counting only those the endpoint accepted (2xx).
	maxPushesPerUserDay = 1000
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
	// device is the key of the /invite-device token the registering
	// connection signed in with, zero for none, so replacing that token
	// removes only the registrations it made.
	device [32]byte
	// session is the key of the session token the registering connection
	// signed in or resumed with, zero for none, so ending that session
	// (account.go) removes the registrations it made.
	session [32]byte
	// keys, for webpush and optionally relay, encrypt the payload.
	keys *webpush.Keys
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
func (s *Server) registerPush(c *client, req request) (any, bool, *Error) {
	if c.service != [32]byte{} {
		// A service token's client is a program, which push does not wake;
		// a registration would outlive the token's revocation.
		return nil, false, &Error{Code: CodeDenied, Message: "A service token's connection does not register for push"}
	}
	d := req.params.Decoder()
	kind := d.String("kind", required)
	if d.Err() == nil && kind != "relay" && kind != "webpush" {
		d.Fail(invalidParams("Unknown push kind %q; this server supports relay and webpush", kind))
	}
	endpoint := d.String("url", required)
	token := d.String("token", optional)
	if err := paramError(d.Err()); err != nil {
		return nil, false, err
	}
	endpoint, problem := webpush.CheckURL(endpoint, s.config.AllowInsecurePush)
	if problem != "" {
		return nil, false, invalidParams("%s", problem)
	}
	if len(endpoint) > maxPushURLBytes || len(token) > maxPushTokenLen {
		return nil, false, &Error{Code: CodeTooLarge, Message: fmt.Sprintf("url is at most %d bytes and token at most %d", maxPushURLBytes, maxPushTokenLen)}
	}
	pushID := d.String("push_id", optional)
	if err := paramError(d.Err()); err != nil {
		return nil, false, err
	}
	if d.Has("push_id") && !pushIDPattern.MatchString(pushID) {
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
	if c.conn.Closing() {
		return nil, false, connectionClosing()
	}
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
		userID: u.id, kind: kind, url: endpoint, token: token, pushID: pushID, device: c.device, session: c.sessionKeyLocked(),
		keys: keys, wake: wake, renewed: now, lastUnread: -1,
	})
	return map[string]any{}, false, nil
}

// parseKeysParam reads a registration's optional keys (§4.9).
func parseKeysParam(params map[string]jsontext.Value) (*webpush.Keys, *Error) {
	if _, has := params["keys"]; !has {
		return nil, nil
	}
	var raw map[string]jsontext.Value
	if json.Unmarshal(params["keys"], &raw) != nil || raw == nil {
		return nil, invalidParams("keys must be an object with p256dh and auth")
	}
	p256dh, err := parseString(raw, "p256dh", required)
	if err != nil {
		return nil, invalidParams("keys.p256dh must be a string")
	}
	auth, err := parseString(raw, "auth", required)
	if err != nil {
		return nil, invalidParams("keys.auth must be a string")
	}
	keys, problem := webpush.ParseKeys(p256dh, auth)
	if problem != nil {
		return nil, invalidParams("%s", problem.Error())
	}
	return &keys, nil
}

// unregisterPush removes the caller's registration of a url; an unknown url
// is already unregistered (§4.9).
func (s *Server) unregisterPush(c *client, req request) (any, bool, *Error) {
	endpoint, err := parseString(req.params, "url", required)
	if err != nil {
		return nil, false, err
	}
	if normalized, problem := webpush.NormalizeURL(endpoint); problem == "" {
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
// scope; their mute of the room, or of a room it is a thread of, every
// scope but mentions, as in other chat apps. What is silenced reaches them
// only as a badge push (badgeLocked).
// Each live registration whose wake scopes select the message gets the
// payload with its push_id and the user's unread count, with the most
// urgent Urgency of the scopes that select it.
func (s *Server) wakeLocked(m *messageState, snapshot, previous map[string]any) {
	// Transient notices, which have no message_id, are never pushed (§4.9).
	if s.config.DisablePush || len(s.pushes) == 0 || snapshot["deleted"] == true || snapshot["message_id"] == nil {
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
		if u == nil || len(u.pushes) == 0 || !r.visibleTo(u) || u.attended() || u.silenced(now) {
			continue
		}
		why := reasons[id]
		if u.roomMuted(r, now) {
			// A mention gets through a room mute; nothing else does.
			if why &= wakeMentions; why == 0 {
				continue
			}
		}
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
	// Once the server is shutting down, Shutdown may already be waiting for
	// the deliveries in progress: none is added, as live text that Shutdown
	// ends could otherwise release a held push.
	if s.closed {
		return
	}
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
	s.push.Deliver(webpush.Target{UserID: p.userID, Kind: p.kind, URL: p.url, Token: p.token, Keys: p.keys}, payload, urgency, latest, done)
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
