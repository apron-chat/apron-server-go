package apron

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"
)

// remoteExt is the ext key ext:remote reserves: where a user, a message,
// or a room comes from, when an extension copies another server's rooms.
// ext:remote is a draft extension in apron's
// recipes/trusted_federation.md. The server owns it: clients never write it.
const remoteExt = "remote"

// shadowState marks a shadow user (Host.Shadow): the extension that owns
// it, and the user of a remote server it stands for.
type shadowState struct {
	owner    string
	server   string
	remoteID string
}

// RemoteUser is the user of another server that a shadow user stands for
// (Host.Shadow).
type RemoteUser struct {
	// Server is the remote server's address (PROTOCOL.md §1), and UserID
	// the user's user_id there.
	Server, UserID string
}

// maxShadowIDBytes bounds a shadow's user_id.
const maxShadowIDBytes = 256

// RemoteAuthority is the authority of a server's address (PROTOCOL.md §1),
// as shadow users' IDs carry it: its host, lowercased, and its port if the
// address has one, such as "b.example" for wss://b.example/ws and
// "127.0.0.1:8787" for ws://127.0.0.1:8787/ws.
func RemoteAuthority(server string) (string, error) {
	parsed, err := url.Parse(server)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" {
		return "", fmt.Errorf("apron: %q is no server address, such as wss://chat.example/ws", server)
	}
	authority := strings.ToLower(parsed.Host)
	if !validID(authority) || strings.ContainsAny(authority, "@/") {
		return "", fmt.Errorf("apron: %q is no server address, such as wss://chat.example/ws", server)
	}
	return authority, nil
}

// ShadowID is the user_id of the shadow user (Host.Shadow) of user
// remoteUserID of the server at address server: `<remoteUserID>@<authority>`
// (RemoteAuthority), such as carol@b.example. remoteUserID is any user_id
// (PROTOCOL.md §2.4) but a system identity's (~).
func ShadowID(server, remoteUserID string) (string, error) {
	authority, err := RemoteAuthority(server)
	if err != nil {
		return "", err
	}
	if !validID(remoteUserID) || strings.HasPrefix(remoteUserID, "~") {
		return "", fmt.Errorf("apron: %q is no user_id a shadow can stand for", remoteUserID)
	}
	id := remoteUserID + "@" + authority
	if len(id) > maxShadowIDBytes {
		return "", fmt.Errorf("apron: shadow user_id %q is over %d bytes", id, maxShadowIDBytes)
	}
	return id, nil
}

// Shadow makes, or returns, the extension's shadow user of user
// remoteUserID of the remote server at address server, such as a user of
// another Apron server whose rooms the extension copies into this one
// (trusted federation). Its user_id is ShadowID's, `<remoteUserID>@<authority>`,
// which no person or bot can request, since requested user_ids have no
// "@" (§3.3), and which is the shadow's forever: Shadow with the same
// arguments, from any start of the server, returns the same account.
//
// A shadow is an account, kept across restarts, that the extension speaks
// through with the returned Bot, as with its bot (Registrar.Bot): its
// sessions (Bot.Connect, Bot.Call, Bot.Post, ...) send ordinary requests as
// the shadow, with the same checks and limits as a client's, so messages,
// edits, deletions, reactions, and joins are the shadow's own, with it as
// `from`. Unlike a bot it holds no roles, joins no room by itself (join it
// with room_join, or have a member add it with room_join and its user_id),
// writes messages and reactions only in rooms it is in, so one that left or
// was removed writes there no more until it is added again,
// shows as offline, cannot sign in from outside the process (it has no
// credential, and Bot.IssueToken refuses it), and its owner may make it
// at any time, not only during Init.
//
// The server sets the shadow's ext.remote to {"server": server, "user_id":
// remoteUserID} (ext:remote), on its user object and on the `from` of its
// records, and on the messages its sessions create, whatever they write.
// Clients and the shadow's own writes cannot change ext.remote (see the
// reserved key in SERVER.md).
//
// Shadow fails for a user_id that someone else has, or ever had, such as
// another extension's shadow, or the same remote user_id in another case.
// The first address given is the one kept: a later call with another
// address of the same authority returns the same shadow.
func (h *Host) Shadow(server, remoteUserID string) (*Bot, error) {
	id, err := ShadowID(server, remoteUserID)
	if err != nil {
		return nil, err
	}
	s := h.s
	s.mu.Lock()
	u := s.users[id]
	switch {
	case s.closed:
		s.unlock()
		return nil, ErrServerClosed
	case u != nil && u.shadow != nil && u.shadow.owner == h.name:
	case u != nil && u.shadow != nil:
		s.unlock()
		return nil, fmt.Errorf("apron: user_id %q is extension %s's shadow", id, u.shadow.owner)
	case u != nil:
		s.unlock()
		return nil, fmt.Errorf("apron: user_id %q belongs to someone who is not a shadow", id)
	default:
		key := strings.ToLower(id)
		if s.usedIDs[key] || s.roomNamedLocked(id) || s.grantedIDs[key] {
			s.unlock()
			return nil, fmt.Errorf("apron: user_id %q was used by someone else", id)
		}
		s.usedIDs[key] = true
		s.touchUsedID(key)
		u = newUserState(id, "")
		u.shadow = &shadowState{owner: h.name, server: server, remoteID: remoteUserID}
		u.ext = extObject{remoteExt: encodeJSON(u.shadow.user())}
		s.users[id] = u
		s.touchUser(id)
	}
	s.unlock()
	return &Bot{s: s, host: h, userID: id}, nil
}

// user is the shadow's ext.remote on its user object and its records'
// `from`.
func (sh *shadowState) user() map[string]any {
	return map[string]any{"server": sh.server, "user_id": sh.remoteID}
}

// remoteTarget is what a write of ext.remote is to.
type remoteTarget int

const (
	remoteOnUser remoteTarget = iota
	remoteOnMessage
	remoteOnRoom
)

// remoteExtWrite applies the reserved key ext.remote to what a request
// writes to ext: it is dropped from every write but those of an
// extension's bot (Bot.Connect), which writes it as it is, so that no
// client can claim that it comes from another server. A message a shadow
// user's session creates gets the shadow's {"server", "user_id"}, whatever
// it writes, and its other writes keep what is stored. A bot's write that
// is not an object is invalid_params.
func (s *Server) remoteExtWrite(c *client, ext extObject, target remoteTarget, creating bool) (extObject, *Error) {
	raw, has := ext[remoteExt]
	_, session := c.conn.(sessionTransport)
	var shadow *shadowState
	if session {
		s.mu.RLock()
		shadow = c.user.shadow
		s.mu.RUnlock()
	}
	switch {
	case session && shadow == nil && target != remoteOnUser:
		if has && !emptyJSON(raw) {
			var written map[string]jsontext.Value
			if json.Unmarshal(raw, &written) != nil || written == nil {
				return nil, invalidParams("ext.remote must be an object")
			}
		}
		return ext, nil
	case has:
		ext = maps.Clone(ext)
		delete(ext, remoteExt)
	}
	if shadow != nil && target == remoteOnMessage && creating {
		if ext == nil {
			ext = make(extObject)
		}
		ext[remoteExt] = encodeJSON(shadow.user())
	}
	return ext, nil
}

// remoteOf is the remote user a user stands for, nil for a local one.
func remoteOf(u *userState) *RemoteUser {
	if u == nil || u.shadow == nil {
		return nil
	}
	return &RemoteUser{Server: u.shadow.server, UserID: u.shadow.remoteID}
}

// errShadowToken refuses a service token for a shadow.
var errShadowToken = errors.New("apron: a shadow user signs in only through its extension's sessions")

// storedShadow is a shadow's shadowState in the store.
type storedShadow struct {
	Owner  string `json:"owner"`
	Server string `json:"server"`
	User   string `json:"user"`
}

func (sh *shadowState) stored() *storedShadow {
	if sh == nil {
		return nil
	}
	return &storedShadow{Owner: sh.owner, Server: sh.server, User: sh.remoteID}
}

func (stored *storedShadow) restore() *shadowState {
	if stored == nil {
		return nil
	}
	return &shadowState{owner: stored.Owner, server: stored.Server, remoteID: stored.User}
}

// ---- Links (ext:remote) ----

// linkState marks a link: the account a remote server's sender signs in
// as, bound to that server's address, to write as its users here.
type linkState struct {
	server string
}

type storedLink struct {
	Server string `json:"server"`
}

func (l *linkState) stored() *storedLink {
	if l == nil {
		return nil
	}
	return &storedLink{Server: l.server}
}

func (stored *storedLink) restore() *linkState {
	if stored == nil {
		return nil
	}
	return &linkState{server: stored.Server}
}

// NormalizeAddress normalizes a server's address (PROTOCOL.md §1) as
// links compare them: a ws: or wss: URL, with its scheme and host
// lowercased, and its port and path as written.
func NormalizeAddress(address string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(address))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("%q is no server address, such as wss://chat.example/ws", address)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "ws" && scheme != "wss" {
		return "", fmt.Errorf("%q is no ws: or wss: address", address)
	}
	return scheme + "://" + strings.ToLower(parsed.Host) + parsed.EscapedPath(), nil
}

// linkLocked returns the link bound to a normalized server address, made
// if there is none: a token-only account with role bot, whose ext.remote
// names the server, with a user_id made from the server's authority.
func (s *Server) linkLocked(server string) (*userState, error) {
	if u := s.links[server]; u != nil {
		return u, nil
	}
	authority, err := RemoteAuthority(server)
	if err != nil {
		return nil, err
	}
	base := "link-" + strings.NewReplacer(":", "-", "[", "", "]", "").Replace(authority)
	if len(base) > 56 || !requestableUserID.MatchString(base) {
		base = "link"
	}
	id := base
	for n := 2; !s.claimUserIDLocked(id); n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	u := newUserState(id, authority)
	u.link = &linkState{server: server}
	u.ext = extObject{remoteExt: encodeJSON(map[string]any{"server": server})}
	s.users[id] = u
	s.touchUser(id)
	s.grantRolesLocked(u)
	s.links[server] = u
	return u, nil
}

// linkTokenLifetime is how long a token minted for a link signs in.
const linkTokenLifetime = 10 * 365 * 24 * time.Hour

// mintLinkToken issues a service token for a link, revoking its oldest
// token first when it holds as many as a bot may.
func (s *Server) mintLinkToken(ctx context.Context, link *userState, name string) (string, error) {
	s.mu.Lock()
	var held []*serviceToken
	for _, t := range s.services {
		if t.user == link {
			held = append(held, t)
		}
	}
	slices.SortFunc(held, func(a, b *serviceToken) int { return a.created.Compare(b.created) })
	for len(held) >= maxServiceTokens {
		for key, t := range s.services {
			if t == held[0] {
				s.revokeServiceLocked(key, "This link's token was replaced")
			}
		}
		held = held[1:]
	}
	s.unlock()
	token, err := s.issueServiceToken(ctx, link.id, ServiceTokenOptions{Name: name, Lifetime: linkTokenLifetime}, "", 0)
	return token.Token, err
}

// actAsMethods are the requests a link sends as a remote user.
var actAsMethods = map[string]bool{"message": true, "reactions": true, "me": true, "room_join": true, "room_leave": true, "room_set": true}

// maxRemoteUserIDBytes bounds the remote user_id a link names.
const maxRemoteUserIDBytes = 128

// remoteActor reads params.ext.remote.user_id, and whether it is there.
func remoteActor(params map[string]jsontext.Value) (string, bool) {
	var ext struct {
		Remote *struct {
			UserID *jsontext.Value `json:"user_id"`
		} `json:"remote"`
	}
	raw, ok := params["ext"]
	if !ok || json.Unmarshal(raw, &ext) != nil || ext.Remote == nil || ext.Remote.UserID == nil {
		return "", false
	}
	var id string
	if json.Unmarshal(*ext.Remote.UserID, &id) != nil {
		return "\x00", true // not a string: refused below
	}
	return id, true
}

// actAs runs a link's request as the remote user it names (ext:remote):
// the link must have joined the request's rooms; the remote user is made
// if there is none, and joined to the room it writes in, unless another
// user removed it from there. The request then runs on the remote user's
// own session, with the same checks as any of its requests, and a thread
// it creates is not private unless it says so.
func (s *Server) actAs(c *client, req request, remoteID string) (any, bool, *Error) {
	if remoteID == "" || len(remoteID) > maxRemoteUserIDBytes || !validID(remoteID) || strings.HasPrefix(remoteID, "~") {
		return nil, false, invalidParams("ext.remote.user_id must be a user_id of at most %d bytes, not a system identity's", maxRemoteUserIDBytes)
	}
	var target struct {
		RoomID       *string `json:"room_id"`
		ParentRoomID *string `json:"parent_room_id"`
		MessageID    *string `json:"message_id"`
		Deleted      bool    `json:"deleted"`
	}
	if err := json.Unmarshal(encodeJSON(req.params), &target); err != nil {
		return nil, false, invalidParams("Invalid params: %v", err)
	}
	s.mu.Lock()
	link := c.user
	if link == nil || link.link == nil {
		s.unlock()
		return nil, false, &Error{Code: CodeDenied, Message: "Only a link acts as remote users"}
	}
	id, idErr := ShadowID(link.link.server, remoteID)
	if idErr != nil {
		s.unlock()
		return nil, false, invalidParams("%v", idErr)
	}
	// The rooms the request is in: the link must have joined each, and the
	// remote user joins the ones it writes in.
	var rooms []string
	write := true
	switch req.method {
	case "message":
		roomID := defaultRoomID
		if target.RoomID != nil {
			roomID = *target.RoomID
		}
		rooms = append(rooms, roomID)
		if target.MessageID != nil {
			m := s.messages[*target.MessageID]
			if m == nil {
				s.unlock()
				return nil, false, invalidParams("Unknown message %q", *target.MessageID)
			}
			if target.Deleted {
				rooms = []string{m.roomID}
			} else if m.roomID != roomID {
				rooms = append(rooms, m.roomID)
			}
		}
	case "reactions":
		var messageID string
		if raw, ok := req.params["message_id"]; ok {
			_ = json.Unmarshal(raw, &messageID)
		}
		m := s.messages[messageID]
		if m == nil {
			s.unlock()
			return nil, false, invalidParams("Unknown message %q", messageID)
		}
		rooms = append(rooms, m.roomID)
	case "room_leave", "room_join":
		if target.RoomID == nil {
			s.unlock()
			return nil, false, invalidParams("Missing room_id")
		}
		if _, other := req.params["user_id"]; other {
			s.unlock()
			return nil, false, invalidParams("A link joins or removes only its remote users, as themselves")
		}
		// A join is a write: it joins the remote user below, or is refused
		// for one that another user removed.
		rooms, write = append(rooms, *target.RoomID), req.method == "room_join"
	case "me":
		write = false
	case "room_set":
		// A link sets only threads of rooms it joined: it creates one under
		// such a room, or saves one.
		switch {
		case target.RoomID != nil:
			if r := s.rooms[*target.RoomID]; r != nil && r.parent == nil {
				s.unlock()
				return nil, false, invalidParams("A link saves only threads")
			}
			rooms = append(rooms, *target.RoomID)
		case target.ParentRoomID != nil:
			rooms = append(rooms, *target.ParentRoomID)
		default:
			s.unlock()
			return nil, false, invalidParams("A link creates only threads: give parent_room_id")
		}
	}
	for _, roomID := range rooms {
		r := s.rooms[roomID]
		joined := linkRoomLocked(link, r)
		switch {
		case r == nil || joined == nil && !r.visibleTo(link):
			s.unlock()
			return nil, false, notJoined(invalidParams("Unknown room %q", roomID))
		case joined == nil:
			s.unlock()
			return nil, false, notJoined(&Error{Code: CodeDenied, Message: "This link is not in that room, so its remote users may not write there"})
		}
	}
	shadow, err := s.remoteUserLocked(link.link.server, remoteID)
	if err != nil {
		s.unlock()
		return nil, false, &Error{Code: CodeDenied, Message: err.Error()}
	}
	if write {
		for _, roomID := range rooms {
			// The remote user joins the room, and the threads on the way
			// down from the room the link joined, so it can see it.
			r := s.rooms[roomID]
			var chain []*roomState
			for room := r; room != nil; room = room.parent {
				chain = append(chain, room)
				if room.members[link.id] != nil {
					break
				}
			}
			for i := len(chain) - 1; i >= 0; i-- {
				room := chain[i]
				if room.members[id] != nil || room != r && !room.private {
					continue
				}
				if shadow.removed[room.id] {
					s.unlock()
					return nil, false, &Error{Code: CodeDenied, Message: fmt.Sprintf("%s was removed from %s, so its link may not write there as it", id, room.title()),
						Data: map[string]any{"ext": map[string]any{remoteExt: "removed"}}}
				}
				s.joinLocked(shadow, room)
			}
		}
	}
	s.unlock()
	if req.method == "room_join" {
		return map[string]any{}, false, nil // joined above
	}

	params := req.params
	if req.method == "room_set" && target.ParentRoomID != nil && target.RoomID == nil {
		// A thread of a private room is private unless it says otherwise,
		// which here would show it only to the remote user: the copy of a
		// thread is seen by whoever sees its parent.
		if _, given := params["private"]; !given {
			params = maps.Clone(params)
			params["private"] = jsontext.Value("false")
		}
	}
	bot := s.remoteBot(id)
	raw, callErr := bot.Call(s.requestContext(c), req.method, params)
	if callErr != nil {
		if e, ok := errors.AsType[*Error](callErr); ok {
			return nil, false, e
		}
		return nil, false, s.remoteHost.protocolError(callErr)
	}
	return raw, false, nil
}

// remoteUserLocked returns the remote user (ext:remote) that stands for
// user remoteID of the server at address server, made if there is none: a
// shadow user of the server's own, whose ext.remote names its server and
// user_id. Links write as these, and mirrors copy their records as them.
func (s *Server) remoteUserLocked(server, remoteID string) (*userState, error) {
	id, err := ShadowID(server, remoteID)
	if err != nil {
		return nil, err
	}
	u := s.users[id]
	switch {
	case u == nil:
		key := strings.ToLower(id)
		if s.usedIDs[key] || s.roomNamedLocked(id) || s.grantedIDs[key] {
			return nil, fmt.Errorf("the user_id %s was used by someone else", id)
		}
		s.usedIDs[key] = true
		s.touchUsedID(key)
		u = newUserState(id, "")
		u.shadow = &shadowState{owner: remoteExt, server: server, remoteID: remoteID}
		u.ext = extObject{remoteExt: encodeJSON(u.shadow.user())}
		s.users[id] = u
		s.touchUser(id)
	case u.shadow == nil || u.shadow.owner != remoteExt:
		return nil, fmt.Errorf("the user_id %s is not a remote user's", id)
	}
	return u, nil
}

// linkRoomLocked is the room a link joined that r is, or is a thread of,
// in which the link acts (ext:remote), or nil.
func linkRoomLocked(link *userState, r *roomState) *roomState {
	for room := r; room != nil; room = room.parent {
		if room.members[link.id] != nil {
			return room
		}
	}
	return nil
}

// notJoined marks a link's error for a room it has not joined, whatever
// its code, so that its sender can tell that the pair ended from any other
// refusal (ext:remote).
func notJoined(e *Error) *Error {
	e.Data = map[string]any{"ext": map[string]any{remoteExt: "not_joined"}}
	return e
}

// remoteBot is the handle of a link's remote user, whose session runs its
// requests.
func (s *Server) remoteBot(userID string) *Bot {
	if b, ok := s.remoteBots.Load(userID); ok {
		return b.(*Bot)
	}
	b, _ := s.remoteBots.LoadOrStore(userID, &Bot{s: s, host: s.remoteHost, userID: userID})
	return b.(*Bot)
}
