package conformance

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/apron-chat/apron-server-go/wire"
)

// What the checks send.
const (
	// missing is a log_id no test server has reached (2^53 - 1).
	missing = "9007199254740991"
	// burstSize is how many messages a burst posts at once, without
	// waiting for each result.
	burstSize = 12
	// slowBurst messages of slowText bytes are posted to a reader that
	// has stopped reading: 8 MiB, past what the kernel buffers for a
	// socket, so that the server's own writes back up.
	slowBurst = 512
	slowText  = 16384
	// retentionPosts is how many messages history.retention.membership
	// posts, at most, waiting for the server to discard the start of the
	// room's log.
	retentionPosts = 64
)

// known are the capabilities and sign-in schemes PROTOCOL.md defines.
var known = map[string][]string{
	"capability": {"command", "history", "rooms", "edit", "status", "activity", "reactions",
		"embed:upload", "embed:append", "embed:terminal", "ext"},
	"auth scheme": {"guest", "token", "webauthn", "email", "external"},
}

// checks are the checks, in conform.py's order, with this package's own
// among them.
var checks = []check{
	// §3.6 core checklist, with the rules of §1 and §2 it points to.
	{id: "core.shape.server-frame", ref: "3.1", title: "sends a server frame on connect", setup: true, fn: serverFrame},
	{id: "auth.order.before-auth", ref: "3.2.2", title: "requests before auth get error/denied", fn: beforeAuth},
	{id: "core.shape.ping", ref: "1", title: "ping before auth gets pong", needs: advertised(func(s obj) bool { return has(s, "ping") }), fn: ping},
	{id: "auth.shape.you", ref: "3.2", title: "auth replies with you", fn: authYou},
	{id: "auth.order.barrier", ref: "3.2.2", title: "auth finishes before later frames", fn: authBarrier},
	{id: "auth.shape.unknown", ref: "3.2", title: "an unknown auth scheme is error/unsupported", fn: authUnknown},
	{id: "auth.shape.unsupported", ref: "3.2", title: "a defined scheme the server does not offer is error/unsupported", fn: authUnsupported},
	{id: "auth.shape.unsupported-first", ref: "3.2", title: "a scheme the server does not offer is error/unsupported whatever else the request holds", fn: authUnsupportedFirst},
	{id: "auth.order.no-dedup", ref: "1.2", title: "auth runs on each connection, also when it repeats an id", fn: authNoDedup},
	{id: "token.shape.denied", ref: "3.2.1", title: "a made-up token is denied", needs: advertised(offers("token")), fn: tokenDenied},
	{id: "core.message.default-room", ref: "3.6.3", title: "accepts message without room_id into its default room", setup: true, fn: defaultRoom},
	{id: "core.message.broadcast-first", ref: "3.6.4", title: "message creation replies with message_id and broadcasts the snapshot first", setup: true, fn: broadcastFirst},
	{id: "core.message.empty", ref: "3.5.1", title: "a message with no text and no embeds is error/invalid_params", fn: emptyMessage},
	{id: "core.message.empty-unknown-room", ref: "3.5.1", title: "an empty message to a room that does not exist is error/invalid_params", fn: emptyUnknownRoom},
	{id: "core.message.reply-to", ref: "3.5.4", title: "reply_to must refer to an existing message", fn: replyTo},
	{id: "core.shape.unknown-method", ref: "3.6.5", title: "unknown requests are error/unsupported", fn: unknownMethod},
	{id: "core.shape.save-without-edit", ref: "3.6.5", title: "message with message_id is error/unsupported without edit", needs: advertised(func(s obj) bool { return !hasString(s["capabilities"], "edit") }), fn: saveWithoutEdit},
	{id: "core.shape.unknown-notification", ref: "3.6.5", title: "unknown notifications are ignored", fn: unknownNotification},
	{id: "core.shape.unknown-keys", ref: "1", title: "unknown keys are ignored", fn: unknownKeys},
	{id: "core.shape.unknown-room", ref: "1.1", title: "a request for a room that does not exist is error/invalid_params", fn: unknownRoom},
	{id: "core.shape.large-frame", ref: "1", title: "frames up to 256 KiB are accepted", fn: largeFrame},
	{id: "core.retry.dedup", ref: "1.2", title: "retries are deduplicated by (user_id, id)", fn: retryDedup},
	{id: "core.identity.me", ref: "3.3.2", title: "me returns the user's complete object", fn: identityMe},

	// §4 capabilities.
	{id: "history.shape.page", ref: "4.3", title: "history pages the log", needs: capability("history"), fn: historyPage},
	{id: "history.shape.all-rooms", ref: "4.3", title: "history without room_id pages every room the connection receives, or is error/unsupported",
		needs: advertised(func(s obj) bool {
			return hasString(s["capabilities"], "history") && hasString(s["capabilities"], "rooms")
		}), fn: historyAllRooms},
	{id: "core.delivery.burst", ref: "3.5.3", title: "live delivery brings every record of a burst once, in log order", needs: capability("history"), fn: deliveryBurst},
	{id: "core.delivery.slow-reader", ref: "3.5.3", title: "a reader that falls behind gets every record, or loses its connection", needs: capability("history"), fn: slowReader},
	{id: "history.bounds.inclusive", ref: "4.3.2", title: "after and before are inclusive bounds", needs: capability("history"), fn: boundsInclusive},
	{id: "history.bounds.limit", ref: "4.3.2", title: "limit takes the oldest with after and the newest without", needs: capability("history"), fn: boundsLimit},
	{id: "history.bounds.empty", ref: "4.3.2", title: "an empty slice has more: false and no bounds", needs: capability("history"), fn: boundsEmpty},
	{id: "history.retention.membership", ref: "4.3.3", title: "a server that discards the start of a room's log keeps its members in it", needs: capability("history"), fn: retentionMembership},
	{id: "edit.shape.save", ref: "4.4", title: "a save is a new snapshot at a later log_id", needs: capability("edit"), fn: editSave},
	{id: "edit.shape.delete", ref: "4.4", title: "delete is a tombstone without body or ext", needs: capability("edit"), fn: editDelete},
	{id: "edit.shape.deleted-on-create", ref: "4.4", title: "deleted: true on creation is error/invalid_params", needs: capability("edit"), fn: deletedOnCreate},
	{id: "edit.shape.unknown-message", ref: "4.4", title: "saving an unknown message is error/invalid_params", needs: capability("edit"), fn: editUnknownMessage},
	{id: "rooms.list.filter", ref: "4.2.1", title: "room_list returns the arrays its filter asks for", needs: capability("rooms"), fn: roomListFilter},
	{id: "rooms.set.create", ref: "4.2.4", title: "room_set creates a room, joins the creator, and edits it", needs: capability("rooms"), setup: true, fn: roomSetCreate},
	{id: "rooms.membership.join-leave", ref: "4.2.2", title: "room_join and room_leave update the user and the room", needs: capability("rooms"), fn: joinLeave},
	{id: "rooms.list.sign-in-joined", ref: "4.2.1", title: "a room that a sign-in joins is in room_list joined", needs: capability("rooms"), fn: signInJoined},
	{id: "rooms.membership.unknown-room", ref: "4.2.2", title: "joining an unknown room is error/invalid_params", needs: capability("rooms"), fn: joinUnknownRoom},
	{id: "rooms.set.private", ref: "4.2.4", title: "a private room is kept private or refused as error/unsupported", needs: capability("rooms"), fn: privateRoom},
	{id: "reactions.shape.broadcast", ref: "4.7", title: "reactions are logged and broadcast before the result", needs: capability("reactions"), fn: reactionsBroadcast},
	{id: "activity.shape.notification", ref: "4.6", title: "activity is a notification and gets no reply", needs: capability("activity"), fn: activityNotification},
	{id: "status.shape.request", ref: "4.5.3", title: "status reports idle, and sends mutes to the user's connections", needs: capability("status"), fn: statusRequest},
	{id: "status.shape.me", ref: "4.5.1", title: "me sets a listed status",
		needs: advertised(func(s obj) bool { return hasString(s["capabilities"], "status") && truthy(s["status"]) }), fn: statusMe},
	{id: "status.seen.invisible", ref: "4.5.2", title: "a chosen invisible is shown offline, and its connections leave seen as it is",
		needs: advertised(func(s obj) bool { return hasString(s["capabilities"], "status") && hasString(s["status"], "invisible") }), fn: seenInvisible},
	{id: "command.shape.help", ref: "4.1", title: "/help replies with a ~private notice", needs: capability("command"), fn: commandHelp},
	{id: "command.shape.message-id", ref: "4.1", title: "a command with message_id is error/invalid_params", needs: capability("command"), fn: commandMessageID},
	{id: "ext.merge.me", ref: "4.13", title: "me merges ext one level deep", needs: capability("ext"), fn: extMergeMe},
	{id: "ext.merge.message", ref: "4.13", title: "messages keep ext, and saves merge it", needs: capability("ext"), fn: extMergeMessage},
	{id: "push.shape.unregister", ref: "4.9.1", title: "unregistering an unknown push url succeeds", needs: advertised(func(s obj) bool { return has(s, "push") }), fn: pushUnregister},

	// Rules checked on every frame along the way.
	{id: "core.shape.schema", ref: "1", title: "every frame matches schema/apron.schema.json", fn: func(c *checker) {
		c.skip("not ported: it needs a JSON Schema validator; conform.py checks it")
	}},
	{id: "core.shape.one-reply", ref: "1.1", title: "every request gets exactly one reply", fn: seen("replies")},
	{id: "core.shape.ids", ref: "2", title: "IDs are well formed", fn: seen("ids")},
	{id: "core.delivery.order", ref: "3.5.3", title: "each room's snapshots arrive in ascending log_id", fn: seen("order")},
	{id: "history.shape.room-records", ref: "4.3.3", title: "listed room records carry log_id, latest_log_id, and history_log_id", needs: capability("history"), fn: seen("listed-rooms")},
	{id: "status.seen.offline", ref: "4.5.2", title: "seen stays the same while the shown status stays offline, and arrives where it must", needs: capability("status"), fn: seen("shown-seen")},
}

// capability needs a capability in the server frame.
func capability(name string) func(obj) (bool, string) {
	return func(s obj) (bool, string) { return hasString(s["capabilities"], name), "no capability " + name }
}

// advertised needs what the server frame has.
func advertised(f func(obj) bool) func(obj) (bool, string) {
	return func(s obj) (bool, string) { return f(s), "not advertised" }
}

// offers reports whether a server frame offers every scheme, to sign in
// or to sign up.
func offers(schemes ...string) func(obj) bool {
	return func(s obj) bool {
		offered := append(stringsOf(s["auth"]), stringsOf(s["signup"])...)
		for _, scheme := range schemes {
			if !slices.Contains(offered, scheme) {
				return false
			}
		}
		return true
	}
}

// joined opens a signed-in connection that receives the deliveries of the
// default room.
func (c *checker) joined(name string, slow bool) *conn {
	c.t.Helper()
	if c.r.room == "" {
		c.skip("the default room is unknown")
	}
	conn := c.open(true, name, slow)
	if c.r.has("rooms") {
		c.call(conn, "room_join", obj{"room_id": c.r.room})
	} else {
		// A round trip, as room_join is, so that what the sign-in sent
		// after its result, such as a welcome notice, has arrived before a
		// check counts the frames that follow.
		c.request(conn, "me", obj{}, "")
	}
	return conn
}

// post posts a message to the default room, with params next to its body,
// and returns the index of its result and its message_id.
func (c *checker) post(conn *conn, text string, params obj) (int, string) {
	c.t.Helper()
	frame := obj{"room_id": c.r.room, "body": obj{"text": text}}
	maps.Copy(frame, params)
	i, reply := c.request(conn, "message", frame, "")
	messageID, ok := get(reply, "result", "message_id").(string)
	c.must(ok, "posting: %s", describe(reply))
	return i, messageID
}

func serverFrame(c *checker) {
	c.open(false, "", false)
	s := c.r.info()
	apron, _ := s["apron"].(float64)
	c.must(has(s, "apron") && apron == wire.Version, "apron is %v, not %d", s["apron"], wire.Version)
	c.must(len(list(s["auth"])) > 0, "auth is not a non-empty list")
	for _, kind := range []string{"capability", "auth scheme"} {
		names := stringsOf(s["capabilities"])
		if kind == "auth scheme" {
			names = append(stringsOf(s["auth"]), stringsOf(s["signup"])...)
		}
		for _, name := range names {
			c.must(slices.Contains(known[kind], name) || strings.HasPrefix(name, "ext:"), "unknown %s %q lacks ext: (§4)", kind, name)
		}
	}
	c.r.mu.Lock()
	found := slices.Sorted(maps.Keys(c.r.violations["server-first"]))
	c.r.mu.Unlock()
	c.must(len(found) == 0, "%s", strings.Join(found, "; "))
}

func beforeAuth(c *checker) {
	conn := c.open(false, "", false)
	c.expectError(c.replyTo(conn, "me", obj{}), "denied", "me before auth")
}

func ping(c *checker) {
	conn := c.open(false, "", false)
	c.notify(conn, "ping", nil)
	_, pong := conn.wait(func(f obj) bool { return len(f) == 1 && f["method"] == "pong" }, 0, waitFor)
	c.must(pong != nil, `no exact {"method":"pong"}`)
}

func authYou(c *checker) {
	conn := c.open(false, "", false)
	you, ok := c.call(conn, "auth", c.auth("Conformance"))["you"].(obj)
	_, isString := you["user_id"].(string)
	c.must(ok && isString, "result has no you.user_id")
}

func authBarrier(c *checker) {
	conn := c.open(false, "", false)
	c.send(conn, obj{"method": "auth", "id": "barrier-auth", "params": c.auth("Conformance")})
	c.send(conn, obj{"method": "me", "id": "barrier-me", "params": obj{}})
	_, auth := conn.reply("barrier-auth", 0, waitFor)
	_, me := conn.reply("barrier-me", 0, waitFor)
	c.must(auth != nil && has(auth, "result"), "auth: %s", describeOr(auth))
	c.must(me != nil && has(me, "result"), "me sent right after auth: %s", describeOr(me))
	c.must(get(me, "result", "you", "user_id") == get(auth, "result", "you", "user_id"), "me ran as a different user than auth")
}

// describeOr describes a reply, or its absence.
func describeOr(reply obj) string {
	if reply == nil {
		return "no reply"
	}
	return describe(reply)
}

func authUnknown(c *checker) {
	conn := c.open(false, "", false)
	c.expectError(c.replyTo(conn, "auth", obj{"scheme": "ext:conformance-unknown"}), "unsupported", "auth with an unknown scheme")
}

// schemeParams are well-formed params of each scheme PROTOCOL.md defines.
var schemeParams = map[string]obj{
	"guest": {}, "token": {"token": "conformance"}, "webauthn": {"action": "login", "step": "begin"},
	"email": {"email": "conformance@example.invalid"}, "external": {},
}

// notOffered returns the schemes PROTOCOL.md defines that the server does
// not offer, in order.
func (c *checker) notOffered() []string {
	var missing []string
	for _, scheme := range slices.Sorted(maps.Keys(schemeParams)) {
		if !offers(scheme)(c.r.info()) {
			missing = append(missing, scheme)
		}
	}
	return missing
}

func authUnsupported(c *checker) {
	missing := c.notOffered()
	if len(missing) == 0 {
		c.skip("the server offers every scheme that PROTOCOL.md defines")
	}
	for _, scheme := range missing {
		conn := c.open(false, "", false)
		params := obj{"scheme": scheme}
		maps.Copy(params, schemeParams[scheme])
		c.expectError(c.replyTo(conn, "auth", params), "unsupported", "auth "+scheme)
	}
}

// authUnsupportedFirst is this package's own: wire leaves a requested
// user_id to the backend, which checks the scheme first (§3.2; see the
// Validation section of package wire).
func authUnsupportedFirst(c *checker) {
	missing := c.notOffered()
	if len(missing) == 0 {
		c.skip("the server offers every scheme that PROTOCOL.md defines")
	}
	for _, scheme := range missing {
		conn := c.open(false, "", false)
		params := obj{"scheme": scheme, "user_id": "not valid"}
		maps.Copy(params, schemeParams[scheme])
		c.expectError(c.replyTo(conn, "auth", params), "unsupported", "auth "+scheme+" with a user_id that is not valid")
	}
}

func authNoDedup(c *checker) {
	id := fmt.Sprintf("conformance-auth-%d", time.Now().UnixNano())
	first := c.open(false, "", false)
	_, reply := c.request(first, "auth", c.auth("Conformance"), id)
	c.policy(reply)
	c.must(signedIn(reply), "auth: %s", describe(reply))
	// The same request on a new connection signs that connection in, and
	// not from a cache.
	second := c.open(false, "", false)
	_, again := c.request(second, "auth", c.auth("Conformance"), id)
	c.policy(again)
	c.must(signedIn(again), "the same auth, with the same id, on a new connection: %s", describe(again))
	me := c.replyTo(second, "me", obj{})
	c.must(has(me, "result"), "me after it got %s: the auth was answered without signing the connection in", describe(me))
	c.must(get(me, "result", "you", "user_id") == get(again, "result", "you", "user_id"), "me runs as another user than the auth's you")
	if !offers("token")(c.r.info()) {
		c.note("the server does not offer token, so a repeat without credentials was not tried")
		return
	}
	// A repeat with a made-up token is answered for that token, as a
	// request with a new id is.
	madeUp := obj{"scheme": "token", "token": fmt.Sprintf("conformance-%d", time.Now().UnixNano())}
	third := c.open(false, "", false)
	_, repeat := c.request(third, "auth", madeUp, id)
	c.policy(repeat)
	me = c.replyTo(third, "me", obj{})
	if signedIn(repeat) {
		c.must(has(me, "result"), "a repeat with a made-up token returned you, then me got %s: a cached result", describe(me))
		c.note("the server's policy accepts a made-up token")
		return
	}
	c.must(code(me) == wire.CodeDenied, "a repeat with a made-up token was refused, then me got %s", describe(me))
	if code(repeat) != wire.CodeDenied {
		fresh := c.open(false, "", false)
		control := c.replyTo(fresh, "auth", madeUp)
		c.must(code(control) == code(repeat), "a repeat with a made-up token got %s, but the same request with a new id got %s: the id was deduplicated",
			describe(repeat), describe(control))
	}
}

func tokenDenied(c *checker) {
	conn := c.open(false, "", false)
	reply := c.replyTo(conn, "auth", obj{"scheme": "token", "token": fmt.Sprintf("conformance-%d", time.Now().UnixNano())})
	c.policy(reply)
	if signedIn(reply) {
		c.skip("the server's policy accepts any token (§3.2): %s", describe(reply))
	}
	c.expectError(reply, "denied", "a made-up token")
}

func defaultRoom(c *checker) {
	conn := c.open(true, "Conformance", false)
	reply := c.replyTo(conn, "message", obj{"body": obj{"text": "conformance: default room"}})
	c.unlessPolicy(reply, "a post", nil)
	messageID, ok := get(reply, "result", "message_id").(string)
	c.must(ok, "got %s", describe(reply))
	if _, snapshot := conn.wait(snapshotOf(messageID, ""), 0, waitFor); snapshot != nil {
		room, ok := get(snapshot, "params", "room_id").(string)
		c.must(ok, "the snapshot has no room_id")
		c.r.room = room
	}
	c.must(c.r.room != "" || c.r.has("rooms"), "no broadcast to the poster")
	if c.r.room == "" {
		c.warn("the poster got no broadcast and the room is unknown; later checks skip")
	}
}

func broadcastFirst(c *checker) {
	a, b := c.joined("Conformance A", false), c.joined("Conformance B", false)
	start := a.count()
	const text = "conformance: hello *world*"
	i, messageID := c.post(a, text, obj{"mentions": []any{}})
	j, mine := a.wait(snapshotOf(messageID, ""), start, waitFor)
	_, theirs := b.wait(snapshotOf(messageID, ""), 0, waitFor)
	c.must(theirs != nil, "no broadcast to another member")
	c.must(mine != nil, "no broadcast to the sender")
	c.must(j < i, "the sender got its result before the broadcast (§1)")
	p, _ := theirs["params"].(obj)
	c.must(p["log_id"] == messageID, "log_id %v is not message_id %s (§2.2)", p["log_id"], messageID)
	c.must(p["room_id"] == c.r.room, "room_id is %v", p["room_id"])
	c.must(get(p, "from", "user_id") == a.you["user_id"], "from is not the sender")
	c.must(get(p, "body", "text") == text, "body.text changed")
	n := c.id(messageID)
	c.must(abs(n-time.Now().UnixMilli()) < 3600_000, "log_id is not the commit time (§2.1)")
	time.Sleep(quiet)
	c.must(len(b.since(0, snapshotOf(messageID, ""))) == 1, "the snapshot arrived more than once (§3.5.3)")
	c.r.message = p
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

func emptyMessage(c *checker) {
	a, b := c.joined("Conformance", false), c.joined("Conformance", false)
	start := b.count()
	reply := c.replyTo(a, "message", obj{"room_id": c.r.room, "body": obj{"text": ""}})
	time.Sleep(quiet)
	c.expectError(reply, "invalid_params", "an empty message")
	c.must(len(b.since(start, method("message"))) == 0, "it was broadcast")
}

// emptyUnknownRoom is this package's own: a backend that refuses an empty
// message checks its room too, and never drops it (§1.1, §3.5.1).
func emptyUnknownRoom(c *checker) {
	conn := c.open(true, "Conformance", false)
	reply := c.replyTo(conn, "message", obj{"room_id": "conformance-no-such-room", "body": obj{"text": ""}})
	var control func() obj
	if c.r.room != "" {
		control = func() obj {
			return c.replyTo(conn, "message", obj{"room_id": c.r.room, "body": obj{"text": ""}})
		}
	}
	c.unlessPolicy(reply, "an empty post", control)
	c.expectError(reply, "invalid_params", "an empty message to an unknown room")
}

func replyTo(c *checker) {
	if c.r.message == nil {
		c.skip("no message to reply to")
	}
	a := c.joined("Conformance", false)
	target := c.r.message["message_id"]
	_, messageID := c.post(a, "conformance: reply", obj{"reply_to": obj{"message_id": target}})
	if _, snapshot := a.wait(snapshotOf(messageID, ""), 0, waitFor); snapshot != nil {
		c.must(get(snapshot, "params", "reply_to", "message_id") == target, "the snapshot lost reply_to")
	}
	reply := c.replyTo(a, "message", obj{"room_id": c.r.room, "body": obj{"text": "x"}, "reply_to": obj{"message_id": missing}})
	c.expectError(reply, "invalid_params", "reply_to an unknown message")
}

func unknownMethod(c *checker) {
	conn := c.open(true, "Conformance", false)
	c.expectError(c.replyTo(conn, "ext:conformance-unknown", obj{}), "unsupported", "unknown method")
}

func saveWithoutEdit(c *checker) {
	if c.r.message == nil {
		c.skip("no message to save")
	}
	conn := c.joined("Conformance", false)
	reply := c.replyTo(conn, "message", obj{"message_id": c.r.message["message_id"], "room_id": c.r.room,
		"body": obj{"text": "conformance: edited"}})
	c.expectError(reply, "unsupported", "a save")
}

func unknownNotification(c *checker) {
	conn := c.open(true, "Conformance", false)
	start := conn.count()
	c.notify(conn, "ext:conformance-unknown", obj{"x": 1})
	if !c.r.has("activity") { // defined, but unknown to this server
		room := c.r.room
		if room == "" {
			room = "general"
		}
		c.notify(conn, "activity", obj{"room_id": room, "typing": 5})
	}
	c.sync(conn)
	errors := conn.since(start, isError)
	c.must(len(errors) == 0, "got %s", describeFirst(errors))
}

// describeFirst describes the first of some replies.
func describeFirst(replies []obj) string {
	if len(replies) == 0 {
		return ""
	}
	return describe(replies[0])
}

func unknownKeys(c *checker) {
	conn := c.joined("Conformance", false)
	reply := c.replyTo(conn, "message", obj{"room_id": c.r.room, "conformance_unknown": true,
		"body": obj{"text": "conformance: unknown keys", "conformance_unknown": 1}})
	c.must(has(reply, "result"), "%s", describe(reply))
}

func unknownRoom(c *checker) {
	conn := c.open(true, "Conformance", false)
	reply := c.replyTo(conn, "message", obj{"room_id": "conformance-no-such-room", "body": obj{"text": "x"}})
	var control func() obj
	if c.r.room != "" {
		control = func() obj {
			return c.replyTo(conn, "message", obj{"room_id": c.r.room, "body": obj{"text": "conformance: a room that exists"}})
		}
	}
	c.unlessPolicy(reply, "a post", control)
	c.expectError(reply, "invalid_params", "posting to an unknown room")
}

func largeFrame(c *checker) {
	conn := c.joined("Conformance", false)
	_, reply, err := c.tryRequest(conn, "message", obj{"room_id": c.r.room, "body": obj{"text": strings.Repeat("x", 250_000)}}, "")
	if err != "" {
		c.warn("%s", err)
	}
	c.should(has(reply, "result"), "a 250 kB message: %s", describe(reply))
}

func retryDedup(c *checker) {
	a, b := c.joined("Conformance", false), c.joined("Conformance", false)
	start := b.count()
	params := obj{"room_id": c.r.room, "body": obj{"text": "conformance: retry"}}
	_, first := c.request(a, "message", params, "conformance-retry")
	_, second := c.request(a, "message", params, "conformance-retry")
	c.must(has(first, "result"), "%s", describe(first))
	time.Sleep(quiet)
	count := len(b.since(start, func(f obj) bool {
		return f["method"] == "message" && get(f, "params", "body", "text") == "conformance: retry"
	}))
	c.should(has(second, "result") && reflect.DeepEqual(second["result"], first["result"]), "the retry got %s", describe(second))
	c.should(count == 1, "the retry was broadcast %d times", count)
	other := obj{"room_id": c.r.room, "body": obj{"text": "conformance: other"}}
	_, reused := c.request(a, "message", other, "conformance-retry")
	c.should(code(reused) == wire.CodeInvalidParams, "an id reused with other params got %s", describe(reused))
}

func identityMe(c *checker) {
	conn := c.open(true, "Conformance", false)
	you := get(c.call(conn, "me", obj{}), "you")
	c.must(get(you, "user_id") == conn.you["user_id"], "me's you.user_id is not the auth's")
	reply := c.replyTo(conn, "me", obj{"name": "Conformance Renamed"})
	if code(reply) == wire.CodeRetryAfter || code(reply) == wire.CodeDenied {
		c.note("the server's policy refused a new name: %s", describe(reply))
		return
	}
	c.must(has(reply, "result"), "me with a new name: %s", describe(reply))
	c.must(get(reply, "result", "you", "user_id") == conn.you["user_id"], "a new name changed you.user_id")
}

// roomParams returns params with the default room's room_id, when it is
// known.
func (c *checker) roomParams(params obj) obj {
	if c.r.room != "" {
		params["room_id"] = c.r.room
	}
	return params
}

func historyPage(c *checker) {
	conn := c.open(true, "Conformance", false)
	page := c.call(conn, "history", c.roomParams(obj{}))
	for _, key := range []string{"latest_log_id", "history_log_id"} {
		c.must(has(page, key), "the result has no %s (§4.3.3)", key)
	}
	c.must(has(page, "first_log_id") == has(page, "last_log_id"), "the result has one of first_log_id and last_log_id")
	_, isBool := page["more"].(bool)
	c.must(isBool, "more is not a boolean")
	for _, key := range []string{"rooms", "messages", "reactions", "memberships"} {
		var logIDs []int64
		for _, r := range list(page[key]) {
			logIDs = append(logIDs, c.id(get(r, "log_id")))
		}
		c.must(slices.IsSorted(logIDs), "%s are not in ascending log_id", key)
	}
	if c.r.message != nil {
		found := slices.ContainsFunc(list(page["messages"]), func(m any) bool { return get(m, "message_id") == c.r.message["message_id"] })
		c.must(found, "the newest page lacks a recent message")
	}
}

func historyAllRooms(c *checker) {
	conn := c.joined("Conformance", false)
	reply := c.replyTo(conn, "room_set", obj{"title": "Conformance history"})
	other, ok := get(reply, "result", "room_id").(string)
	if !ok {
		c.skip("no second room: room_set got %s", describe(reply))
	}
	_, first := c.post(conn, "conformance: history, default room", nil)
	reply = c.replyTo(conn, "message", obj{"room_id": other, "body": obj{"text": "conformance: history, other room"}})
	second, ok := get(reply, "result", "message_id").(string)
	c.must(ok, "posting: %s", describe(reply))
	reply = c.replyTo(conn, "history", obj{"after": first, "before": second})
	if code(reply) == wire.CodeUnsupported {
		c.warn("history without room_id is error/unsupported")
	}
	c.must(has(reply, "result"), "history without room_id: %s", describe(reply))
	rooms := map[string]bool{}
	for _, m := range list(get(reply, "result", "messages")) {
		if id := get(m, "message_id"); id == first || id == second {
			rooms[fmt.Sprint(get(m, "room_id"))] = true
		}
	}
	names := slices.Sorted(maps.Keys(rooms))
	if len(names) == 0 {
		names = []string{"neither room"}
	}
	c.must(len(rooms) == 2 && rooms[c.r.room] && rooms[other], "history without room_id returned only %s", strings.Join(names, ", "))
}

// burst posts the texts at once, each request sent before the previous
// result, and returns their message_ids in order. A refusal by the
// server's policy (retry_after or denied, such as a posting limit) skips
// the check.
func (c *checker) burst(conn *conn, texts []string) []string {
	c.t.Helper()
	var sent []string
	for _, text := range texts {
		id := c.r.id()
		sent = append(sent, id)
		c.send(conn, obj{"method": "message", "id": id, "params": obj{"room_id": c.r.room, "body": obj{"text": text}}})
	}
	var messageIDs []string
	for _, id := range sent {
		_, reply := conn.reply(id, 0, 4*waitFor)
		if reply == nil {
			why := "no reply to a message of the burst"
			if conn.isClosed() {
				why += " (connection closed)"
			}
			c.fail("%s", why)
		}
		if code(reply) == wire.CodeRetryAfter || code(reply) == wire.CodeDenied {
			c.skip("the server's policy refused the burst: %s", describe(reply))
		}
		messageID, ok := get(reply, "result", "message_id").(string)
		c.must(ok, "posting: %s", describe(reply))
		messageIDs = append(messageIDs, messageID)
	}
	return messageIDs
}

// logged returns the room's message snapshots and reaction sets from after
// on, by history, as kinds by log_id.
func (c *checker) logged(conn *conn, room string, after string) map[int64]string {
	c.t.Helper()
	found := map[int64]string{}
	for {
		page := c.call(conn, "history", obj{"room_id": room, "after": after, "limit": 200})
		for _, kind := range [][2]string{{"messages", "message"}, {"reactions", "reactions"}} {
			for _, record := range list(page[kind[0]]) {
				if logID, ok := parseID(get(record, "log_id")); ok {
					found[logID] = kind[1]
				}
			}
		}
		last, ok := parseID(page["last_log_id"])
		if more, _ := page["more"].(bool); !more || !ok {
			return found
		}
		after = strconv.FormatInt(last+1, 10)
	}
}

// liveRecord is a record that arrived live: its log_id and kind.
type liveRecord struct {
	logID int64
	kind  string
}

// delivered returns the room's message snapshots and reaction sets that
// conn received live from start on.
func delivered(conn *conn, start int, room string) []liveRecord {
	var live []liveRecord
	for _, f := range conn.since(start, func(f obj) bool { return f["method"] == "message" || f["method"] == "reactions" }) {
		if get(f, "params", "room_id") != room {
			continue
		}
		if logID, ok := parseID(get(f, "params", "log_id")); ok {
			live = append(live, liveRecord{logID, f["method"].(string)})
		}
	}
	return live
}

func deliveryBurst(c *checker) {
	a, b := c.joined("Conformance burst A", false), c.joined("Conformance burst B", false)
	start := b.count()
	texts := make([]string, burstSize)
	for n := range texts {
		texts[n] = fmt.Sprintf("conformance: burst %d", n)
	}
	messageIDs := c.burst(a, texts)
	// The other kinds of record a room logs, where the server has them.
	if c.r.has("edit") {
		c.call(a, "message", obj{"message_id": messageIDs[0], "room_id": c.r.room, "body": obj{"text": "conformance: burst, edited"}})
		c.call(a, "message", obj{"message_id": messageIDs[1], "room_id": c.r.room, "deleted": true})
	}
	if c.r.has("reactions") {
		c.call(a, "reactions", obj{"message_id": messageIDs[2], "emojis": []any{"👍"}})
	}
	// Every result has reached the poster, so every broadcast went out
	// before the server reads b's next request, and its reply (an error
	// is as good) comes after them (§1).
	c.request(b, "me", obj{}, "")
	loggedIDs := c.logged(b, c.r.room, messageIDs[0])
	live := delivered(b, start, c.r.room)
	liveIDs := map[int64]bool{}
	for _, r := range live {
		liveIDs[r.logID] = true
	}
	missingOf := func(kind string) []int64 {
		var missing []int64
		for logID, k := range loggedIDs {
			if k == kind && !liveIDs[logID] {
				missing = append(missing, logID)
			}
		}
		slices.Sort(missing)
		return missing
	}
	missing := missingOf("message")
	c.must(len(missing) == 0, "%d message snapshots in the log never arrived live, such as %v (§3.5.3)", len(missing), first(missing))
	var messages []int64
	for _, r := range live {
		if r.kind == "message" {
			messages = append(messages, r.logID)
		}
	}
	c.must(slices.Equal(messages, sortedSet(messages)), "the burst's message snapshots arrived out of log order or twice (§3.5.3)")
	missing = missingOf("reactions")
	c.should(len(missing) == 0, "a reaction set in the log never arrived live: %v (§3.4)", first(missing))
	var every []int64
	for _, r := range live {
		every = append(every, r.logID)
	}
	c.should(slices.IsSorted(every), "the room's reaction sets and message snapshots arrived out of log order between them")
}

// first returns the first of some log_ids, or "".
func first(ids []int64) any {
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

// sortedSet returns the distinct log_ids, in order.
func sortedSet(ids []int64) []int64 {
	set := slices.Clone(ids)
	slices.Sort(set)
	return slices.Compact(set)
}

func slowReader(c *checker) {
	a, b := c.joined("Conformance flood A", false), c.joined("Conformance flood B", true)
	start := b.count()
	b.pause()
	filler := strings.Repeat("x", slowText)
	texts := make([]string, slowBurst)
	for n := range texts {
		texts[n] = fmt.Sprintf("conformance: flood %d %s", n, filler)
	}
	messageIDs := c.burst(a, texts)
	b.resume()
	// Everything sent has arrived, or the server closed the connection:
	// wait for the last record, or the close.
	b.wait(snapshotOf(messageIDs[len(messageIDs)-1], ""), start, 4*waitFor)
	var loggedIDs []int64
	for logID, kind := range c.logged(a, c.r.room, messageIDs[0]) {
		if kind == "message" {
			loggedIDs = append(loggedIDs, logID)
		}
	}
	slices.Sort(loggedIDs)
	firstID := c.id(messageIDs[0])
	var live []int64
	for _, r := range delivered(b, start, c.r.room) {
		if r.kind == "message" && r.logID >= firstID {
			live = append(live, r.logID)
		}
	}
	if b.isClosed() {
		// A connection the server gave up on: what did arrive is
		// everything up to its last record, with no gap.
		c.must(len(live) <= len(loggedIDs) && slices.Equal(live, loggedIDs[:len(live)]),
			"a reader that fell behind lost records before later ones, then its connection")
		return
	}
	liveSet := map[int64]bool{}
	for _, id := range live {
		liveSet[id] = true
	}
	var missing []int64
	for _, id := range loggedIDs {
		if !liveSet[id] {
			missing = append(missing, id)
		}
	}
	c.must(len(missing) == 0, "a reader that fell behind kept its connection and lost %d records, such as %v (§3.5.3)", len(missing), first(missing))
	c.must(slices.Equal(live, loggedIDs), "a reader that fell behind got records out of log order or twice (§3.5.3)")
}

func boundsInclusive(c *checker) {
	if c.r.message == nil {
		c.skip("no message")
	}
	conn := c.open(true, "Conformance", false)
	logID := c.r.message["log_id"]
	page := c.call(conn, "history", obj{"room_id": c.r.room, "after": logID, "before": logID})
	var logIDs []any
	for _, m := range list(page["messages"]) {
		logIDs = append(logIDs, get(m, "log_id"))
	}
	c.must(len(logIDs) == 1 && logIDs[0] == logID, "the slice is not the one record")
	c.must(page["first_log_id"] == logID && logID == page["last_log_id"], "first_log_id and last_log_id are not the record")
}

func boundsLimit(c *checker) {
	conn := c.joined("Conformance", false)
	var posted []string
	for n := range 3 {
		_, id := c.post(conn, fmt.Sprintf("conformance: page %d", n), nil)
		posted = append(posted, id)
	}
	forward := c.call(conn, "history", obj{"room_id": c.r.room, "after": posted[0], "limit": 2})
	c.must(forward["first_log_id"] == posted[0], "first_log_id is %v, not %s", forward["first_log_id"], posted[0])
	c.must(forward["more"] == true, "more is not true")
	backward := c.call(conn, "history", obj{"room_id": c.r.room, "before": posted[2], "limit": 2})
	c.must(backward["last_log_id"] == posted[2], "last_log_id is %v, not %s", backward["last_log_id"], posted[2])
	c.must(backward["more"] == true, "more is not true")
	last := c.id(forward["last_log_id"])
	rest := c.call(conn, "history", obj{"room_id": c.r.room, "after": strconv.FormatInt(last+1, 10), "limit": 200})
	found := slices.ContainsFunc(list(rest["messages"]), func(m any) bool { return get(m, "message_id") == posted[2] })
	c.must(found, "continuing forward skipped a message")
}

func boundsEmpty(c *checker) {
	conn := c.open(true, "Conformance", false)
	page := c.call(conn, "history", c.roomParams(obj{"after": missing}))
	c.must(page["more"] == false, "more is not false")
	c.must(!has(page, "first_log_id") && !has(page, "last_log_id"), "the result has bounds")
}

// retentionMembership is this package's own (§4.3.3): it posts until the
// server discards the start of the room's log, past the membership of a
// connection's sign-in, and then the room's log must still say that the
// connection is a member, in a membership record appended before the
// discard. A server that keeps the log skips it.
func retentionMembership(c *checker) {
	a := c.joined("Conformance retention A", false)
	b := c.joined("Conformance retention B", false)
	room := c.r.room
	// The log_id of the latest membership record that arrived live and
	// lists a or b as joined.
	var joinedAt int64
	members := []string{str(a.you, "user_id"), str(b.you, "user_id")}
	for _, conn := range []*conn{a, b} {
		for _, f := range conn.since(0, roomUpdate("memberships", room)) {
			for _, m := range records(f, "memberships", room) {
				for _, entry := range list(m["members"]) {
					if slices.Contains(members, str(entry, "user", "user_id")) && get(entry, "joined") == true {
						if logID, ok := parseID(m["log_id"]); ok {
							joinedAt = max(joinedAt, logID)
						}
					}
				}
			}
		}
	}
	if joinedAt == 0 {
		c.skip("the sign-ins logged no membership in the room, or kept them outside the log")
	}
	bound := c.lowerBound(c.call(a, "history", obj{"room_id": room, "limit": 1}))
	for n := 0; bound <= joinedAt; n++ {
		if n == retentionPosts {
			c.skip("the server kept the room's log back to the sign-ins' memberships, past %d messages", n)
		}
		reply := c.replyTo(a, "message", obj{"room_id": room, "body": obj{"text": fmt.Sprintf("conformance: retention %d", n)}})
		if code(reply) == wire.CodeRetryAfter || code(reply) == wire.CodeDenied {
			c.skip("the server's policy refused a post: %s", describe(reply))
		}
		c.must(has(reply, "result"), "posting: %s", describe(reply))
		next := c.lowerBound(c.call(a, "history", obj{"room_id": room, "limit": 1}))
		c.must(next >= bound, "the effective lower bound decreased from %d to %d (§4.3.3)", bound, next)
		bound = next
	}
	// The memberships of the log that is left, replayed (§2.3).
	joined := map[string]bool{}
	after := bound
	for {
		page := c.call(a, "history", obj{"room_id": room, "after": strconv.FormatInt(after, 10), "limit": 200})
		for _, m := range list(page["memberships"]) {
			for _, entry := range list(get(m, "members")) {
				joined[str(entry, "user", "user_id")] = get(entry, "joined") == true
			}
		}
		last, ok := parseID(page["last_log_id"])
		if more, _ := page["more"].(bool); !more || !ok {
			break
		}
		after = last + 1
	}
	for _, userID := range members {
		c.must(joined[userID], "the server discarded the start of the room's log, and with it the membership of %s, a current member, "+
			"without a membership record that lists every current member (§4.3.3)", userID)
	}
}

// lowerBound returns the effective lower bound of a history result: its
// history_log_id, or latest_log_id + 1 when that is null (§4.3.3).
func (c *checker) lowerBound(page obj) int64 {
	c.t.Helper()
	c.must(has(page, "history_log_id") && has(page, "latest_log_id"), "the result has no history_log_id or latest_log_id (§4.3.3)")
	if page["history_log_id"] == nil {
		return c.id(page["latest_log_id"]) + 1
	}
	return c.id(page["history_log_id"])
}

func editSave(c *checker) {
	a, b := c.joined("Conformance", false), c.joined("Conformance", false)
	_, messageID := c.post(a, "conformance: before", nil)
	i, _ := b.wait(snapshotOf(messageID, ""), 0, waitFor)
	c.must(i >= 0, "no broadcast to another member")
	start := i + 1
	reply := c.replyTo(a, "message", obj{"message_id": messageID, "room_id": c.r.room, "body": obj{"text": "conformance: after"}})
	c.must(get(reply, "result", "message_id") == messageID, "the save got %s", describe(reply))
	_, saved := b.wait(snapshotOf(messageID, ""), start, waitFor)
	c.must(saved != nil, "no snapshot of the save")
	p := saved["params"]
	c.must(c.id(get(p, "log_id")) > c.id(messageID), "the save's log_id is not greater")
	c.must(get(p, "body", "text") == "conformance: after", "body was not replaced")
	c.must(get(p, "from", "user_id") == a.you["user_id"], "from changed")
}

func editDelete(c *checker) {
	a, b := c.joined("Conformance", false), c.joined("Conformance", false)
	_, messageID := c.post(a, "conformance: delete me", nil)
	i, _ := b.wait(snapshotOf(messageID, ""), 0, waitFor)
	c.must(i >= 0, "no broadcast to another member")
	c.call(a, "message", obj{"message_id": messageID, "room_id": c.r.room, "deleted": true})
	_, tomb := b.wait(snapshotOf(messageID, ""), i+1, waitFor)
	c.must(tomb != nil, "no tombstone")
	c.must(get(tomb, "params", "deleted") == true, "deleted is not true")
	c.must(!has(tomb["params"], "body") && !has(tomb["params"], "ext"), "the tombstone has body or ext")
}

func deletedOnCreate(c *checker) {
	conn := c.joined("Conformance", false)
	reply := c.replyTo(conn, "message", obj{"room_id": c.r.room, "body": obj{"text": "x"}, "deleted": true})
	c.expectError(reply, "invalid_params", "creating a deleted message")
}

func editUnknownMessage(c *checker) {
	conn := c.joined("Conformance", false)
	reply := c.replyTo(conn, "message", obj{"message_id": missing, "room_id": c.r.room, "body": obj{"text": "x"}})
	c.expectError(reply, "invalid_params", "saving an unknown message")
}

func roomListFilter(c *checker) {
	conn := c.open(true, "Conformance", false)
	joined := c.call(conn, "room_list", obj{"filter": "joined", "members": true})
	_, isList := joined["joined"].([]any)
	c.must(isList && !has(joined, "not_joined"), "filter joined")
	for _, room := range list(joined["joined"]) {
		_, isList := get(room, "members").([]any)
		c.must(isList, "room %v has no members", get(room, "room_id"))
	}
	notJoined := c.call(conn, "room_list", obj{"filter": "not_joined"})
	_, isList = notJoined["not_joined"].([]any)
	c.must(isList && !has(notJoined, "joined"), "filter not_joined")
	for _, room := range list(notJoined["not_joined"]) {
		c.must(!has(room, "members"), "members without members: true")
	}
	if len(list(joined["joined"])) > 0 {
		_, isList := joined["users"].([]any)
		c.should(isList, "members: true has no users")
	}
}

func roomSetCreate(c *checker) {
	conn := c.open(true, "Conformance", false)
	start := conn.count()
	i, reply := c.request(conn, "room_set", obj{"title": "Conformance"}, "")
	c.unlessPolicy(reply, "a new room", nil)
	room, ok := get(reply, "result", "room_id").(string)
	c.must(ok, "room_set got %s", describe(reply))
	j, update := conn.wait(roomUpdate("joined", room), start, waitFor)
	c.must(update != nil && j < i, "no room_update joined before the result")
	record := records(update, "joined", room)[0]
	_, isList := record["members"].([]any)
	c.must(isList, "joined has no members")
	c.must(record["title"] == "Conformance", "the title was not kept")
	start = conn.count()
	i, reply = c.request(conn, "room_set", obj{"room_id": room, "title": "Conformance 2"}, "")
	c.must(get(reply, "result", "room_id") == room, "editing got %s", describe(reply))
	j, update = conn.wait(roomUpdate("updated", room), start, waitFor)
	c.must(update != nil && j < i, "no room_update updated before the result")
	c.must(records(update, "updated", room)[0]["title"] == "Conformance 2", "the new title is missing")
	// An edit merges: a field it leaves out keeps its value, and "" clears
	// it (§4.2.4).
	for _, description := range []string{"About", ""} {
		start = conn.count()
		c.call(conn, "room_set", obj{"room_id": room, "description": description})
		_, update = conn.wait(roomUpdate("updated", room), start, waitFor)
		record := obj{}
		if update != nil {
			record = records(update, "updated", room)[0]
		}
		c.must(record["title"] == "Conformance 2", "an edit without title did not keep the title")
		got, _ := record["description"].(string)
		c.must(got == description, "description is %v, not %q", record["description"], description)
	}
	c.r.newRoom = room
}

func joinLeave(c *checker) {
	room := c.r.newRoom
	if room == "" {
		c.skip("no room was created")
	}
	a := c.open(true, "Conformance", false)
	b := c.open(true, "Conformance", false)
	// When every sign-in is the same user, there is no other member to
	// tell.
	twoUsers := a.you["user_id"] != b.you["user_id"]
	if twoUsers {
		c.call(a, "room_join", obj{"room_id": room})
	}
	aStart, bStart := a.count(), b.count()
	i, reply := c.request(b, "room_join", obj{"room_id": room}, "")
	c.must(isEmptyObject(reply["result"]), "room_join got %s", describe(reply))
	j, update := b.wait(roomUpdate("joined", room), bStart, waitFor)
	c.must(update != nil && j < i, "no room_update joined before the result")
	if twoUsers {
		_, membership := a.wait(roomUpdate("memberships", room), aStart, waitFor)
		c.should(membership != nil, "another member got no membership record")
	}
	bStart = b.count()
	i, reply = c.request(b, "room_leave", obj{"room_id": room}, "")
	c.must(isEmptyObject(reply["result"]), "room_leave got %s", describe(reply))
	j, update = b.wait(roomUpdate("left", room), bStart, waitFor)
	c.must(update != nil && j < i, "no room_update left before the result")
}

func signInJoined(c *checker) {
	conn := c.open(true, "Conformance", false)
	you := conn.you["user_id"]
	listing := c.call(conn, "room_list", obj{"filter": "joined"})
	c.sync(conn)
	rooms := map[string]bool{}
	for _, f := range conn.since(0, method("room_update")) {
		for _, m := range list(get(f, "params", "memberships")) {
			for _, e := range list(get(m, "members")) {
				if get(e, "user", "user_id") == you && truthy(get(e, "joined")) {
					rooms[str(m, "room_id")] = true
				}
			}
		}
	}
	if len(rooms) == 0 {
		c.skip("the sign-in joined no room, or kept its memberships outside the log")
	}
	listed := map[string]bool{}
	for _, r := range list(listing["joined"]) {
		listed[str(r, "room_id")] = true
	}
	var lacking []string
	for room := range rooms {
		if !listed[room] {
			lacking = append(lacking, room)
		}
	}
	sort.Strings(lacking)
	c.must(len(lacking) == 0, "room_list joined lacks %s", strings.Join(lacking, ", "))
}

func joinUnknownRoom(c *checker) {
	conn := c.open(true, "Conformance", false)
	reply := c.replyTo(conn, "room_join", obj{"room_id": "conformance-no-such-room"})
	var control func() obj
	if c.r.room != "" {
		control = func() obj { return c.replyTo(conn, "room_join", obj{"room_id": c.r.room}) }
	}
	c.unlessPolicy(reply, "room_join", control)
	c.expectError(reply, "invalid_params", "room_join")
}

func privateRoom(c *checker) {
	a := c.open(true, "Conformance", false)
	start := a.count()
	reply := c.replyTo(a, "room_set", obj{"title": "Conformance private", "private": true})
	if code(reply) == wire.CodeUnsupported {
		return
	}
	c.unlessPolicy(reply, "a new room", nil)
	c.must(has(reply, "result"), "got %s", describe(reply))
	room := str(reply, "result", "room_id")
	_, update := a.wait(roomUpdate("joined", room), start, waitFor)
	c.must(update != nil && records(update, "joined", room)[0]["private"] == true, "joined lacks private: true")
	b := c.open(true, "Conformance", false)
	listing := c.call(b, "room_list", obj{"filter": "not_joined"})
	c.must(!slices.ContainsFunc(list(listing["not_joined"]), func(r any) bool { return get(r, "room_id") == room }), "a non-member sees it")
}

func reactionsBroadcast(c *checker) {
	a, b := c.joined("Conformance", false), c.joined("Conformance", false)
	_, messageID := c.post(a, "conformance: react to me", nil)
	aStart, bStart := a.count(), b.count()
	i, reply := c.request(a, "reactions", obj{"message_id": messageID, "emojis": []any{"👍"}}, "")
	c.must(isEmptyObject(reply["result"]), "got %s", describe(reply))
	isSet := func(f obj) bool { return f["method"] == "reactions" && get(f, "params", "message_id") == messageID }
	j, mine := a.wait(isSet, aStart, waitFor)
	_, theirs := b.wait(isSet, bStart, waitFor)
	c.must(theirs != nil, "no broadcast to another member")
	c.must(mine != nil && j < i, "the sender got its result before the broadcast")
	p := theirs["params"]
	c.must(get(p, "room_id") == c.r.room && has(p, "log_id"), "no room_id or log_id")
	sets := list(get(p, "reactions"))
	c.must(len(sets) == 1 && get(sets[0], "from", "user_id") == a.you["user_id"] && reflect.DeepEqual(get(sets[0], "emojis"), []any{"👍"}),
		"reactions is not the sender's one set")
	c.expectError(c.replyTo(a, "reactions", obj{"message_id": missing, "emojis": []any{"👍"}}), "invalid_params", "an unknown message")
}

func activityNotification(c *checker) {
	conn := c.joined("Conformance", false)
	start := conn.count()
	c.notify(conn, "activity", obj{"room_id": c.r.room, "typing": 5})
	c.notify(conn, "activity", obj{"room_id": c.r.room, "typing": 0})
	c.sync(conn)
	c.must(len(conn.since(start, isError)) == 0, "activity got an error")
}

func statusRequest(c *checker) {
	conn := c.open(true, "Conformance", false)
	c.must(isEmptyObject(c.call(conn, "status", obj{"idle": 300})), "idle did not reply {}")
	c.must(isEmptyObject(c.call(conn, "status", obj{"idle": false})), "idle: false did not reply {}")
	room := c.r.room
	if room == "" {
		room = "general"
	}
	c.expectError(c.replyTo(conn, "status", obj{"room_id": room}), "invalid_params", "room_id without mute")
	for _, mute := range []bool{true, false} {
		start := conn.count()
		c.call(conn, "status", obj{"mute": mute})
		_, sent := conn.wait(func(f obj) bool { return f["method"] == "status" && get(f, "params", "mute") == mute }, start, waitFor)
		c.must(sent != nil, "no status notification with mute: %v", mute)
	}
}

func statusMe(c *checker) {
	conn := c.open(true, "Conformance", false)
	value := list(c.r.info()["status"])[0]
	you := get(c.call(conn, "me", obj{"status": value}), "you")
	c.must(get(you, "status") == value, "you.status is %v, not %v", get(you, "status"), value)
	c.call(conn, "me", obj{"status": "online"})
}

func seenInvisible(c *checker) {
	watcher := c.joined("Watcher", false)
	subject := c.open(false, "", false)
	signedIn := c.call(subject, "auth", c.auth("Subject"))
	you, _ := signedIn["you"].(obj)
	if you["user_id"] == watcher.you["user_id"] {
		c.skip("needs two users, and every sign-in is the same user")
	}
	token, _ := signedIn["token"].(string)
	if c.r.has("rooms") {
		c.call(subject, "room_join", obj{"room_id": c.r.room})
	}
	about := func(f obj) bool {
		return f["method"] == "user" && get(f, "params", "new", "user_id") == you["user_id"] && has(get(f, "params", "new"), "status")
	}
	c.sync(watcher) // the subject's sign-in has been announced
	start := watcher.count()
	mine := get(c.call(subject, "me", obj{"status": "invisible"}), "you")
	c.must(get(mine, "status") == "invisible", "you.status is %v, not 'invisible'", get(mine, "status"))
	c.must(!has(mine, "seen"), "you carries seen")
	_, hidden := watcher.wait(about, start, waitFor)
	if hidden != nil {
		status := get(hidden, "params", "new", "status")
		c.must(status == "offline", "a chosen invisible is shown as %v", status)
	}
	firstSeen := get(hidden, "params", "new", "seen")
	start = watcher.count()
	if token != "" { // a second connection of the same user comes and goes
		again := c.open(false, "", false)
		c.call(again, "auth", obj{"scheme": "token", "token": token})
		again.close()
	}
	c.sync(subject)
	c.sync(watcher)
	var leaks []obj
	for _, f := range watcher.since(start, about) {
		user, _ := get(f, "params", "new").(obj)
		seen, hasSeen := user["seen"]
		if !hasSeen {
			seen = firstSeen
		}
		if user["status"] != "offline" || !reflect.DeepEqual(seen, firstSeen) {
			leaks = append(leaks, user)
		}
	}
	c.must(len(leaks) == 0, "the watcher got %v while the subject stayed invisible", firstLeak(leaks))
	c.should(hidden != nil, "no user notification when the subject chose invisible")
}

func firstLeak(leaks []obj) any {
	if len(leaks) == 0 {
		return ""
	}
	return leaks[0]
}

func commandHelp(c *checker) {
	conn := c.joined("Conformance", false)
	start := conn.count()
	i, reply := c.request(conn, "command", obj{"room_id": c.r.room, "body": obj{"text": "/help"}}, "")
	c.must(has(reply, "result") && !has(reply["result"], "message_id"), "got %s", describe(reply))
	j, notice := conn.wait(func(f obj) bool {
		return f["method"] == "message" && get(f, "params", "from", "user_id") == wire.Private.UserID
	}, start, waitFor)
	c.should(notice != nil && j < i, "no ~private notice before the result")
	c.must(!has(notice["params"], "message_id"), "the notice has a message_id")
}

func commandMessageID(c *checker) {
	conn := c.joined("Conformance", false)
	reply := c.replyTo(conn, "command", obj{"room_id": c.r.room, "message_id": missing, "body": obj{"text": "/help"}})
	c.expectError(reply, "invalid_params", "command with message_id")
}

func extMergeMe(c *checker) {
	conn := c.open(true, "Conformance", false)
	reply := c.replyTo(conn, "me", obj{"ext": obj{"conformance": obj{"a": 1}}})
	c.unlessPolicy(reply, "ext on the user", nil)
	you := get(c.call(conn, "me", obj{"ext": obj{"conformance": obj{"a": 1}}}), "you")
	c.must(reflect.DeepEqual(get(you, "ext", "conformance"), obj{"a": 1.0}), "ext.conformance was not kept")
	you = get(c.call(conn, "me", obj{"ext": obj{"conformance2": "x"}}), "you")
	c.must(reflect.DeepEqual(get(you, "ext", "conformance"), obj{"a": 1.0}), "a write of another key dropped ext.conformance")
	you = get(c.call(conn, "me", obj{"ext": obj{"conformance": "", "conformance2": ""}}), "you")
	c.must(!has(get(you, "ext"), "conformance"), "an empty value did not clear the key")
}

func extMergeMessage(c *checker) {
	conn := c.joined("Conformance", false)
	_, messageID := c.post(conn, "conformance: ext", obj{"ext": obj{"conformance": obj{"a": 1}}})
	_, snapshot := conn.wait(snapshotOf(messageID, ""), 0, waitFor)
	c.must(snapshot != nil && reflect.DeepEqual(get(snapshot, "params", "ext", "conformance"), obj{"a": 1.0}), "the snapshot lost ext")
	if c.r.has("edit") {
		start := conn.count()
		c.call(conn, "message", obj{"message_id": messageID, "room_id": c.r.room, "body": obj{"text": "conformance: ext"},
			"ext": obj{"conformance2": 1}})
		_, saved := conn.wait(snapshotOf(messageID, ""), start, waitFor)
		c.must(saved != nil && reflect.DeepEqual(get(saved, "params", "ext"), obj{"conformance": obj{"a": 1.0}, "conformance2": 1.0}),
			"the save did not merge ext")
	}
}

func pushUnregister(c *checker) {
	conn := c.open(true, "Conformance", false)
	result := c.call(conn, "push_unregister", obj{"url": "https://push.example.invalid/conformance"})
	c.must(isEmptyObject(result), "result is not {}")
}

// seen reports what the frames broke of a rule, as inspect found it.
func seen(key string) func(*checker) {
	return func(c *checker) {
		c.r.mu.Lock()
		found := slices.Sorted(maps.Keys(c.r.violations[key]))
		c.r.mu.Unlock()
		if len(found) > 3 {
			found = append(found[:3:3], fmt.Sprintf("(+%d more)", len(found)-3))
		}
		c.must(len(found) == 0, "%s", strings.Join(found, "; "))
	}
}
