package server

import (
	"context"
	"fmt"
	"maps"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/internal/store"
)

const otherOrigin = "http://127.0.0.1:5173"

// commandIDs numbers command requests across connections: one account's
// connections share its request deduplication (§1), so they must not reuse
// an ID.
var commandIDs atomic.Int64

func commandID() string { return fmt.Sprintf("command-%d", commandIDs.Add(1)) }

const invalidDeviceToken = "This device token is not valid; get a new one with /invite-device on a signed-in device"

// passkeyAccount signs a new connection in as a passkey account and returns
// it with its user_id.
func passkeyAccount(t *testing.T, httpServer *httptest.Server) (*testClient, string) {
	t.Helper()
	c, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	registered := registerTestPasskey(t, c, newTestAuthenticator(t))
	c.drain(t)
	return c, registered["you"].(map[string]any)["user_id"].(string)
}

// privateReply runs a command in general and returns the text of the one
// ~private notice before its empty result.
func privateReply(t *testing.T, c *testClient, text string) string {
	t.Helper()
	before, result := c.request(t, "command", commandID(), map[string]any{"body": map[string]any{"text": text}})
	if len(result) != 0 || len(before) != 1 {
		t.Fatalf("%s: result %#v after %#v", text, result, before)
	}
	notice := notificationParams(t, before[0], "message")
	body := notice["body"].(map[string]any)
	if _, has := notice["message_id"]; has || notice["log_id"] != nil || notice["room_id"] != "general" || body["format"] != "markdown" ||
		!reflect.DeepEqual(notice["from"], map[string]any{"user_id": "~private", "name": "System message to you"}) {
		t.Fatalf("%s notice: %#v", text, notice)
	}
	return body["text"].(string)
}

// inviteDevice runs /invite-device name, checks the whole notice, and
// returns its token. linkServer is the escaped server of the notice's
// sign-in link, "" for none.
func inviteDevice(t *testing.T, c *testClient, userID, name string, replaces bool, linkServer string) string {
	t.Helper()
	text := privateReply(t, c, "/invite-device "+name)
	_, block, _ := strings.Cut(text, "```\n")
	token, _, _ := strings.Cut(block, "\n```")
	if !strings.HasPrefix(token, "apron_device_") || len(token) != len("apron_device_")+43 {
		t.Fatalf("token %q in %q", token, text)
	}
	replaced := ""
	if replaces {
		replaced = ", and replaces its earlier token"
	}
	link := ""
	if linkServer != "" {
		link = fmt.Sprintf("Or open this link on **%s**, which signs you in there with any Apron app: <https://link.apron.chat/#server=%s&token=%s>\n\n", name, linkServer, token)
	}
	want := fmt.Sprintf("This token signs in as you (`%s`) on your device **%s**%s. It does not expire, and anyone who has it can sign in as you, so keep it secret. `/invite-device %s` again replaces it.\n\n```\n%s\n```\n\n%sTo sign in, authenticate with the token scheme: `{\"method\": \"auth\", \"params\": {\"scheme\": \"token\", \"token\": \"%s\"}}`", userID, name, replaced, name, token, link, token)
	if text != want {
		t.Fatalf("notice:\n%s\nwant:\n%s", text, want)
	}
	return token
}

// deviceSignIn connects from origin ("" for none) and signs in with a
// device token, returning the connection and the auth result.
func deviceSignIn(t *testing.T, httpServer *httptest.Server, origin, token string) (*testClient, map[string]any) {
	t.Helper()
	c, _ := dialOrigin(t, httpServer, origin)
	result := c.result(t, "auth", "device", map[string]any{"scheme": "token", "token": token})
	c.drain(t)
	return c, result
}

// commandError runs a command that fails and returns its error.
func commandError(t *testing.T, c *testClient, text string) map[string]any {
	t.Helper()
	frame := c.call(t, "command", commandID(), map[string]any{"body": map[string]any{"text": text}})
	failure, ok := frame["error"].(map[string]any)
	if !ok {
		t.Fatalf("%s: %#v", text, frame)
	}
	return failure
}

func expectFailure(t *testing.T, failure map[string]any, code int, message string) {
	t.Helper()
	if failure["code"] != float64(code) || failure["message"] != message {
		t.Fatalf("error %#v, want %d %q", failure, code, message)
	}
}

// expectClosedWithError reads the connection-level error, with no id, that
// closes c (§1.1), and then the close.
func expectClosedWithError(t *testing.T, c *testClient, code int, message string) {
	t.Helper()
	frame := c.read(t)
	if _, has := frame["id"]; has {
		t.Fatalf("closing frame %#v has an id", frame)
	}
	expectFailure(t, frame["error"].(map[string]any), code, message)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, err := c.ws.Read(ctx); err == nil {
		t.Fatal("connection still open")
	}
}

// pushURLs lists the urls of a user's push registrations, sorted.
func pushURLs(app *Server, userID string) []string {
	app.mu.Lock()
	defer app.unlock()
	return slices.Sorted(maps.Keys(app.users[userID].pushes))
}

func TestInviteDeviceSignsInFromAnyOrigin(t *testing.T) {
	_, httpServer := passkeyTestServer(t)
	ada, adaID := passkeyAccount(t, httpServer)

	// Without a public URL, the link names the WebSocket URL the asking
	// connection reached, as email sign-in links do.
	token := inviteDevice(t, ada, adaID, "phone", false, wsURL(httpServer))
	ada.expectQuiet(t)

	for _, origin := range []string{otherOrigin, ""} {
		phone, result := deviceSignIn(t, httpServer, origin, token)
		if result["you"].(map[string]any)["user_id"] != adaID {
			t.Fatalf("origin %q signed in as %#v", origin, result["you"])
		}
		if _, has := result["token"]; has {
			t.Fatalf("origin %q: a device sign-in returned a session token: %#v", origin, result)
		}
		if text := privateReply(t, phone, "/help"); !strings.Contains(text, "- `/invite-device <name>`: get a token that signs in as you on another device, named <name>; at most 3 names, and a name again replaces its token") {
			t.Fatalf("a device's /help: %q", text)
		}
	}

	stranger, _ := dialOrigin(t, httpServer, otherOrigin)
	forged := stranger.call(t, "auth", "forged", map[string]any{"scheme": "token", "token": "apron_device_" + strings.Repeat("A", 43)})
	expectFailure(t, forged["error"].(map[string]any), codeDenied, invalidDeviceToken)
}

func TestInviteDeviceLinkUsesThePublicURL(t *testing.T) {
	config := DefaultConfig()
	config.WebAuthn = testWebAuthn(t)
	config.PublicURL = "https://chat.example/app/"
	_, httpServer := newTestServer(t, config)
	ada, adaID := passkeyAccount(t, httpServer)
	token := inviteDevice(t, ada, adaID, "my.phone_2", false, "wss://chat.example/app/ws")
	if want := "https://link.apron.chat/#server=wss://chat.example/app/ws&token=" + token; apronLink("wss://chat.example/app/ws", token) != want {
		t.Fatalf("link %q", apronLink("wss://chat.example/app/ws", token))
	}
	// Other bytes are percent-encoded, as apron-server-cloudflare's apronLink does.
	if link := apronLink("wss://chat.example/a b?x=1&y", "t+/="); link != "https://link.apron.chat/#server=wss://chat.example/a%20b%3Fx%3D1%26y&token=t%2B/%3D" {
		t.Fatalf("escaped link %q", link)
	}
}

func TestInviteDeviceNames(t *testing.T) {
	_, httpServer := passkeyTestServer(t)
	ada, adaID := passkeyAccount(t, httpServer)
	for _, args := range []string{"", "phone tablet"} {
		expectFailure(t, commandError(t, ada, "/invite-device "+args), codeInvalidParams, "Usage: /invite-device <name>")
	}
	for _, name := range []string{"-phone", ".phone", "_phone", "ph/one", "phöne", strings.Repeat("a", 33)} {
		expectFailure(t, commandError(t, ada, "/invite-device "+name), codeInvalidParams, "A device name is 1 to 32 letters, digits, _, . or -, starting with a letter or digit")
	}
	inviteDevice(t, ada, adaID, strings.Repeat("a", 32), false, wsURL(httpServer))
	inviteDevice(t, ada, adaID, "9", false, wsURL(httpServer))
}

func TestInviteDeviceHoldsThreeNames(t *testing.T) {
	_, httpServer := passkeyTestServer(t)
	ada, adaID := passkeyAccount(t, httpServer)
	link := wsURL(httpServer)
	for _, name := range []string{"phone", "tablet", "laptop"} {
		inviteDevice(t, ada, adaID, name, false, link)
	}
	expectFailure(t, commandError(t, ada, "/invite-device watch"), codeDenied,
		"You have 3 device tokens (`phone`, `tablet`, `laptop`); run /invite-device with one of those names to replace its token")
	inviteDevice(t, ada, adaID, "tablet", true, link)
	expectFailure(t, commandError(t, ada, "/invite-device watch"), codeDenied,
		"You have 3 device tokens (`phone`, `laptop`, `tablet`); run /invite-device with one of those names to replace its token")
}

func TestInviteDeviceReplacesTheNamesToken(t *testing.T) {
	app, httpServer := passkeyTestServer(t)
	ada, adaID := passkeyAccount(t, httpServer)
	link := wsURL(httpServer)
	old := inviteDevice(t, ada, adaID, "phone", false, link)
	tablet := inviteDevice(t, ada, adaID, "tablet", false, link)
	phone, _ := deviceSignIn(t, httpServer, "", old)
	other, _ := deviceSignIn(t, httpServer, "", tablet)
	ada.drain(t)
	ada.result(t, "push_register", "ada-push", map[string]any{"kind": "relay", "url": "https://push.example/ada"})
	phone.result(t, "push_register", "phone-push", map[string]any{"kind": "relay", "url": "https://push.example/phone"})
	other.result(t, "push_register", "tablet-push", map[string]any{"kind": "relay", "url": "https://push.example/tablet"})

	current := inviteDevice(t, ada, adaID, "phone", true, link)
	expectClosedWithError(t, phone, codeDenied, "Device token replaced; sign in with the new one")
	ada.expectQuiet(t)
	other.expectQuiet(t)
	if urls := pushURLs(app, adaID); !slices.Equal(urls, []string{"https://push.example/ada", "https://push.example/tablet"}) {
		t.Fatalf("push registrations after a replacement: %q", urls)
	}
	again, _ := dialRaw(t, httpServer)
	expectFailure(t, again.call(t, "auth", "old", map[string]any{"scheme": "token", "token": old})["error"].(map[string]any), codeDenied, invalidDeviceToken)

	// The asking connection stays open when it signed in with the token it
	// replaces, and counts as signed in with the new one, so the next
	// replacement closes it.
	phone, _ = deviceSignIn(t, httpServer, "", current)
	inviteDevice(t, phone, adaID, "phone", true, link)
	phone.expectQuiet(t)
	inviteDevice(t, ada, adaID, "phone", true, link)
	expectClosedWithError(t, phone, codeDenied, "Device token replaced; sign in with the new one")
	other.expectQuiet(t)
}

func TestInviteDeviceClosesADeviceThatGotASessionToken(t *testing.T) {
	_, httpServer := passkeyTestServer(t)
	ada, adaID := passkeyAccount(t, httpServer)
	link := wsURL(httpServer)
	phone, _ := deviceSignIn(t, httpServer, testPasskeyOrigin, inviteDevice(t, ada, adaID, "phone", false, link))
	ada.drain(t)

	// Whoever holds a device token can add a passkey, which gives the
	// connection a session token.
	options := passkeyResult(t, passkeyCall(t, phone, "register-start", "register", "begin", nil))
	registered := passkeyResult(t, passkeyCall(t, phone, "register-end", "register", "finish", map[string]any{"credential": newTestAuthenticator(t).registration(t, options, testPasskeyOrigin)}))
	if _, ok := registered["token"].(string); !ok || registered["you"].(map[string]any)["user_id"] != adaID {
		t.Fatalf("register on a device connection: %#v", registered)
	}
	phone.drain(t)

	inviteDevice(t, ada, adaID, "phone", true, link)
	expectClosedWithError(t, phone, codeDenied, "Device token replaced; sign in with the new one")
}

func TestInviteDeviceDeniesGuests(t *testing.T) {
	_, httpServer := passkeyTestServer(t)
	guest := dialTestClient(t, httpServer)
	expectFailure(t, commandError(t, guest, "/invite-device phone"), codeDenied, "Sign in with a passkey or email to use /invite-device")
	if text := privateReply(t, guest, "/help"); strings.Contains(text, "invite-device") {
		t.Fatalf("a guest's /help: %q", text)
	}
}

func TestDeviceTokensSurviveRestart(t *testing.T) {
	for name, opener := range map[string]func() func(t *testing.T, path string) store.Store{
		"memory": func() func(*testing.T, string) store.Store {
			memory := store.NewMemory()
			return func(*testing.T, string) store.Store { return memory }
		},
		"sqlite": func() func(*testing.T, string) store.Store {
			return func(t *testing.T, path string) store.Store {
				s, err := store.OpenSQLite(path)
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			open := opener()
			dir := t.TempDir()
			dbPath, uploadDir := filepath.Join(dir, "aprond.db"), filepath.Join(dir, "uploads")

			app, httpServer, stop := startWithStore(t, open(t, dbPath), uploadDir)
			ada, adaID := passkeyAccount(t, httpServer)
			link := wsURL(httpServer)
			replaced := inviteDevice(t, ada, adaID, "phone", false, link)
			phone := inviteDevice(t, ada, adaID, "phone", true, link)
			tablet := inviteDevice(t, ada, adaID, "tablet", false, link)
			inviteDevice(t, ada, adaID, "laptop", false, link)
			for name, token := range map[string]string{"phone": phone, "tablet": tablet} {
				device, _ := deviceSignIn(t, httpServer, "", token)
				device.result(t, "push_register", name, map[string]any{"kind": "relay", "url": "https://push.example/" + name})
			}
			// A registration stored before /invite-device existed records
			// no device token.
			app.mu.Lock()
			u := app.users[adaID]
			app.unlock()
			addPush(app, u, pushRegistration{url: "https://push.example/untagged"})
			stop()

			app, httpServer, _ = startWithStore(t, open(t, dbPath), uploadDir)
			for _, token := range []string{phone, tablet} {
				if _, result := deviceSignIn(t, httpServer, otherOrigin, token); result["you"].(map[string]any)["user_id"] != adaID {
					t.Fatalf("after a restart, signed in as %#v", result["you"])
				}
			}
			stale, _ := dialRaw(t, httpServer)
			stale.expectError(t, "auth", "replaced", map[string]any{"scheme": "token", "token": replaced}, codeDenied)
			device, _ := deviceSignIn(t, httpServer, "", tablet)
			expectFailure(t, commandError(t, device, "/invite-device watch"), codeDenied,
				"You have 3 device tokens (`phone`, `tablet`, `laptop`); run /invite-device with one of those names to replace its token")
			inviteDevice(t, device, adaID, "phone", true, wsURL(httpServer))
			if urls := pushURLs(app, adaID); !slices.Equal(urls, []string{"https://push.example/tablet", "https://push.example/untagged"}) {
				t.Fatalf("push registrations after a restart and a replacement: %q", urls)
			}
		})
	}
}
