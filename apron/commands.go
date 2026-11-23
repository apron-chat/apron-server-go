package apron

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/apron-chat/apron-server-go/apron/internal/messages"
	"github.com/apron-chat/apron-server-go/apron/internal/roomlog"
	"github.com/apron-chat/apron-server-go/wire"
	"github.com/apron-chat/apron-server-go/wire/commands"
)

// Scoped system identities (Appendix A.1). user_ids starting with `~` are
// reserved for them (Appendix A.3): requestableUserID never matches one.
const (
	roomNoticeID    = "~room"
	privateNoticeID = "~private"
)

// The server's commands (§4.1) run on a commands.Router, as any wire
// backend's may: their forms, in the order /help shows them, with their
// hints, descriptions, fields, and groups, who may use each form, and how
// each runs. The Router's Before checks what every command needs, first:
// the room and the reply (beforeCommand). The server's own commands check
// the rest themselves, with their own errors (commands.CheckAll), and run
// while they hold s.mu; the extensions' run outside it, and the Router
// asks whether they are available (extensionCommand.may), also outside
// it.

// serverCommands are the core commands, then the extensions'. The Router
// keeps what /help and ext:command_list (apron's
// extensions/ext_command_list.md) show of each, and runs them. It does
// not change once Open returns, so reading it needs no lock; a form's May
// takes s.mu itself.
type serverCommands struct {
	router commands.Router
}

// coreRun is how one of the server's own commands runs, under s.mu,
// sending the frames it causes; it returns its result, which the caller
// sends after them (§1). args is the command line after the command's
// name, and replyTo the command's reply_to, if any.
type coreRun func(s *Server, c *client, r *roomState, body map[string]any, args, replyTo string) (wire.CommandResult, *Error)

// clientOf is the client of a command's connection.
func clientOf(wc *wire.Conn) (*client, *Error) {
	c, ok := wire.DataOf[*client](wc)
	if !ok {
		return nil, internalError()
	}
	return c, nil
}

// replyIDOf is the message_id a command replies to, or "".
func replyIDOf(p wire.CommandParams) string {
	if p.ReplyTo == nil {
		return ""
	}
	return p.ReplyTo.MessageID.String()
}

// replyError is an *Error as a command's error: nil stays nil.
func replyError(err *Error) error {
	if err == nil {
		return nil
	}
	return err
}

// mayLocked makes a check of who may use a command form a Form's May: it
// reports whether c may use the form in r, or, for a nil r, in some room
// it can see, under s.mu, which May takes. A user who sees no room may
// use no command in "some room": each runs in a room.
func mayLocked(may func(s *Server, c *client, r *roomState) bool) func(context.Context, *wire.Conn, string) bool {
	return func(_ context.Context, wc *wire.Conn, roomID string) bool {
		c, ok := wire.DataOf[*client](wc)
		if !ok {
			return false
		}
		s := c.server
		s.mu.RLock()
		defer s.mu.RUnlock()
		var r *roomState
		if roomID != "" {
			r = s.rooms.Get(roomID)
		} else if !s.seesARoomLocked(c.user) {
			return false
		}
		return may == nil || may(s, c, r)
	}
}

// adminForm lets admins use a command form.
var adminForm = mayLocked(func(_ *Server, c *client, _ *roomState) bool { return c.user.hasRole(RoleAdmin) })

// coreCommands lists the server's own commands in the order /help shows
// them. Open adds the extensions' commands after them.
func coreCommands() *serverCommands {
	embeds := []string{commands.FieldEmbeds}
	sc := &serverCommands{}
	sc.router.Before = beforeCommand
	for _, core := range []struct {
		command commands.Command
		run     coreRun
	}{
		{
			command: commands.Command{
				Name:  "help",
				Forms: []commands.Form{{Group: commands.GroupAll, Help: "list the commands you can use here"}},
				Run:   runHelp,
			},
		},
		{
			command: commands.Command{
				Name: "avatar",
				Forms: []commands.Form{{
					Group: commands.GroupAll, Help: "with one image attached: set your avatar",
					Uses: embeds, Requires: embeds,
				}},
			},
			run: (*Server).avatarCommand,
		},
		{
			command: commands.Command{
				Name: "embed-terminal",
				Forms: []commands.Form{{
					Group: commands.GroupAll, Hint: "[<title>]", Help: "get a URL to stream a program's output into this room, shown as an 80×40 terminal",
					Uses: []string{commands.FieldRoom, commands.FieldReplyTo}, Requires: []string{commands.FieldRoom},
				}},
			},
			run: (*Server).embedTerminalCommand,
		},
		{
			command: commands.Command{
				Name: "embed-append",
				Forms: []commands.Form{{
					Group: commands.GroupAll, Hint: "[<markdown>]", Help: "get a URL to stream text into this room, as a message that starts with the Markdown given, if any, and grows as the text arrives",
					Uses: []string{commands.FieldRoom, commands.FieldReplyTo}, Requires: []string{commands.FieldRoom},
				}},
			},
			run: (*Server).embedAppendCommand,
		},
		{
			command: commands.Command{
				Name: "invite-device",
				Forms: []commands.Form{{
					Group: commands.GroupMember, Hint: "<name>", Help: fmt.Sprintf("get a token that signs in as you on another device, named <name>; at most %d names, and a name again replaces its token", maxDeviceTokens),
					May: mayLocked(func(_ *Server, c *client, _ *roomState) bool { return c.user.Account() && !c.restrictedService() }),
				}},
			},
			run: (*Server).inviteDeviceCommand,
		},
		{
			command: commands.Command{
				Name: "emoji",
				Forms: []commands.Form{
					{
						Name: "emoji add", Group: commands.GroupAll, Hint: ":<name>:", Help: "with one image attached, add a custom emoji or replace one you added",
						Uses: embeds, Requires: embeds,
						// A connection not offered ext:emoji may not add one.
						May: mayLocked(func(_ *Server, c *client, _ *roomState) bool { return clientsLoad(c.baseURL) }),
					},
					{
						Name: "emoji remove", Group: commands.GroupAdmin, Hint: ":<name>:", Help: "remove a custom emoji",
						May: adminForm,
					},
				},
			},
			run: (*Server).emojiCommand,
		},
		{
			command: commands.Command{
				Name: "server",
				Forms: []commands.Form{
					{Name: "server", Group: commands.GroupAdmin, Help: "show the server's title, description, and avatar", May: adminForm},
					{Name: "server title", Group: commands.GroupAdmin, Hint: "[<text>]", Help: "set the server's title, or without the text clear it back to its flag's", May: adminForm},
					{Name: "server description", Group: commands.GroupAdmin, Hint: "[<text>]", Help: "set the server's description, or without the text clear it back to its flag's", May: adminForm},
					{
						// Without an image it clears the avatar: it requires none.
						Name: "server avatar", Group: commands.GroupAdmin, Help: "with one image attached, set the server's avatar, or without one clear it back to its flag's",
						Uses: embeds, May: adminForm,
					},
				},
			},
			run: (*Server).serverCommand,
		},
		{
			command: commands.Command{
				Name: "kick",
				Forms: []commands.Form{{
					// A room's creator, a guest too, may kick in it; admins and
					// moderators anywhere.
					Group: commands.GroupRoom, Hint: "@user [<reason>]", Help: "remove someone from this room (its creator or a moderator)",
					Uses: []string{commands.FieldRoom, commands.FieldMentions}, Requires: []string{commands.FieldRoom, commands.FieldMentions},
					May: mayLocked((*Server).mayKickLocked),
				}},
			},
			run: (*Server).kickCommand,
		},
	} {
		command := core.command
		// The server's own commands check the command line, the fields
		// they need, and who may use them themselves, with errors that say
		// more than the Router's: their forms' Requires and May are for
		// /help and ext:command_list.
		command.RunChecks = commands.CheckAll
		for i := range command.Forms {
			if command.Forms[i].May == nil {
				command.Forms[i].May = mayLocked(nil)
			}
		}
		if core.run != nil {
			command.Run = runCore(core.run)
		}
		if err := sc.router.Add(command); err != nil {
			panic("apron: " + err.Error())
		}
	}
	return sc
}

// mayKickLocked reports whether c's user may remove others from r, or, for
// a nil r, from a room it can see: an admin or moderator may in any, and
// otherwise only a room's creator, in that room.
func (s *Server) mayKickLocked(c *client, r *roomState) bool {
	u := c.user
	if r != nil {
		return mayRemove(r, u)
	}
	moderates := u.hasRole(RoleAdmin) || u.hasRole(RoleModerator)
	for _, r := range s.rooms.All() {
		if (moderates || r.Creator() == u.id) && r.VisibleTo(u) {
			return true
		}
	}
	return false
}

// seesARoomLocked reports whether u can see at least one room.
func (s *Server) seesARoomLocked(u *userState) bool {
	for _, r := range s.rooms.All() {
		if r.VisibleTo(u) {
			return true
		}
	}
	return false
}

// visibleRoomsLocked lists the rooms u can see, as an extension's
// Available sees them, in a stable order, so that it sees them the same
// way on each listing: the default room, where a command without room_id
// runs, first.
func (s *Server) visibleRoomsLocked(u *userState) []Room {
	var rooms []*roomState
	for _, room := range s.rooms.All() {
		if room.VisibleTo(u) {
			rooms = append(rooms, room)
		}
	}
	rank := func(r *roomState) int {
		if r.ID() == defaultRoomID {
			return 0
		}
		return 1
	}
	slices.SortFunc(rooms, func(a, b *roomState) int {
		return cmp.Or(cmp.Compare(rank(a), rank(b)), strings.Compare(a.ID(), b.ID()))
	})
	views := make([]Room, len(rooms))
	for i, room := range rooms {
		views[i] = s.roomViewLocked(room)
	}
	return views
}

// command runs a `command` request (§4.1). It takes the params of a new
// message and is never logged, broadcast, or saved: body.text is the command
// line, and mentions, reply_to, and embeds are arguments that notify no one.
// Without room_id it runs in the default room. The result is {} or, for new
// upload, terminal, or append embeds, their write URLs; replies arrive as
// scoped system notices (Appendix A.1) and effects as the frames they cause,
// all before the result (§1). Only /embed-terminal and /embed-append make
// live text, and they make their own embed: a terminal or append embed in a
// command is invalid_params.
func (s *Server) command(c *client, p wire.CommandParams, reply wire.Reply[wire.CommandResult]) *Error {
	if err := messages.CheckBody(p.Body); err != nil {
		return err
	}
	body := bodyObject(p.Body)
	// The name ends at the first whitespace, a line break too, as in
	// "/embed-append\n# Plan". Names are case-sensitive (ext:command_list):
	// /Kick is not /kick. A line that is no command is the Router's to
	// refuse.
	if _, _, ok := commands.Parse(p.Body.Text); ok {
		for i, value := range asList(body["embeds"]) {
			if kind := value.(map[string]any)["kind"]; liveKind(kind) {
				return invalidParams("body.embeds[%d]: a command takes no %s embed; use /embed-%s", i, kind, kind)
			}
		}
	}
	err := s.commands.router.Command(s.requestContext(c), c.conn, p, reply)
	if err == nil {
		return nil
	}
	if e, ok := errors.AsType[*Error](err); ok && e != nil {
		return e
	}
	s.logger.Error("a command failed", "error", err)
	return internalError()
}

// beforeCommand is the Router's Before: it checks what every command
// needs, before the command's name. First the opening text of
// /embed-append, which becomes its message's text, goes through the
// extensions' filters, as messages do. Then the connection must still be
// open, the room, the default room for a command without room_id, which
// the call then names, one the user may see, and reply_to a message the
// user may see.
func beforeCommand(_ context.Context, call *commands.Call) error {
	c, err := clientOf(call.Conn)
	if err != nil {
		return err
	}
	return replyError(c.server.checkCommand(c, call))
}

func (s *Server) checkCommand(c *client, call *commands.Call) *Error {
	p := &call.Params
	roomID := p.RoomID
	if roomID == "" {
		roomID = defaultRoomID
	}
	replyID := replyIDOf(*p)
	if call.Name == "embed-append" {
		draft := &Draft{RoomID: roomID, Body: map[string]any{"text": call.Args, "format": "markdown"}, ReplyTo: replyID}
		if err := s.filterMessage(c, draft); err != nil {
			return err
		}
		call.Args = draft.Text()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	switch {
	case c.conn.Closing():
		return connectionClosing()
	case s.visibleRoomLocked(c.user, roomID) == nil:
		return invalidParams("Unknown room %q", roomID)
	case replyID != "" && s.visibleMessageLocked(c.user, replyID) == nil:
		return invalidParams("reply_to must name an existing message")
	}
	p.RoomID = roomID
	return nil
}

// runCore makes one of the server's own commands a Router's Run: it runs
// under s.mu, in the room beforeCommand checked, unless it has gone since,
// and its result goes while s.mu is held, after the frames the command
// sent (§1).
func runCore(run coreRun) func(context.Context, *commands.Call) error {
	return func(_ context.Context, call *commands.Call) error {
		c, err := clientOf(call.Conn)
		if err != nil {
			return err
		}
		return replyError(c.server.runCoreCommand(c, call, run))
	}
}

func (s *Server) runCoreCommand(c *client, call *commands.Call, run coreRun) *Error {
	replyID := replyIDOf(call.Params)
	s.mu.Lock()
	defer s.unlock()
	r := s.visibleRoomLocked(c.user, call.Params.RoomID)
	switch {
	case c.conn.Closing():
		return connectionClosing()
	case r == nil:
		return invalidParams("Unknown room %q", call.Params.RoomID)
	}
	result, err := run(s, c, r, bodyObject(call.Params.Body), call.Args, replyID)
	if err != nil {
		return err
	}
	_ = call.Reply.Send(result)
	return nil
}

// cutWord splits text at its first whitespace, of any kind, into the word
// before it and the rest, trimmed: how a command line's name, and the
// fixed word of a form, are told from what follows them.
func cutWord(text string) (word, rest string) { return commands.Cut(text) }

// privateNotice renders a notice for the one connection it is queued on:
// unlogged, without message_id or log_id (Appendix A.1).
func privateNotice(roomID, text string) jsontext.Value {
	return encodeNotification(wire.Message{
		RoomID: roomID,
		From:   wire.User{UserID: privateNoticeID, Name: "System message to you"},
		Body:   &wire.Body{Text: text, Format: wire.Markdown},
	})
}

// postRoomNoticeLocked logs and delivers a message from ~room to the room's
// members (Appendix A.1). It mentions no one.
func (s *Server) postRoomNoticeLocked(r *roomState, text string) {
	logID := s.log.NextID()
	messageID := formatID(logID)
	from := roomlog.Author{UserID: roomNoticeID, Name: r.Title()}
	m := s.newMessageLocked(messageID, from, roomNoticeID)
	s.commitSnapshotLocked(m, &roomlog.Message{
		MessageID: messageID, LogID: messageID, RoomID: r.ID(), From: from,
		Body: &roomlog.Body{Body: wire.Body{Text: text}},
	}, logID)
}

// runHelp answers /help with a ~private notice in the command's room that
// lists the command forms the sender may use there, as Markdown: what
// ext:command_list lists, narrowed to the room.
func runHelp(ctx context.Context, call *commands.Call) error {
	c, err := clientOf(call.Conn)
	if err != nil {
		return err
	}
	roomID := call.Params.RoomID
	c.enqueue(privateNotice(roomID, commands.Help(c.server.commands.router.Usable(ctx, c.conn, roomID))))
	_ = call.Done()
	return nil
}

// commandListMethod is ext:command_list's capability and its request
// (apron's extensions/ext_command_list.md).
const commandListMethod = commands.ListMethod

// listCommands answers ext:command_list with the commands c's user may use
// on this connection, in at least one room it can see: what /help lists in
// each room, as data, one Command object for each form. It takes no params,
// and ignores any.
func (s *Server) listCommands(c *client, r *wire.Request) *Error {
	r.Reply(map[string]any{"commands": s.commands.router.List(s.requestContext(c), c.conn)})
	return nil
}

// avatarCommand takes exactly one upload embed, whose file becomes the
// sender's avatar when its write finishes (§4.8.6). The result carries the
// write URL.
func (s *Server) avatarCommand(c *client, _ *roomState, body map[string]any, _, _ string) (wire.CommandResult, *Error) {
	embeds := asList(body["embeds"])
	if len(embeds) != 1 || embeds[0].(map[string]any)["kind"] != "upload" {
		return wire.CommandResult{}, invalidParams("/avatar takes exactly one upload embed: attach an image")
	}
	if err := s.admitPostLocked(c.user); err != nil {
		return wire.CommandResult{}, err
	}
	s.embedNumber++
	e := s.newWriteLocked(c, fmt.Sprintf("embed_%d", s.embedNumber), "upload", "")
	e.avatarFor = c.user
	return e.writeResult(), nil
}

const maxKickReasonRunes = 200

// kickCommand removes the one mentioned user from the room: the room's
// members, the target included, receive the logged leave membership
// (§4.2.2), the target room_update left, and the remaining members a ~room
// notice with the reason. Only the room's creator and admins or moderators
// may kick.
func (s *Server) kickCommand(c *client, r *roomState, body map[string]any, args, _ string) (wire.CommandResult, *Error) {
	u := c.user
	targets := mentions(body)
	if len(targets) != 1 {
		return wire.CommandResult{}, invalidParams("Usage: /kick @user [<reason>], mentioning exactly one user")
	}
	if !mayRemove(r, u) {
		return wire.CommandResult{}, &Error{Code: CodeDenied, Message: fmt.Sprintf("Only the creator of %s or a moderator can remove people from it", r.Title())}
	}
	target := r.Member(targets[0])
	if target == nil {
		return wire.CommandResult{}, invalidParams("@%s is not in %s", targets[0], r.Title())
	}
	if target == u {
		return wire.CommandResult{}, invalidParams("You cannot remove yourself; leave the room instead")
	}
	// The first argument names the target as the user typed it. Only
	// body.mentions says who it is (§3.5.5); a user_id has no whitespace
	// (§2.4), so the typed mention ends with the first word at the latest.
	reason := args
	if first, rest := cutWord(args); strings.HasPrefix(first, "@") {
		reason = rest
	}
	// The reason is one plain line inside a system notice.
	reason, _, _ = strings.Cut(reason, "\n")
	reason = strings.TrimSpace(reason)
	if runes := []rune(reason); len(runes) > maxKickReasonRunes {
		reason = strings.TrimSpace(string(runes[:maxKickReasonRunes])) + "…"
	}
	s.leaveLocked(target, r)
	text := fmt.Sprintf("@%s was removed by @%s", target.id, u.id)
	if reason != "" {
		text += ": " + reason
	}
	s.postRoomNoticeLocked(r, text)
	return wire.CommandResult{}, nil
}
