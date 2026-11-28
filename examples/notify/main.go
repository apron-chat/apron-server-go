// Command notify posts one message to a room of any Apron server, and
// exits: a notice from CI, a cron job, or a script. The text is its
// arguments, or else its standard input:
//
//	export APRON_URL=wss://chat.example/ws APRON_TOKEN=...
//	go run ./examples/notify --room deploys "Deployed $(git rev-parse --short HEAD)"
//	make test 2>&1 | tail -n 5 | go run ./examples/notify --room ci
//
// It signs in with the token in $APRON_TOKEN (§3.2.1), such as a bot
// account's, or, without one, as a guest named --name.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/client"
)

func main() {
	log.SetFlags(0)
	if err := run(os.Args[1:], os.Stdin, os.Getenv); err != nil {
		log.Fatalf("notify: %v", err)
	}
}

// run posts the message that args and stdin say, on the server that the
// flags and the environment, read with getenv, name.
func run(args []string, stdin io.Reader, getenv func(string) string) error {
	flags := flag.NewFlagSet("notify", flag.ContinueOnError)
	url := flags.String("url", getenv("APRON_URL"), "the server's address, such as wss://chat.example/ws; $APRON_URL by default")
	room := flags.String("room", "", "the room_id to post to; empty is the server's default room")
	name := flags.String("name", "notify", "the name to sign in as a guest with, without $APRON_TOKEN")
	wait := flags.Duration("timeout", 30*time.Second, "how long to try for")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *url == "" {
		return errors.New("no server: set --url or $APRON_URL")
	}
	text := strings.Join(flags.Args(), " ")
	if text == "" {
		read, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		text = strings.TrimSpace(string(read))
	}
	if text == "" {
		return errors.New("nothing to post")
	}
	auth := apron.AuthParams{Scheme: "guest", Name: *name, Agent: "apron-notify"}
	if token := getenv("APRON_TOKEN"); token != "" {
		auth = apron.AuthParams{Scheme: "token", Token: token, Agent: "apron-notify"}
	}

	ctx, cancel := context.WithTimeout(context.Background(), *wait)
	defer cancel()
	return notify(ctx, *url, auth, *room, text)
}

// notify signs in to the server at url with auth, and posts text to room.
func notify(ctx context.Context, url string, auth apron.AuthParams, room, text string) error {
	conn, err := client.Dial(ctx, url, auth)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.Post(ctx, apron.MessageParams{RoomID: room, Body: apron.Body{Text: text}}); err != nil {
		return fmt.Errorf("posting to %q: %w", room, err)
	}
	return nil
}
