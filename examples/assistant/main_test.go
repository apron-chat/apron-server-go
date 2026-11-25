package main

import (
	"testing"

	"github.com/apron-chat/apron-server-go/aprond"
	"github.com/apron-chat/apron-server-go/aprond/aprondtest"
)

func TestAssistantAnswersMentions(t *testing.T) {
	// Its settings take their defaults: the bot is @assistant.
	server := aprondtest.Serve(t, aprond.DefaultConfig(), &Assistant{})
	ada := server.Guest("Ada")
	question, err := ada.Post("general", "@assistant what is up?", aprond.Mention("assistant"))
	if err != nil {
		t.Fatal(err)
	}
	// The reply grows as an append embed, then its final snapshot has the
	// whole text.
	reply := ada.Message(func(m aprond.Message) bool {
		return m.From.UserID == "assistant" && m.ReplyTo == question && m.Body["embeds"] == nil
	})
	if reply.Text != "You asked: @assistant what is up?" {
		t.Fatalf("reply = %q", reply.Text)
	}
}
