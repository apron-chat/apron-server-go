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
// or a room comes from, in trusted federation (federation.go). ext:remote
// is a draft extension in apron's recipes/trusted_federation.md. The server
// owns it: clients never write it.
const remoteExt = "remote"

// shadowState marks a remote user (ext:remote): the user of a remote
// server that it stands for, which links write as and mirrors copy records
// as.
type shadowState struct {
	server   string
	remoteID string
}

// maxShadowIDBytes bounds a remote user's user_id.
const maxShadowIDBytes = 256

// remoteAuthority is the authority of a server's address (PROTOCOL.md §1),
// as remote users' IDs carry it: its host, lowercased, and its port if the
// address has one, such as "b.example" for wss://b.example/ws and
// "127.0.0.1:8787" for ws://127.0.0.1:8787/ws.
func remoteAuthority(server string) (string, error) {
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

// shadowID is the user_id of the remote user that stands for user
// remoteUserID of the server at address server: `<remoteUserID>@<authority>`
// (remoteAuthority), such as carol@b.example. remoteUserID is any user_id
// (PROTOCOL.md §2.4) but a system identity's (~).
func shadowID(server, remoteUserID string) (string, error) {
	authority, err := remoteAuthority(server)
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

// user is the remote user's ext.remote on its user object and its records'
// `from`.
func (sh *shadowState) user() map[string]any {
	return map[string]any{"server": sh.server, "user_id": sh.remoteID}
}

// remoteExtWrite drops the reserved key ext.remote from what a request
// writes to ext, so that no client can claim that it comes from another
// server; a save keeps what is stored. A message that a remote user
// creates gets the remote user's {"server", "user_id"}.
func (s *Server) remoteExtWrite(c *client, ext extObject, creating bool) extObject {
	if _, has := ext[remoteExt]; has {
		ext = maps.Clone(ext)
		delete(ext, remoteExt)
	}
	if !creating {
		return ext
	}
	s.mu.RLock()
	shadow := c.user.shadow
	s.mu.RUnlock()
	if shadow != nil {
		if ext == nil {
			ext = make(extObject)
		}
		ext[remoteExt] = encodeJSON(shadow.user())
	}
	return ext
}

// storedShadow is a remote user's shadowState in the store.
type storedShadow struct {
	Server string `json:"server"`
	User   string `json:"user"`
}

func (sh *shadowState) stored() *storedShadow {
	if sh == nil {
		return nil
	}
	return &storedShadow{Server: sh.server, User: sh.remoteID}
}

func (stored *storedShadow) restore() *shadowState {
	if stored == nil {
		return nil
	}
	return &shadowState{server: stored.Server, remoteID: stored.User}
}

// ---- Links (ext:remote) ----

// linkState marks a link: the account a mirror server signs in as, bound
// to that server's address, to read the rooms shared with it and write as
// its users here.
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

// normalizeAddress normalizes a server's address (PROTOCOL.md §1) as
// links compare them: a ws: or wss: URL, with its scheme and host
// lowercased, and its port and path as written.
func normalizeAddress(address string) (string, error) {
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
func (f *federation) linkLocked(server string) (*userState, error) {
	s := f.s
	if u := f.links[server]; u != nil {
		return u, nil
	}
	authority, err := remoteAuthority(server)
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
	f.links[server] = u
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
func (f *federation) actAs(c *client, req request, remoteID string) (any, bool, *Error) {
	s := f.s
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
	id, idErr := shadowID(link.link.server, remoteID)
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
	if req.method == "room_set" {
		// A link's private is ignored: a thread that a remote user creates
		// is seen by whoever sees its parent, as its mirror thread is, and
		// not private as a thread of a private room would be by default.
		params = maps.Clone(params)
		delete(params, "private")
		if target.RoomID == nil {
			params["private"] = jsontext.Value("false")
		}
	}
	bot := f.remoteBot(id)
	raw, callErr := bot.Call(s.requestContext(c), req.method, params)
	if callErr != nil {
		if e, ok := errors.AsType[*Error](callErr); ok {
			return nil, false, e
		}
		return nil, false, f.h.protocolError(callErr)
	}
	return raw, false, nil
}

// remoteUserLocked returns the remote user (ext:remote) that stands for
// user remoteID of the server at address server, made if there is none:
// an account whose ext.remote names its server and user_id. Links write as
// these, and mirrors copy their records as them.
func (s *Server) remoteUserLocked(server, remoteID string) (*userState, error) {
	id, err := shadowID(server, remoteID)
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
		u.shadow = &shadowState{server: server, remoteID: remoteID}
		u.ext = extObject{remoteExt: encodeJSON(u.shadow.user())}
		s.users[id] = u
		s.touchUser(id)
	case u.shadow == nil:
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
// its code, so that the mirror can tell that the room is no longer shared
// from any other refusal (ext:remote).
func notJoined(e *Error) *Error {
	e.Data = map[string]any{"ext": map[string]any{remoteExt: "not_joined"}}
	return e
}

// remoteBot is the handle of a link's remote user, whose session runs its
// requests.
func (f *federation) remoteBot(userID string) *Bot {
	if b, ok := f.bots.Load(userID); ok {
		return b.(*Bot)
	}
	b, _ := f.bots.LoadOrStore(userID, &Bot{s: f.s, host: f.h, userID: userID})
	return b.(*Bot)
}
