package apron

import (
	"context"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"
)

const testRemote = "ws://b.example:8787/ws"

func TestShadowIDs(t *testing.T) {
	for _, tc := range []struct{ server, user, want string }{
		{"wss://B.example/ws", "carol", "carol@b.example"},
		{"ws://127.0.0.1:8787/ws", "carol", "carol@127.0.0.1:8787"},
		{"ws://[::1]:8081/ws", "Dave", "Dave@[::1]:8081"},
		{"wss://b.example/ws", "dave@c.example", "dave@c.example@b.example"},
	} {
		if got, err := shadowID(tc.server, tc.user); err != nil || got != tc.want {
			t.Errorf("shadowID(%q, %q) = %q, %v; want %q", tc.server, tc.user, got, err, tc.want)
		}
	}
	for _, bad := range [][2]string{{"b.example", "carol"}, {"wss:///ws", "carol"}, {"wss://u@b.example/ws", "carol"}, {testRemote, ""}, {testRemote, "~server"}, {testRemote, "a b"}} {
		if id, err := shadowID(bad[0], bad[1]); err == nil {
			t.Errorf("shadowID(%q, %q) = %q, want an error", bad[0], bad[1], id)
		}
	}
}

// callAs sends a request on a bot's session and decodes its result.
func callAs(t *testing.T, b *Bot, method string, params map[string]any) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := b.Call(ctx, method, params)
	if err != nil {
		t.Fatalf("%s as %s: %v", method, b.UserID(), err)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

// wstestText is a snapshot's body.text.
func wstestText(snapshot map[string]any) string {
	body, _ := snapshot["body"].(map[string]any)
	text, _ := body["text"].(string)
	return text
}

// An embed is not copied: a line names it, with its link when it has one.
func TestRemoteBodyEmbeds(t *testing.T) {
	body := remoteBody(map[string]any{"text": "see", "embeds": []any{
		map[string]any{"kind": "upload", "title": "plan.pdf"},
		map[string]any{"kind": "og", "og": map[string]any{"title": "Docs", "url": "https://docs.example/"}},
	}}, nil)
	if want := "see\n[plan.pdf, not mirrored]\n[Docs: https://docs.example/]"; body["text"] != want {
		t.Fatalf("text = %q, want %q", body["text"], want)
	}
}

// A link without server in a copy's text gets the owner's.
func TestWithServer(t *testing.T) {
	const owner = "wss://a.example/ws"
	for _, tc := range []struct {
		text     string
		markdown bool
		want     string
	}{
		{"see apron:?room=ops&message=1 now", false, "see apron:?server=wss://a.example/ws&room=ops&message=1 now"},
		{"[ops](https://link.apron.chat/#room=ops)", true, "[ops](https://link.apron.chat/#server=wss://a.example/ws&room=ops)"},
		{"<https://link.apron.chat/#server=wss://c.example/ws&room=x>", false, "<https://link.apron.chat/#server=wss://c.example/ws&room=x>"},
		{"`apron:?room=ops` apron:?room=ops", true, "`apron:?room=ops` apron:?server=wss://a.example/ws&room=ops"},
		{"no links", false, "no links"},
	} {
		if got := withServer(tc.text, tc.markdown, owner); got != tc.want {
			t.Errorf("withServer(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

// A mirror maps an owner's user_id that it has not seen by its form.
func TestMirrorUnseenUserIDs(t *testing.T) {
	ms := &mirror{f: &federation{selfAuthority: "b.example"}, server: "wss://a.example/ws", users: map[string]string{"seen": "seen@a.example"}}
	for ownerID, want := range map[string]string{
		"seen":                     "seen@a.example",
		"carol":                    "carol@a.example",
		"bob@b.example":            "bob",
		"dave@c.example":           "dave@c.example",
		"erin@c.example@a.example": "erin@c.example@a.example",
	} {
		if got := ms.localUserID(ownerID); got != want {
			t.Errorf("localUserID(%q) = %q, want %q", ownerID, got, want)
		}
	}
}

// A mirror keeps its request ids within 128 bytes, the same for a resend.
func TestOwnerRequestID(t *testing.T) {
	if id := ownerRequestID("bob:c1"); id != "bob:c1" {
		t.Fatalf("a short id = %q", id)
	}
	long := "bob:" + strings.Repeat("x", 200)
	if id := ownerRequestID(long); len(id) > maxOwnerRequestIDBytes || id != ownerRequestID(long) || id == ownerRequestID(long+"y") {
		t.Fatalf("a long id = %q", id)
	}
}
