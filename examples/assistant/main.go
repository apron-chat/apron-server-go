// Command assistant is an Apron server with a bot that answers when
// someone mentions it, streaming its reply into the message as it is
// written (an append embed, PROTOCOL.md §4.8.5). The reply comes from a
// command of your choice, which reads the conversation on its stdin and
// writes the answer to its stdout, such as a local model:
//
//	go run ./examples/assistant --assistant.command "ollama run llama3.2"
//
// Without a command, the bot is a rubber duck that repeats the question.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/aproncmd"
)

func main() { aproncmd.Main(&Assistant{}) }

// Assistant is a bot that answers mentions.
type Assistant struct {
	Settings struct {
		UserID  string        `long:"user-id" default:"assistant" description:"The bot's user_id, which people mention"`
		Name    string        `long:"name" default:"Assistant" description:"The bot's display name"`
		Command string        `long:"command" description:"A command that reads the conversation on stdin and writes the reply to stdout, split into words without a shell; empty repeats the question"`
		Context int           `long:"context" default:"20" description:"How many earlier messages the command reads"`
		Timeout time.Duration `long:"timeout" default:"2m" description:"How long one reply may take"`
	}
	host *apron.Host
	bot  *apron.Bot
}

func (*Assistant) Name() string { return "assistant" }
func (a *Assistant) Flags() any { return &a.Settings }

func (a *Assistant) Init(r *apron.Registrar) error {
	a.host = r.Host
	// Settings left at zero, as when the extension is used without
	// aproncmd's flags, take the flags' defaults.
	if a.Settings.UserID == "" {
		a.Settings.UserID = "assistant"
	}
	if a.Settings.Name == "" {
		a.Settings.Name = "Assistant"
	}
	if a.Settings.Context <= 0 {
		a.Settings.Context = 20
	}
	if a.Settings.Timeout <= 0 {
		a.Settings.Timeout = 2 * time.Minute
	}
	var err error
	if a.bot, err = r.Bot(a.Settings.UserID, apron.BotOptions{Name: a.Settings.Name}); err != nil {
		return err
	}
	// The bot joins every room that is not restricted, so that it sees its
	// mentions there.
	for _, room := range r.Rooms() {
		if !room.Restricted {
			_ = a.bot.Join(r.Context(), room.ID)
		}
	}
	r.OnEvent(func(ctx context.Context, e apron.Event) {
		if e.Kind == apron.EventRoom {
			if room, ok := a.host.Room(e.Rooms[0]); ok && !room.Restricted {
				_ = a.bot.Join(ctx, room.ID)
			}
			return
		}
		m, ok := apron.DecodeMessage(e)
		if !ok || !m.New() || m.Deleted || m.From.UserID == a.bot.UserID() || !m.MentionsUser(a.bot.UserID()) {
			return
		}
		// A mention in a room the bot may not see, such as a private room
		// it is not in, gets no answer: its conversation is not the bot's
		// to read.
		if bot, _ := a.host.User(a.bot.UserID()); !a.host.Visible(bot, m.RoomID) {
			return
		}
		// Replies take a while: they run on their own, so that the bot
		// keeps reading.
		a.host.Go(func(ctx context.Context) { a.answer(ctx, m) })
	})
	return nil
}

func (a *Assistant) answer(ctx context.Context, question apron.Message) {
	ctx, cancel := context.WithTimeout(ctx, a.Settings.Timeout)
	defer cancel()
	earlier, err := a.bot.History(ctx, question.RoomID, apron.HistoryOptions{Before: question.LogID, Limit: a.Settings.Context})
	if err != nil {
		a.host.Logger().Error("reading the conversation", "error", err)
		return
	}
	w, err := a.bot.StreamAppend(ctx, question.RoomID, "", apron.ReplyTo(question.ID))
	if err != nil {
		a.host.Logger().Error("starting a reply", "error", err)
		return
	}
	defer w.Close()
	if err := a.reply(ctx, transcript(append(earlier, question)), w); err != nil && !errors.Is(err, apron.ErrLiveTextEnded) {
		a.host.Logger().Error("replying", "error", err)
		fmt.Fprintf(w, "\n\n_(I could not finish: %s)_", err)
	}
}

// transcript renders the conversation as the command reads it: one
// "name: text" paragraph per message, the question last.
func transcript(messages []apron.Message) string {
	var b strings.Builder
	for _, m := range messages {
		if m.Deleted || strings.HasPrefix(m.From.UserID, "~") {
			continue
		}
		name := m.From.Name
		if name == "" {
			name = m.From.UserID
		}
		fmt.Fprintf(&b, "%s: %s\n\n", name, m.Text)
	}
	return b.String()
}

func (a *Assistant) reply(ctx context.Context, conversation string, w io.Writer) error {
	argv := strings.Fields(a.Settings.Command)
	if len(argv) == 0 {
		return duck(ctx, conversation, w)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = strings.NewReader(conversation)
	cmd.Stdout = w
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// duck repeats the question back a word at a time, as a model would stream
// its answer.
func duck(ctx context.Context, conversation string, w io.Writer) error {
	paragraphs := strings.Split(strings.TrimSpace(conversation), "\n\n")
	_, question, _ := strings.Cut(paragraphs[len(paragraphs)-1], ": ")
	for i, word := range strings.Fields("You asked: " + question) {
		if i > 0 {
			word = " " + word
		}
		if _, err := io.WriteString(w, word); err != nil {
			return err
		}
		select {
		case <-time.After(80 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
