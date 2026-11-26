package main

import (
	"testing"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/aprond"
	"github.com/apron-chat/apron-server-go/aprond/aprondtest"
)

func TestEcho(t *testing.T) {
	server := aprondtest.Serve(t, aprond.DefaultConfig())
	account := server.Member("echo", "bot")
	echo, err := start(server.WebSocketURL(), apron.AuthParams{Scheme: "token", Token: account.Token})
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	if echo.UserID() != "echo" {
		t.Fatalf("signed in as %+v", echo.User())
	}

	ada := server.Guest("Ada")
	for _, post := range []struct {
		text    string
		options []aprond.PostOption
	}{
		{"@echo hi", []aprond.PostOption{aprond.Mention("echo")}},
		{"echo: hello", nil},
	} {
		question, err := ada.Post("general", post.text, post.options...)
		if err != nil {
			t.Fatal(err)
		}
		reply := ada.Message(func(m aprond.Message) bool { return m.From.UserID == "echo" && m.ReplyTo == question })
		if reply.Text != "You said: "+post.text {
			t.Fatalf("reply %q", reply.Text)
		}
	}
}
