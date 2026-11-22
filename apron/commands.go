package apron

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/apron-chat/apron-server-go/wire"
	"github.com/apron-chat/apron-server-go/wire/commands"
)

// Scoped system identities (Appendix A.1). user_ids starting with `~` are
// reserved for them (Appendix A.3): requestableUserID never matches one.
const (
	roomNoticeID    = "~room"
	privateNoticeID = "~private"
)

// The server's commands (§4.1) are kept in a commands.Router, as any wire
// backend's may be: their forms, in the order /help shows them, with
// their hints, descriptions, fields, and groups, and who may use each
// form, and the form a command line selects. The server runs them itself
// (command), rather than with the Router's Command: it checks the room
// and the reply before the command, runs its own commands while it holds
// s.mu, and asks the extensions whether theirs are available outside it.

// serverCommand is how the server runs one of its commands: run for its
// own, or extension for one an Extension added. /help has neither:
// command runs it.
type serverCommand struct {
	// run executes the command, under s.mu, sending the frames it causes,
	// and returns its result, which the caller sends after them (§1).
	// args is the command line after the command's name, and replyTo the
	// command's reply_to, if any.
	run func(s *Server, c *client, r *roomState, body map[string]any, args, replyTo string) (map[string]any, *Error)
	// extension is a command an Extension added, which runs outside s.mu
	// (runExtensionCommand). Its one form has no May: whether it is
	// available is the extension's Available, asked outside s.mu.
	extension *extensionCommand
}

// serverCommands are the core commands, then the extensions'. forms keeps
// what /help and ext:command_list (apron's extensions/ext_command_list.md)
// show of each; a form's May reads the server's state, so it is asked
// under s.mu (usableCommandsLocked). runs says how each runs, by name.
// Neither changes once Open returns, so reading them needs no lock.
type serverCommands struct {
	forms commands.Router
	runs  map[string]serverCommand
}

// add adds a command: its forms, and how it runs.
func (sc *serverCommands) add(command commands.Command, run serverCommand) error {
	if err := sc.forms.Add(command); err != nil {
		return err
	}
	sc.runs[command.Name] = run
	return nil
}

// extension returns the Extension's command a form is of, or nil for one
// of the server's own.
func (sc *serverCommands) extension(f commands.Form) *extensionCommand {
	name, _ := commands.Cut(f.Name)
	return sc.runs[name].extension
}

// mayLocked makes a check of who may use a command form a Form's May: it
// reports whether c may use the form in r, or, for a nil r, in some room
// it can see. Callers of May hold s.mu.
func mayLocked(may func(s *Server, c *client, r *roomState) bool) func(context.Context, *wire.Conn, string) bool {
	return func(_ context.Context, wc *wire.Conn, roomID string) bool {
		c, ok := wire.DataOf[*client](wc)
		if !ok {
			return false
		}
		var r *roomState
		if roomID != "" {
			r = c.server.rooms[roomID]
		}
		return may(c.server, c, r)
	}
}

// adminForm lets admins use a command form.
var adminForm = mayLocked(func(_ *Server, c *client, _ *roomState) bool { return c.user.hasRole(RoleAdmin) })

// coreCommands lists the server's own commands in the order /help shows
// them. Open adds the extensions' commands after them.
func coreCommands() *serverCommands {
	embeds := []string{commands.FieldEmbeds}
	sc := &serverCommands{runs: make(map[string]serverCommand)}
	for _, core := range []struct {
		command commands.Command
		run     func(s *Server, c *client, r *roomState, body map[string]any, args, replyTo string) (map[string]any, *Error)
	}{
		{
			// command() runs /help itself (s.help).
			command: commands.Command{
				Name:  "help",
				Forms: []commands.Form{{Group: commands.GroupAll, Help: "list the commands you can use here"}},
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
					May: mayLocked(func(_ *Server, c *client, _ *roomState) bool { return c.user.account() && !c.restrictedService() }),
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
		if err := sc.add(core.command, serverCommand{run: core.run}); err != nil {
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
		return r.mayRemove(u)
	}
	moderates := u.hasRole(RoleAdmin) || u.hasRole(RoleModerator)
	for _, r := range s.rooms {
		if (moderates || r.creator == u.id) && r.visibleTo(u) {
			return true
		}
	}
	return false
}

// seesARoomLocked reports whether u can see at least one room.
func (s *Server) seesARoomLocked(u *userState) bool {
	for _, r := range s.rooms {
		if r.visibleTo(u) {
			return true
		}
	}
	return false
}

// usableCommands is what /help and ext:command_list list for a connection:
// the command forms its user may use in a room, or in any room it can see.
// usableCommandsLocked decides the server's own commands under s.mu, and
// finishUsable asks the extensions about theirs outside it.
type usableCommands struct {
	forms []commands.Form
	user  User
	// rooms are the rooms in any of which an extension may make its
	// command available, as it sees them.
	rooms []Room
}

// usableCommandsLocked starts listing the command forms c may use in r, as
// /help does, or, for a nil r, in at least one room it can see, as
// ext:command_list does, in the order /help shows them. For the list it
// makes the views of every room the user can see, once, if an extension's
// command has an Available to ask about them.
func (s *Server) usableCommandsLocked(c *client, r *roomState) usableCommands {
	usable := usableCommands{user: userViewOf(c.user)}
	// A user who sees no room can run no command: each runs in a room.
	if r == nil && !s.seesARoomLocked(c.user) {
		return usable
	}
	roomID := ""
	if r != nil {
		roomID = r.id
	}
	usable.forms = s.commands.forms.Usable(s.requestContext(c), c.conn, roomID)
	asks := slices.ContainsFunc(usable.forms, func(f commands.Form) bool {
		e := s.commands.extension(f)
		return e != nil && e.command.Available != nil
	})
	switch {
	case !asks:
	case r != nil:
		usable.rooms = []Room{s.roomViewLocked(r)}
	default:
		var rooms []*roomState
		for _, room := range s.rooms {
			if room.visibleTo(c.user) {
				rooms = append(rooms, room)
			}
		}
		// In a stable order, so that Available sees the rooms the same
		// way on each listing; the default room, where a command without
		// room_id runs, first.
		rank := func(r *roomState) int {
			if r.id == defaultRoomID {
				return 0
			}
			return 1
		}
		slices.SortFunc(rooms, func(a, b *roomState) int {
			return cmp.Or(cmp.Compare(rank(a), rank(b)), strings.Compare(a.id, b.id))
		})
		usable.rooms = make([]Room, len(rooms))
		for i, room := range rooms {
			usable.rooms[i] = s.roomViewLocked(room)
		}
	}
	return usable
}

// finishUsable asks the extensions, outside s.mu, whether the user may use
// their commands in any of usable.rooms, in order, stopping at the first
// that says yes, and returns the forms it may use.
func (s *Server) finishUsable(usable usableCommands) []commands.Form {
	return slices.DeleteFunc(usable.forms, func(f commands.Form) bool {
		e := s.commands.extension(f)
		return e != nil && e.command.Available != nil && !slices.ContainsFunc(usable.rooms, func(r Room) bool {
			return e.command.Available(usable.user, r)
		})
	})
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
func (s *Server) command(c *client, req request, p wire.CommandParams) (any, bool, *Error) {
	roomID, hasRoom := p.RoomID, p.RoomID != ""
	if !hasRoom {
		roomID = defaultRoomID
	}
	replyID, hasReply := "", p.ReplyTo != nil
	if hasReply {
		replyID = p.ReplyTo.MessageID.String()
	}
	body := req.body()
	if err := validateBody(body); err != nil {
		return nil, false, err
	}
	text, _ := body["text"].(string)
	// The name ends at the first whitespace, a line break too, as in
	// "/embed-append\n# Plan". Names are case-sensitive (ext:command_list):
	// /Kick is not /kick.
	name, args, ok := commands.Parse(text)
	if !ok {
		return nil, false, invalidParams("A command starts with /; try /help")
	}
	for i, value := range asList(body["embeds"]) {
		if kind := value.(map[string]any)["kind"]; liveKind(kind) {
			return nil, false, invalidParams("body.embeds[%d]: a command takes no %s embed; use /embed-%s", i, kind, kind)
		}
	}

	// A form that requires room_id acts only on a room the client names
	// (ext:command_list): without one it is refused, not run in the
	// default room.
	if !hasRoom {
		if _, form := s.commands.forms.Find(name, args); form != nil && slices.Contains(form.Requires, commands.FieldRoom) {
			return nil, false, invalidParams("Send /%s from a room: it needs room_id", form.Name)
		}
	}
	// The opening text of /embed-append becomes its message's text, so the
	// extensions' filters see it as they see messages.
	if name == "embed-append" {
		draft := &Draft{RoomID: roomID, Body: map[string]any{"text": args, "format": "markdown"}}
		if hasReply {
			draft.ReplyTo = replyID
		}
		if err := s.filterMessage(c, draft); err != nil {
			return nil, false, err
		}
		args = draft.Text()
	}

	s.mu.Lock()
	if c.conn.Closing() {
		s.unlock()
		return nil, false, connectionClosing()
	}
	command, known := s.commands.runs[name]
	r := s.visibleRoomLocked(c.user, roomID)
	switch {
	case r == nil:
		s.unlock()
		return nil, false, invalidParams("Unknown room %q", roomID)
	case hasReply && s.visibleMessageLocked(c.user, replyID) == nil:
		s.unlock()
		return nil, false, invalidParams("reply_to must name an existing message")
	case !known:
		s.unlock()
		return nil, false, invalidParams("Unknown command /%s; try /help", name)
	case name == "help":
		usable := s.usableCommandsLocked(c, r)
		s.unlock()
		return s.help(c, req, r.id, usable)
	case command.extension != nil:
		// An extension's command runs outside s.mu, with copies of what it
		// is told.
		call := &CommandCall{
			User: userViewOf(c.user), Room: s.roomViewLocked(r), Args: args, ReplyTo: replyID,
			Mentions: mentions(body), Body: cloneObject(body), client: c,
		}
		s.unlock()
		return s.runExtensionCommand(c, req, command.extension, call)
	}
	defer s.unlock()
	result, err := command.run(s, c, r, body, args, replyID)
	if err != nil {
		return nil, false, err
	}
	c.sendResult(req, result)
	return result, true, nil
}

// cutWord splits text at its first whitespace, of any kind, into the word
// before it and the rest, trimmed: how a command line's name, and the
// fixed word of a form, are told from what follows them.
func cutWord(text string) (word, rest string) { return commands.Cut(text) }

// privateNotice renders a notice for the one connection it is queued on:
// unlogged, without message_id or log_id (Appendix A.1).
func privateNotice(roomID, text string) map[string]any {
	return map[string]any{"method": "message", "params": map[string]any{
		"room_id": roomID,
		"from":    map[string]any{"user_id": privateNoticeID, "name": "System message to you"},
		"body":    map[string]any{"text": text, "format": "markdown"},
	}}
}

// postRoomNoticeLocked logs and delivers a message from ~room to the room's
// members (Appendix A.1). It mentions no one.
func (s *Server) postRoomNoticeLocked(r *roomState, text string) {
	logID := s.nextIDLocked()
	messageID := formatID(logID)
	from := map[string]any{"user_id": roomNoticeID, "name": r.title()}
	m := &messageState{id: messageID, from: from, owner: roomNoticeID, reactions: make(map[string]reactionSet)}
	s.messages[messageID] = m
	s.commitSnapshotLocked(m, map[string]any{
		"message_id": messageID,
		"log_id":     messageID,
		"room_id":    r.id,
		"from":       from,
		"body":       map[string]any{"text": text},
	}, logID)
}

// help replies to /help with a ~private notice in the command's room that
// lists the command forms the sender may use there, as Markdown: what
// ext:command_list lists, narrowed to the room.
func (s *Server) help(c *client, req request, roomID string, usable usableCommands) (any, bool, *Error) {
	c.enqueue(privateNotice(roomID, commands.Help(s.finishUsable(usable))))
	result := map[string]any{}
	c.sendResult(req, result)
	return result, true, nil
}

// commandListMethod is ext:command_list's capability and its request
// (apron's extensions/ext_command_list.md).
const commandListMethod = commands.ListMethod

// listCommands answers ext:command_list with the commands c's user may use
// on this connection, in at least one room it can see: what /help lists in
// each room, as data, one Command object for each form. It takes no params,
// and ignores any.
func (s *Server) listCommands(c *client, _ request) (any, bool, *Error) {
	s.mu.RLock()
	usable := s.usableCommandsLocked(c, nil)
	s.mu.RUnlock()
	return map[string]any{"commands": commands.Entries(s.finishUsable(usable))}, false, nil
}

// avatarCommand takes exactly one upload embed, whose file becomes the
// sender's avatar when its write finishes (§4.8.6). The result carries the
// write URL.
func (s *Server) avatarCommand(c *client, _ *roomState, body map[string]any, _, _ string) (map[string]any, *Error) {
	embeds := asList(body["embeds"])
	if len(embeds) != 1 || embeds[0].(map[string]any)["kind"] != "upload" {
		return nil, invalidParams("/avatar takes exactly one upload embed: attach an image")
	}
	if err := s.admitPostLocked(c.user); err != nil {
		return nil, err
	}
	s.embedNumber++
	e := s.newWriteLocked(c, fmt.Sprintf("embed_%d", s.embedNumber), "upload", "")
	e.avatarFor = c.user
	return map[string]any{
		"embeds": []any{map[string]any{"embed_id": e.id, "kind": "upload", "write_url": e.baseURL + writePath + e.token}},
	}, nil
}

const maxKickReasonRunes = 200

// kickCommand removes the one mentioned user from the room: the room's
// members, the target included, receive the logged leave membership
// (§4.2.2), the target room_update left, and the remaining members a ~room
// notice with the reason. Only the room's creator and admins or moderators
// may kick.
func (s *Server) kickCommand(c *client, r *roomState, body map[string]any, args, _ string) (map[string]any, *Error) {
	u := c.user
	targets := mentions(body)
	if len(targets) != 1 {
		return nil, invalidParams("Usage: /kick @user [<reason>], mentioning exactly one user")
	}
	if !r.mayRemove(u) {
		return nil, &Error{Code: CodeDenied, Message: fmt.Sprintf("Only the creator of %s or a moderator can remove people from it", r.title())}
	}
	target := r.members[targets[0]]
	if target == nil {
		return nil, invalidParams("@%s is not in %s", targets[0], r.title())
	}
	if target == u {
		return nil, invalidParams("You cannot remove yourself; leave the room instead")
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
	return map[string]any{}, nil
}
