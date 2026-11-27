package bot_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/bot"
	"github.com/apron-chat/apron-server-go/apron/internal/benchchat"
	"github.com/apron-chat/apron-server-go/apron/server"
)

// BenchmarkBotEchoStorm posts messages on a Local connection, as a busy room
// does, to a bot on another: every message reaches the bot, which decodes
// it, and every tenth names it, which it answers. An op is one message,
// handled by the bot.
func BenchmarkBotEchoStorm(b *testing.B) {
	chat := benchchat.New(server.Config{})
	defer chat.Server.Shutdown(context.Background())
	var seen, echoed atomic.Int64
	done := make(chan struct{}, 1)
	var target atomic.Int64
	echo, err := bot.Start(func(_ context.Context, onFrame func(apron.Frame)) (bot.Conn, error) {
		return chat.Server.Local(server.LocalOptions{User: apron.User{UserID: "echo", Name: "echo"}, OnFrame: onFrame})
	}, bot.Options{OnMessage: func(ctx context.Context, b *bot.Bot, m apron.Message) {
		if b.Addressed(m) {
			if _, err := b.Reply(ctx, m, "You said: "+m.Body.Text); err == nil {
				echoed.Add(1)
			}
		}
		if seen.Add(1) >= target.Load() {
			select {
			case done <- struct{}{}:
			default:
			}
		}
	}})
	if err != nil {
		b.Fatal(err)
	}
	defer echo.Close()
	user, err := chat.Server.Local(server.LocalOptions{User: apron.User{UserID: "ada", Name: "Ada"}, OnFrame: func(apron.Frame) {}})
	if err != nil {
		b.Fatal(err)
	}
	defer user.Close()
	plain := apron.MessageParams{Body: &apron.Body{Text: "Hello, everyone: this is a line of chat of a usual length."}}
	addressed := apron.MessageParams{Body: &apron.Body{Text: "echo: are you there?"}}
	ctx := context.Background()
	i := 0
	for b.Loop() {
		p := plain
		if i%10 == 0 {
			p = addressed
		}
		i++
		if _, err := user.Call(ctx, "message", p); err != nil {
			b.Fatal(err)
		}
		if i%256 == 0 {
			// The bot keeps up, rather than fall behind its queue.
			target.Store(int64(i))
			for seen.Load() < int64(i) {
				<-done
			}
		}
	}
	// The bot has seen every message, and its own echoes are not its.
	target.Store(int64(i))
	for seen.Load() < int64(i) {
		<-done
	}
}
