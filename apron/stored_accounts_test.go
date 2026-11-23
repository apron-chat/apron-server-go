package apron

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron/internal/devices"
	"github.com/apron-chat/apron-server-go/apron/internal/embeds"
	"github.com/apron-chat/apron-server-go/apron/internal/emoji"
	"github.com/apron-chat/apron-server-go/apron/internal/invitations"
	"github.com/apron-chat/apron-server-go/apron/internal/services"
	"github.com/apron-chat/apron-server-go/apron/internal/users"
	"github.com/apron-chat/apron-server-go/store"
)

// testdata/stored-accounts.jsonl holds the user, device, service token,
// invitation, embed, custom emoji, and push entries a server stored before
// they had shapes of their own in modules, after storedAccountsScenario.
// Each is stored again byte for byte. Run TestStoredAccountsScenario with
// APRON_WRITE_STORED_ACCOUNTS=1 to write the file again.

const storedAccountsFile = "testdata/stored-accounts.jsonl"

// storedAccountKinds are the kinds of entries the file holds.
var storedAccountKinds = []string{entryUser, devices.EntryKind, services.EntryKind, invitations.EntryKind, embeds.EntryKind, entryPush, entryMeta}

// storedAccountsScenario makes a bot with two service tokens, an
// outstanding invitation and an account an invitation made, a passkey
// account with a device token, mutes, a status, an avatar, a custom emoji, a message
// upload, a write that has not started, and a push registration, and
// returns what it stored.
func storedAccountsScenario(t *testing.T) []store.Entry {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	backing := store.NewMemory()
	var host *Host
	var bot *Bot
	config := extensionConfig(testExtension{"agents", func(r *Registrar) error {
		host = r.Host
		var err error
		bot, err = r.Bot("agent", BotOptions{Name: "Agent", Scope: &RoomScope{Rooms: []string{defaultRoomID}}})
		return err
	}})
	config.WebAuthn = testWebAuthn(t)
	config.Store = backing
	app, httpServer := newTestServer(t, config)
	// Lifetimes far ahead, so that the entries restore for decades.
	const long = 50 * 365 * 24 * time.Hour
	if _, err := bot.IssueToken(ctx, ServiceTokenOptions{Name: "python agent", Lifetime: long}); err != nil {
		t.Fatal(err)
	}
	if _, err := bot.IssueToken(ctx, ServiceTokenOptions{Lifetime: long, ManageAccount: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Invite(ctx, InvitationOptions{
		Lifetime: long, Name: "Owner", UserID: "owner", Roles: []string{RoleAdmin}, Rooms: []string{defaultRoomID},
		Data: map[string]any{"grant": "owner", "n": jsontext.Value("12345678901234567890")},
	}); err != nil {
		t.Fatal(err)
	}
	used, err := app.Invite(ctx, InvitationOptions{Lifetime: long, Data: map[string]any{"x": 1}})
	if err != nil {
		t.Fatal(err)
	}
	invited, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	passkeyResult(t, registerInvited(t, invited, newTestAuthenticator(t), used.Token, map[string]any{"name": "Invited"}))

	ada, adaID := passkeyAccount(t, httpServer)
	inviteDevice(t, ada, adaID, "laptop", false, wsURL(httpServer))
	ada.request(t, "status", "mute", map[string]any{"mute": true})
	ada.request(t, "status", "mute-general", map[string]any{"room_id": defaultRoomID, "mute": 3600})
	ada.request(t, "me", "dnd", map[string]any{"status": "dnd"})
	ada.drain(t)
	ada.result(t, "push_register", "push", map[string]any{"kind": "relay", "url": "https://push.example/ada", "token": "t", "push_id": "p1", "wake": []any{"mentions", "badge"}})
	avatar := ada.result(t, "command", "avatar", map[string]any{"body": map[string]any{"text": "/avatar", "embeds": []any{map[string]any{"kind": "upload"}}}})
	if status, _, _ := httpDo(t, http.MethodPut, avatar["embeds"].([]any)[0].(map[string]any)["write_url"].(string), bytes.NewReader(testPNG(t)), "image/png"); status != http.StatusCreated {
		t.Fatalf("avatar upload: %d", status)
	}
	ada.drain(t)
	if status := addEmoji(t, ada, ":party:", testPNG(t), "image/png"); status != http.StatusCreated {
		t.Fatalf("emoji upload: %d", status)
	}
	ada.drain(t)
	posted := postEmbeds(t, ada, "upload", map[string]any{"body": map[string]any{"text": "files", "embeds": []any{
		map[string]any{"kind": "upload", "title": "notes.txt"},
		map[string]any{"kind": "upload", "title": "later.png", "og": map[string]any{"image": map[string]any{"alt": "Later"}}},
	}}})
	if status, _, _ := httpDo(t, http.MethodPut, posted["embeds"].([]any)[0].(map[string]any)["write_url"].(string), bytes.NewReader([]byte("some notes")), "text/plain"); status != http.StatusCreated {
		t.Fatalf("message upload: %d", status)
	}
	if err := app.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	var entries []store.Entry
	_ = backing.Load(func(e store.Entry) error {
		if slices.Contains(storedAccountKinds, e.Kind) && !(e.Kind == entryMeta && e.ID == "vapid") {
			entries = append(entries, e)
		}
		return nil
	})
	slices.SortFunc(entries, func(a, b store.Entry) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.ID, b.ID))
	})
	return entries
}

// TestStoredAccountsScenario runs the scenario, and with
// APRON_WRITE_STORED_ACCOUNTS=1 writes what it stored to the file.
func TestStoredAccountsScenario(t *testing.T) {
	entries := storedAccountsScenario(t)
	kinds := make(map[string]int)
	for _, e := range entries {
		kinds[e.Kind]++
	}
	for _, kind := range storedAccountKinds {
		if kinds[kind] == 0 {
			t.Errorf("no %s entry stored", kind)
		}
	}
	if os.Getenv("APRON_WRITE_STORED_ACCOUNTS") == "" {
		return
	}
	var out bytes.Buffer
	for _, e := range entries {
		line, err := json.Marshal(struct {
			Kind  string `json:"kind"`
			ID    string `json:"id"`
			Value string `json:"value"`
		}{e.Kind, e.ID, string(e.Value)})
		if err != nil {
			t.Fatal(err)
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	if err := os.WriteFile(storedAccountsFile, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// loadStoredAccounts reads the file's entries by kind and ID.
func loadStoredAccounts(t *testing.T) map[string]map[string]jsontext.Value {
	t.Helper()
	file, err := os.Open(storedAccountsFile)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	entries := make(map[string]map[string]jsontext.Value)
	lines := bufio.NewScanner(file)
	lines.Buffer(nil, 1<<20)
	for lines.Scan() {
		var line struct{ Kind, ID, Value string }
		if err := json.Unmarshal(lines.Bytes(), &line, json.MatchCaseInsensitiveNames(true)); err != nil {
			t.Fatal(err)
		}
		if entries[line.Kind] == nil {
			entries[line.Kind] = make(map[string]jsontext.Value)
		}
		entries[line.Kind][line.ID] = jsontext.Value(line.Value)
	}
	return entries
}

// expectStoredAgain checks that entries are stored again as the file holds
// them, and that every one of kind is.
func expectStoredAgain(t *testing.T, kind string, want map[string]jsontext.Value, got []store.Entry) {
	t.Helper()
	seen := make(map[string]bool)
	for _, e := range got {
		if e.Kind != kind {
			t.Errorf("%s entry stored as %s", kind, e.Kind)
		}
		seen[e.ID] = true
		if !bytes.Equal(e.Value, want[e.ID]) {
			t.Errorf("%s %s is stored again as\n%s\nwant\n%s", kind, e.ID, e.Value, want[e.ID])
		}
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("%s %s is not stored again", kind, id)
		}
	}
}

// TestStoredAccountsAreStoredAgain restores each module's entries of the
// file, and checks that the module stores them again as they were.
func TestStoredAccountsAreStoredAgain(t *testing.T) {
	stored := loadStoredAccounts(t)
	known := func(id string) (string, bool) { return id, true }
	same := func(id string) string { return id }

	deviceTable := devices.New[string]()
	if err := deviceTable.Restore(stored[devices.EntryKind], known); err != nil {
		t.Fatal(err)
	}
	expectStoredAgain(t, devices.EntryKind, stored[devices.EntryKind], deviceTable.Snapshot(same))

	serviceTable := services.New(func(services.Key, *services.Token[string]) {})
	defer serviceTable.Stop()
	if err := serviceTable.Restore(stored[services.EntryKind], known, time.Now()); err != nil {
		t.Fatal(err)
	}
	expectStoredAgain(t, services.EntryKind, stored[services.EntryKind], serviceTable.Snapshot(same))

	invitationTable := invitations.New()
	if err := invitationTable.Restore(stored[invitations.EntryKind], time.Now()); err != nil {
		t.Fatal(err)
	}
	expectStoredAgain(t, invitations.EntryKind, stored[invitations.EntryKind], invitationTable.Snapshot())

	embedTable := embeds.New[*storedEmbed]()
	if _, err := embedTable.Restore(stored[embeds.EntryKind], func(e embeds.Embed, avatarFor string) *storedEmbed {
		return &storedEmbed{e, avatarFor}
	}, func(file string) string { return "/uploads/" + file }); err != nil {
		t.Fatal(err)
	}
	expectStoredAgain(t, embeds.EntryKind, stored[embeds.EntryKind], embedTable.Snapshot(func(e *storedEmbed) string { return e.avatarFor }))

	catalog := emoji.NewCatalog[storedImage]()
	if err := catalog.Restore(stored[emoji.StoreKind][emoji.StoreID], func(id, key string) (storedImage, bool) { return storedImage(id), true }); err != nil {
		t.Fatal(err)
	}
	expectStoredAgain(t, emoji.StoreKind, map[string]jsontext.Value{emoji.StoreID: stored[emoji.StoreKind][emoji.StoreID]}, catalog.Snapshot())

	expectUsersStoredAgain(t, stored[entryUser], stored[entryUser])
}

// storedEmbed is an embed as the store keeps it.
type storedEmbed struct {
	embeds.Embed
	avatarFor string
}

func (e *storedEmbed) Hosted() *embeds.Embed { return &e.Embed }

// storedImage is a custom emoji's image as the store names it.
type storedImage string

func (i storedImage) ImageID() string  { return string(i) }
func (i storedImage) ImageURL() string { return "" }
func (i storedImage) ImageAlt() string { return "" }
func (i storedImage) ImageSize() int64 { return 0 }

// TestStoredAccountShapesRoundTrip decodes each entry of the file into the
// shape that stores it, and checks that it encodes again as it was.
func TestStoredAccountShapesRoundTrip(t *testing.T) {
	stored := loadStoredAccounts(t)
	roundTrip := func(kind string, shape func() any) {
		t.Helper()
		var again []store.Entry
		for id, raw := range stored[kind] {
			value := shape()
			if err := json.Unmarshal(raw, value); err != nil {
				t.Fatalf("%s %s: %v", kind, id, err)
			}
			again = append(again, store.Entry{Kind: kind, ID: id, Value: encodeJSON(value)})
		}
		expectStoredAgain(t, kind, stored[kind], again)
	}
	roundTrip(entryUser, func() any { return new(users.Stored) })
	roundTrip(entryPush, func() any { return new(storedPush) })
}
