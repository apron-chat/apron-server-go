package apron

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"strings"
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
// was removed (Host.Member) writes there no more until it is added again,
// shows as offline, cannot sign in from outside the process (it has no
// credential, and Bot.IssueToken refuses it), and its owner may make it
// at any time, not only during Init.
//
// The server sets the shadow's ext.remote to {"server": server, "user_id":
// remoteUserID} (ext:remote), on its user object and on the `from` of its
// records. A message a shadow's session creates gets ext.remote {"server",
// "user_id"} too, and a session may add the remote message's "room_id" and
// "message_id" (strings) by writing ext.remote, whose "server" and
// "user_id" the server sets. Clients, the extension's bot, and the
// shadow's own profile writes cannot change ext.remote (see the reserved
// key in SERVER.md).
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
// extension's sessions (Bot.Connect) to messages and rooms, so that no
// client can claim that it comes from another server. A shadow's session
// writes it with "server", and for a message "user_id", set to the
// shadow's, keeping only the string "room_id" and "message_id" it writes,
// and a message it creates gets the shadow's even when it writes none; an
// empty value, which would clear the key, is dropped from its writes. An
// extension's bot writes the key as it is. A write that is not an object
// is invalid_params.
func (s *Server) remoteExtWrite(c *client, ext extObject, target remoteTarget, creating bool) (extObject, *Error) {
	raw, has := ext[remoteExt]
	_, session := c.conn.(sessionTransport)
	if target == remoteOnUser || !session {
		if has {
			ext = maps.Clone(ext)
			delete(ext, remoteExt)
		}
		return ext, nil
	}
	s.mu.RLock()
	shadow := c.user.shadow
	s.mu.RUnlock()
	if shadow == nil {
		return ext, nil
	}
	var written map[string]jsontext.Value
	switch {
	case has && emptyJSON(raw):
		ext = maps.Clone(ext)
		delete(ext, remoteExt)
		if !creating || target != remoteOnMessage {
			return ext, nil
		}
	case has:
		if json.Unmarshal(raw, &written) != nil || written == nil {
			return nil, invalidParams("ext.remote must be an object")
		}
	case !creating || target != remoteOnMessage:
		return ext, nil
	}
	value := map[string]any{"server": shadow.server}
	fields := []string{"room_id"}
	if target == remoteOnMessage {
		value["user_id"] = shadow.remoteID
		fields = append(fields, "message_id")
	}
	for _, field := range fields {
		var id string
		if json.Unmarshal(written[field], &id) == nil && id != "" {
			value[field] = id
		}
	}
	ext = maps.Clone(ext)
	if ext == nil {
		ext = make(extObject)
	}
	ext[remoteExt] = encodeJSON(value)
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
