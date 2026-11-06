package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/internal/wstest"
)

func TestSearch(t *testing.T) {
	s := &Search{}
	_, server := wstest.Serve(t, apron.DefaultConfig(), s)
	ada, bob := wstest.Guest(t, server, "Ada"), wstest.Guest(t, server, "Bob")
	if _, err := ada.Post("general", "The deploy failed again <script>"); err != nil {
		t.Fatal(err)
	}
	room, err := ada.Call("room_set", map[string]any{"title": "Secret", "private": true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ada.Post(room["room_id"].(string), "the secret deploy plan"); err != nil {
		t.Fatal(err)
	}
	edited, err := ada.Post("general", "edit me")
	if err != nil {
		t.Fatal(err)
	}

	// The index follows the log on its own goroutine: wait for it.
	expect := func(c *wstest.Client, q string, want int) {
		t.Helper()
		var n int
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			result, err := c.Call("search.query", map[string]any{"q": q})
			if err != nil {
				t.Fatal(err)
			}
			if n = len(result["results"].([]any)); n == want {
				return
			}
		}
		t.Fatalf("%s finds %d for %q, want %d", c.UserID, n, q, want)
	}
	expect(ada, "deploy", 2)
	// Bob is not in the private room.
	expect(bob, "deploy", 1)
	// An edit replaces what is indexed, and a deletion removes it.
	if _, err := ada.Call("message", map[string]any{"room_id": "general", "message_id": edited, "body": map[string]any{"text": "edited deploy"}}); err != nil {
		t.Fatal(err)
	}
	expect(bob, "deploy", 2)
	expect(bob, "edit me", 0)
	if _, err := ada.Call("message", map[string]any{"room_id": "general", "message_id": edited, "deleted": true}); err != nil {
		t.Fatal(err)
	}
	expect(bob, "deploy", 1)
	notices, err := bob.Command("general", "/search deploy failed")
	if err != nil || len(notices) != 1 || !strings.Contains(notices[0], "The deploy failed again") {
		t.Fatalf("/search = %q, %v", notices, err)
	}

	response, err := http.Get(server.URL + "/search/archive/general")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !strings.Contains(string(page), "The deploy failed again &lt;script&gt;") || strings.Contains(string(page), "secret") {
		t.Fatalf("archive page:\n%s", page)
	}
	response, err = http.Get(server.URL + "/search/archive/" + room["room_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("private room archive = %d", response.StatusCode)
	}
}

func TestNoArchiveWithoutGuests(t *testing.T) {
	config := apron.DefaultConfig()
	config.DisableGuests = true
	config.EmailSender = apron.LogEmailSender{}
	_, server := wstest.Serve(t, config, &Search{})
	response, err := http.Get(server.URL + "/search/archive/general")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("archive on a server without guests = %d, want 404", response.StatusCode)
	}
}
