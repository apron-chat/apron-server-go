package main

import (
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/internal/wstest"
)

func TestAssistantAnswersMentions(t *testing.T) {
	a := &Assistant{}
	a.Settings.UserID, a.Settings.Name, a.Settings.Context, a.Settings.Timeout = "assistant", "Assistant", 20, time.Minute
	_, server := wstest.Serve(t, apron.DefaultConfig(), a)
	ada := wstest.Guest(t, server, "Ada")
	question, err := ada.Call("message", map[string]any{
		"room_id": "general",
		"body":    map[string]any{"text": "@assistant what is up?", "mentions": []any{"assistant"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The reply grows as an append embed, then its final snapshot has the
	// whole text.
	reply := ada.Message(func(p map[string]any) bool {
		replyTo, _ := p["reply_to"].(map[string]any)
		body, _ := p["body"].(map[string]any)
		return wstest.From(p) == "assistant" && replyTo["message_id"] == question["message_id"] && body["embeds"] == nil
	})
	if got := wstest.Text(reply); got != "You asked: @assistant what is up?" {
		t.Fatalf("reply = %q", got)
	}
}
