package wire

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"slices"
)

// CurrentUser is a current user object (§3.3.1), as a user notification,
// or a me result, carries it: User, and the fields it clears. Each field
// Cleared names is sent as its empty value ("" for a string, [] for roles,
// {} for ext), in place of User's, so that a client that merges the object
// into the one it keeps clears it; a decoded CurrentUser lists in Cleared
// the fields of User it carried empty. A complete object, an auth or me
// result's you, also names in Cleared the fields the server publishes that
// User has empty (see AuthResult).
//
// A cleared ext key is not a cleared field: it is a key of Ext with an
// empty value (§4.13), as Patch makes it. An empty ext clears nothing.
type CurrentUser struct {
	User
	// Cleared names fields of User, by their JSON names, such as "avatar"
	// and "roles".
	Cleared []string
}

// userFields are the JSON names of User's fields that a current object may
// clear, in User's order, with their empty values.
var userFields = []struct{ name, empty string }{
	{"name", `""`}, {"avatar", `""`}, {"roles", `[]`}, {"status", `""`}, {"seen", `""`}, {"ext", `{}`},
}

// MarshalJSONTo writes u as one object: User's fields, then each cleared
// field as its empty value.
func (u CurrentUser) MarshalJSONTo(enc *jsontext.Encoder) error {
	if len(u.Cleared) == 0 {
		return json.MarshalEncode(enc, u.User)
	}
	user := u.User
	for _, name := range u.Cleared {
		switch name {
		case "user_id":
			return errors.New("wire: a current user object cannot clear user_id")
		case "name":
			user.Name = ""
		case "avatar":
			user.Avatar = ""
		case "roles":
			user.Roles = nil
		case "status":
			user.Status = ""
		case "seen":
			user.Seen = ""
		case "ext":
			user.Ext = nil
		}
	}
	raw, err := json.Marshal(user, enc.Options())
	if err != nil {
		return err
	}
	// raw is an object with user_id, so never {}.
	raw = raw[:len(raw)-1]
	for _, name := range u.Cleared {
		empty := `""`
		if i := slices.IndexFunc(userFields, func(f struct{ name, empty string }) bool { return f.name == name }); i >= 0 {
			empty = userFields[i].empty
		}
		raw = append(raw, ',')
		raw, err = jsontext.AppendQuote(raw, name)
		if err != nil {
			return err
		}
		raw = append(raw, ':')
		raw = append(raw, empty...)
	}
	return enc.WriteValue(append(raw, '}'))
}

// UnmarshalJSONFrom reads a current user object, noting in Cleared each of
// User's fields it carries as an empty value.
func (u *CurrentUser) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	raw, err := dec.ReadValue()
	if err != nil {
		return err
	}
	var user User
	if err := json.Unmarshal(raw, &user, dec.Options()); err != nil {
		return err
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	*u = CurrentUser{User: user}
	for _, field := range userFields {
		if value, ok := fields[field.name]; ok {
			if value = slices.Clone(value); value.Compact() == nil && emptyJSON(value) {
				u.Cleared = append(u.Cleared, field.name)
			}
		}
	}
	return nil
}

// Patch returns the current object that brings a client that keeps old up
// to date with u (§3.3.1): user_id and the fields that changed, a cleared
// one as its empty value, such as roles taken away as "roles": [], and in
// ext each key that changed, a removed one as its empty value (§4.13).
// Unknown is carried whole when it changed. It reports false when nothing
// did.
//
// When old is another user, or none (a zero User), the patch is all of u:
// a client keeps nothing of the user yet.
func (u User) Patch(old User) (CurrentUser, bool) {
	if old.UserID != u.UserID {
		return CurrentUser{User: u.clone()}, true
	}
	patch := CurrentUser{User: User{UserID: u.UserID}}
	changed := false
	for _, f := range []struct {
		name     string
		cur, old string
		to       *string
	}{
		{"name", u.Name, old.Name, &patch.Name},
		{"avatar", u.Avatar, old.Avatar, &patch.Avatar},
		{"status", u.Status, old.Status, &patch.Status},
		{"seen", u.Seen, old.Seen, &patch.Seen},
	} {
		switch {
		case f.cur == f.old:
		case f.cur == "":
			patch.Cleared, changed = append(patch.Cleared, f.name), true
		default:
			*f.to, changed = f.cur, true
		}
	}
	switch {
	case slices.Equal(u.Roles, old.Roles):
	case len(u.Roles) == 0:
		patch.Cleared, changed = append(patch.Cleared, "roles"), true
	default:
		patch.Roles, changed = slices.Clone(u.Roles), true
	}
	setExt := func(key string, value jsontext.Value) {
		if patch.Ext == nil {
			patch.Ext = make(Ext)
		}
		patch.Ext[key], changed = cloneRaw(value), true
	}
	for key, value := range u.Ext {
		if kept, ok := old.Ext[key]; !ok || !bytes.Equal(kept, value) {
			setExt(key, value)
		}
	}
	for key, kept := range old.Ext {
		if _, ok := u.Ext[key]; !ok {
			setExt(key, emptyOf(kept))
		}
	}
	if !bytes.Equal(u.Unknown, old.Unknown) {
		patch.Unknown, changed = cloneRaw(u.Unknown), true
	}
	// Cleared in User's order, whatever the order above.
	slices.SortFunc(patch.Cleared, func(a, b string) int { return fieldIndex(a) - fieldIndex(b) })
	return patch, changed
}

// Complete returns u as a complete current object (§3.3.1) for a client
// that keeps old, as a me result carries it (§3.3.2): all of u, with
// Cleared naming each field old had and u has not, which it carries as its
// empty value, as u.Patch(old) does. A field the server publishes that
// neither has, such as a status of "" with capability status (§4.5.2), a
// complete object carries too: the caller adds it to Cleared. For a
// client that keeps nothing of the user, as one signing in, old is the
// zero User (see AuthResult).
func (u User) Complete(old User) CurrentUser {
	patch, _ := u.Patch(old)
	return CurrentUser{User: u, Cleared: patch.Cleared}
}

// ApplyMe returns old with what a me request changes (§3.3.2), merged as
// the protocol merges it: a name or avatar the request carries replaces
// old's, and "" clears it; its ext merges into old's (MergeExt). Status is
// left as it was: the chosen status is capability status's (§4.5.1),
// which others see as the shown status, so a backend with it applies it
// itself, and publishes the two views with PublishUserChange. The backend
// checks the result against its own policy, such as a name's length, or
// normalizes it, before it makes it the user's. The result shares nothing
// with old.
func ApplyMe(old User, p MeParams) User {
	u := old.clone()
	if p.Name != nil {
		u.Name = *p.Name
	}
	if p.Avatar != nil {
		u.Avatar = *p.Avatar
	}
	if p.Ext != nil {
		u.Ext = MergeExt(old.Ext, p.Ext).clone()
	}
	return u
}

// fieldIndex is the place of a field among userFields.
func fieldIndex(name string) int {
	return slices.IndexFunc(userFields, func(f struct{ name, empty string }) bool { return f.name == name })
}

// emptyOf is the empty value of a kept ext value's kind, which clears it:
// [] for an array, {} for an object, and "" for anything else.
func emptyOf(kept jsontext.Value) jsontext.Value {
	switch kept.Kind() {
	case '[':
		return jsontext.Value(`[]`)
	case '{':
		return jsontext.Value(`{}`)
	}
	return jsontext.Value(`""`)
}

// UserUpdate is a user notification (§3.3.3): You to the user's own
// connections, New to others who share a room with the user, and Old with
// New when the account's user_id changed.
type UserUpdate struct {
	You *CurrentUser `json:"you,omitzero"`
	New *CurrentUser `json:"new,omitzero"`
	Old *CurrentUser `json:"old,omitzero"`
}

// Method makes a UserUpdate a Notification.
func (UserUpdate) Method() string { return "user" }

// PublishUser announces a change of a user from old to cur (§3.3.3), and
// reports whether there was one: whether cur.Patch(old) changed anything.
// When it did, PublishUser makes cur the identity of the user's own
// connections (Conn.SetUser), and sends them {"you": patch}, and the other
// signed-in connections for which shares returns true, or every one when
// shares is nil, {"new": patch}, where patch is cur.Patch(old). The user's
// own connections are those signed in as cur's user_id, or old's: when the
// two differ, as when a guest becomes an account, others are sent old too.
//
// When nothing changed, PublishUser does nothing at all: it neither walks
// the connections nor calls SetUser, so a backend may call it on every
// change it might have made, and learn from the result whether it did.
// Patch covers every field of User, so nothing changed means cur is old;
// old is what the backend last made the user's connections' identity and
// told their clients, and a connection whose identity is something else,
// such as one the backend set itself with Conn.SetUser, is the backend's
// to bring up to date.
//
// A backend tells others only of users who share a room with them
// (§3.3.3): shares says who does. A backend whose user's own connections
// see another object than others do, such as the chosen status rather than
// the shown one (§4.5.2), publishes with PublishUserChange.
//
// Like Broadcast, it never blocks, and is meant to be called while the
// backend holds the lock of the change, so that every connection gets a
// user's changes in the order they were made; shares is called in the
// caller's goroutine, so it may read what that lock guards.
func (s *Server) PublishUser(old, cur User, shares func(*Conn) bool) bool {
	return s.PublishUserChange(UserChange{Old: old, New: cur}, shares)
}

// SendMe answers a me request that changed the user from old to cur, as
// ProfileHandler says: it tells the change to the user's other
// connections, and to the other signed-in connections for which shares
// returns true, or every one when shares is nil (PublishUserChange, with
// the request's connection its Except), then sends the result, all of cur
// with the fields the change cleared (cur.Complete(old)), which makes cur
// the connection's user, and returns what Send returns. A me that changed
// nothing is answered with cur, and tells no one.
//
// It is for a backend whose user is one object to everyone; one whose own
// connections see another than others do, such as the chosen status and
// the shown one (§4.5.2), publishes with PublishUserChange and sends
// cur.Complete(old) itself. Like PublishUser, it never blocks, and is
// meant to be called while the backend holds the lock of the change;
// shares is called in the caller's goroutine.
//
//	func (b *Backend) Me(ctx context.Context, c *wire.Conn, p wire.MeParams, reply wire.Reply[wire.CurrentUser]) error {
//		b.mu.Lock()
//		defer b.mu.Unlock()
//		old := *c.User()
//		cur := wire.ApplyMe(old, p)
//		if len(cur.Name) > 64 {
//			return wire.InvalidParams("A name is at most 64 bytes")
//		}
//		// Everyone shares the one room: shares is nil.
//		return b.server.SendMe(reply, old, cur, nil)
//	}
func (s *Server) SendMe(reply Reply[CurrentUser], old, cur User, shares func(*Conn) bool) error {
	s.PublishUserChange(UserChange{Old: old, New: cur, Except: reply.Request().Conn()}, shares)
	return reply.Send(cur.Complete(old))
}

// UserChange is a change of a user (§3.3.3) as its two audiences see it:
// the user's own connections, which are sent you, and everyone else, who
// are sent new. The two see different objects when the user's own see
// what the user chose, and others what the server shows of it, such as
// the chosen status and the shown one (§4.5.2).
type UserChange struct {
	// Old and New are the user before and after the change as its own
	// connections see it: what the backend last made their identity and
	// told their clients, and what it makes them now.
	Old, New User
	// OldShown and NewShown are the user before and after the change as
	// everyone else sees it, when NewShown has a user_id; without one,
	// others see Old and New. Their user_ids are Old's and New's, whatever
	// they hold. A change may be others' alone, such as a shown status
	// that follows the user's connections while the chosen one stays.
	OldShown, NewShown User
	// Except, if set, is one of the user's own connections that is told
	// of the change otherwise, and is sent nothing, nor given an identity:
	// the one whose me or auth result carries the user, complete (§3.3.2),
	// and makes it the connection's as it is sent.
	Except *Conn
}

// PublishUserChange announces a change of a user (§3.3.3) whose own
// connections may see another object than everyone else does, and reports
// whether there was one: whether either view of it changed anything, as
// User.Patch tells. Each audience is sent the patch of its own view, and
// only when that view changed:
//
//   - the user's own connections, those signed in as New's user_id or
//     Old's, but Except, get New as their identity (Conn.SetUser), and are
//     sent {"you": New.Patch(Old)};
//   - the other signed-in connections for which shares returns true, or
//     every one when shares is nil, are sent
//     {"new": NewShown.Patch(OldShown)}, with OldShown as old when the
//     user_id changed, as when a guest becomes an account.
//
// When nothing changed, PublishUserChange does nothing at all, as
// PublishUser does; when only others' view did, the user's own
// connections are neither sent anything nor given an identity. It never
// blocks, and is meant to be called while the backend holds the lock of
// the change, as PublishUser is.
func (s *Server) PublishUserChange(change UserChange, shares func(*Conn) bool) bool {
	old, cur := change.Old, change.New
	oldShown, curShown := old, cur
	if change.NewShown.UserID != "" {
		oldShown, curShown = change.OldShown, change.NewShown
		oldShown.UserID, curShown.UserID = old.UserID, cur.UserID
	}
	yours, youChanged := cur.Patch(old)
	theirs, othersChanged := curShown.Patch(oldShown)
	if !youChanged && !othersChanged {
		return false
	}
	var you, others Frame
	var err error
	if youChanged {
		you, err = Encode(UserUpdate{You: &yours})
	}
	if othersChanged && err == nil {
		update := UserUpdate{New: &theirs}
		if old.UserID != "" && old.UserID != cur.UserID {
			update.Old = &CurrentUser{User: oldShown.clone()}
		}
		others, err = Encode(update)
	}
	if err != nil {
		s.logger().Error("wire: encoding a user notification", "user_id", cur.UserID, "error", err)
		you, others = nil, nil
	}
	for _, c := range s.Conns() {
		switch id := c.UserID(); {
		case id == "", c == change.Except:
		case id == cur.UserID || id == old.UserID:
			if youChanged {
				c.SetUser(cur)
				if you != nil {
					c.Send(you)
				}
			}
		case others != nil && (shares == nil || shares(c)):
			c.Send(others)
		}
	}
	return true
}
