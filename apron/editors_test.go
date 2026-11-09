package apron

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// ext:editors (apron's EXTENSIONS.md): a sender who sets ext.editors
// {"body.text": "mentioned"} lets each user in body.mentions save body.text.
// Such a save changes only the text, the server keeps every other field,
// and it marks the snapshot with edited_by, which it removes on the
// sender's saves and ignores in requests.
func TestEditorsShareText(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	clients := dialGroup(t, httpServer, 3)
	alice, bob, carol := clients[0], clients[1], clients[2]
	quiet := func() {
		t.Helper()
		bob.drain(t)
		carol.drain(t)
		alice.drain(t)
	}

	_, reply := save(t, alice, "reply-target", map[string]any{"body": map[string]any{"text": "lunch?"}})
	quiet()
	grant := map[string]any{"body.text": "mentioned"}
	id, created := save(t, alice, "create", map[string]any{
		"reply_to": map[string]any{"message_id": reply["message_id"]},
		"body":     map[string]any{"text": "Lunch, @bob:\n- [ ] soup", "format": "markdown", "mentions": []any{bob.userID}},
		"ext":      map[string]any{"editors": map[string]any{"body.text": "mentioned", "edited_by": carol.userID}, "irc": "x"},
	})
	if want := map[string]any{"editors": grant, "irc": "x"}; !reflect.DeepEqual(created["ext"], any(want)) {
		t.Fatalf("created ext %#v, want %#v: requests do not set edited_by", created["ext"], want)
	}
	quiet()

	// Bob saves the text. Everything else in his request is ignored: the
	// room, the format, the mentions, the reply, and ext.
	_, saved := save(t, bob, "bob-save", map[string]any{
		"message_id": id,
		"body":       map[string]any{"text": "Lunch, @bob:\n- [x] soup", "mentions": []any{bob.userID, carol.userID}},
		"ext":        map[string]any{"editors": map[string]any{"body.text": "mentioned", "edited_by": alice.userID}, "irc": ""},
	})
	wantBody := map[string]any{"text": "Lunch, @bob:\n- [x] soup", "format": "markdown", "mentions": []any{bob.userID}}
	if !reflect.DeepEqual(saved["body"], any(wantBody)) {
		t.Fatalf("body after bob's save %#v, want %#v", saved["body"], wantBody)
	}
	wantExt := map[string]any{"editors": map[string]any{"body.text": "mentioned", "edited_by": bob.userID}, "irc": "x"}
	if !reflect.DeepEqual(saved["ext"], any(wantExt)) {
		t.Fatalf("ext after bob's save %#v, want %#v", saved["ext"], wantExt)
	}
	if saved["from"].(map[string]any)["user_id"] != alice.userID {
		t.Fatalf("from after bob's save %#v, want alice", saved["from"])
	}
	if ref, _ := saved["reply_to"].(map[string]any); ref["message_id"] != reply["message_id"] {
		t.Fatalf("reply_to after bob's save %#v", saved["reply_to"])
	}
	quiet()

	// Carol is not mentioned, so she may not save it.
	carol.expectError(t, "message", "carol-save", map[string]any{"message_id": id, "room_id": "general", "body": map[string]any{"text": "mine"}}, CodeDenied)
	// Bob may not delete it.
	bob.expectError(t, "message", "bob-delete", map[string]any{"message_id": id, "room_id": "general", "deleted": true}, CodeDenied)
	quiet()

	// Alice's save removes edited_by, and keeps the grant.
	_, own := save(t, alice, "alice-save", map[string]any{
		"message_id": id,
		"body":       map[string]any{"text": "Lunch, @bob @carol:\n- [x] soup", "mentions": []any{bob.userID, carol.userID}},
	})
	if want := map[string]any{"editors": grant, "irc": "x"}; !reflect.DeepEqual(own["ext"], any(want)) {
		t.Fatalf("ext after alice's save %#v, want %#v", own["ext"], want)
	}
	quiet()
	// Now that she is mentioned, Carol may save it.
	_, saved = save(t, carol, "carol-save-2", map[string]any{"message_id": id, "body": map[string]any{"text": "Lunch, @bob @carol:\n- [x] soup\n- [ ] salad"}})
	if editors := saved["ext"].(map[string]any)["editors"].(map[string]any); editors["edited_by"] != carol.userID {
		t.Fatalf("editors after carol's save %#v", editors)
	}
	quiet()

	// Without the grant, mentions share nothing; nor does a value the server
	// does not know.
	for i, ext := range []map[string]any{{"editors": ""}, {"editors": map[string]any{"body.text": "everyone"}}} {
		save(t, alice, fmt.Sprint("revoke-", i), map[string]any{
			"message_id": id,
			"body":       map[string]any{"text": "closed", "mentions": []any{bob.userID, carol.userID}},
			"ext":        ext,
		})
		quiet()
		bob.expectError(t, "message", fmt.Sprint("bob-after-revoke-", i), map[string]any{"message_id": id, "room_id": "general", "body": map[string]any{"text": "open"}}, CodeDenied)
	}
}

// Filters see a save of shared text as it will be saved: in the message's
// room, with its reply and the rest of its body, whatever room the request
// names. A message with an append embed is refused to its editors.
func TestEditorsFilteredAsSaved(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	_, httpServer := newTestServer(t, extensionConfig(testExtension{"guard", func(r *Registrar) error {
		r.FilterMessage(func(_ context.Context, d *Draft) error {
			mu.Lock()
			seen = append(seen, fmt.Sprint(d.RoomID, "|", d.ReplyTo, "|", d.Body["format"]))
			mu.Unlock()
			if d.RoomID == "general" && strings.Contains(d.Text(), "spam") {
				return Denied("No spam in general")
			}
			return nil
		})
		return nil
	}}))
	clients := dialGroup(t, httpServer, 2)
	alice, bob := clients[0], clients[1]
	quiet := func() {
		t.Helper()
		alice.drain(t)
		bob.drain(t)
	}
	other, _ := saveRoom(t, alice, "room", map[string]any{"title": "Elsewhere"})
	quiet()
	_, target := save(t, alice, "target", map[string]any{"body": map[string]any{"text": "lunch?"}})
	id, _ := save(t, alice, "create", map[string]any{
		"reply_to": map[string]any{"message_id": target["message_id"]},
		"body":     map[string]any{"text": "- [ ] soup", "format": "markdown", "mentions": []any{bob.userID}},
		"ext":      map[string]any{"editors": map[string]any{"body.text": "mentioned"}},
	})
	quiet()
	mu.Lock()
	seen = nil
	mu.Unlock()

	// Naming another room doesn't take the save past a filter of the
	// message's own room.
	bob.expectError(t, "message", "elsewhere", map[string]any{"message_id": id, "room_id": other, "body": map[string]any{"text": "spam"}}, CodeDenied)
	mu.Lock()
	want := fmt.Sprint("general|", target["message_id"], "|markdown")
	if len(seen) != 1 || seen[0] != want {
		t.Fatalf("filter saw %q, want %q", seen, want)
	}
	mu.Unlock()
	// And a save there lands in the message's room.
	_, saved := save(t, bob, "fine", map[string]any{"message_id": id, "body": map[string]any{"text": "- [x] soup"}})
	if saved["room_id"] != "general" {
		t.Fatalf("saved in %v", saved["room_id"])
	}
	quiet()

	// A message with an append embed is its sender's while its text may
	// still be written.
	_, result := alice.request(t, "message", "append", map[string]any{
		"room_id": "general",
		"body":    map[string]any{"text": "plan:", "mentions": []any{bob.userID}, "embeds": []any{map[string]any{"kind": "append"}}},
		"ext":     map[string]any{"editors": map[string]any{"body.text": "mentioned"}},
	})
	appended := result["message_id"].(string)
	quiet()
	bob.expectError(t, "message", "append-save", map[string]any{"message_id": appended, "room_id": "general", "body": map[string]any{"text": "mine"}}, CodeDenied)
}
