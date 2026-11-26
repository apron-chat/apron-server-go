package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/aprond"
	"github.com/apron-chat/apron-server-go/aprond/aprondtest"
)

func TestNotify(t *testing.T) {
	server := aprondtest.Serve(t, aprond.DefaultConfig())
	ci := server.Member("ci")
	ada := server.Guest("Ada")
	env := map[string]string{"APRON_URL": server.WebSocketURL(), "APRON_TOKEN": ci.Token}

	// With a token, as its account; the text from the arguments.
	if err := run([]string{"--room", "general", "Deployed", "v1.2"}, strings.NewReader(""), func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	m := ada.Message(func(m aprond.Message) bool { return m.From.UserID == "ci" })
	if m.Text != "Deployed v1.2" || m.RoomID != "general" {
		t.Fatalf("message %+v", m)
	}

	// Without one, as a guest; the text from stdin, in the default room.
	delete(env, "APRON_TOKEN")
	if err := run([]string{"--name", "Build"}, strings.NewReader("2 tests failed\n"), func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	m = ada.Message(func(m aprond.Message) bool { return m.From.Name == "Build" })
	if m.Text != "2 tests failed" {
		t.Fatalf("message %+v", m)
	}

	// A token the server does not take is its error reply.
	env["APRON_TOKEN"] = "nonsense"
	err := run([]string{"hi"}, nil, func(k string) string { return env[k] })
	if _, ok := errors.AsType[*apron.Error](err); !ok {
		t.Fatalf("bad token: %v", err)
	}
	if err := run(nil, strings.NewReader(""), func(k string) string { return env[k] }); err == nil {
		t.Fatal("nothing to post: no error")
	}
}
