// Command tail prints the recent messages of a room of any Apron server,
// then its new ones as they are posted, until it is interrupted:
//
//	go run ./examples/tail --url ws://127.0.0.1:8080/ws --room general
//
// It signs in with the token in $APRON_TOKEN (§3.2.1), or, without one, as
// a guest named --name. Without --room, it prints every room the
// connection receives. It exits when the connection ends: reconnecting is
// a client's own job, as is catching up on what it missed meanwhile with
// history (§4.3.5).
package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/client"
)

func main() {
	log.SetFlags(0)
	url := flag.String("url", os.Getenv("APRON_URL"), "the server's address, such as wss://chat.example/ws; $APRON_URL by default")
	room := flag.String("room", "", "the room_id to print; empty is every room the connection receives")
	name := flag.String("name", "tail", "the name to sign in as a guest with, without $APRON_TOKEN")
	lines := flag.Int("n", 20, "how many earlier changes of the log to read first")
	flag.Parse()
	auth := apron.AuthParams{Scheme: "guest", Name: *name, Agent: "apron-tail"}
	if token := os.Getenv("APRON_TOKEN"); token != "" {
		auth = apron.AuthParams{Scheme: "token", Token: token, Agent: "apron-tail"}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := tail(ctx, *url, auth, *room, *lines, os.Stdout); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("tail: %v", err)
	}
}

// tail prints room's last messages, from a page of up to limit changes of
// its log, then those posted after, until ctx is done or the connection
// ends.
func tail(ctx context.Context, url string, auth apron.AuthParams, room string, limit int, out io.Writer) error {
	// The messages that arrive meanwhile wait while the history is read.
	live := make(chan apron.Message, 64)
	stopped := make(chan struct{})
	defer close(stopped)
	conn, err := client.Dial(ctx, url, auth, client.OnFrame(func(f apron.Frame) {
		var frame struct {
			Method string        `json:"method"`
			Params apron.Message `json:"params"`
		}
		if json.Unmarshal(f, &frame) != nil || frame.Method != "message" {
			return
		}
		select {
		case live <- frame.Params:
		case <-stopped:
		}
	}))
	if err != nil {
		return err
	}
	defer conn.Close()

	page, err := conn.History(ctx, apron.HistoryParams{RoomID: room, Limit: limit})
	if err != nil {
		return err
	}
	for _, m := range page.Messages {
		printMessage(out, room, m)
	}
	// The page holds every message the server sent the connection before
	// it (§1): a later one has a later log_id. A notice has none.
	seen := max(page.LastLogID, page.LatestLogID)
	for {
		select {
		case m := <-live:
			if (m.LogID > seen || m.MessageID == 0) && (room == "" || m.RoomID == room) {
				printMessage(out, room, m)
			}
		case <-conn.Done():
			return conn.Err()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// printMessage prints a new message, or a notice, as one line: edits and
// deletions are left out. Without a room, the line starts with the
// message's.
func printMessage(out io.Writer, room string, m apron.Message) {
	if m.Body == nil || m.Deleted || m.PrevLogID != 0 {
		return
	}
	line := m.From.Name
	if line == "" {
		line = m.From.UserID
	}
	if room == "" {
		line = "#" + m.RoomID + " " + line
	}
	fmt.Fprintf(out, "%s: %s\n", line, m.Body.Text)
}
