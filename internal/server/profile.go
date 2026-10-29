package server

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The server's profile, the server frame's title, description, and avatar
// (§3.1): Config sets them, and admins override each with /server, §4.1's
// suggested convention. The store keeps what /server sets; clearing a
// value there falls back to Config's. Without either, the description is
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

// serverProfile is what admins set with /server; an empty value or nil
// avatar falls back to Config's.
type serverProfile struct {
	title       string
	description string
	// avatar is the finished /server avatar upload.
	avatar *embedState
}

type storedProfile struct {
	Title       string `json:"title,omitzero"`
	Description string `json:"description,omitzero"`
	AvatarEmbed string `json:"avatar_embed,omitzero"`
}

// profileValueLocked returns the current value of a profile field and
// where it comes from.
func (s *Server) profileValueLocked(field string) (string, int) {
	var set, configured string
	switch field {
	case "title":
		set, configured = s.profile.title, s.config.Title
	case "description":
		set, configured = s.profile.description, s.config.Description
	case "avatar":
		if s.profile.avatar != nil {
			set = s.profile.avatar.fileURL()
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
		if room := s.rooms[defaultRoomID]; room != nil {
			if description, _ := room.record["description"].(string); description != "" && len(description) <= maxServerDescriptionBytes {
				return description, fromRoom
			}
		}
	}
	return "", fromNowhere
}

// profileChangedLocked stores the profile and announces it.
func (s *Server) profileChangedLocked() {
	s.dirty.profile = true
	s.announceServerLocked()
}

// announceServerLocked sends every connection, signed in or not, a new
// server frame (§3.1).
func (s *Server) announceServerLocked() {
	frames := make(map[string]jsontext.Value)
	for c := range s.clients {
		frame, seen := frames[c.baseURL]
		if !seen {
			frame = notification("server", s.serverParams(c.baseURL))
			frames[c.baseURL] = frame
		}
		c.enqueue(frame)
	}
}

func (s *Server) storedProfileLocked() storedProfile {
	stored := storedProfile{Title: s.profile.title, Description: s.profile.description}
	if s.profile.avatar != nil {
		stored.AvatarEmbed = s.profile.avatar.id
	}
	return stored
}

// restoreProfileLocked restores the profile from the store, after the
// embeds. An avatar whose upload is gone is dropped, and so is a finished
// /server avatar upload that is not the avatar.
func (s *Server) restoreProfileLocked(raw jsontext.Value) error {
	if raw != nil {
		var stored storedProfile
		if err := json.Unmarshal(raw, &stored); err != nil {
			return fmt.Errorf("stored profile: %w", err)
		}
		s.profile.title, s.profile.description = stored.Title, stored.Description
		if stored.AvatarEmbed != "" {
			if e := s.embeds[stored.AvatarEmbed]; e != nil && e.finished && e.serverAvatarBy != "" {
				s.profile.avatar = e
			} else {
				s.dirty.profile = true
			}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(s.embeds)) {
		if e := s.embeds[id]; e.serverAvatarBy != "" && e.finished && e != s.profile.avatar {
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
	u := s.users[e.serverAvatarBy]
	switch {
	case u == nil || !u.hasRole("admin"):
		return http.StatusForbidden, "Only an admin can set the server avatar"
	case !clientsLoad(e.baseURL):
		return http.StatusForbidden, serverAvatarNeedHTTPS
	}
	return 0, ""
}

// setServerAvatarLocked makes a finished /server avatar upload the server's
// avatar, deleting the upload it replaces.
func (s *Server) setServerAvatarLocked(e *embedState) {
	if previous := s.profile.avatar; previous != nil && previous != e {
		s.removeEmbedLocked(previous.id)
	}
	s.profile.avatar = e
	s.profileChangedLocked()
}

const serverUsage = "Usage: /server to show the server's title, description, and avatar; /server title <text> or /server description <text> to set one, or without the text to clear it; /server avatar with one image attached to set the avatar, or without one to clear it"

// serverCommand shows the server's profile in a ~private notice, or, with
// a field, sets or clears it (§4.1's suggested convention). Only admins
// may use it. A change sends every connection a new server frame: at once
// for the title, the description, and a cleared avatar, and when the
// upload's write finishes for a new avatar.
func (s *Server) serverCommand(c *client, r *roomState, body map[string]any, args string) (map[string]any, *rpcError) {
	if !c.user.hasRole("admin") {
		return nil, &rpcError{Code: codeDenied, Message: "Only an admin can see and change the server's title, description, and avatar"}
	}
	field, value := args, ""
	if i := strings.IndexFunc(args, unicode.IsSpace); i >= 0 {
		field, value = args[:i], strings.TrimSpace(args[i:])
	}
	field = strings.ToLower(field)
	embeds := asList(body["embeds"])
	if len(embeds) > 0 && field != "avatar" {
		return nil, invalidParams("Only /server avatar takes an attachment")
	}
	var done string
	switch field {
	case "":
		c.enqueue(privateNotice(r, s.profileNoticeLocked()))
		return map[string]any{}, nil
	case "title":
		title, err := serverTitle(value)
		if err != nil {
			return nil, err
		}
		done = s.setProfileTextLocked(field, &s.profile.title, title)
	case "description":
		description, err := serverDescription(value)
		if err != nil {
			return nil, err
		}
		done = s.setProfileTextLocked(field, &s.profile.description, description)
	case "avatar":
		if value != "" {
			return nil, invalidParams("/server avatar takes an attached image, not text")
		}
		if len(embeds) == 0 {
			done = "The avatar was not set."
			if s.profile.avatar != nil {
				s.removeEmbedLocked(s.profile.avatar.id)
				s.profile.avatar = nil
				s.profileChangedLocked()
				done = "Cleared the avatar."
			}
			break
		}
		if len(embeds) != 1 || embeds[0].(map[string]any)["kind"] != "upload" {
			return nil, invalidParams("/server avatar takes exactly one upload embed: attach an image")
		}
		if !clientsLoad(c.baseURL) {
			return nil, &rpcError{Code: codeDenied, Message: serverAvatarNeedHTTPS}
		}
		s.embedNumber++
		e := s.newWriteLocked(c, fmt.Sprintf("embed_%d", s.embedNumber), "upload", "")
		e.serverAvatarBy = c.user.id
		return map[string]any{
			"embeds": []any{map[string]any{"embed_id": e.id, "kind": "upload", "write_url": e.baseURL + writePath + e.token}},
		}, nil
	default:
		return nil, invalidParams(serverUsage)
	}
	c.enqueue(privateNotice(r, done+"\n\n"+s.profileNoticeLocked()))
	return map[string]any{}, nil
}

// setProfileTextLocked sets the title or description, kept in *target, to
// value, empty to clear it, and describes what it did.
func (s *Server) setProfileTextLocked(field string, target *string, value string) string {
	switch {
	case value == *target && value == "":
		return fmt.Sprintf("The %s was not set.", field)
	case value == *target:
		return fmt.Sprintf("The %s is unchanged.", field)
	}
	*target = value
	s.profileChangedLocked()
	if value == "" {
		return fmt.Sprintf("Cleared the %s.", field)
	}
	return fmt.Sprintf("Set the %s.", field)
}

// serverTitle validates a title as a display name (§3.3): one line,
// prepared with the PRECIS Nickname profile, at most maxNameRunes. Empty
// clears it.
func serverTitle(value string) (string, *rpcError) {
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
		return "", &rpcError{Code: codeTooLarge, Message: fmt.Sprintf("A title is at most %d characters", maxNameRunes)}
	}
	return title, nil
}

// serverDescription validates a description: CommonMark of at most
// maxServerDescriptionBytes, without control characters other than line
// breaks. Empty clears it.
func serverDescription(value string) (string, *rpcError) {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	if strings.ContainsFunc(value, func(r rune) bool { return r != '\n' && unicode.IsControl(r) }) {
		return "", invalidParams("A description holds no control characters other than line breaks")
	}
	if len(value) > maxServerDescriptionBytes {
		return "", &rpcError{Code: codeTooLarge, Message: fmt.Sprintf("A description is at most %d bytes", maxServerDescriptionBytes)}
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
