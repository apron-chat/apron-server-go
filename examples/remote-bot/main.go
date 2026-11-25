// Command remote-bot is an echo bot on any Apron server, over the network:
// it answers each message that mentions it, or addresses it by name, such
// as "echo: hi", with what it said. It is package apron/bot on a
// connection of package apron/client, which the bot opens again whenever
// the server ends it:
//
//	APRON_TOKEN=... go run ./examples/remote-bot --url wss://chat.example/ws
//
// It signs in with the token in $APRON_TOKEN (§3.2.1), such as a bot
// account's, or, without one, as a guest named --name. A bot in the same
// program as its server, such as examples/minimal's, opens the same Bot on
// an in-process connection instead.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/bot"
	"github.com/apron-chat/apron-server-go/apron/client"
)

func main() {
	log.SetFlags(0)
	url := flag.String("url", os.Getenv("APRON_URL"), "the server's address, such as wss://chat.example/ws; $APRON_URL by default")
	name := flag.String("name", "echo", "the name to sign in as a guest with, without $APRON_TOKEN")
	flag.Parse()
	auth := apron.AuthParams{Scheme: "guest", Name: *name, Agent: "apron-remote-bot"}
	if token := os.Getenv("APRON_TOKEN"); token != "" {
		auth = apron.AuthParams{Scheme: "token", Token: token, Agent: "apron-remote-bot"}
	}
	echo, err := start(*url, auth)
	if err != nil {
		log.Fatalf("remote-bot: %v", err)
	}
	log.Printf("remote-bot: signed in as %s (@%s)", echo.User().Name, echo.UserID())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	<-ctx.Done()
	echo.Close()
}

// start starts the echo bot on the server at url, signed in with auth.
func start(url string, auth apron.AuthParams) (*bot.Bot, error) {
	dial := func(ctx context.Context, onFrame func(apron.Frame)) (bot.Conn, error) {
		return client.Dial(ctx, url, auth, client.OnFrame(onFrame))
	}
	return bot.Start(dial, bot.Options{
		OnMessage: func(ctx context.Context, b *bot.Bot, m apron.Message) {
			if b.Addressed(m) {
				b.Reply(ctx, m, "You said: "+m.Body.Text)
			}
		},
	})
}
