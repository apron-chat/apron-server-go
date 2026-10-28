package server

import (
	"bytes"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/apron-chat/apron-server-go/internal/store"
)

const flagAvatar = "https://example.com/flag.png"

// signInAdmin signs c in as the account of ada@example.com, which the
// tests' configurations make an admin.
func signInAdmin(t *testing.T, c *testClient, mailbox testMailbox) {
	t.Helper()
	propose(t, c, "ada@example.com")
	signInByEmail(t, c, mailbox.receive(t).Code, nil)
	c.drain(t)
}

// runServer runs /server with text after it, and embeds when given,
// on c, and returns the frames before its result, then the result.
func runServer(t *testing.T, c *testClient, text string, embeds ...any) ([]map[string]any, map[string]any) {
	t.Helper()
	body := map[string]any{"text": strings.TrimSpace("/server " + text)}
	if len(embeds) > 0 {
		body["embeds"] = embeds
	}
	return c.request(t, "command", c.nextID("server"), map[string]any{"body": body})
}

// profileChange runs /server with text on admin, which must send admin a
// server frame and then a ~private notice before its result, and every
// other client the same server frame. It returns the frame's params and the
// notice's text.
func profileChange(t *testing.T, admin *testClient, others []*testClient, text string) (map[string]any, string) {
	t.Helper()
	before, result := runServer(t, admin, text)
	if len(result) != 0 || len(before) != 2 {
		t.Fatalf("/server %s: %#v before %#v", text, before, result)
	}
	params := notificationParams(t, before[0], "server")
	notice := privateNoticeText(t, before[1])
	for _, c := range others {
		if got := c.notification(t, "server"); fmt.Sprint(got) != fmt.Sprint(params) {
			t.Fatalf("/server %s sent %#v, and %#v to its sender", text, got, params)
		}
	}
	return params, notice
}

// privateNoticeText requires a frame to be a ~private notice and returns its
// text.
func privateNoticeText(t *testing.T, frame map[string]any) string {
	t.Helper()
	params := notificationParams(t, frame, "message")
	if params["from"].(map[string]any)["user_id"] != privateNoticeID {
		t.Fatalf("not a private notice: %#v", frame)
	}
	return params["body"].(map[string]any)["text"].(string)
}

// serverAvatarWriteURL runs /server avatar with one upload embed on c and
// returns its write URL.
func serverAvatarWriteURL(t *testing.T, c *testClient) string {
	t.Helper()
	before, result := runServer(t, c, "avatar", map[string]any{"kind": "upload"})
	if len(before) != 0 {
		t.Fatalf("frames before the /server avatar result: %#v", before)
	}
	return result["embeds"].([]any)[0].(map[string]any)["write_url"].(string)
}

func profileOf(params map[string]any) [3]any {
	return [3]any{params["title"], params["description"], params["avatar"]}
}

// /server shows the server's title, description, and avatar to admins, and
// sets and clears them, overriding the flags; every change sends every
// connection a new server frame.
func TestServerCommand(t *testing.T) {
	dir := t.TempDir()
	app, mailbox, httpServer := emailTestServer(t, func(config *Config) {
		config.UploadDir = dir
		config.Roles = map[string][]string{"admin": {"ada@example.com"}}
		config.Title = "Flag Title"
		config.Description = "From the *flag*"
		config.Avatar = flagAvatar
	})
	watcher, frame := dialRaw(t, httpServer)
	if got, want := profileOf(frame["params"].(map[string]any)), [3]any{"Flag Title", "From the *flag*", flagAvatar}; got != want {
		t.Fatalf("profile from the flags: %v, want %v", got, want)
	}
	guest := dialTestClient(t, httpServer)
	admin, _ := dialRaw(t, httpServer)
	signInAdmin(t, admin, mailbox)
	guest.drain(t)
	others := []*testClient{watcher, guest}

	// Only admins may use it, or see it in /help.
	for i, text := range []string{"", "title Mine", "avatar"} {
		guest.expectError(t, "command", fmt.Sprint("guest-", i), map[string]any{"body": map[string]any{"text": "/server " + text}}, codeDenied)
	}
	helpOf := func(c *testClient) string {
		before, _ := c.request(t, "command", c.nextID("help"), map[string]any{"body": map[string]any{"text": "/help"}})
		return privateNoticeText(t, before[0])
	}
	if strings.Contains(helpOf(guest), "/server") || !strings.Contains(helpOf(admin), "/server") {
		t.Fatal("/help lists /server for guests, or not for admins")
	}

	// /server alone shows where each value comes from.
	before, _ := runServer(t, admin, "")
	if len(before) != 1 {
		t.Fatalf("/server: %#v", before)
	}
	if notice := privateNoticeText(t, before[0]); !strings.Contains(notice, "**Title** (from `--title`): `Flag Title`") ||
		!strings.Contains(notice, "> From the *flag*") || !strings.Contains(notice, "**Avatar** (from `--avatar`): `"+flagAvatar+"`") {
		t.Fatalf("/server notice: %q", notice)
	}

	// The title is prepared as a display name.
	params, notice := profileChange(t, admin, others, "title   The  ﬁne  Server ")
	if params["title"] != "The fine Server" || !strings.Contains(notice, "**Title** (set with `/server title`): `The fine Server`") {
		t.Fatalf("title set: %#v, %q", params, notice)
	}
	params, _ = profileChange(t, admin, others, "description Talk about *anything*\nall day")
	if params["description"] != "Talk about *anything*\nall day" || params["title"] != "The fine Server" {
		t.Fatalf("description set: %#v", params)
	}

	// Invalid values are refused and change nothing.
	for i, tc := range []struct {
		text string
		code int
	}{
		{"title two\nlines", codeInvalidParams},
		{"title ​​", codeInvalidParams},
		{"title " + strings.Repeat("x", maxNameRunes+1), codeTooLarge},
		{"description tab\there", codeInvalidParams},
		{"description " + strings.Repeat("x", maxServerDescriptionBytes+1), codeTooLarge},
		{"avatar https://example.com/a.png", codeInvalidParams},
		{"colour red", codeInvalidParams},
	} {
		admin.expectError(t, "command", fmt.Sprint("invalid-", i), map[string]any{"body": map[string]any{"text": "/server " + tc.text}}, tc.code)
	}
	admin.expectError(t, "command", "attached", map[string]any{"body": map[string]any{"text": "/server title Pic", "embeds": []any{map[string]any{"kind": "upload"}}}}, codeInvalidParams)
	admin.expectError(t, "command", "two", map[string]any{"body": map[string]any{"text": "/server avatar", "embeds": []any{map[string]any{"kind": "upload"}, map[string]any{"kind": "upload"}}}}, codeInvalidParams)
	before, _ = runServer(t, admin, "title The fine Server")
	if len(before) != 1 || !strings.Contains(privateNoticeText(t, before[0]), "unchanged") {
		t.Fatalf("an unchanged title: %#v", before)
	}
	for _, c := range others {
		c.expectQuiet(t)
	}

	// The avatar is an upload, kept apart from message uploads.
	if status, _, _ := httpDo(t, http.MethodPut, serverAvatarWriteURL(t, admin), bytes.NewReader([]byte("plain text")), "text/plain"); status != http.StatusUnsupportedMediaType {
		t.Fatalf("a text avatar: %d", status)
	}
	if status, _, _ := httpDo(t, http.MethodPut, serverAvatarWriteURL(t, admin), bytes.NewReader(testPNG(t)), "image/png"); status != http.StatusCreated {
		t.Fatalf("avatar upload: %d", status)
	}
	params = admin.notification(t, "server")
	for _, c := range others {
		c.notification(t, "server")
	}
	avatar, _ := params["avatar"].(string)
	if !strings.HasPrefix(avatar, httpServer.URL+filePath) {
		t.Fatalf("avatar after the upload: %#v", params)
	}
	if status, _, _ := httpDo(t, http.MethodGet, avatar, nil, ""); status != http.StatusOK {
		t.Fatalf("server avatar: %d", status)
	}
	app.mu.RLock()
	uploadCount := app.uploads.Len()
	app.mu.RUnlock()
	if uploadCount != 0 {
		t.Fatalf("the server avatar is among %d message uploads", uploadCount)
	}

	// A new avatar deletes the one it replaces.
	if status, _, _ := httpDo(t, http.MethodPut, serverAvatarWriteURL(t, admin), bytes.NewReader(testPNG(t)), "image/png"); status != http.StatusCreated {
		t.Fatalf("avatar replacement: %d", status)
	}
	params = admin.notification(t, "server")
	for _, c := range others {
		c.notification(t, "server")
	}
	if params["avatar"] == avatar {
		t.Fatalf("the replacement kept the avatar: %#v", params)
	}
	if status, _, _ := httpDo(t, http.MethodGet, avatar, nil, ""); status != http.StatusNotFound {
		t.Fatalf("the replaced avatar: %d", status)
	}
	avatar = params["avatar"].(string)

	// Clearing falls back to the flags, and deletes the avatar's upload.
	params, notice = profileChange(t, admin, others, "avatar")
	if params["avatar"] != flagAvatar || !strings.Contains(notice, "Cleared the avatar") {
		t.Fatalf("avatar cleared: %#v, %q", params, notice)
	}
	if status, _, _ := httpDo(t, http.MethodGet, avatar, nil, ""); status != http.StatusNotFound {
		t.Fatalf("the cleared avatar: %d", status)
	}
	if files := uploadFiles(t, dir); files != 0 {
		t.Fatalf("upload files after the avatar was cleared: %d", files)
	}
	profileChange(t, admin, others, "title")
	params, notice = profileChange(t, admin, others, "DESCRIPTION")
	if got, want := profileOf(params), [3]any{"Flag Title", "From the *flag*", flagAvatar}; got != want || !strings.Contains(notice, "(from `--description`)") {
		t.Fatalf("profile cleared: %v, want %v; %q", got, want, notice)
	}

	// Without flags, a cleared value is absent.
	_, mailbox, bare := emailTestServer(t, func(config *Config) {
		config.Roles = map[string][]string{"admin": {"ada@example.com"}}
	})
	admin, _ = dialRaw(t, bare)
	signInAdmin(t, admin, mailbox)
	if params, _ := profileChange(t, admin, nil, "title Bare"); params["title"] != "Bare" {
		t.Fatalf("title: %#v", params)
	}
	if params, notice := profileChange(t, admin, nil, "title"); profileOf(params) != [3]any{} || !strings.Contains(notice, "**Title**: none") {
		t.Fatalf("cleared without a flag: %#v, %q", params, notice)
	}
}

// A /server avatar write is checked again when it finishes: its sender must
// still be an admin, or the write is refused and its upload deleted.
func TestServerAvatarWriteRechecksAdmin(t *testing.T) {
	dir := t.TempDir()
	app, mailbox, httpServer := emailTestServer(t, func(config *Config) {
		config.UploadDir = dir
		config.Roles = map[string][]string{"admin": {"ada@example.com"}}
	})
	admin, _ := dialRaw(t, httpServer)
	signInAdmin(t, admin, mailbox)
	writeURL := serverAvatarWriteURL(t, admin)
	app.mu.Lock()
	app.users[admin.userID].roles = nil
	app.mu.Unlock()
	if status, _, _ := httpDo(t, http.MethodPut, writeURL, bytes.NewReader(testPNG(t)), "image/png"); status != http.StatusForbidden {
		t.Fatalf("a former admin's write: %d", status)
	}
	admin.expectQuiet(t)
	if files := uploadFiles(t, dir); files != 0 {
		t.Fatalf("upload files after the refused write: %d", files)
	}
}

// The values /server sets survive a restart and override the flags of the
// new run; cleared, they fall back to them.
func TestServerProfileSurvivesRestart(t *testing.T) {
	kept, uploads := store.NewMemory(), t.TempDir()
	mailbox := make(testMailbox, 16)
	configure := func(title string) func(*Config) {
		return func(config *Config) {
			config.EmailSender = mailbox
			config.Roles = map[string][]string{"admin": {"ada@example.com"}}
			config.Title = title
			config.Avatar = flagAvatar
		}
	}
	_, httpServer, stop := startWith(t, kept, uploads, configure("First flag"))
	admin, _ := dialRaw(t, httpServer)
	signInAdmin(t, admin, mailbox)
	profileChange(t, admin, nil, "title Kept")
	profileChange(t, admin, nil, "description Kept *too*")
	if status, _, _ := httpDo(t, http.MethodPut, serverAvatarWriteURL(t, admin), bytes.NewReader(testPNG(t)), "image/png"); status != http.StatusCreated {
		t.Fatalf("avatar upload: %d", status)
	}
	avatar := admin.notification(t, "server")["avatar"].(string)
	stop()

	app, httpServer, stop := startWith(t, kept, uploads, configure("Second flag"))
	_, frame := dialRaw(t, httpServer)
	params := frame["params"].(map[string]any)
	restored, _ := params["avatar"].(string)
	// The restarted test server listens on another port.
	if params["title"] != "Kept" || params["description"] != "Kept *too*" || path.Base(restored) != path.Base(avatar) {
		t.Fatalf("profile after a restart: %#v, want the avatar %s", params, avatar)
	}
	if status, _, _ := httpDo(t, http.MethodGet, httpServer.URL+restored[strings.Index(restored, filePath):], nil, ""); status != http.StatusOK {
		t.Fatalf("restored avatar: %d", status)
	}
	app.mu.RLock()
	uploadCount := app.uploads.Len()
	app.mu.RUnlock()
	if uploadCount != 0 {
		t.Fatalf("the restored server avatar is among %d message uploads", uploadCount)
	}
	admin, _ = dialRaw(t, httpServer)
	signInAdmin(t, admin, mailbox)
	profileChange(t, admin, nil, "title")
	profileChange(t, admin, nil, "avatar")
	stop()

	_, httpServer, _ = startWith(t, kept, uploads, configure("Third flag"))
	_, frame = dialRaw(t, httpServer)
	if got, want := profileOf(frame["params"].(map[string]any)), [3]any{"Third flag", "Kept *too*", flagAvatar}; got != want {
		t.Fatalf("profile after clearing and a restart: %v, want %v", got, want)
	}
	if files := uploadFiles(t, uploads); files != 0 {
		t.Fatalf("upload files after the avatar was cleared: %d", files)
	}
}

// A connection whose URLs clients would not load, http: on a host other
// than a loopback one, may not set the server avatar; the title and
// description need no URL.
func TestServerAvatarNeedsAUsableURL(t *testing.T) {
	_, mailbox, httpServer := emailTestServer(t, func(config *Config) {
		config.Roles = map[string][]string{"admin": {"ada@example.com"}}
	})
	admin, _ := dialHost(t, httpServer, "chat.example")
	signInAdmin(t, admin, mailbox)
	admin.expectError(t, "command", "avatar", map[string]any{"body": map[string]any{"text": "/server avatar", "embeds": []any{map[string]any{"kind": "upload"}}}}, codeDenied)
	if params, _ := profileChange(t, admin, nil, "title Proxied"); params["title"] != "Proxied" {
		t.Fatalf("title over plain HTTP: %#v", params)
	}
}

// setGeneral sets the description of the default room with room_set on c
// and returns the frames before the result.
func setGeneral(t *testing.T, c *testClient, description string) []map[string]any {
	t.Helper()
	before, _ := c.request(t, "room_set", c.nextID("general"), map[string]any{"room_id": defaultRoomID, "title": "General", "description": description})
	return before
}

// Without a description set with /server or --description, the server
// frame's is the default room's, and a change to that one announces a new
// server frame; a stored or flag description overrides it.
func TestServerDescriptionFallsBackToDefaultRoom(t *testing.T) {
	_, mailbox, httpServer := emailTestServer(t, func(config *Config) {
		config.Roles = map[string][]string{"admin": {"ada@example.com"}}
		config.Description = "" // An empty flag is no description.
	})
	watcher, frame := dialRaw(t, httpServer)
	if _, has := frame["params"].(map[string]any)["description"]; has {
		t.Fatalf("a description without any: %#v", frame)
	}
	admin, _ := dialRaw(t, httpServer)
	signInAdmin(t, admin, mailbox)
	expectAnnounced := func(before []map[string]any, want any) {
		t.Helper()
		if !slices.Equal(methods(before), []string{"room_update", "server"}) {
			t.Fatalf("frames before the room_set result: %v, want room_update and server", methods(before))
		}
		params := notificationParams(t, before[1], "server")
		if got := watcher.notification(t, "server"); params["description"] != want || got["description"] != want {
			t.Fatalf("server description %#v and %#v, want %#v", params["description"], got["description"], want)
		}
	}
	expectQuiet := func(before []map[string]any) {
		t.Helper()
		if !slices.Equal(methods(before), []string{"room_update"}) {
			t.Fatalf("frames before the room_set result: %v, want room_update", methods(before))
		}
		watcher.expectQuiet(t)
	}

	// An unchanged, empty description announces nothing.
	expectQuiet(setGeneral(t, admin, ""))
	expectAnnounced(setGeneral(t, admin, "All about *general*"), "All about *general*")
	before, _ := runServer(t, admin, "")
	if notice := privateNoticeText(t, before[0]); !strings.Contains(notice, "**Description** (the `general` room's):") {
		t.Fatalf("/server notice: %q", notice)
	}
	// Other rooms are not the default room.
	saveRoom(t, admin, "other", map[string]any{"title": "Other", "description": "Not the server's"})
	watcher.expectQuiet(t)

	// A stored description overrides the room's, whose changes then
	// announce nothing; cleared, the room's is the fallback again.
	if params, _ := profileChange(t, admin, []*testClient{watcher}, "description Stored"); params["description"] != "Stored" {
		t.Fatalf("stored description: %#v", params)
	}
	expectQuiet(setGeneral(t, admin, "Changed"))
	if params, _ := profileChange(t, admin, []*testClient{watcher}, "description"); params["description"] != "Changed" {
		t.Fatalf("description after clearing: %#v", params)
	}

	// A room description longer than a server description is left out.
	expectAnnounced(setGeneral(t, admin, strings.Repeat("x", maxServerDescriptionBytes+1)), nil)
	expectAnnounced(setGeneral(t, admin, "Short"), "Short")

	// A flag description overrides the room's too.
	_, flagged := newTestServer(t, func() Config {
		config := DefaultConfig()
		config.Description = "From the flag"
		return config
	}())
	watcher, _ = dialRaw(t, flagged)
	guest := dialTestClient(t, flagged)
	expectQuiet(setGeneral(t, guest, "The room's"))
	_, frame = dialRaw(t, flagged)
	if description := frame["params"].(map[string]any)["description"]; description != "From the flag" {
		t.Fatalf("description with the flag: %#v", description)
	}
}
