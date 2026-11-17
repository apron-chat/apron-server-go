package apron

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RemoteConfig turns on trusted federation (apron's
// recipes/trusted_federation.md, with ext:remote): rooms shared with other
// Apron servers. Each server has one link for each remote server, an
// account that the remote server's sender signs in as to write as its
// users here, and sends its own users' records in each shared room to the
// remote server through that server's link for it, on demand.
type RemoteConfig struct {
	// Self is this server's address (PROTOCOL.md §1), as other servers dial
	// it and as its invites carry it, such as wss://chat.example/ws. Empty
	// turns federation off.
	Self string
	// Linger is how long a sender keeps its connection to a remote server
	// once it has nothing to send; zero is DefaultRemoteLinger.
	Linger time.Duration
}

// DefaultRemoteLinger is how long a sender keeps an idle connection.
const DefaultRemoteLinger = 30 * time.Second

// federation is trusted federation (RemoteConfig): the /remote commands,
// the ext:remote request that pairs rooms, and a sender for each remote
// server. It runs as an extension of the server's own, named remote, whose
// KV keeps the pairs, the tokens, and each sender's place in the log and
// map of copies.
type federation struct {
	s      *Server
	h      *Host
	kv     KV
	self   string
	linger time.Duration

	// save serializes the writes of pairs, tokens, and stopped to the KV.
	save sync.Mutex
	mu   sync.Mutex
	// pairs maps each shared room here to the remote servers it is paired
	// with, and the room there.
	pairs map[string]map[string]string
	// tokens are the newest token of each remote server's link for this
	// server, and stopped the remote servers that refused it.
	tokens  map[string]string
	stopped map[string]bool
	senders map[string]*sender
}

func newFederation(s *Server, config RemoteConfig) (*federation, error) {
	self, err := NormalizeAddress(config.Self)
	if err != nil {
		return nil, fmt.Errorf("Config.Remote.Self: %w", err)
	}
	linger := config.Linger
	if linger <= 0 {
		linger = DefaultRemoteLinger
	}
	return &federation{
		s: s, self: self, linger: linger,
		pairs: make(map[string]map[string]string), tokens: make(map[string]string),
		stopped: make(map[string]bool), senders: make(map[string]*sender),
	}, nil
}

func (f *federation) Name() string { return remoteExt }

func (f *federation) Init(r *Registrar) error {
	f.h, f.kv = r.Host, r.Store()
	for key, into := range map[string]any{"pairs": &f.pairs, "tokens": &f.tokens, "stopped": &f.stopped} {
		if _, err := f.kv.Get(key, into); err != nil {
			return err
		}
	}
	for _, m := range []*map[string]string{&f.tokens} {
		if *m == nil {
			*m = make(map[string]string)
		}
	}
	if f.pairs == nil {
		f.pairs = make(map[string]map[string]string)
	}
	if f.stopped == nil {
		f.stopped = make(map[string]bool)
	}
	r.Command(Command{
		Name:  "remote",
		Usage: "/remote invite <address> | connect <invite> | unshare | list | revoke <address>",
		Help:  "share this room with another Apron server (admins)",
		Available: func(u User, _ Room) bool {
			return u.HasRole(RoleAdmin)
		},
		Run: f.command,
	})
	r.register("the ext:remote request", func() error {
		f.s.ops["ext:remote"] = func(s *Server, c *client, req request) (any, bool, *Error) { return f.pairRequest(c, req) }
		return nil
	})
	f.mu.Lock()
	var servers []string
	for _, remotes := range f.pairs {
		for server := range remotes {
			servers = append(servers, server)
		}
	}
	f.mu.Unlock()
	for _, server := range servers {
		f.sender(server).wake()
	}
	return nil
}

// ---- Invites ----

// parseInvite reads an invite (PROTOCOL.md Appendix A.4), in its https or
// apron: form, for its server, room, and token.
func parseInvite(link string) (server, room, token string, err error) {
	link = strings.Trim(strings.TrimSpace(link), "`<>")
	var query string
	switch {
	case strings.HasPrefix(link, "apron:?"):
		query = strings.TrimPrefix(link, "apron:?")
	case strings.HasPrefix(link, "https://"):
		_, query, _ = strings.Cut(link, "#")
	default:
		return "", "", "", errors.New("an invite is a link: https://link.apron.chat/#server=…&room=…&token=…")
	}
	values, err := url.ParseQuery(query)
	if err != nil {
		return "", "", "", fmt.Errorf("the invite: %w", err)
	}
	server, room, token = values.Get("server"), values.Get("room"), values.Get("token")
	if server == "" || room == "" || token == "" {
		return "", "", "", errors.New("the invite needs server, room, and token")
	}
	if server, err = NormalizeAddress(server); err != nil {
		return "", "", "", err
	}
	return server, room, token, nil
}

// linkValue escapes a value of a link, leaving : and / as they are
// (Appendix A.4).
func linkValue(value string) string {
	return strings.NewReplacer("%3A", ":", "%2F", "/").Replace(url.QueryEscape(value))
}

// inviteLink is this server's invite to a room with a link's token.
func (f *federation) inviteLink(roomID, token string) string {
	return "https://link.apron.chat/#server=" + linkValue(f.self) + "&room=" + linkValue(roomID) + "&token=" + linkValue(token)
}

// ---- Pairs ----

// persist writes the pairs, tokens, and stopped servers to the KV.
func (f *federation) persist() error {
	f.save.Lock()
	defer f.save.Unlock()
	f.mu.Lock()
	pairs := make(map[string]map[string]string, len(f.pairs))
	for room, remotes := range f.pairs {
		pairs[room] = maps.Clone(remotes)
	}
	tokens, stopped := maps.Clone(f.tokens), maps.Clone(f.stopped)
	f.mu.Unlock()
	return errors.Join(f.kv.Put("pairs", pairs), f.kv.Put("tokens", tokens), f.kv.Put("stopped", stopped))
}

// setPair pairs a room here with a room of a remote server, and keeps the
// token of that server's link for this server, the newest one. The
// sender's place in the log starts at the latest record when the server's
// first pair is made: older records are not sent.
func (f *federation) setPair(roomID, server, remoteRoom, token string) error {
	cursorKey := "cursor " + server
	var cursor int64
	if found, err := f.kv.Get(cursorKey, &cursor); err != nil {
		return err
	} else if !found {
		if err := f.kv.Put(cursorKey, f.h.LastLogID()); err != nil {
			return err
		}
	}
	f.mu.Lock()
	if f.pairs[roomID] == nil {
		f.pairs[roomID] = make(map[string]string)
	}
	f.pairs[roomID][server] = remoteRoom
	f.tokens[server] = token
	delete(f.stopped, server)
	f.mu.Unlock()
	if err := f.persist(); err != nil {
		return err
	}
	f.sender(server).wake()
	return nil
}

// dropPairs ends the pairs of a room (roomID set), of a remote server
// (server set), or one pair (both).
func (f *federation) dropPairs(roomID, server string) error {
	f.mu.Lock()
	for room, remotes := range f.pairs {
		if roomID != "" && room != roomID {
			continue
		}
		for remote := range remotes {
			if server == "" || remote == server {
				delete(remotes, remote)
			}
		}
		if len(remotes) == 0 {
			delete(f.pairs, room)
		}
	}
	f.mu.Unlock()
	return f.persist()
}

// pairsWith are the rooms here paired with a remote server whose link for
// it has joined them, mapped to the rooms there.
func (f *federation) pairsWith(server string) map[string]string {
	f.mu.Lock()
	rooms := make(map[string]string)
	for room, remotes := range f.pairs {
		if remote, ok := remotes[server]; ok {
			rooms[room] = remote
		}
	}
	f.mu.Unlock()
	s := f.s
	s.mu.RLock()
	defer s.mu.RUnlock()
	link := s.links[server]
	for room := range rooms {
		if r := s.rooms[room]; link == nil || r == nil || r.members[link.id] == nil {
			delete(rooms, room)
		}
	}
	return rooms
}

// pairRequest answers an ext:remote request: a link pairs a room it
// joined with its server's room, and gives the way to send there.
func (f *federation) pairRequest(c *client, req request) (any, bool, *Error) {
	roomID, err := parseString(req.params, "room_id", true)
	if err != nil {
		return nil, false, err
	}
	link, err := parseString(req.params, "link", true)
	if err != nil {
		return nil, false, err
	}
	s := f.s
	s.mu.RLock()
	u := c.user
	bound := ""
	if u.link != nil {
		bound = u.link.server
	}
	r := s.rooms[roomID]
	joined := r != nil && r.members[u.id] != nil
	s.mu.RUnlock()
	switch {
	case bound == "":
		return nil, false, &Error{Code: CodeDenied, Message: "Only a link pairs rooms"}
	case !joined:
		return nil, false, invalidParams("This link has not joined room %q", roomID)
	}
	server, room, token, parseErr := parseInvite(link)
	switch {
	case parseErr != nil:
		return nil, false, invalidParams("link: %v", parseErr)
	case server != bound:
		return nil, false, invalidParams("The link's server is %s, not this link's", server)
	}
	if err := f.setPair(roomID, server, room, token); err != nil {
		return nil, false, f.h.protocolError(err)
	}
	f.h.logger.Info("a remote server paired a room", "room", roomID, "server", server, "remote_room", room)
	return map[string]any{}, false, nil
}

// ---- Commands ----

func (f *federation) command(ctx context.Context, call *CommandCall) (map[string]any, error) {
	verb, arg, _ := strings.Cut(call.Args, " ")
	arg = strings.TrimSpace(arg)
	switch verb {
	case "invite":
		return nil, f.invite(ctx, call, arg)
	case "connect":
		return nil, f.connect(ctx, call, arg)
	case "unshare":
		return nil, f.unshare(call)
	case "list":
		call.Notice(f.list())
		return nil, nil
	case "revoke":
		return nil, f.revoke(call, arg)
	}
	return nil, InvalidParams("Usage: /remote invite <address> | connect <invite> | unshare | list | revoke <address>")
}

// joinLink makes the link for a remote server, if there is none, and joins
// it to the room.
func (f *federation) joinLink(server, roomID string) (*userState, error) {
	if server == f.self {
		return nil, InvalidParams("That is this server's own address")
	}
	s := f.s
	s.mu.Lock()
	defer s.unlock()
	link, err := s.linkLocked(server)
	if err != nil {
		return nil, InvalidParams(err.Error())
	}
	r := s.rooms[roomID]
	if r == nil {
		return nil, InvalidParams("Unknown room " + roomID)
	}
	s.joinLocked(link, r)
	return link, nil
}

// invite makes or reuses the link for a remote server, joins it to the
// room, and replies with an invite that carries a new token for it.
func (f *federation) invite(ctx context.Context, call *CommandCall, address string) error {
	server, err := NormalizeAddress(address)
	if err != nil {
		return InvalidParams("Usage: /remote invite <address>, such as wss://chat.example/ws: " + err.Error())
	}
	link, err := f.joinLink(server, call.Room.ID)
	if err != nil {
		return err
	}
	token, err := f.s.mintLinkToken(ctx, link, "invite to "+call.Room.ID)
	if err != nil {
		return err
	}
	call.Notice(fmt.Sprintf("Give this invite to an admin of %s, to run `/remote connect` with in the room to share there; it signs in as %s, this server's link for that server:\n\n`%s`", server, link.id, f.inviteLink(call.Room.ID, token)))
	return nil
}

// connect pairs the room with the room of an invite: it joins this
// server's link for the inviting server to the room, and sends the
// inviting server an invite back, as an ext:remote request through its
// link for this server.
func (f *federation) connect(ctx context.Context, call *CommandCall, invite string) error {
	server, room, token, err := parseInvite(invite)
	if err != nil {
		return InvalidParams("Usage: /remote connect <invite>: " + err.Error())
	}
	link, err := f.joinLink(server, call.Room.ID)
	if err != nil {
		return err
	}
	own, err := f.s.mintLinkToken(ctx, link, "connect to "+call.Room.ID)
	if err != nil {
		return err
	}
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := f.dial(dialCtx, server, token)
	if err != nil {
		call.Notice(fmt.Sprintf("Could not connect to %s: %v", server, err))
		return nil
	}
	defer conn.close()
	params := map[string]any{"room_id": room, "link": f.inviteLink(call.Room.ID, own)}
	if _, err := conn.call(dialCtx, "connect-"+strconv.FormatInt(time.Now().UnixNano(), 36), "ext:remote", params); err != nil {
		call.Notice(fmt.Sprintf("%s refused to pair the rooms: %v", server, err))
		return nil
	}
	if err := f.setPair(call.Room.ID, server, room, token); err != nil {
		return err
	}
	call.Notice(fmt.Sprintf("Connected: this room is shared with room %s of %s.", room, server))
	return nil
}

// unshare takes every link out of the room, and ends its pairs.
func (f *federation) unshare(call *CommandCall) error {
	s := f.s
	s.mu.Lock()
	r := s.rooms[call.Room.ID]
	var servers []string
	if r != nil {
		for server, link := range s.links {
			if s.leaveLocked(link, r) {
				servers = append(servers, server)
			}
		}
	}
	s.unlock()
	if err := f.dropPairs(call.Room.ID, ""); err != nil {
		return err
	}
	slices.Sort(servers)
	if len(servers) == 0 {
		call.Notice("This room was not shared.")
	} else {
		call.Notice("This room is no longer shared with " + strings.Join(servers, ", ") + ".")
	}
	return nil
}

// list describes the links and the pairs.
func (f *federation) list() string {
	s := f.s
	var b strings.Builder
	b.WriteString("This server is " + f.self + ".\n\nLinks:\n")
	s.mu.RLock()
	for _, server := range slices.Sorted(maps.Keys(s.links)) {
		link := s.links[server]
		rooms := slices.Sorted(maps.Keys(link.joined))
		fmt.Fprintf(&b, "- %s for %s, in %s\n", link.id, server, strings.Join(rooms, ", "))
	}
	s.mu.RUnlock()
	b.WriteString("\nShared rooms:\n")
	f.mu.Lock()
	for _, room := range slices.Sorted(maps.Keys(f.pairs)) {
		for _, server := range slices.Sorted(maps.Keys(f.pairs[room])) {
			state := ""
			if f.stopped[server] {
				state = " (stopped: it refused the token)"
			}
			fmt.Fprintf(&b, "- %s with room %s of %s%s\n", room, f.pairs[room][server], server, state)
		}
	}
	f.mu.Unlock()
	return b.String()
}

// revoke revokes the tokens of the link for a remote server, which closes
// its connections, and ends every pair with that server. Its remote users
// stay.
func (f *federation) revoke(call *CommandCall, address string) error {
	server, err := NormalizeAddress(address)
	if err != nil {
		return InvalidParams("Usage: /remote revoke <address>: " + err.Error())
	}
	s := f.s
	s.mu.Lock()
	link := s.links[server]
	if link != nil {
		s.revokeServicesOfLocked(link)
	}
	s.unlock()
	if err := f.dropPairs("", server); err != nil {
		return err
	}
	if link == nil {
		call.Notice("There is no link for " + server + ".")
	} else {
		call.Notice(fmt.Sprintf("Revoked %s, the link for %s, and ended the rooms shared with it.", link.id, server))
	}
	return nil
}

// ---- Sending ----

// dial connects to a remote server and signs in with a token of its link
// for this server.
func (f *federation) dial(ctx context.Context, server, token string) (*remoteConn, error) {
	conn, err := dialRemote(ctx, server)
	if err != nil {
		return nil, err
	}
	var frame struct {
		Ping float64 `json:"ping"`
	}
	select {
	case raw := <-conn.server:
		_ = json.Unmarshal(raw, &frame)
	case <-conn.done:
		return nil, cmpErr(conn.err, errRemoteDisconnected)
	case <-ctx.Done():
		conn.close()
		return nil, ctx.Err()
	}
	if frame.Ping > 0 {
		go conn.ping(time.Duration(frame.Ping * float64(time.Second)))
	}
	if _, err := conn.call(ctx, "auth-"+strconv.FormatInt(time.Now().UnixNano(), 36), "auth", map[string]any{"scheme": "token", "token": token, "agent": "apron-go/10 federation"}); err != nil {
		conn.close()
		return nil, err
	}
	return conn, nil
}

func cmpErr(err, otherwise error) error {
	if err != nil {
		return err
	}
	return otherwise
}

// sender sends this server's records in the rooms shared with one remote
// server, through that server's link for it, from its place in the log.
type sender struct {
	f      *federation
	server string
	wakeCh chan struct{}
	// profiles are local users whose profile changed, guarded by f.mu.
	profiles map[string]bool
}

// sender returns the remote server's sender, started if it was not.
func (f *federation) sender(server string) *sender {
	f.mu.Lock()
	sd := f.senders[server]
	started := sd == nil
	if started {
		sd = &sender{f: f, server: server, wakeCh: make(chan struct{}, 1), profiles: make(map[string]bool)}
		f.senders[server] = sd
	}
	f.mu.Unlock()
	// Go takes s.mu, which profileChanged holds as it takes f.mu.
	if started {
		f.h.Go(sd.run)
	}
	return sd
}

func (sd *sender) wake() {
	select {
	case sd.wakeCh <- struct{}{}:
	default:
	}
}

// profileChanged notes a local user's profile change for every sender,
// which sends it to remote servers that the user's records went to. It is
// called holding s.mu.
func (f *federation) profileChanged(userID string) {
	f.mu.Lock()
	senders := slices.Collect(maps.Values(f.senders))
	for _, sd := range senders {
		sd.profiles[userID] = true
	}
	f.mu.Unlock()
	for _, sd := range senders {
		sd.wake()
	}
}

// sentRecord is a record of the log a sender reads.
type sentRecord struct {
	id    int64
	kind  recordKind
	rooms []string
	raw   jsontext.Value
}

// errStopSending ends sending to a server that refused the token.
var errStopSending = errors.New("the remote server refused this server's token")

// run sends what is after the sender's place in the log whenever there is
// something, keeping the connection for the linger time after the last,
// and dialing again with a growing delay after a failure.
func (sd *sender) run(ctx context.Context) {
	f, s := sd.f, sd.f.s
	logger := f.h.logger.With("server", sd.server)
	cursorKey := "cursor " + sd.server
	var conn *remoteConn
	defer func() {
		if conn != nil {
			conn.close()
		}
	}()
	backoff := time.Second
	for ctx.Err() == nil {
		f.mu.Lock()
		stopped := f.stopped[sd.server]
		f.mu.Unlock()
		pairs := f.pairsWith(sd.server)
		if stopped || len(pairs) == 0 {
			if conn != nil {
				conn.close()
				conn = nil
			}
			select {
			case <-sd.wakeCh:
				continue
			case <-ctx.Done():
				return
			}
		}
		var cursor int64
		if _, err := f.kv.Get(cursorKey, &cursor); err != nil {
			return
		}
		s.mu.RLock()
		i, found := slices.BinarySearchFunc(s.records, cursor, compareLogID)
		if found {
			i++
		}
		var batch []sentRecord
		for _, r := range s.records[i:min(len(s.records), i+eventBatch)] {
			if slices.ContainsFunc(r.rooms, func(room string) bool { _, ok := pairs[room]; return ok }) {
				batch = append(batch, sentRecord{r.id, r.kind, slices.Clone(r.rooms), slices.Clone(r.raw)})
			} else {
				batch = append(batch, sentRecord{id: r.id}) // not this server's: skipped
			}
		}
		changed := s.recordsChanged
		s.mu.RUnlock()
		f.mu.Lock()
		profiles := slices.Sorted(maps.Keys(sd.profiles))
		clear(sd.profiles)
		f.mu.Unlock()

		pending := len(profiles) > 0 || slices.ContainsFunc(batch, func(r sentRecord) bool { return r.raw != nil })
		if pending && conn == nil {
			f.mu.Lock()
			token := f.tokens[sd.server]
			f.mu.Unlock()
			dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			c, err := f.dial(dialCtx, sd.server, token)
			cancel()
			if e, ok := errors.AsType[*remoteError](err); ok && e.Code == remoteCodeDenied {
				logger.Error("the remote server refused this server's token; sending to it stops until an admin runs /remote connect again", "error", err)
				f.mu.Lock()
				f.stopped[sd.server] = true
				f.mu.Unlock()
				_ = f.persist()
				continue
			}
			if err != nil {
				f.mu.Lock()
				for _, id := range profiles {
					sd.profiles[id] = true
				}
				f.mu.Unlock()
				logger.Warn("connecting to the remote server failed", "error", err, "retry_in", backoff)
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return
				}
				backoff = min(2*backoff, 5*time.Minute)
				continue
			}
			conn, backoff = c, time.Second
			logger.Info("connected to the remote server to send")
		}
		var sendErr error
		for _, id := range profiles {
			if sendErr = sd.profile(ctx, conn, id); sendErr != nil {
				break
			}
		}
		moved := cursor
		for _, r := range batch {
			if sendErr != nil {
				break
			}
			if r.raw != nil {
				if sendErr = sd.send(ctx, conn, r, pairs); sendErr != nil {
					break
				}
			}
			moved = r.id
			if r.raw != nil {
				if err := f.kv.Put(cursorKey, moved); err != nil {
					return
				}
			}
		}
		if moved != cursor {
			if err := f.kv.Put(cursorKey, moved); err != nil {
				return
			}
		}
		if sendErr != nil {
			if ctx.Err() != nil {
				return
			}
			logger.Warn("sending to the remote server failed; sending again", "error", sendErr)
			if conn != nil {
				conn.close()
				conn = nil
			}
			f.mu.Lock()
			for _, id := range profiles {
				sd.profiles[id] = true
			}
			f.mu.Unlock()
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			backoff = min(2*backoff, 5*time.Minute)
			continue
		}
		if len(batch) > 0 || len(profiles) > 0 {
			continue // there may be more
		}
		// Nothing to send: linger, then close.
		var linger <-chan time.Time
		if conn != nil {
			linger = time.After(f.linger)
		}
		var done <-chan struct{}
		if conn != nil {
			done = conn.done
		}
		select {
		case <-changed:
		case <-sd.wakeCh:
		case <-linger:
			conn.close()
			conn = nil
			logger.Info("closed the idle connection to the remote server")
		case <-done:
			conn = nil
		case <-ctx.Done():
			return
		}
	}
}

// call sends one request, waiting out retry_after. A refusal of the
// request is logged and skipped (nil); a link that is not in the room any
// more ends that pair.
func (sd *sender) call(ctx context.Context, conn *remoteConn, op string, logID int64, method string, params map[string]any, roomID string) (jsontext.Value, bool, error) {
	encoded, _ := json.Marshal(params, json.Deterministic(true))
	sum := sha256.Sum256(encoded)
	id := fmt.Sprintf("fed-%d-%s-%s", logID, op, hex.EncodeToString(sum[:6]))
	for {
		result, err := conn.call(ctx, id, method, params)
		if wait := remoteRetryAfter(err); wait > 0 {
			select {
			case <-time.After(wait):
				continue
			case <-ctx.Done():
				return nil, false, ctx.Err()
			}
		}
		e, refused := errors.AsType[*remoteError](err)
		if !refused {
			return result, err == nil, err
		}
		sd.f.h.logger.Warn("the remote server refused a record; skipping it", "server", sd.server, "log_id", logID, "method", method, "error", err)
		// The protocol has no reason codes: a link that left a public room
		// is told so (denied), and one that left a private room is told
		// the room is unknown (invalid_params), as anyone else would be.
		gone := e.Code == remoteCodeDenied && strings.Contains(e.Message, "not in that room") ||
			e.Code == remoteCodeInvalidParams && method == "message" && params["message_id"] == nil && strings.Contains(strings.ToLower(e.Message), "unknown room")
		if gone && roomID != "" {
			sd.f.h.logger.Warn("this server's link left the remote room: the pair ends", "server", sd.server, "room", roomID)
			_ = sd.f.dropPairs(roomID, sd.server)
		}
		return nil, false, nil
	}
}

// localAuthor reports whether a user is this server's own: not a remote
// user, not a link, and not a system identity. A guest that left is.
func (f *federation) localAuthor(userID string) bool {
	if userID == "" || strings.HasPrefix(userID, "~") {
		return false
	}
	s := f.s
	s.mu.RLock()
	defer s.mu.RUnlock()
	u := s.users[userID]
	return u == nil || u.shadow == nil && u.link == nil
}

// remoteUser is a local user's remote user on a remote server, and the
// name last sent for it.
type remoteUser struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (sd *sender) userKey(userID string) string { return "user " + sd.server + " " + userID }

// user sends a local user's profile the first time the sender sends for
// them, and after a change, and returns their user_id on the remote server.
func (sd *sender) user(ctx context.Context, conn *remoteConn, userID, recorded string, logID int64) (string, error) {
	var kept remoteUser
	found, err := sd.f.kv.Get(sd.userKey(userID), &kept)
	if err != nil {
		return "", err
	}
	name := recorded
	s := sd.f.s
	s.mu.RLock()
	if u := s.users[userID]; u != nil {
		name = u.name
	}
	s.mu.RUnlock()
	if found && kept.Name == name {
		return kept.ID, nil
	}
	params := map[string]any{"name": name, "ext": map[string]any{"remote": map[string]any{"user_id": userID}}}
	raw, ok, err := sd.call(ctx, conn, "me-"+userID+"-"+strconv.FormatInt(time.Now().UnixNano(), 36), logID, "me", params, "")
	if err != nil || !ok {
		return kept.ID, err
	}
	var result struct {
		You struct {
			UserID string `json:"user_id"`
		} `json:"you"`
	}
	_ = json.Unmarshal(raw, &result)
	kept = remoteUser{ID: result.You.UserID, Name: name}
	return kept.ID, sd.f.kv.Put(sd.userKey(userID), kept)
}

// profile sends a local user's changed profile, if the sender sent for them.
func (sd *sender) profile(ctx context.Context, conn *remoteConn, userID string) error {
	var kept remoteUser
	if found, err := sd.f.kv.Get(sd.userKey(userID), &kept); err != nil || !found {
		return err
	}
	_, err := sd.user(ctx, conn, userID, kept.Name, 0)
	return err
}

func (sd *sender) copyKey(messageID string) string { return "copy " + sd.server + " " + messageID }

func (sd *sender) copyOf(messageID string) string {
	var id string
	_, _ = sd.f.kv.Get(sd.copyKey(messageID), &id)
	return id
}

// target is the message on the remote server that a message here stands
// for: the copy of this server's own message, the original of a copy of
// the remote server's, or "" for none, such as a message never sent or a
// copy of a third server's.
func (sd *sender) target(messageID string) string {
	s := sd.f.s
	s.mu.RLock()
	m := s.messages[messageID]
	var owner *userState
	var remote map[string]any
	if m != nil {
		owner = s.users[m.owner]
		var ext struct {
			Remote map[string]any `json:"remote"`
		}
		_ = json.Unmarshal(encodeJSON(m.snapshot()["ext"]), &ext)
		remote = ext.Remote
	}
	s.mu.RUnlock()
	switch {
	case m == nil:
		return ""
	case owner != nil && owner.link != nil:
		return ""
	case owner != nil && owner.shadow != nil:
		if owner.shadow.owner != remoteExt || owner.shadow.server != sd.server {
			return ""
		}
		id, _ := remote["message_id"].(string)
		return id
	}
	return sd.copyOf(messageID)
}

// sentMessage is a message record as the sender reads it.
type sentMessage struct {
	MessageID string `json:"message_id"`
	LogID     string `json:"log_id"`
	RoomID    string `json:"room_id"`
	From      struct {
		UserID string `json:"user_id"`
		Name   string `json:"name"`
	} `json:"from"`
	Body    map[string]any `json:"body"`
	ReplyTo *struct {
		MessageID string `json:"message_id"`
	} `json:"reply_to"`
	Deleted bool `json:"deleted"`
}

// send sends one record of a shared room.
func (sd *sender) send(ctx context.Context, conn *remoteConn, r sentRecord, pairs map[string]string) error {
	switch r.kind {
	case kindMessage:
		var m sentMessage
		if json.Unmarshal(r.raw, &m) != nil {
			return nil
		}
		return sd.message(ctx, conn, r, m, pairs)
	case kindReactions:
		return sd.reactions(ctx, conn, r, pairs)
	case kindMembership:
		return sd.membership(ctx, conn, r, pairs)
	}
	return nil
}

func (sd *sender) message(ctx context.Context, conn *remoteConn, r sentRecord, m sentMessage, pairs map[string]string) error {
	if !sd.f.localAuthor(m.From.UserID) {
		return nil
	}
	movedFrom := ""
	if len(r.rooms) == 2 {
		for _, room := range r.rooms {
			if room != m.RoomID {
				movedFrom = room
			}
		}
	}
	remoteRoom, paired := pairs[m.RoomID]
	copyID := sd.copyOf(m.MessageID)
	act := map[string]any{"user_id": m.From.UserID}
	switch {
	case m.Deleted:
		// A message that was never sent stays unsent.
		if !paired || copyID == "" {
			return nil
		}
		params := map[string]any{"message_id": copyID, "room_id": remoteRoom, "deleted": true, "ext": map[string]any{"remote": act}}
		_, _, err := sd.call(ctx, conn, "delete", r.id, "message", params, m.RoomID)
		return err
	case paired:
		if copyID == "" && m.MessageID != m.LogID && movedFrom == "" {
			return nil // an edit of a message from before the pair, never sent
		}
		if _, err := sd.user(ctx, conn, m.From.UserID, m.From.Name, r.id); err != nil {
			return err
		}
		mapped, err := sd.mentions(ctx, conn, m.Body, r.id)
		if err != nil {
			return err
		}
		act["room_id"], act["message_id"] = m.RoomID, m.MessageID
		params := map[string]any{"room_id": remoteRoom, "body": remoteBody(m.Body, mapped), "ext": map[string]any{"remote": act}}
		if m.ReplyTo != nil {
			if target := sd.target(m.ReplyTo.MessageID); target != "" {
				params["reply_to"] = map[string]any{"message_id": target}
			}
		}
		op := "create"
		if copyID != "" {
			params["message_id"], op = copyID, "save"
		}
		raw, ok, err := sd.call(ctx, conn, op, r.id, "message", params, m.RoomID)
		if err != nil || !ok || copyID != "" {
			return err
		}
		var result struct {
			MessageID string `json:"message_id"`
		}
		if json.Unmarshal(raw, &result) == nil && result.MessageID != "" {
			return sd.f.kv.Put(sd.copyKey(m.MessageID), result.MessageID)
		}
		return nil
	case movedFrom != "" && copyID != "":
		// A move out of a shared room deletes the copy.
		from, ok := pairs[movedFrom]
		if !ok {
			return nil
		}
		params := map[string]any{"message_id": copyID, "room_id": from, "deleted": true, "ext": map[string]any{"remote": act}}
		_, _, err := sd.call(ctx, conn, "delete", r.id, "message", params, movedFrom)
		return err
	}
	return nil
}

// mentions maps the users a body mentions to their user_ids on the remote
// server: a local user to their remote user there, and a remote user of
// that server to its original user. Others are left out.
func (sd *sender) mentions(ctx context.Context, conn *remoteConn, body map[string]any, logID int64) (map[string]string, error) {
	mapped := make(map[string]string)
	s := sd.f.s
	for _, value := range asList(body["mentions"]) {
		id, _ := value.(string)
		if id == "" || strings.HasPrefix(id, "~") || mapped[id] != "" {
			continue
		}
		s.mu.RLock()
		u := s.users[id]
		var shadow *shadowState
		link := false
		name := ""
		if u != nil {
			shadow, link, name = u.shadow, u.link != nil, u.name
		}
		s.mu.RUnlock()
		switch {
		case link:
		case shadow != nil:
			if shadow.owner == remoteExt && shadow.server == sd.server {
				mapped[id] = shadow.remoteID
			}
		default:
			remoteID, err := sd.user(ctx, conn, id, name, logID)
			if err != nil {
				return nil, err
			}
			if remoteID != "" {
				mapped[id] = remoteID
			}
		}
	}
	return mapped, nil
}

func (sd *sender) reactions(ctx context.Context, conn *remoteConn, r sentRecord, pairs map[string]string) error {
	var record struct {
		MessageID string `json:"message_id"`
		RoomID    string `json:"room_id"`
		Reactions []struct {
			From struct {
				UserID string `json:"user_id"`
				Name   string `json:"name"`
			} `json:"from"`
			Emojis []string `json:"emojis"`
		} `json:"reactions"`
	}
	if json.Unmarshal(r.raw, &record) != nil {
		return nil
	}
	if _, paired := pairs[record.RoomID]; !paired {
		return nil
	}
	target := ""
	for _, set := range record.Reactions {
		if !sd.f.localAuthor(set.From.UserID) {
			continue
		}
		if target == "" {
			if target = sd.target(record.MessageID); target == "" {
				return nil
			}
		}
		if _, err := sd.user(ctx, conn, set.From.UserID, set.From.Name, r.id); err != nil {
			return err
		}
		emojis := set.Emojis
		if emojis == nil {
			emojis = []string{}
		}
		params := map[string]any{"message_id": target, "emojis": emojis, "ext": map[string]any{"remote": map[string]any{"user_id": set.From.UserID}}}
		if _, _, err := sd.call(ctx, conn, "react-"+set.From.UserID, r.id, "reactions", params, record.RoomID); err != nil {
			return err
		}
	}
	return nil
}

func (sd *sender) membership(ctx context.Context, conn *remoteConn, r sentRecord, pairs map[string]string) error {
	var record struct {
		RoomID  string `json:"room_id"`
		Members []struct {
			User struct {
				UserID string `json:"user_id"`
			} `json:"user"`
			Joined bool `json:"joined"`
		} `json:"members"`
	}
	if json.Unmarshal(r.raw, &record) != nil {
		return nil
	}
	remoteRoom, paired := pairs[record.RoomID]
	if !paired {
		return nil
	}
	for _, member := range record.Members {
		if member.Joined || !sd.f.localAuthor(member.User.UserID) {
			continue
		}
		// Only a user whose records went there has a remote user to leave.
		if found, err := sd.f.kv.Get(sd.userKey(member.User.UserID), &remoteUser{}); err != nil || !found {
			continue
		}
		params := map[string]any{"room_id": remoteRoom, "ext": map[string]any{"remote": map[string]any{"user_id": member.User.UserID}}}
		if _, _, err := sd.call(ctx, conn, "leave-"+member.User.UserID, r.id, "room_leave", params, record.RoomID); err != nil {
			return err
		}
	}
	return nil
}
