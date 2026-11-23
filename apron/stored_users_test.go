package apron

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"testing"

	"github.com/apron-chat/apron-server-go/store"
)

// testdata/stored-users.jsonl holds user and push entries with every field
// their shapes have, as the server at 909c09b stored them from users and
// registrations built by hand, and user entries as servers stored them
// before identities, each with the entry this server stores again for it
// ("stored"). Each of the others is stored again byte for byte.

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

// TestStoredUserShapesRoundTrip decodes each entry of the file stored as
// it is into the shape that stores it, and checks that it encodes again as
// it was.
func TestStoredUserShapesRoundTrip(t *testing.T) {
	for _, line := range loadStoredUsers(t) {
		if !bytes.Equal(line.Value, line.Stored) {
			continue
		}
		var value any
		switch line.Kind {
		case entryUser:
			value = new(storedUser)
		case entryPush:
			value = new(storedPush)
		default:
			t.Fatalf("%s entry in %s", line.Kind, storedUsersFile)
		}
		if err := json.Unmarshal(line.Value, value); err != nil {
			t.Fatalf("%s %s: %v", line.Kind, line.ID, err)
		}
		if again := encodeJSON(value); !bytes.Equal(again, line.Value) {
			t.Errorf("%s %s is stored again as\n%s\nwant\n%s", line.Kind, line.ID, again, line.Value)
		}
	}
}

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
