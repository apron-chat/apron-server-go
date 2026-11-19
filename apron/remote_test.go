package apron

import (
	"context"
	"encoding/json/v2"
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
		if got, err := ShadowID(tc.server, tc.user); err != nil || got != tc.want {
			t.Errorf("ShadowID(%q, %q) = %q, %v; want %q", tc.server, tc.user, got, err, tc.want)
		}
	}
	for _, bad := range [][2]string{{"b.example", "carol"}, {"wss:///ws", "carol"}, {"wss://u@b.example/ws", "carol"}, {testRemote, ""}, {testRemote, "~server"}, {testRemote, "a b"}} {
		if id, err := ShadowID(bad[0], bad[1]); err == nil {
			t.Errorf("ShadowID(%q, %q) = %q, want an error", bad[0], bad[1], id)
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
