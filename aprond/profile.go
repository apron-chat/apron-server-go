package aprond

import (
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/apron-chat/apron-server-go/wire"
)

// The server's profile, the server frame's title, description, and avatar
// (§3.1): Config sets them, and admins override each with /server, §4.1's
// suggested convention. The store keeps what /server sets (profile);
// clearing a value there falls back to Config's. Without either, the description is
// the default room's, unless that is longer than a server description may
// be.

// maxServerDescriptionBytes bounds a description set with /server, or
// taken from the default room: a few words on what the server is about.
const maxServerDescriptionBytes = 500

// Where a profile value comes from (profileValueLocked).
const (
	fromNowhere = iota
	fromCommand // set with /server
	fromFlag    // Config's
	fromRoom    // the default room's description
)

// profileFields are the profile's fields, in the order /server shows them.
// Each is a server frame field, a /server subcommand, and, with --, the
// flag that sets its default.
var profileFields = []string{"title", "description", "avatar"}

// profileValueLocked returns the current value of a profile field and
// where it comes from.
func (s *Server) profileValueLocked(field string) (string, int) {
	var set, configured string
	switch field {
	case "title":
		set, configured = s.profile.Title(), s.config.Title
	case "description":
		set, configured = s.profile.Description(), s.config.Description
	case "avatar":
		if avatar := s.profile.Avatar(); avatar != nil {
			set = avatar.fileURL()
		}
		configured = s.config.Avatar
	}
	switch {
	case set != "":
		return set, fromCommand
	case configured != "":
		return configured, fromFlag
	case field == "description":
		// A room description longer than a server description is left
		// out rather than cut, which could break its markup.
		if room := s.rooms.Get(defaultRoomID); room != nil {
			if description := room.Record().Description; description != "" && len(description) <= maxServerDescriptionBytes {
				return description, fromRoom
			}
		}
	}
	return "", fromNowhere
}

// announceServerLocked sends every connection, signed in or not, a new
// server frame (§3.1).
func (s *Server) announceServerLocked() {
	type kind struct {
		baseURL  string
		revealed bool
	}
	frames := make(map[kind]wire.ServerInfo)
	for c := range s.clients {
		key := kind{c.baseURL, s.revealedLocked(c)}
		frame, seen := frames[key]
		if !seen {
			frame = s.serverFrameLocked(c)
			frames[key] = frame
		}
		c.conn.Announce(frame)
	}
}

// restoreProfileLocked restores the profile from the store, after the
// embeds. An avatar whose upload is gone is dropped, and so is a finished
// /server avatar upload that is not the avatar.
func (s *Server) restoreProfileLocked(raw jsontext.Value) error {
	err := s.profile.Restore(raw, func(id string) (*embedState, bool) {
		e := s.embeds.Get(id)
		return e, e != nil && e.Finished && e.ServerAvatarBy != ""
	})
	if err != nil {
		return err
	}
	for _, id := range s.embeds.IDs() {
		if e := s.embeds.Get(id); e.ServerAvatarBy != "" && e.Finished && e != s.profile.Avatar() {
			s.removeEmbedLocked(id)
		}
	}
	return nil
}

// serverAvatarNeedHTTPS refuses a server avatar from a connection whose
// URLs clients would not load (clientsLoad).
const serverAvatarNeedHTTPS = "The server avatar needs an https address" + setPublicURL

// refuseServerAvatarLocked checks a finished /server avatar write again:
// its sender must still be an admin. It returns the HTTP status and
// message to refuse the write with, or 0.
func (s *Server) refuseServerAvatarLocked(e *embedState) (int, string) {
	u := s.users.Get(e.ServerAvatarBy)
	switch {
	case u == nil || !u.hasRole("admin"):
		return http.StatusForbidden, "Only an admin can set the server avatar"
	case !clientsLoad(e.BaseURL):
		return http.StatusForbidden, serverAvatarNeedHTTPS
	}
	return 0, ""
}

// setServerAvatarLocked makes a finished /server avatar upload the server's
// avatar, deleting the upload it replaces.
func (s *Server) setServerAvatarLocked(e *embedState) {
	if previous := s.profile.Avatar(); previous != nil && previous != e {
		s.removeEmbedLocked(previous.ID)
	}
	s.profile.SetAvatar(e)
	s.announceServerLocked()
}

const serverUsage = "Usage: /server to show the server's title, description, and avatar; /server title <text> or /server description <text> to set one, or without the text to clear it; /server avatar with one image attached to set the avatar, or without one to clear it"

// serverCommand shows the server's profile in a ~private notice, or, with
// a field, sets or clears it (§4.1's suggested convention). Only admins
// may use it. A change sends every connection a new server frame: at once
// for the title, the description, and a cleared avatar, and when the
// upload's write finishes for a new avatar.
func (s *Server) serverCommand(c *client, r *roomState, body map[string]any, args, _ string) (wire.CommandResult, *Error) {
	if !c.user.hasRole("admin") {
		return wire.CommandResult{}, &Error{Code: CodeDenied, Message: "Only an admin can see and change the server's title, description, and avatar"}
	}
	field, value := cutWord(args)
	// The form's word matches exactly, as command names do: /server Title
	// is not /server title.
	embeds := asList(body["embeds"])
	if len(embeds) > 0 && field != "avatar" {
		return wire.CommandResult{}, invalidParams("Only /server avatar takes an attachment")
	}
	var done string
	switch field {
	case "":
		c.enqueue(privateNotice(r.ID(), s.profileNoticeLocked()))
		return wire.CommandResult{}, nil
	case "title":
		title, err := serverTitle(value)
		if err != nil {
			return wire.CommandResult{}, err
		}
		done = s.setProfileTextLocked(field, title)
	case "description":
		description, err := serverDescription(value)
		if err != nil {
			return wire.CommandResult{}, err
		}
		done = s.setProfileTextLocked(field, description)
	case "avatar":
		if value != "" {
			return wire.CommandResult{}, invalidParams("/server avatar takes an attached image, not text")
		}
		if len(embeds) == 0 {
			done = "The avatar was not set."
			if avatar := s.profile.Avatar(); avatar != nil {
				s.removeEmbedLocked(avatar.ID)
				s.profile.SetAvatar(nil)
				s.announceServerLocked()
				done = "Cleared the avatar."
			}
			break
		}
		if len(embeds) != 1 || embeds[0].(map[string]any)["kind"] != "upload" {
			return wire.CommandResult{}, invalidParams("/server avatar takes exactly one upload embed: attach an image")
		}
		if !clientsLoad(c.baseURL) {
			return wire.CommandResult{}, &Error{Code: CodeDenied, Message: serverAvatarNeedHTTPS}
		}
		e := s.newWriteLocked(c, s.embeds.NewID(), "upload", "")
		e.ServerAvatarBy = c.user.id
		return e.writeResult(), nil
	default:
		return wire.CommandResult{}, invalidParams(serverUsage)
	}
	c.enqueue(privateNotice(r.ID(), done+"\n\n"+s.profileNoticeLocked()))
	return wire.CommandResult{}, nil
}

// setProfileTextLocked sets the title or description, as field names it,
// to value, empty to clear it, announces it, and describes what it did.
func (s *Server) setProfileTextLocked(field string, value string) string {
	current, set := s.profile.Title(), s.profile.SetTitle
	if field == "description" {
		current, set = s.profile.Description(), s.profile.SetDescription
	}
	switch {
	case value == current && value == "":
		return fmt.Sprintf("The %s was not set.", field)
	case value == current:
		return fmt.Sprintf("The %s is unchanged.", field)
	}
	set(value)
	s.announceServerLocked()
	if value == "" {
		return fmt.Sprintf("Cleared the %s.", field)
	}
	return fmt.Sprintf("Set the %s.", field)
}

// serverTitle validates a title as a display name (§3.3): one line,
// prepared with the PRECIS Nickname profile, at most maxNameRunes. Empty
// clears it.
func serverTitle(value string) (string, *Error) {
	if value == "" {
		return "", nil
	}
	if strings.ContainsFunc(value, unicode.IsControl) {
		return "", invalidParams("A title is one line of plain text")
	}
	title := prepareName(value)
	if title == "" {
		return "", invalidParams("%q cannot be a title; use visible letters, digits, or symbols", value)
	}
	if utf8.RuneCountInString(title) > maxNameRunes {
		return "", &Error{Code: CodeTooLarge, Message: fmt.Sprintf("A title is at most %d characters", maxNameRunes)}
	}
	return title, nil
}

// serverDescription validates a description: CommonMark of at most
// maxServerDescriptionBytes, without control characters other than line
// breaks. Empty clears it.
func serverDescription(value string) (string, *Error) {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	if strings.ContainsFunc(value, func(r rune) bool { return r != '\n' && unicode.IsControl(r) }) {
		return "", invalidParams("A description holds no control characters other than line breaks")
	}
	if len(value) > maxServerDescriptionBytes {
		return "", &Error{Code: CodeTooLarge, Message: fmt.Sprintf("A description is at most %d bytes", maxServerDescriptionBytes)}
	}
	return value, nil
}

// profileNoticeLocked describes the profile in CommonMark: each value and
// where it comes from.
func (s *Server) profileNoticeLocked() string {
	var b strings.Builder
	for _, field := range profileFields {
		value, from := s.profileValueLocked(field)
		label := strings.ToUpper(field[:1]) + field[1:]
		switch from {
		case fromNowhere:
			fmt.Fprintf(&b, "**%s**: none\n\n", label)
			continue
		case fromCommand:
			fmt.Fprintf(&b, "**%s** (set with `/server %s`): ", label, field)
		case fromFlag:
			fmt.Fprintf(&b, "**%s** (from `--%s`): ", label, field)
		case fromRoom:
			fmt.Fprintf(&b, "**%s** (the `%s` room's): ", label, defaultRoomID)
		}
		switch {
		case field == "description":
			b.WriteString("\n\n> " + strings.ReplaceAll(value, "\n", "\n> "))
		case strings.HasPrefix(value, "data:"):
			fmt.Fprintf(&b, "a `data:` image URL of %d bytes", len(value))
		default:
			b.WriteString(codeSpan(value))
		}
		b.WriteString("\n\n")
	}
	b.WriteString("`/server title <text>`, `/server description <text>`, and `/server avatar` with an image attached override the flags; without the text or image, each clears its value back to the flag's. Without either, the description is the `" + defaultRoomID + "` room's, if at most " + fmt.Sprint(maxServerDescriptionBytes) + " bytes.")
	return b.String()
}

// codeSpan renders text, one line, as a CommonMark code span.
func codeSpan(text string) string {
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	if strings.HasPrefix(text, "`") || strings.HasSuffix(text, "`") {
		text = " " + text + " "
	}
	return fence + text + fence
}
