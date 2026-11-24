package main

import (
	"testing"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/aprontest"
)

func TestAssistantAnswersMentions(t *testing.T) {
	// Its settings take their defaults: the bot is @assistant.
	server := aprontest.Serve(t, apron.DefaultConfig(), &Assistant{})
	ada := server.Guest("Ada")
	var question struct {
		MessageID string `json:"message_id"`
	}
	if err := ada.Call("message", map[string]any{
		"room_id": "general",
		"body":    map[string]any{"text": "@assistant what is up?", "mentions": []string{"assistant"}},
	}, &question); err != nil {
		t.Fatal(err)
	}
	// The reply grows as an append embed, then its final snapshot has the
	// whole text.
	reply := ada.Message(func(m apron.Message) bool {
		return m.From.UserID == "assistant" && m.ReplyTo == question.MessageID && m.Body["embeds"] == nil
	})
	if reply.Text != "You asked: @assistant what is up?" {
		t.Fatalf("reply = %q", reply.Text)
	}
}
