package main

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/apron-chat/apron-server-go/aprond"
	"github.com/apron-chat/apron-server-go/aprond/aprondtest"
)

func TestSearch(t *testing.T) {
	server := aprondtest.Serve(t, aprond.DefaultConfig(), &Search{})
	ada, bob := server.Guest("Ada"), server.Guest("Bob")
	if _, err := ada.Post("general", "The deploy failed again <script>"); err != nil {
		t.Fatal(err)
	}
	room, err := ada.CreateRoom("Secret", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ada.Post(room, "the secret deploy plan"); err != nil {
		t.Fatal(err)
	}
	edited, err := ada.Post("general", "edit me")
	if err != nil {
		t.Fatal(err)
	}

	// The index follows the log on its own goroutine: wait for it.
	expect := func(c *aprondtest.Client, q string, want int) {
		t.Helper()
		server.Settle()
		var result struct{ Results []any }
		if err := c.Call("search.query", map[string]any{"q": q}, &result); err != nil || len(result.Results) != want {
			t.Fatalf("%s finds %d for %q, want %d: %v", c.UserID, len(result.Results), q, want, err)
		}
	}
	expect(ada, "deploy", 2)
	// Bob is not in the private room.
	expect(bob, "deploy", 1)
	// An edit replaces what is indexed, and a deletion removes it.
	if err := ada.Edit("general", edited, "edited deploy"); err != nil {
		t.Fatal(err)
	}
	expect(bob, "deploy", 2)
	expect(bob, "edit me", 0)
	if err := ada.Delete("general", edited); err != nil {
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
	response, err = http.Get(server.URL + "/search/archive/" + room)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("private room archive = %d", response.StatusCode)
	}
}

func TestNoArchiveWithoutGuests(t *testing.T) {
	config := aprond.DefaultConfig()
	config.DisableGuests = true
	config.EmailSender = aprond.LogEmailSender{}
	server := aprondtest.Serve(t, config, &Search{})
	response, err := http.Get(server.URL + "/search/archive/general")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("archive on a server without guests = %d, want 404", response.StatusCode)
	}
}
