package apron

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron/internal/push"
	"github.com/apron-chat/apron-server-go/apron/internal/users"
	"github.com/apron-chat/apron-server-go/store"
)

// testdata/stored-users.jsonl holds user and push entries with every field
// their shapes have, as the server at 909c09b stored them from users and
// registrations built by hand, and user entries as servers stored them
// before identities, each with the entry that server stored again for it
// ("stored"). Each of the others is stored again byte for byte, but for
// the users stored as shown other than offline (expectUsersStoredAgain).

const storedUsersFile = "testdata/stored-users.jsonl"

// storedUserLine is a line of the file.
type storedUserLine struct {
	Kind, ID string
	Value    jsontext.Value
	// Stored is the value stored again, when it differs from Value.
	Stored jsontext.Value
}

// loadStoredUsers reads the file's lines.
func loadStoredUsers(t *testing.T) []storedUserLine {
	t.Helper()
	file, err := os.Open(storedUsersFile)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var lines []storedUserLine
	scanner := bufio.NewScanner(file)
	scanner.Buffer(nil, 1<<20)
	for scanner.Scan() {
		var line struct{ Kind, ID, Value, Stored string }
		if err := json.Unmarshal(scanner.Bytes(), &line, json.MatchCaseInsensitiveNames(true)); err != nil {
			t.Fatal(err)
		}
		stored := jsontext.Value(line.Stored)
		if line.Stored == "" {
			stored = jsontext.Value(line.Value)
		}
		lines = append(lines, storedUserLine{line.Kind, line.ID, jsontext.Value(line.Value), stored})
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

// TestStoredUserShapesRoundTrip decodes each user the file holds as stored
// into the shape that stores it, and checks that it encodes again as it
// was.
func TestStoredUserShapesRoundTrip(t *testing.T) {
	for _, line := range loadStoredUsers(t) {
		if line.Kind != entryUser || !bytes.Equal(line.Value, line.Stored) {
			continue
		}
		var value users.Stored
		if err := json.Unmarshal(line.Value, &value); err != nil {
			t.Fatalf("%s %s: %v", line.Kind, line.ID, err)
		}
		if again := encodeJSON(value); !bytes.Equal(again, line.Value) {
			t.Errorf("%s %s is stored again as\n%s\nwant\n%s", line.Kind, line.ID, again, line.Value)
		}
	}
}

// TestStoredPushesAreStoredAgain restores the file's registrations into a
// table, and checks that it stores each again as it was.
func TestStoredPushesAreStoredAgain(t *testing.T) {
	entries := make(map[string]jsontext.Value)
	for _, line := range loadStoredUsers(t) {
		if line.Kind == entryPush {
			entries[line.ID] = line.Value
		}
	}
	if len(entries) == 0 {
		t.Fatal("no registrations")
	}
	table := push.NewTable(nil)
	if err := table.Restore(entries, func(*push.Registration) bool { return true }); err != nil {
		t.Fatal(err)
	}
	expectStoredAgain(t, entryPush, entries, table.Snapshot())
}

// TestStoredUsersAreStoredAgain restores the file's users into a table,
// and checks that the table stores each again as the file says.
func TestStoredUsersAreStoredAgain(t *testing.T) {
	entries := make(map[string]jsontext.Value)
	want := make(map[string]jsontext.Value)
	for _, line := range loadStoredUsers(t) {
		if line.Kind == entryUser {
			entries[line.ID], want[line.ID] = line.Value, line.Stored
		}
	}
	expectUsersStoredAgain(t, entries, want)
}

// expectUsersStoredAgain restores stored users into a table, and checks
// that it stores each again as want has it, and those Restore changed,
// stored again at once, also as want has them: but a user stored as shown
// other than offline (a crash) is stored again as no longer shown, with
// seen when it was restored (users.Table.Restore), and without its mutes
// that ran out since, and want has it as it was before.
func expectUsersStoredAgain(t *testing.T, entries, want map[string]jsontext.Value) {
	t.Helper()
	now := time.UnixMilli(1795400000000)
	table := users.NewTable[*userState](users.Timers[*userState]{})
	avatars, err := table.Restore(entries, restoredUser, now)
	if err != nil {
		t.Fatal(err)
	}
	avatarEmbed := func(u *userState) string { return avatars[u.id] }
	changed := table.Entries(avatarEmbed)
	again := table.Snapshot(avatarEmbed)
	want = maps.Clone(want)
	for id, value := range want {
		want[id] = withoutRanOutMutes(t, value, time.Now())
	}
	for _, e := range changed {
		if u := table.Get(e.ID); u.Presence.Status == statusOffline && shownField.Match(entries[e.ID]) {
			want[e.ID] = shownField.ReplaceAll(want[e.ID], fmt.Appendf(nil, `"seen":%d`, now.UnixMilli()))
		}
	}
	byID := make(map[string]jsontext.Value)
	for _, e := range changed {
		byID[e.ID] = e.Value
	}
	for id, value := range byID {
		if !bytes.Equal(value, want[id]) {
			t.Errorf("user %s is stored again at once as\n%s\nwant\n%s", id, value, want[id])
		}
	}
	expectStoredAgain(t, entryUser, want, again)
}

// withoutRanOutMutes is a stored user without its mutes that ran out at
// now, which it is not stored again with: decoded and encoded again
// (TestStoredUserShapesRoundTrip) when it has any.
func withoutRanOutMutes(t *testing.T, value jsontext.Value, now time.Time) jsontext.Value {
	t.Helper()
	var stored users.Stored
	if err := json.Unmarshal(value, &stored); err != nil {
		t.Fatal(err)
	}
	ranOut := false
	if m := stored.Mute; m != nil && !m.Forever && !now.Before(m.Until) {
		stored.Mute, ranOut = nil, true
	}
	for id, m := range stored.RoomMutes {
		if !m.Forever && !now.Before(m.Until) {
			delete(stored.RoomMutes, id)
			ranOut = true
		}
	}
	if !ranOut {
		return value
	}
	if len(stored.RoomMutes) == 0 {
		stored.RoomMutes = nil
	}
	return encodeJSON(stored)
}

// shownField is the stored fields of a user shown other than offline: seen,
// if known, and shown.
var shownField = regexp.MustCompile(`("seen":\d+,)?"shown":true`)

// TestLegacyUsersAreStoredAgain restores the users the file holds as
// servers stored them before identities, and checks that they are stored
// again with them as identities.
func TestLegacyUsersAreStoredAgain(t *testing.T) {
	memory := store.NewMemory()
	want := make(map[string]jsontext.Value)
	for _, line := range loadStoredUsers(t) {
		if bytes.Equal(line.Value, line.Stored) {
			continue
		}
		want[line.ID] = line.Stored
		if err := memory.Apply([]store.Entry{{Kind: line.Kind, ID: line.ID, Value: line.Value}}); err != nil {
			t.Fatal(err)
		}
	}
	if len(want) == 0 {
		t.Fatal("no legacy users")
	}
	config := DefaultConfig()
	config.Store = memory
	app, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got []store.Entry
	_ = memory.Load(func(e store.Entry) error {
		if e.Kind == entryUser {
			got = append(got, e)
		}
		return nil
	})
	expectStoredAgain(t, entryUser, want, got)
}
