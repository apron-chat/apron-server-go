package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/internal/wstest"
)

func TestOps(t *testing.T) {
	o := &Ops{}
	o.Settings.Room, o.Settings.WebhookSecret, o.Settings.Timeout = "general", "s3cret", time.Minute
	o.Settings.Services = []string{"api=echo deployed the api"}
	_, server := wstest.Serve(t, apron.DefaultConfig(), o)
	ada := wstest.Guest(t, server, "Ada")

	body := []byte(`{"action":"opened","repository":{"full_name":"acme/app"},"sender":{"login":"octocat"},
		"pull_request":{"number":7,"title":"Fix it","html_url":"https://github.com/acme/app/pull/7"}}`)
	post := func(signature string) int {
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/ops/github", bytes.NewReader(body))
		request.Header.Set("X-GitHub-Event", "pull_request")
		request.Header.Set("X-Hub-Signature-256", signature)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response.StatusCode
	}
	if status := post("sha256=00"); status != http.StatusUnauthorized {
		t.Fatalf("unsigned webhook = %d", status)
	}
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(body)
	if status := post("sha256=" + hex.EncodeToString(mac.Sum(nil))); status != http.StatusNoContent {
		t.Fatalf("signed webhook = %d", status)
	}
	got := ada.Message(func(p map[string]any) bool { return wstest.From(p) == "ops" })
	if text := wstest.Text(got); !strings.Contains(text, "**octocat** opened [acme/app#7 Fix it]") {
		t.Fatalf("webhook message = %q", text)
	}

	// Guests may not deploy.
	if _, err := ada.Command("general", "/deploy api"); err == nil {
		t.Fatal("a guest deployed")
	}
}

// lead is a test extension whose bot holds the deployer role.
type lead struct{ session *apron.Session }

func (*lead) Name() string { return "lead" }
func (l *lead) Init(h *apron.Host) (err error) {
	l.session, err = h.Session(h.Context(), "lead", apron.SessionOptions{})
	return err
}

func TestDeploy(t *testing.T) {
	o := &Ops{}
	o.Settings.Room, o.Settings.Timeout = "general", time.Minute
	o.Settings.Services = []string{"api=echo deployed the api"}
	config := apron.DefaultConfig()
	config.Roles = map[string][]string{"deployer": {"lead"}}
	l := &lead{}
	_, server := wstest.Serve(t, config, o, l)
	ada := wstest.Guest(t, server, "Ada")

	_, err := l.session.Call(t.Context(), "command", map[string]any{"room_id": "general", "body": map[string]any{"text": "/deploy api"}})
	if err != nil {
		t.Fatal(err)
	}
	ada.Message(func(p map[string]any) bool {
		body, _ := p["body"].(map[string]any)
		embeds, _ := body["embeds"].([]any)
		if len(embeds) != 1 {
			return false
		}
		output, _ := embeds[0].(map[string]any)["output"].(string)
		return strings.Contains(output, "deployed the api")
	})
	done := ada.Message(func(p map[string]any) bool { return strings.HasPrefix(wstest.Text(p), "✅") })
	if wstest.Text(done) != "✅ deployed api" {
		t.Fatalf("outcome = %q", wstest.Text(done))
	}
	if _, err := l.session.Call(t.Context(), "command", map[string]any{"room_id": "general", "body": map[string]any{"text": "/deploy web"}}); err == nil {
		t.Fatal("deployed an unknown service")
	}
}
