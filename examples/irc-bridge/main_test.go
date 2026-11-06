package main

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/internal/wstest"
)

func TestBridge(t *testing.T) {
	// A fake IRC server: it welcomes the bridge, confirms its join, says a
	// line in the channel, and hands back what the bridge sends.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	sent, say := make(chan string, 16), make(chan string)
	joined := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		lines := bufio.NewScanner(conn)
		for lines.Scan() {
			line := lines.Text()
			switch {
			case strings.HasPrefix(line, "USER "):
				conn.Write([]byte(":irc.test 001 bridge :Welcome\r\n"))
			case strings.HasPrefix(line, "JOIN "):
				conn.Write([]byte(":bridge!b@test JOIN #apron\r\n"))
				close(joined)
				go func() {
					for line := range say {
						conn.Write([]byte(line + "\r\n"))
					}
				}()
			case strings.HasPrefix(line, "PRIVMSG "):
				sent <- line
			}
		}
	}()

	b := &Bridge{}
	b.Settings.Server, b.Settings.Plaintext, b.Settings.Nick = listener.Addr().String(), true, "bridge"
	b.Settings.Channel, b.Settings.Room = "#apron", "general"
	_, server := wstest.Serve(t, apron.DefaultConfig(), b)
	ada := wstest.Guest(t, server, "Ada")
	<-joined
	say <- ":alice!a@test PRIVMSG #apron :hello from irc"

	relayed := ada.Message(func(p map[string]any) bool { return wstest.From(p) == "irc" })
	if text := wstest.Text(relayed); text != "**alice:** hello from irc" {
		t.Fatalf("relayed = %q", text)
	}
	if ext := relayed["ext"].(map[string]any)["irc"].(map[string]any); ext["nick"] != "alice" {
		t.Fatalf("ext = %#v", ext)
	}
	if _, err := ada.Post("general", "hi alice\nhow are you"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PRIVMSG #apron :<Ada> hi alice", "PRIVMSG #apron :<Ada> how are you"} {
		select {
		case line := <-sent:
			if line != want {
				t.Fatalf("sent %q, want %q", line, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("nothing sent, want %q", want)
		}
	}
}
