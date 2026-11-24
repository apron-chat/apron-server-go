// Command minimal is a whole Apron server in about a hundred lines, built on
// package wire rather than package apron: one room, guests, profiles,
// commands (package wire/commands), and a bot that answers its mentions
// (package wire/bot), with its state in a map. It is the starting point
// for a server that keeps its own state, such as a bridge or a gateway in
// front of another chat system.
//
//	go run ./examples/minimal --addr 127.0.0.1:8080
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/apron-chat/apron-server-go/wire"
	"github.com/apron-chat/apron-server-go/wire/bot"
	"github.com/apron-chat/apron-server-go/wire/commands"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "address to listen on")
	flag.Parse()
	chat := NewChat()
	mux := http.NewServeMux()
	mux.Handle("/ws", chat.Server)
	log.Printf("listening on ws://%s/ws", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

// Chat is a one-room chat: the backend of its wire.Server.
type Chat struct {
	Server   *wire.Server
	clock    wire.Clock
	commands commands.Router

	mu       sync.Mutex
	guests   int
	messages map[wire.LogID]bool
}

const room = "general"

func NewChat() *Chat {
	c := &Chat{messages: make(map[wire.LogID]bool)}
	// The Router answers /help, and lists the commands for clients that
	// complete them (ext:command_list).
	c.commands.MustAdd(commands.Command{
		Name:  "nick",
		Forms: []commands.Form{{Usage: "/nick <name>", Help: "change your name"}},
		Run:   c.nick,
	})
	c.Server = &wire.Server{
		Backend:  c,
		Messages: c,
		Profiles: c,
		Commands: &c.commands,
		// Browsers open a WebSocket from the page's origin, and by
		// default only the server's own may: a web client served from
		// elsewhere, such as a dev server on another port, needs its
		// origin let in, as wire.OriginPatterns("localhost:*") does. This
		// server signs in guests, with no cookies for another site's page
		// to use, so it lets in any.
		CheckOrigin: wire.AnyOrigin,
	}
	c.Server.Handle(commands.ListMethod, c.commands.ListHandler())
	// The bot is a user on a connection of its own, in process: what it
	// posts goes through Message, as anyone's does. Its connection is the
	// Server's first, which takes the Server's configuration, so it
	// starts once the fields are set.
	if _, err := bot.Start(c.Server, bot.Options{User: wire.User{UserID: "echo", Name: "Echo"}, OnMessage: echo}); err != nil {
		panic(err) // A Server that is not shut down opens it.
	}
	return c
}

// echo is the bot's: it answers a message that mentions it, or names it
// in its text, as in "echo: hi" from a client that sends no mentions, with
// what it said, as a reply.
func echo(ctx context.Context, b *bot.Bot, m wire.Message) {
	if b.Addressed(m) {
		_, _ = b.Reply(ctx, m, m.From.Name+" said: "+m.Body.Text)
	}
}

func (*Chat) Hello(context.Context, *wire.Conn) wire.ServerInfo {
	return wire.ServerInfo{Auth: []string{"guest"}, Agent: "apron-minimal/1", Title: "Minimal"}
}

// Each handler replies with reply.Send under c.mu, as it changes what the
// lock guards: sending never blocks, so every connection gets the frames in
// the order of the changes (§1), and a connection is signed in as Send
// queues its auth result, before anything broadcast after it.

func (c *Chat) Auth(_ context.Context, _ *wire.Conn, p wire.AuthParams, reply wire.Reply[wire.AuthResult]) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.guests++
	id := fmt.Sprintf("guest_%d", c.guests)
	name := strings.TrimSpace(p.Name)
	if name == "" {
		name = id
	}
	welcome, err := wire.Encode(wire.Notice(room, "Welcome, "+name+"! Try /help."))
	if err != nil {
		return err
	}
	// The welcome goes right after the result (§3.2.2).
	return reply.Send(wire.AuthResult{You: &wire.CurrentUser{User: wire.User{UserID: id, Name: name}}}, welcome)
}

func (c *Chat) Message(_ context.Context, conn *wire.Conn, p wire.MessageParams, reply wire.Reply[wire.MessageResult]) error {
	if p.RoomID != "" && p.RoomID != room {
		return wire.InvalidParams("There is only one room, " + room)
	}
	if p.Body.Empty() {
		return wire.InvalidParams("A new message needs text or embeds") // §3.5.1
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if p.ReplyTo != nil && !c.messages[p.ReplyTo.MessageID] {
		return wire.InvalidParams("reply_to names no message")
	}
	id := c.clock.Next()
	frame, err := wire.Encode(wire.Message{
		MessageID: id, LogID: id, RoomID: room, From: *conn.User(), Body: p.Body, ReplyTo: p.ReplyTo,
	})
	if err != nil {
		return err
	}
	c.messages[id] = true
	// Every connection is in the one room; the sender's copy goes before
	// the result.
	c.Server.Broadcast(frame, nil)
	return reply.Send(wire.MessageResult{MessageID: id})
}

// Me changes the user's profile (§3.3.2). SendMe answers with the
// complete object, the fields the change cleared as their empty values,
// and tells what changed to the user's other connections as you, and to
// everyone else as new (§3.3.3): everyone shares the one room.
func (c *Chat) Me(_ context.Context, conn *wire.Conn, p wire.MeParams, reply wire.Reply[wire.CurrentUser]) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	old := *conn.User()
	you := wire.ApplyMe(old, p)
	you.Name = strings.TrimSpace(you.Name)
	return c.Server.SendMe(reply, old, you, nil)
}

// nick is /nick <name>: a rename, as me does it. Its effect, the user
// update, goes out before the result (§4.1).
func (c *Chat) nick(_ context.Context, call *commands.Call) error {
	if call.Args == "" {
		return call.UsageError("")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	old := *call.Conn.User()
	you := old
	you.Name = call.Args
	c.Server.PublishUser(old, you, nil)
	return call.Done()
}
