package apron

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/apron-chat/apron-server-go/apron/internal/push"
	"github.com/apron-chat/apron-server-go/apron/internal/roomlog"
	"github.com/apron-chat/apron-server-go/apron/internal/webpush"
	"github.com/apron-chat/apron-server-go/wire"
)

const (
	maxPushTextRunes = 1000
	maxPushesPerUser = push.MaxPerUser
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
	// (§4.9, push.Expiry). pushSweepInterval is how often expired
	// registrations are looked for.
	pushExpiry        = push.Expiry
	pushSweepInterval = time.Hour
	// maxPushesPerUserDay bounds the pushes delivered to one user in a UTC
	// day, counting only those the endpoint accepted (2xx).
	maxPushesPerUserDay = 1000
)

// pushRegistration is one push endpoint of a user (§4.9), and wakeScope a
// set of wake scopes (push).
type (
	pushRegistration = push.Registration
	wakeScope        = push.Wake
)

const (
	wakeMentions = push.Mentions
	wakeReplies  = push.Replies
	wakePrivate  = push.Private
	wakeJoined   = push.Joined
	wakeBadge    = push.Badge
	defaultWake  = push.DefaultWake
	urgentScopes = push.Urgent
)

// pushIDPattern is the shape of a client's push_id (§4.9).
var pushIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// removePushLocked forgets a registration, if it is still the one kept. A
// user left without registrations has no unread counts kept, nor badge
// push pending (push.Table.Remove).
func (s *Server) removePushLocked(p *pushRegistration) {
	if !s.pushes.Remove(p) || s.pushes.Count(p.User) > 0 {
		return
	}
	if u := s.users.Get(p.User); u != nil {
		u.unread = nil
	}
}

// registerPush records a push endpoint for the caller (§4.9): kind relay,
// with an optional bearer token, or webpush, with the subscription's keys;
// a relay with keys gets the payload encrypted as for webpush. Registering
// a url again replaces the caller's registration of it and renews it; other
// users' registrations of the same url are their own. A user holds at most
// maxPushesPerUser: another replaces the one least recently registered.
// It answers {} once the registration is kept.
func (s *Server) registerPush(c *client, r *wire.Request) *Error {
	if c.service != [32]byte{} {
		// A service token's client is a program, which push does not wake;
		// a registration would outlive the token's revocation.
		return &Error{Code: CodeDenied, Message: "A service token's connection does not register for push"}
	}
	d := r.Decoder()
	kind := d.String("kind", required)
	if d.Err() == nil && kind != "relay" && kind != "webpush" {
		d.Fail(invalidParams("Unknown push kind %q; this server supports relay and webpush", kind))
	}
	endpoint := d.String("url", required)
	token := d.String("token", optional)
	if err := paramError(d.Err()); err != nil {
		return err
	}
	endpoint, problem := webpush.CheckURL(endpoint, s.config.AllowInsecurePush)
	if problem != "" {
		return invalidParams("%s", problem)
	}
	if len(endpoint) > maxPushURLBytes || len(token) > maxPushTokenLen {
		return &Error{Code: CodeTooLarge, Message: fmt.Sprintf("url is at most %d bytes and token at most %d", maxPushURLBytes, maxPushTokenLen)}
	}
	pushID := d.String("push_id", optional)
	if err := paramError(d.Err()); err != nil {
		return err
	}
	if d.Has("push_id") && !pushIDPattern.MatchString(pushID) {
		return invalidParams("push_id is 1 to 64 letters, digits, _ or -")
	}
	keys, err := parseKeysParam(r)
	if err != nil {
		return err
	}
	if kind == "webpush" {
		if keys == nil {
			return invalidParams("A webpush registration needs keys with p256dh and auth")
		}
		// A webpush subscription has no bearer token.
		token = ""
	}
	wake := defaultWake
	if raw, has := r.Param("wake"); has {
		var names []string
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &names) != nil || names == nil {
			return invalidParams("wake must be an array of scope names")
		}
		// Past the bounds, names are ignored like unknown scopes.
		names = names[:min(len(names), maxWakeScopes)]
		names = slices.DeleteFunc(names, func(name string) bool { return len(name) > maxWakeScopeBytes })
		wake = push.ParseWake(names)
	}

	s.mu.Lock()
	defer s.unlock()
	if c.conn.Closing() {
		return connectionClosing()
	}
	u := c.user
	now := time.Now()
	s.expirePushesLocked(u, now)
	if s.pushes.Get(u.id, endpoint) == nil && s.pushes.Count(u.id) >= maxPushesPerUser {
		s.removePushLocked(s.pushes.Oldest(u.id))
	}
	s.pushes.Add(push.New(pushRegistration{
		User: u.id, Kind: kind, URL: endpoint, Token: token, PushID: pushID, Device: c.device, Session: c.sessionKeyLocked(),
		Keys: keys, Wake: wake, Renewed: now,
	}))
	return nil
}

// parseKeysParam reads a registration's optional keys (§4.9).
func parseKeysParam(r *wire.Request) (*webpush.Keys, *Error) {
	raw, present, objectErr := r.Object("keys")
	if !present {
		return nil, nil
	}
	if objectErr != nil {
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
// is already unregistered (§4.9). It answers {} once it is.
func (s *Server) unregisterPush(c *client, r *wire.Request) *Error {
	endpoint, err := parseString(r.Fields(), "url", required)
	if err != nil {
		return err
	}
	if normalized, problem := webpush.NormalizeURL(endpoint); problem == "" {
		endpoint = normalized
	}
	s.mu.Lock()
	defer s.unlock()
	if registration := s.pushes.Get(c.user.id, endpoint); registration != nil {
		s.removePushLocked(registration)
	}
	return nil
}

// expirePushesLocked forgets the user's registrations that were not
// registered again within pushExpiry.
func (s *Server) expirePushesLocked(u *userState, now time.Time) {
	for _, p := range s.pushes.Of(u.id) {
		if !p.Live(now) {
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
			for _, p := range s.pushes.Expired(time.Now()) {
				s.removePushLocked(p)
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
func (s *Server) wakeLocked(m *messageState, snapshot, previous *roomlog.Message) {
	// Transient notices, which have no message_id, are never pushed (§4.9).
	if s.config.DisablePush || s.pushes.Len() == 0 || snapshot.Deleted || snapshot.MessageID == "" {
		return
	}
	r := s.rooms.Get(m.RoomID())
	reasons := make(map[string]wakeScope)
	for _, id := range snapshot.Mentions() {
		reasons[id] |= wakeMentions
	}
	if previous == nil {
		if replyID := snapshot.ReplyID(); replyID != "" {
			if target := s.rooms.Message(replyID); target != nil {
				reasons[target.Owner()] |= wakeReplies
			}
		}
		for id := range r.Members() {
			if s.pushes.Count(id) > 0 {
				reasons[id] |= wakeJoined
			}
		}
		// private selects messages in the private rooms the user joined and
		// in their threads (§4.9): those who can see a private room's
		// thread joined every private room above it.
		if everyone, audience := r.Audience(); !everyone {
			for id := range audience {
				if s.pushes.Count(id) > 0 {
					reasons[id] |= wakePrivate
				}
			}
		}
	} else {
		for _, id := range previous.Mentions() {
			delete(reasons, id)
		}
	}
	delete(reasons, m.Owner())
	now := time.Now()
	for _, id := range slices.Sorted(maps.Keys(reasons)) {
		u := s.users.Get(id)
		if u == nil || s.pushes.Count(id) == 0 || !r.VisibleTo(u) || u.attended() || u.silenced(now) {
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
		for _, p := range s.pushes.Of(id) {
			scopes := why & p.Wake
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
			s.deliverPushLocked(p, unread, pushPayload(p.PushID, unread, snapshot), urgency, false)
		}
	}
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
	if u := s.users.Get(p.User); u == nil || u.pushDay.Count(time.Now()) >= maxPushesPerUserDay {
		return
	}
	seq := p.Sending(unread)
	done := func(status int) {
		accepted := status >= 200 && status < 300
		gone := status == http.StatusNotFound || status == http.StatusGone
		s.mu.Lock()
		defer s.unlock()
		p.Delivered(seq, unread, accepted)
		u := s.users.Get(p.User)
		switch {
		case u == nil:
		case accepted:
			u.pushDay.Accepted(time.Now())
		case gone:
			s.removePushLocked(p)
		}
	}
	latest := ""
	if badge {
		latest = push.Key(p.User, p.URL)
	}
	s.push.Deliver(p.Target(), payload, urgency, latest, done)
}

// pushPayload is the payload every kind delivers (§4.9): push_id, unread,
// and the message without log_id, format, embeds, or ext, its text
// truncated to maxPushTextRunes. When the JSON is longer than
// maxPushPayloadBytes the message is cut to fit: mentions go first, then
// text is shortened, and then body is dropped, from keeps only user_id, and
// reply_to goes, in turn.
func pushPayload(pushID string, unread int, snapshot *roomlog.Message) []byte {
	message := map[string]any{
		"message_id": snapshot.MessageID,
		"room_id":    snapshot.RoomID,
		"from":       snapshot.From,
	}
	if replyID := snapshot.ReplyID(); replyID != "" {
		message["reply_to"] = map[string]any{"message_id": replyID}
	}
	text, mentioned := "", snapshot.Mentions()
	if snapshot.Body != nil {
		text = snapshot.Body.Text
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
		func() { message["from"] = map[string]any{"user_id": snapshot.From.UserID} },
		func() { delete(message, "reply_to") },
	} {
		if cut(); fits() {
			break
		}
	}
	return payload
}
