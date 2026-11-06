package main

import (
	"io"
	"net/http"
	"strings"
	"testing"

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
	if _, err := ada.Post("general", "edit me"); err != nil {
		t.Fatal(err)
	}

	count := func(c *wstest.Client, q string) int {
		t.Helper()
		for range 50 { // the index follows the log on its own goroutine
			result, err := c.Call("search.query", map[string]any{"q": q})
			if err != nil {
				t.Fatal(err)
			}
			if n := len(result["results"].([]any)); n > 0 || q == "nothing" {
				return n
			}
		}
		return 0
	}
	if n := count(ada, "deploy"); n != 2 {
		t.Fatalf("ada finds %d, want 2", n)
	}
	if n := count(bob, "deploy"); n != 1 {
		t.Fatalf("bob finds %d, want 1: not the private room's", n)
	}
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
