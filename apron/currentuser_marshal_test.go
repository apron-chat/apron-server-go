package apron

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"testing"
)

// marshalCurrentUser encodes a current user as CurrentUser.MarshalJSONTo
// did before it wrote tokens: the User, cleared fields blanked, then each
// cleared field as its empty value.
func marshalCurrentUser(u CurrentUser) ([]byte, error) {
	user := u.User
	for _, name := range u.Cleared {
		switch name {
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
	raw, err := Marshal(user)
	if err != nil || len(u.Cleared) == 0 {
		return raw, err
	}
	raw = raw[:len(raw)-1]
	for _, name := range u.Cleared {
		empty := `""`
		if i := slices.IndexFunc(userFields, func(f struct{ name, empty string }) bool { return f.name == name }); i >= 0 {
			empty = userFields[i].empty
		}
		raw = append(raw, ',')
		raw, _ = jsontext.AppendQuote(raw, name)
		raw = append(append(raw, ':'), empty...)
	}
	return append(raw, '}'), nil
}

// A current user is written token by token as it was encoded.
func TestCurrentUserMarshal(t *testing.T) {
	full := User{UserID: "ada", Name: "Ada é", Avatar: "https://a/b.png", Roles: []string{"admin", "op"}, Status: "online", Seen: "2026-01-01T00:00:00Z",
		Ext: Ext{"z": jsontext.Value(`1`), "a": jsontext.Value(`{ "b" : "\u00e9" }`)}, Unknown: jsontext.Value(`{"x":[1, 2],"y":null}`)}
	for _, u := range []CurrentUser{
		{User: User{UserID: "a"}},
		{User: full},
		{User: full, Cleared: []string{"status", "roles"}},
		{User: User{UserID: "a", Name: "A"}, Cleared: []string{"avatar", "roles", "status", "seen", "ext", "name"}},
		{User: User{UserID: "a", Roles: []string{}}, Cleared: []string{"other"}},
		{User: User{UserID: "a", Unknown: jsontext.Value(`{"q":"\u0071"}`)}, Cleared: []string{"status"}},
	} {
		got, err := Marshal(u)
		want, werr := marshalCurrentUser(u)
		if err != nil || werr != nil || string(got) != string(want) {
			t.Errorf("%+v:\n got %s, %v\nwant %s, %v", u, got, err, want, werr)
		}
		var back CurrentUser
		if err := json.Unmarshal(got, &back); err != nil {
			t.Errorf("%s does not decode: %v", got, err)
		}
	}
	if _, err := Marshal(CurrentUser{User: User{UserID: "a"}, Cleared: []string{"user_id"}}); err == nil {
		t.Error("cleared user_id")
	}
}
