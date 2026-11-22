package apron

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// Scoped system identities (Appendix A.1). user_ids starting with `~` are
// reserved for them (Appendix A.3): requestableUserID never matches one.
const (
	roomNoticeID    = "~room"
	privateNoticeID = "~private"
)

// serverCommand is one command this server provides (§4.1), named by the
// first word after the slash.
type serverCommand struct {
	name string
	// forms are what /help and ext:command_list list of the command, in
	// order: one for each fixed word that may follow its name, such as
	// /emoji add and /emoji remove, or one for the command alone.
	forms []commandForm
	// run executes the command, sending the frames it causes, and returns
	// its result, which the caller sends after them (§1). args is the
	// command line after the command's name, and replyTo the command's
	// reply_to, if any.
	run func(s *Server, c *client, r *roomState, body map[string]any, args, replyTo string) (map[string]any, *Error)
	// extension is a command an Extension added, which runs outside s.mu
	// (runExtensionCommand); run is nil, and its one form has no may.
	extension *extensionCommand
}

// commandForm is one form of a command, as /help and ext:command_list
// (apron's extensions/ext_command_list.md) list it.
type commandForm struct {
	// name is the words the user types after the slash: the command's
	// name, then any fixed word, such as "emoji add".
	name string
	// hint describes the text after the name, such as "@user [<reason>]";
	// "" for none.
	hint string
	// description says what the form does, in plain text on one line; ""
	// for none, which an extension's command without Help has.
	description string
	// uses are the fields of the `command` request, besides body.text,
	// that the form reads (commandFields): room_id when it acts on the
	// room it is sent in, and the arguments it takes (§4.1). requires are
	// those of them it fails without; with room_id, the form runs only in
	// a room the client names, and is refused without one (command).
	uses, requires []string
	// group says who may use the form (commandGroups), for display: the
	// narrowest group that holds everyone its check lets use it.
	group string
	// may reports whether c may use the form in r, or, for a nil r, in
	// some room it can see; nil lets everyone. Callers hold s.mu.
	may func(s *Server, c *client, r *roomState) bool
}

// The fields a command form may use or require (ext:command_list): room_id,
// and the arguments of §4.1 that a command reads. A command's `ext` is an
// argument too, but no command reads it.
const (
	fieldRoom     = "room_id"
	fieldReplyTo  = "reply_to"
	fieldMentions = "body.mentions"
	fieldEmbeds   = "body.embeds"
)

var commandFields = []string{fieldRoom, fieldReplyTo, fieldMentions, fieldEmbeds}

// The groups of ext:command_list, from the widest to the narrowest: who
// may use a command, for display only.
const (
	groupAll    = "all"
	groupGuest  = "guest"
	groupMember = "member"
	groupRoom   = "room"
	groupRole   = "role"
	groupAdmin  = "admin"
)

var commandGroups = []string{groupAll, groupGuest, groupMember, groupRoom, groupRole, groupAdmin}

// checkCommandFields checks the uses and requires of a command form:
// fields of commandFields, each once, and requires among uses.
func checkCommandFields(uses, requires []string) error {
	for i, field := range uses {
		switch {
		case !slices.Contains(commandFields, field):
			return fmt.Errorf("field %q is none of %s", field, strings.Join(commandFields, ", "))
		case slices.Contains(uses[:i], field):
			return fmt.Errorf("field %q is used twice", field)
		}
	}
	for i, field := range requires {
		switch {
		case !slices.Contains(uses, field):
			return fmt.Errorf("field %q is required but not in Uses", field)
		case slices.Contains(requires[:i], field):
			return fmt.Errorf("field %q is required twice", field)
		}
	}
	return nil
}

// helpLine is the form's line in /help, such as
// "- `/kick @user [<reason>]`: remove someone …", or "- `/quiet`" without a
// description.
func (f commandForm) helpLine() string {
	if f.description == "" {
		return fmt.Sprintf("- `%s`", f.usage())
	}
	return fmt.Sprintf("- `%s`: %s", f.usage(), f.description)
}

// usage is the form as /help shows it, such as "/kick @user [<reason>]".
func (f commandForm) usage() string {
	if f.hint == "" {
		return "/" + f.name
	}
	return "/" + f.name + " " + f.hint
}

// entry is the form as ext:command_list lists it: a Command object.
func (f commandForm) entry() map[string]any {
	entry := map[string]any{"name": f.name}
	if f.description != "" {
		entry["description"] = f.description
	}
	if f.group != "" {
		entry["group"] = f.group
	}
	if f.hint != "" {
		entry["hint"] = f.hint
	}
	if len(f.uses) > 0 {
		entry["uses"] = slices.Clone(f.uses)
	}
	if len(f.requires) > 0 {
		entry["requires"] = slices.Clone(f.requires)
	}
	return entry
}

// adminForm lets admins use a command form.
func adminForm(_ *Server, c *client, _ *roomState) bool { return c.user.hasRole(RoleAdmin) }

// coreCommands lists the server's own commands in the order /help shows
// them. Open appends the extensions' commands to them, in s.commands.
func coreCommands() []serverCommand {
	embeds := []string{fieldEmbeds}
	return []serverCommand{
		{
			// command() runs /help itself (s.help).
			name: "help",
			forms: []commandForm{{
				name: "help", group: groupAll, description: "list the commands you can use here",
			}},
		},
		{
			name: "avatar",
			forms: []commandForm{{
				name: "avatar", group: groupAll, description: "with one image attached: set your avatar",
				uses: embeds, requires: embeds,
			}},
			run: (*Server).avatarCommand,
		},
		{
			name: "embed-terminal",
			forms: []commandForm{{
				name: "embed-terminal", group: groupAll, hint: "[<title>]", description: "get a URL to stream a program's output into this room, shown as an 80×40 terminal",
				uses: []string{fieldRoom, fieldReplyTo}, requires: []string{fieldRoom},
			}},
			run: (*Server).embedTerminalCommand,
		},
		{
			name: "embed-append",
			forms: []commandForm{{
				name: "embed-append", group: groupAll, hint: "[<markdown>]", description: "get a URL to stream text into this room, as a message that starts with the Markdown given, if any, and grows as the text arrives",
				uses: []string{fieldRoom, fieldReplyTo}, requires: []string{fieldRoom},
			}},
			run: (*Server).embedAppendCommand,
		},
		{
			name: "invite-device",
			forms: []commandForm{{
				name: "invite-device", group: groupMember, hint: "<name>", description: fmt.Sprintf("get a token that signs in as you on another device, named <name>; at most %d names, and a name again replaces its token", maxDeviceTokens),
				may: func(_ *Server, c *client, _ *roomState) bool { return c.user.account() && !c.restrictedService() },
			}},
			run: (*Server).inviteDeviceCommand,
		},
		{
			name: "emoji",
			forms: []commandForm{
				{
					name: "emoji add", group: groupAll, hint: ":<name>:", description: "with one image attached, add a custom emoji or replace one you added",
					uses: embeds, requires: embeds,
					// A connection not offered ext:emoji may not add one.
					may: func(_ *Server, c *client, _ *roomState) bool { return clientsLoad(c.baseURL) },
				},
				{
					name: "emoji remove", group: groupAdmin, hint: ":<name>:", description: "remove a custom emoji",
					may: adminForm,
				},
			},
			run: (*Server).emojiCommand,
		},
		{
			name: "server",
			forms: []commandForm{
				{name: "server", group: groupAdmin, description: "show the server's title, description, and avatar", may: adminForm},
				{name: "server title", group: groupAdmin, hint: "[<text>]", description: "set the server's title, or without the text clear it back to its flag's", may: adminForm},
				{name: "server description", group: groupAdmin, hint: "[<text>]", description: "set the server's description, or without the text clear it back to its flag's", may: adminForm},
				{
					// Without an image it clears the avatar: it requires none.
					name: "server avatar", group: groupAdmin, description: "with one image attached, set the server's avatar, or without one clear it back to its flag's",
					uses: embeds, may: adminForm,
				},
			},
			run: (*Server).serverCommand,
		},
		{
			name: "kick",
			forms: []commandForm{{
				// A room's creator, a guest too, may kick in it; admins and
				// moderators anywhere.
				name: "kick", group: groupRoom, hint: "@user [<reason>]", description: "remove someone from this room (its creator or a moderator)",
				uses: []string{fieldRoom, fieldMentions}, requires: []string{fieldRoom, fieldMentions},
				may: (*Server).mayKickLocked,
			}},
			run: (*Server).kickCommand,
		},
	}
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

// usableForm is a command form that /help or ext:command_list may list,
// with the extension's command that decides whether it does, nil for a
// form already decided.
type usableForm struct {
	form commandForm
	ask  *extensionCommand
}

// usableCommands is what /help and ext:command_list list for a connection:
// the command forms its user may use in a room, or in any room it can see.
// usableCommandsLocked decides the server's own commands under s.mu, and
// finishUsable asks the extensions about theirs outside it.
type usableCommands struct {
	forms []usableForm
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
	asks := false
	for _, command := range s.commands {
		for _, form := range command.forms {
			if form.may != nil && !form.may(s, c, r) {
				continue
			}
			usable.forms = append(usable.forms, usableForm{form, command.extension})
			asks = asks || command.extension != nil && command.extension.command.Available != nil
		}
	}
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
func (s *Server) finishUsable(usable usableCommands) []commandForm {
	forms := make([]commandForm, 0, len(usable.forms))
	for _, f := range usable.forms {
		if f.ask != nil && f.ask.command.Available != nil && !slices.ContainsFunc(usable.rooms, func(r Room) bool {
			return f.ask.command.Available(usable.user, r)
		}) {
			continue
		}
		forms = append(forms, f.form)
	}
	return forms
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
func (s *Server) command(c *client, req request) (any, bool, *Error) {
	for _, name := range []string{"message_id", "deleted"} {
		if _, has := req.params[name]; has {
			return nil, false, invalidParams("A command has no %s; send it as a message instead", name)
		}
	}
	roomID, err := parseString(req.params, "room_id", false)
	if err != nil {
		return nil, false, err
	}
	if _, has := req.params["room_id"]; !has {
		roomID = defaultRoomID
	}
	replyID, hasReply, err := parseMessageRef(req.params, "reply_to")
	if err != nil {
		return nil, false, err
	}
	body, err := parseObject(req.params, "body", true)
	if err != nil {
		return nil, false, err
	}
	if err := validateBody(body); err != nil {
		return nil, false, err
	}
	if _, _, err := parseExt(req.params, "ext"); err != nil {
		return nil, false, err
	}
	text, _ := body["text"].(string)
	line := strings.TrimSpace(text)
	if !strings.HasPrefix(line, "/") {
		return nil, false, invalidParams("A command starts with /; try /help")
	}
	word, args, _ := strings.Cut(line[1:], " ")
	// Names are case-sensitive (ext:command_list): /Kick is not /kick.
	name := word
	for i, value := range asList(body["embeds"]) {
		if kind := value.(map[string]any)["kind"]; liveKind(kind) {
			return nil, false, invalidParams("body.embeds[%d]: a command takes no %s embed; use /embed-%s", i, kind, kind)
		}
	}

	args = strings.TrimSpace(args)
	// A form that requires room_id acts only on a room the client names
	// (ext:command_list): without one it is refused, not run in the
	// default room.
	if _, has := req.params["room_id"]; !has {
		if form := s.formOf(name, args); form != nil && slices.Contains(form.requires, fieldRoom) {
			return nil, false, invalidParams("Send /%s from a room: it needs room_id", form.name)
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
	if c.closing.Load() {
		s.unlock()
		return nil, false, connectionClosing()
	}
	var command *serverCommand
	for i := range s.commands {
		if s.commands[i].name == name {
			command = &s.commands[i]
			break
		}
	}
	r := s.visibleRoomLocked(c.user, roomID)
	switch {
	case r == nil:
		s.unlock()
		return nil, false, invalidParams("Unknown room %q", roomID)
	case hasReply && s.visibleMessageLocked(c.user, replyID) == nil:
		s.unlock()
		return nil, false, invalidParams("reply_to must name an existing message")
	case command == nil:
		s.unlock()
		return nil, false, invalidParams("Unknown command /%s; try /help", word)
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

// formOf returns the form of the command named name that the command
// line's args select: the one with the most fixed words that start args,
// matched exactly, as names are; nil for an unknown command. s.commands does not change once Open returns, so this
// needs no lock.
func (s *Server) formOf(name, args string) *commandForm {
	words := strings.Fields(args)
	var best *commandForm
	most := -1
	for i := range s.commands {
		if s.commands[i].name != name {
			continue
		}
		for j := range s.commands[i].forms {
			form := &s.commands[i].forms[j]
			fixed := strings.Fields(form.name)[1:]
			if len(fixed) > most && len(fixed) <= len(words) && slices.Equal(fixed, words[:len(fixed)]) {
				best, most = form, len(fixed)
			}
		}
	}
	return best
}

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
	forms := s.finishUsable(usable)
	lines := make([]string, len(forms))
	for i, form := range forms {
		lines[i] = form.helpLine()
	}
	c.enqueue(privateNotice(roomID, strings.Join(lines, "\n")))
	result := map[string]any{}
	c.sendResult(req, result)
	return result, true, nil
}

// commandListMethod is ext:command_list's capability and its request
// (apron's extensions/ext_command_list.md).
const commandListMethod = "ext:command_list"

// listCommands answers ext:command_list with the commands c's user may use
// on this connection, in at least one room it can see: what /help lists in
// each room, as data, one Command object for each form. It takes no params,
// and ignores any.
func (s *Server) listCommands(c *client, _ request) (any, bool, *Error) {
	s.mu.RLock()
	usable := s.usableCommandsLocked(c, nil)
	s.mu.RUnlock()
	forms := s.finishUsable(usable)
	commands := make([]any, len(forms))
	for i, form := range forms {
		commands[i] = form.entry()
	}
	return map[string]any{"commands": commands}, false, nil
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
	if first, rest, _ := strings.Cut(args, " "); strings.HasPrefix(first, "@") {
		reason = strings.TrimSpace(rest)
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
