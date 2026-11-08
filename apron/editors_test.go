package apron

import (
	"fmt"
	"reflect"
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
