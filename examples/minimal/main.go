// Command minimal is a whole Apron server in about a hundred lines, built on
// package wire rather than package apron: one room, guests, profiles, and a
// /help command, with its state in a map. It is the starting point for a
// server that keeps its own state, such as a bridge or a gateway in front of
// another chat system.
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
	Server *wire.Server
	clock  wire.Clock

	mu       sync.Mutex
	guests   int
	messages map[wire.LogID]bool
}

const room = "general"

func NewChat() *Chat {
	c := &Chat{messages: make(map[wire.LogID]bool)}
	c.Server = &wire.Server{
		Backend:     c,
		Messages:    c,
		Profiles:    c,
		Commands:    c,
		CheckOrigin: wire.AnyOrigin,
	}
	return c
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
	return reply.Send(wire.AuthResult{You: &wire.User{UserID: id, Name: name}}, welcome)
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

func (c *Chat) Me(_ context.Context, conn *wire.Conn, p wire.MeParams, reply wire.Reply[wire.CurrentUser]) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	old := *conn.User()
	you := old
	if p.Name != nil {
		you.Name = strings.TrimSpace(*p.Name)
	}
	if p.Avatar != nil {
		you.Avatar = *p.Avatar
	}
	// What changed goes to every connection, "you" to the user's own
	// (§3.3.3); everyone shares the one room.
	c.Server.PublishUser(old, you, nil)
	// The result is the complete object, with the fields cleared as their
	// empty values (§3.3.2).
	patch, _ := you.Patch(old)
	return reply.Send(wire.CurrentUser{User: you, Cleared: patch.Cleared})
}

func (c *Chat) Command(_ context.Context, conn *wire.Conn, p wire.CommandParams, reply wire.Reply[wire.CommandResult]) error {
	switch strings.Fields(p.Body.Text + " ")[0] {
	case "/help":
		conn.Notify(wire.Notice(p.RoomID, "- `/help`: this list"))
		return reply.Send(wire.CommandResult{})
	}
	return wire.InvalidParams("Unknown command; try /help")
}
