package main

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/apron-chat/apron-server-go/apron"
)

// info prints the server frame (§3.1), and who the connection signed in
// as, if it did.
func info(ctx context.Context, c *cli, args []string) error {
	flags := c.flags("info")
	asJSON := flags.Bool("json", false, "print the server frame and you as one JSON object")
	if err := parse(flags, args, 0, 0); err != nil {
		return err
	}
	conn, err := c.connect(ctx, false)
	if err != nil {
		return err
	}
	defer conn.Close()
	hello, you := conn.Hello(), conn.User()
	if *asJSON {
		out := map[string]any{"server": hello}
		if you.UserID != "" {
			out["you"] = you
		}
		return c.printJSON(out)
	}
	w := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
	row := func(name, value string) {
		if value != "" {
			fmt.Fprintf(w, "%s\t%s\n", name, value)
		}
	}
	row("server", c.url)
	row("title", hello.Title)
	row("description", oneLine(hello.Description))
	row("apron", fmt.Sprint(hello.Apron))
	row("agent", hello.Agent)
	row("auth", strings.Join(hello.Auth, ", "))
	if hello.Signup != nil {
		row("signup", strings.Join(hello.Signup, ", "))
	}
	row("capabilities", strings.Join(hello.Capabilities, ", "))
	if you.UserID != "" {
		row("you", display(you))
	} else {
		row("you", "not signed in")
	}
	return w.Flush()
}

// message posts a message (§3.5.1), and prints its message_id.
func message(ctx context.Context, c *cli, args []string) error {
	flags := c.flags("message")
	room := flags.String("room", "", "the room_id to post to; empty is the server's default room")
	format := flags.String("format", apron.Plain, "the text's format: plain or markdown (CommonMark)")
	replyTo := flags.String("reply-to", "", "the message_id of the message this replies to")
	if err := parse(flags, args, 1, -1); err != nil {
		return err
	}
	if *format != apron.Plain && *format != apron.Markdown {
		return fmt.Errorf("--format is plain or markdown, not %q", *format)
	}
	text := strings.Join(flags.Args(), " ")
	if text == "-" {
		read, err := io.ReadAll(c.stdin)
		if err != nil {
			return err
		}
		text = strings.TrimRight(string(read), "\r\n")
	}
	p := apron.MessageParams{RoomID: *room, Body: &apron.Body{Text: text}}
	if *format == apron.Markdown {
		p.Body.Format = apron.Markdown
	}
	if *replyTo != "" {
		id, err := apron.ParseLogID(*replyTo)
		if err != nil || id == 0 {
			return fmt.Errorf("--reply-to is a message_id, not %q", *replyTo)
		}
		p.ReplyTo = &apron.MessageRef{MessageID: id}
	}
	conn, err := c.connect(ctx, true)
	if err != nil {
		return err
	}
	defer conn.Close()
	sent, err := conn.Post(ctx, p)
	if err != nil {
		return fmt.Errorf("posting: %s", describe(err))
	}
	fmt.Fprintln(c.stdout, sent.MessageID)
	return nil
}

// rooms lists the rooms the user can see (§4.2.1).
func rooms(ctx context.Context, c *cli, args []string) error {
	flags := c.flags("rooms")
	joined := flags.Bool("joined", false, "list only the rooms you joined")
	asJSON := flags.Bool("json", false, "print the room_list result as JSON")
	if err := parse(flags, args, 0, 0); err != nil {
		return err
	}
	conn, err := c.connect(ctx, true)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := needs(conn, "rooms"); err != nil {
		return err
	}
	p := apron.RoomListParams{Members: true}
	if *joined {
		p.Filter = apron.RoomsJoined
	}
	listed, err := conn.RoomList(ctx, p)
	if err != nil {
		return fmt.Errorf("listing rooms: %s", describe(err))
	}
	if *asJSON {
		return c.printJSON(listed)
	}
	w := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ROOM\tTITLE\tMEMBERS\tJOINED")
	list := func(views []apron.RoomView, joined string) {
		for _, v := range views {
			title := v.Title
			if v.ParentRoomID != "" {
				title += " (thread of " + v.ParentRoomID + ")"
			}
			members := max(v.MemberCount, len(v.Members))
			fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", v.RoomID, oneLine(title), members, joined)
		}
	}
	list(listed.Joined, "yes")
	list(listed.NotJoined, "no")
	return w.Flush()
}

// join joins a room (§4.2.2).
func join(ctx context.Context, c *cli, args []string) error {
	return membership(ctx, c, "join", args)
}

// leave leaves a room (§4.2.2).
func leave(ctx context.Context, c *cli, args []string) error {
	return membership(ctx, c, "leave", args)
}

func membership(ctx context.Context, c *cli, name string, args []string) error {
	flags := c.flags(name)
	if err := parse(flags, args, 1, 1); err != nil {
		return err
	}
	room := flags.Arg(0)
	conn, err := c.connect(ctx, true)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := needs(conn, "rooms"); err != nil {
		return err
	}
	if name == "join" {
		err = conn.RoomJoin(ctx, apron.RoomJoinParams{RoomID: room})
	} else {
		err = conn.RoomLeave(ctx, apron.RoomLeaveParams{RoomID: room})
	}
	if err != nil {
		return fmt.Errorf("%s %s: %s", name, room, describe(err))
	}
	return nil
}

// call sends a request of any method, with params as JSON, and prints its
// result as the server sent it.
func call(ctx context.Context, c *cli, args []string) error {
	flags := c.flags("call")
	if err := parse(flags, args, 1, 2); err != nil {
		return err
	}
	method := flags.Arg(0)
	var params any
	if flags.NArg() == 2 {
		raw := []byte(flags.Arg(1))
		if flags.Arg(1) == "-" {
			read, err := io.ReadAll(c.stdin)
			if err != nil {
				return err
			}
			raw = read
		}
		value := jsontext.Value(strings.TrimSpace(string(raw)))
		if !value.IsValid() || value.Kind() != '{' {
			return errors.New("the params are a JSON object, such as {\"room_id\":\"general\"}")
		}
		params = value
	}
	conn, err := c.connect(ctx, method != "auth")
	if err != nil {
		return err
	}
	defer conn.Close()
	result, err := conn.Call(ctx, method, params)
	if err != nil {
		return fmt.Errorf("%s: %s", method, describe(err))
	}
	_, err = fmt.Fprintf(c.stdout, "%s\n", result)
	return err
}

// printJSON prints v as one line of JSON.
func (c *cli) printJSON(v any) error {
	raw, err := apron.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(c.stdout, "%s\n", raw)
	return err
}

// codeNames are the error codes' names (§1.1).
var codeNames = map[int]string{
	apron.CodeParseError: "parse_error", apron.CodeInvalidRequest: "invalid_request",
	apron.CodeUnsupported: "unsupported", apron.CodeInvalidParams: "invalid_params",
	apron.CodeInternalError: "internal_error", apron.CodeDenied: "denied",
	apron.CodeRetryAfter: "retry_after", apron.CodeTooLarge: "too_large",
}

// describe says what an error is: an error reply by its name, as the
// protocol writes it, such as error/denied, and its message (§1.1).
func describe(err error) string {
	e, ok := errors.AsType[*apron.Error](err)
	if !ok {
		return err.Error()
	}
	name, ok := codeNames[e.Code]
	if !ok {
		name = fmt.Sprint(e.Code)
	}
	s := "error/" + name + ": " + e.Message
	if delay, _, ok := apron.RetryDelay(err); ok {
		s += fmt.Sprintf(" (retry after %v)", delay)
	}
	return s
}

// display is how a user is shown: its name and its user_id (§3.6).
func display(u apron.User) string {
	if u.Name == "" {
		return "@" + u.UserID
	}
	return u.Name + " (@" + u.UserID + ")"
}

// oneLine is s on one line, for a table.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
