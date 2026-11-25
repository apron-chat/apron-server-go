package main

import (
	"context"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/aprond"
	"github.com/apron-chat/apron-server-go/aprond/aprondtest"
)

// lines is an io.Writer that hands on each line written to it.
type lines chan string

func (l lines) Write(p []byte) (int, error) {
	l <- string(p)
	return len(p), nil
}

func (l lines) expect(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-l:
		if got != want {
			t.Fatalf("line %q, want %q", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("no line %q", want)
	}
}

func TestTail(t *testing.T) {
	server := aprondtest.Serve(t, aprond.DefaultConfig())
	ada := server.Guest("Ada")
	for _, text := range []string{"one", "two"} {
		if _, err := ada.Post("general", text); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	out := make(lines, 16)
	done := make(chan error, 1)
	go func() {
		done <- tail(ctx, server.WebSocketURL(), apron.AuthParams{Scheme: "guest", Name: "tail"}, "general", 20, out)
	}()
	// First the room's history, then what is posted after.
	out.expect(t, "Ada: one\n")
	out.expect(t, "Ada: two\n")
	if _, err := ada.Post("general", "three"); err != nil {
		t.Fatal(err)
	}
	out.expect(t, "Ada: three\n")

	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("tail: %v", err)
	}
	select {
	case extra := <-out:
		t.Fatalf("extra line %q", extra)
	default:
	}
}
