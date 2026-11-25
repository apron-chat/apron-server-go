package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/apron-chat/apron-server-go/aprond"
	"github.com/apron-chat/apron-server-go/aprond/aprondtest"
)

func TestOps(t *testing.T) {
	o := &Ops{}
	o.Settings.WebhookSecret = "s3cret"
	o.Settings.Services = []string{"api=echo deployed the api"}
	server := aprondtest.Serve(t, aprond.DefaultConfig(), o)
	ada := server.Guest("Ada")

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
	got := ada.Message(func(m aprond.Message) bool { return m.From.UserID == "ops" })
	if !strings.Contains(got.Text, "**octocat** opened [acme/app#7 Fix it]") {
		t.Fatalf("webhook message = %q", got.Text)
	}

	// Guests may not deploy.
	if _, err := ada.Command("general", "/deploy api"); err == nil {
		t.Fatal("a guest deployed")
	}
}

func TestDeploy(t *testing.T) {
	o := &Ops{}
	o.Settings.Services = []string{"api=echo deployed the api"}
	server := aprondtest.Serve(t, aprond.DefaultConfig(), o)
	ada, lead := server.Guest("Ada"), server.Member("lead", RoleDeployer)

	if _, err := lead.Command("general", "/deploy api"); err != nil {
		t.Fatal(err)
	}
	ada.Message(func(m aprond.Message) bool {
		// The terminal embed shows the program's output.
		var terminal struct{ Output string }
		body := m.Apron().Body
		return body != nil && len(body.Embeds) == 1 && json.Unmarshal(body.Embeds[0], &terminal) == nil &&
			strings.Contains(terminal.Output, "deployed the api")
	})
	done := ada.Message(func(m aprond.Message) bool { return strings.HasPrefix(m.Text, "✅") })
	if done.Text != "✅ deployed api" {
		t.Fatalf("outcome = %q", done.Text)
	}
	if _, err := lead.Command("general", "/deploy web"); err == nil {
		t.Fatal("deployed an unknown service")
	}
}
