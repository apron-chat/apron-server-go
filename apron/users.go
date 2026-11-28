package apron

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"maps"
	"slices"

	"github.com/apron-chat/apron-server-go/apron/internal/core"
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
	// Written token by token, as encoding the User, with each cleared
	// field left out, then each as its empty value, did, without what that
	// allocated: a room's members are hundreds of these.
	user := u.User
	for _, name := range u.Cleared {
		switch name {
		case "user_id":
			return errors.New("apron: a current user object cannot clear user_id")
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
	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	if err := writeUserMembers(enc, &user); err != nil {
		return err
	}
	for _, name := range u.Cleared {
		empty := `""`
		if i := slices.IndexFunc(userFields, func(f struct{ name, empty string }) bool { return f.name == name }); i >= 0 {
			empty = userFields[i].empty
		}
		if err := enc.WriteToken(jsontext.String(name)); err != nil {
			return err
		}
		if err := enc.WriteValue(jsontext.Value(empty)); err != nil {
			return err
		}
	}
	return enc.WriteToken(jsontext.EndObject)
}

// writeUserMembers writes the members of a user object as encoding a User
// does: user_id, each other field that is not empty, then the members it
// has no field for (Unknown).
func writeUserMembers(enc *jsontext.Encoder, u *User) error {
	member := func(name, value string) error {
		if err := enc.WriteToken(jsontext.String(name)); err != nil {
			return err
		}
		return enc.WriteToken(jsontext.String(value))
	}
	if err := member("user_id", u.UserID); err != nil {
		return err
	}
	for _, f := range []struct{ name, value string }{{"name", u.Name}, {"avatar", u.Avatar}} {
		if f.value != "" {
			if err := member(f.name, f.value); err != nil {
				return err
			}
		}
	}
	if len(u.Roles) > 0 {
		if err := enc.WriteToken(jsontext.String("roles")); err != nil {
			return err
		}
		if err := enc.WriteToken(jsontext.BeginArray); err != nil {
			return err
		}
		for _, role := range u.Roles {
			if err := enc.WriteToken(jsontext.String(role)); err != nil {
				return err
			}
		}
		if err := enc.WriteToken(jsontext.EndArray); err != nil {
			return err
		}
	}
	for _, f := range []struct{ name, value string }{{"status", u.Status}, {"seen", u.Seen}} {
		if f.value != "" {
			if err := member(f.name, f.value); err != nil {
				return err
			}
		}
	}
	if len(u.Ext) > 0 {
		if err := enc.WriteToken(jsontext.String("ext")); err != nil {
			return err
		}
		if err := enc.WriteToken(jsontext.BeginObject); err != nil {
			return err
		}
		// In the order of their keys, as Marshal writes a map.
		for _, key := range slices.Sorted(maps.Keys(u.Ext)) {
			if err := enc.WriteToken(jsontext.String(key)); err != nil {
				return err
			}
			if err := enc.WriteValue(u.Ext[key]); err != nil {
				return err
			}
		}
		if err := enc.WriteToken(jsontext.EndObject); err != nil {
			return err
		}
	}
	if len(u.Unknown) > 0 && u.Unknown.Kind() != 'n' {
		members, ok := core.Members(u.Unknown)
		if !ok {
			return errors.New("apron: a user's Unknown is not a JSON object")
		}
		for _, m := range members {
			if err := enc.WriteToken(jsontext.String(m.Name)); err != nil {
				return err
			}
			if err := enc.WriteValue(m.Value); err != nil {
				return err
			}
		}
	}
	return nil
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
	patch := CurrentUser{UserID: u.UserID}
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
		patch.Ext[key], changed = core.CloneRaw(value), true
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
		patch.Unknown, changed = core.CloneRaw(u.Unknown), true
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
// New when the account's user_id changed. Each is the zero CurrentUser,
// which is left out, when the notification does not carry it: one with a
// user_id is one that does.
type UserUpdate struct {
	You CurrentUser `json:"you,omitzero"`
	New CurrentUser `json:"new,omitzero"`
	Old CurrentUser `json:"old,omitzero"`
}

// Method makes a UserUpdate a Notification.
func (UserUpdate) Method() string { return "user" }
