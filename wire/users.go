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
// the fields of User it carried empty.
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

// PublishUser announces a change of a user from old to cur (§3.3.3): it
// makes cur the identity of the user's own connections (Conn.SetUser), and
// sends them {"you": patch}, and the other signed-in connections for which
// shares returns true, or every one when shares is nil, {"new": patch},
// where patch is cur.Patch(old). Nothing is sent when nothing changed. The
// user's own connections are those signed in as cur's user_id, or old's:
// when the two differ, as when a guest becomes an account, others are sent
// old too.
//
// A backend tells others only of users who share a room with them
// (§3.3.3): shares says who does. A backend whose user's own connections
// see another object than others do, such as the chosen status rather than
// the shown one (§4.5.2), sends its own UserUpdates.
//
// Like Broadcast, it never blocks, and is meant to be called while the
// backend holds the lock of the change, so that every connection gets a
// user's changes in the order they were made; shares is called in the
// caller's goroutine, so it may read what that lock guards.
func (s *Server) PublishUser(old, cur User, shares func(*Conn) bool) {
	patch, changed := cur.Patch(old)
	var you, others Frame
	if changed {
		update := UserUpdate{New: &patch}
		if old.UserID != "" && old.UserID != cur.UserID {
			update.Old = &CurrentUser{User: old.clone()}
		}
		var err error
		if you, err = Encode(UserUpdate{You: &patch}); err == nil {
			others, err = Encode(update)
		}
		if err != nil {
			s.logger().Error("wire: encoding a user notification", "user_id", cur.UserID, "error", err)
			you, others = nil, nil
		}
	}
	for _, c := range s.Conns() {
		switch id := c.UserID(); {
		case id == "":
		case id == cur.UserID || id == old.UserID:
			c.SetUser(cur)
			if you != nil {
				c.Send(you)
			}
		case others != nil && (shares == nil || shares(c)):
			c.Send(others)
		}
	}
}
