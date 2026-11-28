package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/client"
	"github.com/apron-chat/apron-server-go/aprond"
	"github.com/apron-chat/apron-server-go/aprond/aprondtest"
)

// aproncli runs a command line against server, with stdin, and returns
// what it printed and its error.
func aproncli(t *testing.T, server *aprondtest.Server, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var out, errOut bytes.Buffer
	env := env{strings.NewReader(stdin), &out, &errOut, func(name string) string {
		if name == "APRON_URL" {
			return server.WebSocketURL()
		}
		return ""
	}}
	err = run(ctx, args, env)
	return out.String(), errOut.String(), err
}

// ok runs a command line that succeeds, and returns its output.
func ok(t *testing.T, server *aprondtest.Server, args ...string) string {
	t.Helper()
	out, errOut, err := aproncli(t, server, "", args...)
	if err != nil {
		t.Fatalf("aproncli %s: %v\n%s", strings.Join(args, " "), err, errOut)
	}
	return out
}

// conn connects to server as a client of the library would, to check
// what aproncli did.
func conn(t *testing.T, server *aprondtest.Server, auth apron.AuthParams) *client.Conn {
	t.Helper()
	c, err := client.Dial(t.Context(), server.WebSocketURL(), auth)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// messages returns every message of a room, by replay of its whole log.
func messages(t *testing.T, c *client.Conn, room string) []apron.Message {
	t.Helper()
	var state apron.Replay
	zero := apron.LogID(0)
	for page, err := range c.HistoryPages(t.Context(), apron.HistoryParams{RoomID: room, After: &zero}) {
		if err != nil {
			t.Fatal(err)
		}
		state.ApplyHistory(page)
	}
	return state.Messages(room)
}

// The command line's usage, info, and sign-in, on one server: --help for
// each command, info signed out and in, and the errors that say how to
// sign in. Without a token or a guest name, a command that needs a
// sign-in says how; a scheme the server does not offer is named.
func TestCommandLine(t *testing.T) {
	server := aprondtest.Serve(t, aprond.DefaultConfig())
	t.Run("help", func(t *testing.T) {
		_, errOut, err := aproncli(t, server, "", "--help")
		if !errors.Is(err, flag.ErrHelp) || !strings.Contains(errOut, "Usage: aproncli [flags] COMMAND") || !strings.Contains(errOut, "export") {
			t.Fatalf("--help: %v\n%s", err, errOut)
		}
		for _, cmd := range commands {
			_, errOut, _ := aproncli(t, server, "", cmd.name, "--help")
			if !strings.HasPrefix(errOut, "Usage: aproncli "+cmd.name+" ") {
				t.Errorf("%s --help:\n%s", cmd.name, errOut)
			}
		}
		if _, _, err := aproncli(t, server, "", "nonsense"); !errors.Is(err, errUsage) {
			t.Fatalf("an unknown command: %v", err)
		}
		if _, _, err := aproncli(t, server, "", "join"); !errors.Is(err, errUsage) {
			t.Fatalf("join without a room: %v", err)
		}
	})
	t.Run("info", func(t *testing.T) {
		out := ok(t, server, "info")
		if !strings.Contains(out, "you           not signed in") || !strings.Contains(out, "history") || !strings.Contains(out, "auth          ") {
			t.Fatalf("info signed out:\n%s", out)
		}
		out = ok(t, server, "--guest", "Ada", "info")
		if !strings.Contains(out, "you           Ada (@guest_") {
			t.Fatalf("info as a guest:\n%s", out)
		}
		var got struct {
			Server apron.ServerInfo `json:"server"`
			You    apron.User       `json:"you"`
		}
		if err := json.Unmarshal([]byte(ok(t, server, "--guest", "Ada", "info", "--json")), &got); err != nil || got.Server.Apron != apron.Version || got.You.Name != "Ada" {
			t.Fatalf("info --json: %+v, %v", got, err)
		}
	})
	t.Run("sign-in", func(t *testing.T) {
		if _, _, err := aproncli(t, server, "", "message", "hi"); err == nil || !strings.Contains(err.Error(), "--guest NAME") {
			t.Fatalf("no sign-in: %v", err)
		}
		if _, _, err := aproncli(t, server, "", "--token", "nonsense", "message", "hi"); err == nil || !strings.Contains(err.Error(), "token") {
			t.Fatalf("a bad token: %v", err)
		}
		config := aprond.DefaultConfig()
		config.DisableGuests, config.EmailSender = true, &aprondtest.Mailbox{}
		closed := aprondtest.Serve(t, config)
		_, _, err := aproncli(t, closed, "", "--guest", "Ada", "message", "hi")
		if err == nil || !strings.Contains(err.Error(), "does not offer sign-in with guest") {
			t.Fatalf("no guests: %v", err)
		}
		if _, _, err := aproncli(t, server, "", "--url", "", "info"); err == nil || !strings.Contains(err.Error(), "--url") {
			t.Fatalf("no url: %v", err)
		}
	})
}

func TestMessage(t *testing.T) {
	server := aprondtest.Serve(t, aprond.DefaultConfig())
	ada := server.Member("ada")
	plain := strings.TrimSpace(ok(t, server, "--token", ada.Token, "message", "hello", "world"))
	marked := strings.TrimSpace(ok(t, server, "--token", ada.Token, "message", "--room", "general", "--format", "markdown", "--reply-to", plain, "some *bold*"))
	out, errOut, err := aproncli(t, server, "line one\nline two\n", "--token", ada.Token, "message", "-")
	if err != nil {
		t.Fatalf("message -: %v\n%s", err, errOut)
	}
	piped := strings.TrimSpace(out)

	got := messages(t, conn(t, server, apron.AuthParams{Scheme: "guest"}), "general")
	if len(got) != 3 {
		t.Fatalf("messages %+v", got)
	}
	for i, want := range []struct {
		id, text, format string
		replyTo          string
	}{
		{plain, "hello world", "", ""},
		{marked, "some *bold*", apron.Markdown, plain},
		{piped, "line one\nline two", "", ""},
	} {
		m := got[i]
		replyTo := ""
		if m.ReplyTo.MessageID != 0 {
			replyTo = m.ReplyTo.MessageID.String()
		}
		if m.MessageID.String() != want.id || m.Body.Text != want.text || m.Body.Format != want.format || replyTo != want.replyTo || m.From.UserID != "ada" {
			t.Errorf("message %d: %+v %+v, want %+v", i, m, m.Body, want)
		}
	}
	if _, _, err := aproncli(t, server, "", "--token", ada.Token, "message", "--format", "html", "<b>"); err == nil {
		t.Fatal("--format html")
	}
	if _, _, err := aproncli(t, server, "", "--token", ada.Token, "message", "--room", "nowhere", "hi"); err == nil || !strings.Contains(err.Error(), "error/invalid_params") {
		t.Fatalf("an unknown room: %v", err)
	}
}

// A request refused with retry_after waits out the limit, as long as
// --max-wait allows.
func TestMessageRetryAfter(t *testing.T) {
	config := aprond.DefaultConfig()
	config.MessagesPerMinute = 60 // a burst of 60, then one a second
	server := aprondtest.Serve(t, config)
	ada := server.Member("ada")
	for i := 0; ; i++ {
		if _, err := ada.Post("general", fmt.Sprint(i)); err != nil {
			if apron.CodeOf(err) != apron.CodeRetryAfter {
				t.Fatal(err)
			}
			break
		}
	}
	if _, _, err := aproncli(t, server, "", "--token", ada.Token, "--max-wait", "0", "message", "now"); err == nil || !strings.Contains(err.Error(), "error/retry_after") {
		t.Fatalf("without waiting: %v", err)
	}
	start := time.Now()
	ok(t, server, "--token", ada.Token, "message", "later")
	if took := time.Since(start); took < 100*time.Millisecond {
		t.Errorf("posted after %v: it did not wait", took)
	}
	got := messages(t, conn(t, server, apron.AuthParams{Scheme: "guest"}), "general")
	if last := got[len(got)-1]; last.Body.Text != "later" {
		t.Fatalf("last message %+v", last.Body)
	}
}

func TestRooms(t *testing.T) {
	server := aprondtest.Serve(t, aprond.DefaultConfig())
	bob := server.Member("bob")
	ada := server.Member("ada")
	room, err := bob.CreateRoom("Deploys", false)
	if err != nil {
		t.Fatal(err)
	}
	as := func(args ...string) string { return ok(t, server, append([]string{"--token", ada.Token}, args...)...) }
	listed := func(out, room, joined string) bool {
		for _, line := range strings.Split(out, "\n") {
			if fields := strings.Fields(line); len(fields) > 0 && fields[0] == room && fields[len(fields)-1] == joined {
				return true
			}
		}
		return false
	}
	out := as("rooms")
	if !listed(out, "general", "yes") || !listed(out, room, "no") || !strings.Contains(out, "Deploys") {
		t.Fatalf("rooms:\n%s", out)
	}
	if out := as("rooms", "--joined"); listed(out, room, "no") || listed(out, room, "yes") {
		t.Fatalf("rooms --joined before the join:\n%s", out)
	}
	as("join", room)
	if out := as("rooms", "--joined"); !listed(out, room, "yes") {
		t.Fatalf("rooms --joined after the join:\n%s", out)
	}
	var result apron.RoomListResult
	if err := json.Unmarshal([]byte(as("rooms", "--joined", "--json")), &result); err != nil || len(result.Joined) != 2 {
		t.Fatalf("rooms --json: %+v, %v", result, err)
	}
	as("leave", room)
	if out := as("rooms", "--joined"); listed(out, room, "yes") {
		t.Fatalf("rooms --joined after the leave:\n%s", out)
	}
	if _, _, err := aproncli(t, server, "", "--token", ada.Token, "join", "nowhere"); err == nil {
		t.Fatal("joined a room that is not")
	}
}

// history reads back across pages until it has the messages asked for.
func TestHistory(t *testing.T) {
	server := aprondtest.Serve(t, aprond.DefaultConfig())
	ada := server.Guest("Ada")
	var ids []string
	for i := range 30 {
		id, err := ada.Post("general", fmt.Sprintf("message %d", i))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := ada.Edit("general", ids[28], "message 28, edited"); err != nil {
		t.Fatal(err)
	}
	if err := ada.Delete("general", ids[29]); err != nil {
		t.Fatal(err)
	}
	// Pages of 4 changes, of messages, edits, and memberships: several.
	out := ok(t, server, "--guest", "Bob", "history", "--room", "general", "--limit", "12", "--page", "4")
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 12 {
		t.Fatalf("%d lines:\n%s", len(lines), out)
	}
	for i, line := range lines[:10] {
		if want := fmt.Sprintf(" Ada: message %d", 18+i); !strings.HasSuffix(line, want) {
			t.Errorf("line %d %q, want ...%q", i, line, want)
		}
	}
	if !strings.HasSuffix(lines[10], "Ada: message 28, edited (edited)") || !strings.HasSuffix(lines[11], "Ada: (deleted)") {
		t.Errorf("the last lines:\n%s\n%s", lines[10], lines[11])
	}

	out = ok(t, server, "--guest", "Bob", "history", "--room", "general", "--limit", "3", "--json")
	var got []apron.Message
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		var m apron.Message
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		got = append(got, m)
	}
	if len(got) != 3 || got[0].Body.Text != "message 27" || !got[2].Deleted {
		t.Fatalf("history --json: %+v", got)
	}
	if _, _, err := aproncli(t, server, "", "--guest", "Bob", "history"); !errors.Is(err, errUsage) {
		t.Fatalf("history without --room: %v", err)
	}
}

// lines is an io.Writer that hands on each line written to it.
type lines chan string

func (l lines) Write(p []byte) (int, error) {
	for line := range strings.SplitSeq(strings.TrimSuffix(string(p), "\n"), "\n") {
		l <- line
	}
	return len(p), nil
}

func (l lines) next(t *testing.T) string {
	t.Helper()
	select {
	case line := <-l:
		return line
	case <-time.After(10 * time.Second):
		t.Fatal("no line")
		return ""
	}
}

// tail prints the latest messages, then those posted after, each once.
func TestTail(t *testing.T) {
	server := aprondtest.Serve(t, aprond.DefaultConfig())
	ada := server.Guest("Ada")
	for _, text := range []string{"one", "two", "three"} {
		if _, err := ada.Post("general", text); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	out := make(lines, 64)
	var errOut bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"--url", server.WebSocketURL(), "--guest", "tail", "tail", "--room", "general", "--limit", "2"},
			env{strings.NewReader(""), out, &errOut, func(string) string { return "" }})
	}()
	for _, want := range []string{" Ada: two", " Ada: three"} {
		if line := out.next(t); !strings.HasSuffix(line, want) {
			t.Fatalf("line %q, want ...%q", line, want)
		}
	}
	if _, err := ada.Post("general", "four"); err != nil {
		t.Fatal(err)
	}
	if line := out.next(t); !strings.HasSuffix(line, " Ada: four") {
		t.Fatalf("live line %q", line)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("tail: %v\n%s", err, errOut.String())
	}
	select {
	case extra := <-out:
		t.Fatalf("extra line %q", extra)
	default:
	}
}

// tail --json prints live messages as JSON lines, from every room.
func TestTailJSON(t *testing.T) {
	server := aprondtest.Serve(t, aprond.DefaultConfig())
	ada := server.Guest("Ada")
	ctx, cancel := context.WithCancel(t.Context())
	out := make(lines, 64)
	var wg sync.WaitGroup
	wg.Go(func() {
		run(ctx, []string{"--url", server.WebSocketURL(), "--guest", "tail", "tail", "--json", "--limit", "0"},
			env{strings.NewReader(""), out, io.Discard, func(string) string { return "" }})
	})
	defer func() {
		cancel()
		wg.Wait()
	}()
	// The tail is connected once its own join is in the room's log.
	watch := conn(t, server, apron.AuthParams{Scheme: "guest", Name: "watch"})
	for joined := false; !joined; time.Sleep(10 * time.Millisecond) {
		page, err := watch.History(ctx, apron.HistoryParams{RoomID: "general", Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range page.Memberships {
			joined = joined || m.Members[0].User.Name == "tail" && m.Members[0].Joined
		}
	}
	if _, err := ada.Post("general", "live"); err != nil {
		t.Fatal(err)
	}
	var m apron.Message
	if err := json.Unmarshal([]byte(out.next(t)), &m); err != nil || m.Body.Text != "live" || m.RoomID != "general" {
		t.Fatalf("tail --json: %+v, %v", m, err)
	}
}

func TestCall(t *testing.T) {
	server := aprondtest.Serve(t, aprond.DefaultConfig())
	out := ok(t, server, "--guest", "Ada", "call", "room_list", `{"filter":"joined"}`)
	var listed apron.RoomListResult
	if err := json.Unmarshal([]byte(out), &listed); err != nil || len(listed.Joined) != 1 || listed.Joined[0].RoomID != "general" {
		t.Fatalf("call room_list: %q, %v", out, err)
	}
	out, _, err := aproncli(t, server, `{"room_id":"general","body":{"text":"from stdin"}}`, "--guest", "Ada", "call", "message", "-")
	if err != nil || !strings.Contains(out, `"message_id"`) {
		t.Fatalf("call message -: %q, %v", out, err)
	}
	// auth runs signed out, as the request itself signs in.
	if out := ok(t, server, "call", "auth", `{"scheme":"guest","name":"Raw"}`); !strings.Contains(out, `"name":"Raw"`) {
		t.Fatalf("call auth: %q", out)
	}
	if _, _, err := aproncli(t, server, "", "--guest", "Ada", "call", "nonsense"); err == nil || !strings.Contains(err.Error(), "error/unsupported") {
		t.Fatalf("an unknown method: %v", err)
	}
	if _, _, err := aproncli(t, server, "", "--guest", "Ada", "call", "room_list", "[1]"); err == nil {
		t.Fatal("params that are not an object")
	}
}
