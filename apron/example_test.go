package apron_test

import (
	"context"
	"log"
	"net/http"
	"strings"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/store"
)

// A program serves Apron itself: here, beside its own pages, with its state
// in SQLite.
func ExampleOpen() {
	config := apron.DefaultConfig()
	config.PublicURL = "https://chat.example"
	config.OriginPatterns = []string{"https://chat.example"}
	var err error
	if config.Store, err = store.Open("sqlite:chat.db"); err != nil {
		log.Fatal(err)
	}
	app, err := apron.Open(config)
	if err != nil {
		log.Fatal(err)
	}
	defer app.Shutdown(context.Background())

	mux := http.NewServeMux()
	mux.Handle("/", app.Handler()) // /ws and the server's own routes
	mux.HandleFunc("/about", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("Our chat server"))
	})
	log.Fatal(http.ListenAndServe(":8080", mux))
}

// Shout is an extension with a command, a filter, and a bot.
type Shout struct{}

func (Shout) Name() string { return "shout" }

func (Shout) Init(r *apron.Registrar) error {
	// /shout replies to the person who ran it.
	r.Command(apron.Command{
		Name: "shout", Usage: "/shout <text>", Help: "say it louder",
		Run: func(_ context.Context, call *apron.CommandCall) (map[string]any, error) {
			if call.Args == "" {
				return nil, apron.InvalidParams("Usage: /shout <text>")
			}
			call.Notice(strings.ToUpper(call.Args) + "!")
			return nil, nil
		},
	})
	// Nobody may post in all capitals.
	r.FilterMessage(func(_ context.Context, d *apron.Draft) error {
		if text := d.Text(); len(text) > 8 && text == strings.ToUpper(text) {
			return apron.Denied("Please don't shout; try /shout")
		}
		return nil
	})
	// A bot greets everyone in the default room once the server serves.
	bot, err := r.Bot("herald", apron.BotOptions{Name: "Herald"})
	if err != nil {
		return err
	}
	r.Go(func(ctx context.Context) {
		bot.Post(ctx, "general", "The server is up.")
	})
	return nil
}

// An extension goes in Config.Extensions, or to aproncmd.Main.
func Example_extension() {
	config := apron.DefaultConfig()
	config.Extensions = []apron.Extension{Shout{}}
	app, err := apron.Open(config)
	if err != nil {
		log.Fatal(err)
	}
	defer app.Shutdown(context.Background())
	log.Fatal(http.ListenAndServe("127.0.0.1:8080", app))
}
