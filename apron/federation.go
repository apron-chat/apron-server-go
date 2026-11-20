package apron

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"encoding/json/v2"
)

// Trusted federation (apron's recipes/trusted_federation.md, with the draft
// ext:remote): rooms hosted by one server, the owner, and mirrored by
// others. This file holds the configuration, the /remote commands, and the
// owner's side; mirror.go the mirror's; remote.go the links and the remote
// users they write as. The server's own code calls into it only through
// s.fed, which is nil when federation is off, at hooks marked
// "ext:remote hook".

// RemoteConfig turns on trusted federation: rooms that this server hosts
// for other Apron servers, which mirror them, and rooms of other servers
// that this server mirrors.
type RemoteConfig struct {
	// Self is this server's address (PROTOCOL.md §1), as other servers dial
	// it and as its invites carry it, such as wss://chat.example/ws. Empty
	// turns federation off.
	Self string
	// Linger is how long a mirror keeps its connection to an owner once
	// nothing is in flight, before it closes it and waits for the owner's
	// wake push; zero stays connected.
	Linger time.Duration
}

// federation is trusted federation. It runs as an extension of the server's
// own, named remote, whose KV keeps what it needs across restarts.
type federation struct {
	s      *Server
	h      *Host
	kv     KV
	self   string
	linger time.Duration
	// selfAuthority is remoteAuthority(self), and wakeBase the http(s)
	// origin of self, where owners send wake pushes.
	selfAuthority, wakeBase string
	client                  *http.Client
	// links are this server's links (remote.go), by the address of the
	// remote server each is bound to, guarded by s.mu; bots are the
	// handles of the remote users that links write as, by user_id.
	links map[string]*userState
	bots  sync.Map

	mu sync.Mutex
	// mirrors are the owners this server mirrors rooms of, by address, and
	// rooms the mirror rooms and threads here, by room_id.
	mirrors map[string]*mirror
	rooms   map[string]*mirrorRoom
	// wakes are the owner side: the links' wake registrations and when each
	// was last woken.
	wakes map[string]*linkWake
}

func newFederation(s *Server, config RemoteConfig) (*federation, error) {
	self, err := normalizeAddress(config.Self)
	if err != nil {
		return nil, fmt.Errorf("Config.Remote.Self: %w", err)
	}
	authority, _ := remoteAuthority(self)
	base := "https://" + authority
	if strings.HasPrefix(self, "ws://") {
		base = "http://" + authority
	}
	f := &federation{
		s: s, self: self, linger: max(config.Linger, 0), selfAuthority: authority, wakeBase: base,
		client: &http.Client{Timeout: 10 * time.Second}, links: make(map[string]*userState),
		mirrors: make(map[string]*mirror), rooms: make(map[string]*mirrorRoom), wakes: make(map[string]*linkWake),
	}
	s.mu.RLock()
	for _, u := range s.users {
		if u.link != nil {
			f.links[u.link.server] = u
		}
	}
	s.mu.RUnlock()
	return f, nil
}

func (f *federation) Name() string { return remoteExt }

func (f *federation) Init(r *Registrar) error {
	f.h, f.kv = r.Host, r.Store()
	r.Command(Command{
		Name:  "remote",
		Usage: "/remote invite <address> | connect <invite> | unshare | list | revoke <address>",
		Help:  "share this room with another Apron server, or mirror one of its rooms (admins)",
		Available: func(u User, _ Room) bool {
			return u.HasRole(RoleAdmin)
		},
		Run: f.command,
	})
	r.Handle("POST /remote/wake/{id}", http.HandlerFunc(f.wakeHandler))
	// A link registers its wake push also where push is off.
	r.register("the links' wake push", func() error {
		if f.s.ops["push_register"] == nil {
			f.s.ops["push_register"] = func(s *Server, c *client, req request) (any, bool, *Error) {
				if took, result, err := f.registerWake(c, req); took {
					return result, false, err
				}
				return nil, false, &Error{Code: CodeUnsupported, Message: "This server does not offer push"}
			}
		}
		return nil
	})
	var wakes map[string]linkWake
	if _, err := f.kv.Get("wakes", &wakes); err != nil {
		return err
	}
	for id, w := range wakes {
		f.wakes[id] = &w
	}
	return f.initMirrors(r)
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
	if server, err = normalizeAddress(server); err != nil {
		return "", "", "", err
	}
	return server, room, token, nil
}

// linkValue escapes a value of a link, leaving : and / as they are
// (Appendix A.4).
func linkValue(value string) string {
	return strings.NewReplacer("%3A", ":", "%2F", "/").Replace(url.QueryEscape(value))
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
		return nil, f.unshare(ctx, call)
	case "list":
		call.Notice(f.list())
		return nil, nil
	case "revoke":
		return nil, f.revoke(call, arg)
	}
	return nil, InvalidParams("Usage: /remote invite <address> | connect <invite> | unshare | list | revoke <address>")
}

// invite makes or reuses the link for a mirror server, joins it to the room
// and to the room's threads that every member can see, and replies with an
// invite that carries a new token of the link.
func (f *federation) invite(ctx context.Context, call *CommandCall, address string) error {
	server, err := normalizeAddress(address)
	switch {
	case err != nil:
		return InvalidParams("Usage: /remote invite <address>, such as wss://chat.example/ws: " + err.Error())
	case server == f.self:
		return InvalidParams("That is this server's own address")
	}
	s := f.s
	s.mu.Lock()
	r := s.rooms[call.Room.ID]
	if r == nil || f.rooms[call.Room.ID] != nil {
		s.unlock()
		return InvalidParams("A mirror room is shared by its owner, not here")
	}
	link, linkErr := f.linkLocked(server)
	if linkErr != nil {
		s.unlock()
		return InvalidParams(linkErr.Error())
	}
	s.joinLocked(link, r)
	f.joinThreadsLocked(link, r)
	s.unlock()
	token, err := s.mintLinkToken(ctx, link, "invite to "+call.Room.ID)
	if err != nil {
		return err
	}
	invite := "https://link.apron.chat/#server=" + linkValue(f.self) + "&room=" + linkValue(call.Room.ID) + "&token=" + linkValue(token)
	call.Notice(fmt.Sprintf("Give this invite to an admin of %s, to run `/remote connect` with there; it signs in as %s, this server's link for that server:\n\n`%s`", server, link.id, invite))
	return nil
}

// joinThreadsLocked joins a link to the threads of a room, at any depth,
// that every member of the room can see: those that are not private.
func (f *federation) joinThreadsLocked(link *userState, r *roomState) {
	for _, thread := range r.children {
		if !thread.private {
			f.s.joinLocked(link, thread)
			f.joinThreadsLocked(link, thread)
		}
	}
}

// threadMadeLocked joins the links of a new thread's parent to it, when
// every member of the parent can see it. ext:remote hook in setRoom.
func (f *federation) threadMadeLocked(r *roomState) {
	if r.parent == nil || r.private {
		return
	}
	for _, link := range f.links {
		if r.parent.members[link.id] != nil {
			f.s.joinLocked(link, r)
		}
	}
}

// linkLeftLocked takes a link's remote users out of a room the link left
// or was removed from, and its threads: they wrote there only through it.
// They may be joined again, by the link's next write or join.
func (f *federation) linkLeftLocked(link *userState, r *roomState) {
	for _, u := range slices.Collect(maps.Values(r.members)) {
		if u.shadow != nil && u.shadow.server == link.link.server {
			f.s.leaveLocked(u, r)
		}
	}
	// And its threads.
	for _, thread := range r.children {
		f.linkLeftLocked(link, thread)
	}
}

// unshare, in a mirror room, makes this server's link on the owner leave
// the owner's room, and stops the mirror: the room stays, and takes no more
// writes. In a room of this server's, it takes every link out of the room.
func (f *federation) unshare(ctx context.Context, call *CommandCall) error {
	f.mu.Lock()
	room := f.rooms[call.Room.ID]
	f.mu.Unlock()
	if room != nil {
		return f.unmirror(ctx, call, room)
	}
	s := f.s
	s.mu.Lock()
	r := s.rooms[call.Room.ID]
	var servers []string
	if r != nil {
		for server, link := range f.links {
			if s.leaveLocked(link, r) {
				servers = append(servers, server)
			}
		}
	}
	s.unlock()
	slices.Sort(servers)
	if len(servers) == 0 {
		call.Notice("This room is not shared.")
	} else {
		call.Notice("This room is no longer shared with " + strings.Join(servers, ", ") + ".")
	}
	return nil
}

// list describes the links, the rooms they are in, and the mirror rooms.
func (f *federation) list() string {
	s := f.s
	var b strings.Builder
	b.WriteString("This server is " + f.self + ".\n\nLinks of mirrors of this server's rooms:\n")
	s.mu.RLock()
	for _, server := range slices.Sorted(maps.Keys(f.links)) {
		link := f.links[server]
		rooms := slices.Sorted(maps.Keys(link.joined))
		fmt.Fprintf(&b, "- %s for %s, in %s\n", link.id, server, strings.Join(rooms, ", "))
	}
	s.mu.RUnlock()
	b.WriteString("\nMirror rooms:\n")
	f.mu.Lock()
	for _, id := range slices.Sorted(maps.Keys(f.rooms)) {
		room := f.rooms[id]
		if room.Parent != "" {
			continue
		}
		state := ""
		if room.Unshared {
			state = " (no longer shared)"
		}
		fmt.Fprintf(&b, "- %s mirrors room %s of %s%s\n", id, room.OwnerRoom, room.Server, state)
	}
	f.mu.Unlock()
	return b.String()
}

// revoke revokes the tokens of the link for a mirror server, which closes
// its connections. Its remote users stay.
func (f *federation) revoke(call *CommandCall, address string) error {
	server, err := normalizeAddress(address)
	if err != nil {
		return InvalidParams("Usage: /remote revoke <address>: " + err.Error())
	}
	s := f.s
	s.mu.Lock()
	link := f.links[server]
	if link != nil {
		s.revokeServicesOfLocked(link)
	}
	s.unlock()
	if link == nil {
		call.Notice("There is no link for " + server + ".")
	} else {
		call.Notice(fmt.Sprintf("Revoked the tokens of %s, the link for %s.", link.id, server))
	}
	return nil
}

// ---- Wake pushes (the owner's side) ----

// linkWake is a link's wake registration: a relay push with the wake scope
// ext:remote, which the owner POSTs to for every record of the link's
// rooms, at most one each wakeEvery.
type linkWake struct {
	URL    string `json:"url"`
	Token  string `json:"token,omitzero"`
	PushID string `json:"push_id,omitzero"`

	last  time.Time
	timer *time.Timer
}

// wakeEvery spaces a link's wake pushes.
const wakeEvery = 2 * time.Second

// registerWake takes a link's push_register with the wake scope ext:remote:
// a relay with a token, to a URL on the link's own server, https or, on a
// loopback host, http.
// ext:remote hook in registerPush; it reports whether it took the request.
func (f *federation) registerWake(c *client, req request) (bool, any, *Error) {
	f.s.mu.RLock()
	link := c.user.link
	userID := c.user.id
	f.s.mu.RUnlock()
	var params struct {
		Kind   string   `json:"kind"`
		URL    string   `json:"url"`
		Token  string   `json:"token"`
		PushID string   `json:"push_id"`
		Wake   []string `json:"wake"`
	}
	if link == nil || json.Unmarshal(encodeJSON(req.params), &params) != nil || !slices.Contains(params.Wake, "ext:remote") {
		return false, nil, nil
	}
	parsed, err := url.Parse(params.URL)
	authority, _ := remoteAuthority(link.server)
	switch {
	case params.Kind != "relay":
		return true, nil, invalidParams("A link's wake push is a relay")
	case params.Token == "":
		return true, nil, invalidParams("A link's wake push has a token")
	case err != nil || strings.ToLower(parsed.Host) != authority:
		return true, nil, invalidParams("A link's wake URL is on its own server, %s", authority)
	case parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopbackHost(parsed.Hostname())):
		return true, nil, invalidParams("A link's wake URL is https, or http on a loopback host")
	case len(params.URL) > maxPushURLBytes || len(params.Token) > maxPushTokenLen:
		return true, nil, &Error{Code: CodeTooLarge, Message: "url or token too long"}
	}
	f.mu.Lock()
	f.wakes[userID] = &linkWake{URL: params.URL, Token: params.Token, PushID: params.PushID}
	f.mu.Unlock()
	if err := f.saveWakes(); err != nil {
		return true, nil, f.h.protocolError(err)
	}
	return true, map[string]any{}, nil
}

func (f *federation) saveWakes() error {
	f.mu.Lock()
	wakes := make(map[string]linkWake, len(f.wakes))
	for id, w := range f.wakes {
		wakes[id] = linkWake{URL: w.URL, Token: w.Token, PushID: w.PushID}
	}
	f.mu.Unlock()
	return f.kv.Put("wakes", wakes)
}

// recordLocked wakes the links of the rooms a record is in. ext:remote
// hook in appendLocked.
func (f *federation) recordLocked(rooms []*roomState) {
	for _, link := range f.links {
		if slices.ContainsFunc(rooms, func(r *roomState) bool { return r.members[link.id] != nil }) {
			f.wake(link.id)
		}
	}
}

// wake sends a link's wake push now, or wakeEvery after the last one.
func (f *federation) wake(linkID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := f.wakes[linkID]
	if w == nil || w.timer != nil {
		return
	}
	w.timer = time.AfterFunc(max(0, time.Until(w.last.Add(wakeEvery))), func() {
		f.mu.Lock()
		w.timer, w.last = nil, time.Now()
		target := *w
		f.mu.Unlock()
		f.h.Go(func(ctx context.Context) { f.postWake(ctx, target) })
	})
}

func (f *federation) postWake(ctx context.Context, w linkWake) {
	payload := map[string]any{}
	if w.PushID != "" {
		payload["push_id"] = w.PushID
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(encodeJSON(payload)))
	if err != nil {
		return
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+w.Token)
	// §4.9.2: a push without message is low.
	request.Header.Set("TTL", pushTTL)
	request.Header.Set("Urgency", "low")
	response, err := f.client.Do(request)
	if err != nil {
		f.h.logger.Debug("a wake push failed", "url", w.URL, "error", err)
		return
	}
	response.Body.Close()
}

// randomID is an opaque random identifier.
func randomID() string { return strings.ToLower(rand.Text()) }
