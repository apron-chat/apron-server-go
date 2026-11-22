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
	c.Server = &wire.Server{Backend: c, AllowAnyOrigin: true}
	return c
}

func (*Chat) Hello(*wire.Conn) wire.ServerInfo {
	return wire.ServerInfo{Auth: []string{"guest"}, Agent: "apron-minimal/1", Title: "Minimal"}
}

func (c *Chat) Auth(_ context.Context, conn *wire.Conn, p wire.AuthParams) (wire.AuthResult, error) {
	c.mu.Lock()
	c.guests++
	id := fmt.Sprintf("guest_%d", c.guests)
	c.mu.Unlock()
	name := strings.TrimSpace(p.Name)
	if name == "" {
		name = id
	}
	you := wire.User{UserID: id, Name: name}
	// Sent after the auth result, though queued before it (§3.2.2).
	conn.SendMessage(wire.Notice(room, "Welcome, "+name+"! Try /help."))
	return wire.AuthResult{You: &you}, nil
}

func (c *Chat) Message(_ context.Context, conn *wire.Conn, p wire.MessageParams) (wire.MessageResult, error) {
	if p.RoomID != "" && p.RoomID != room {
		return wire.MessageResult{}, wire.InvalidParams("There is only one room, " + room)
	}
	if p.Body.Empty() {
		return wire.MessageResult{}, wire.InvalidParams("A new message needs text or embeds") // §3.5.1
	}
	// The log_id is assigned, and the message broadcast, under one lock,
	// so that every connection gets messages in log_id order (§1): sending
	// never blocks, and is meant to be done under the backend's lock.
	c.mu.Lock()
	defer c.mu.Unlock()
	if p.ReplyTo != nil && !c.messages[p.ReplyTo.MessageID] {
		return wire.MessageResult{}, wire.InvalidParams("reply_to names no message")
	}
	id := c.clock.Next()
	frame, err := wire.Encode("message", wire.Message{
		MessageID: id, LogID: id, RoomID: room, From: *conn.User(), Body: p.Body, ReplyTo: p.ReplyTo,
	})
	if err != nil {
		return wire.MessageResult{}, err
	}
	c.messages[id] = true
	// Every connection is in the one room; the sender's copy is queued
	// before the result.
	c.Server.Broadcast(frame, nil)
	return wire.MessageResult{MessageID: id}, nil
}

func (c *Chat) Me(_ context.Context, conn *wire.Conn, p wire.MeParams) (wire.User, error) {
	// Under the lock, as Message is, so that profile changes reach every
	// connection in the order they were made.
	c.mu.Lock()
	defer c.mu.Unlock()
	you := *conn.User()
	if p.Name != nil {
		you.Name = strings.TrimSpace(*p.Name)
	}
	if p.Avatar != nil {
		you.Avatar = *p.Avatar
	}
	frame, err := wire.Encode("user", map[string]any{"new": you})
	if err != nil {
		return you, err
	}
	conn.Notify("user", map[string]any{"you": you})
	c.Server.Broadcast(frame, func(other *wire.Conn) bool { return other != conn })
	return you, nil
}

func (c *Chat) Command(_ context.Context, conn *wire.Conn, p wire.MessageParams) (map[string]any, error) {
	switch strings.Fields(p.Body.Text + " ")[0] {
	case "/help":
		conn.SendMessage(wire.Notice(p.RoomID, "- `/help`: this list"))
		return nil, nil
	}
	return nil, wire.InvalidParams("Unknown command; try /help")
}
