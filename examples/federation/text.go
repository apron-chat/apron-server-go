package main

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// idChar reports a character an ID after a prefix may hold (PROTOCOL.md
// Appendix A.3): [A-Za-z0-9_.+@:-].
func idChar(r rune) bool {
	return r < utf8.RuneSelf && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_.+@:-", r))
}

// idEdge reports a character an ID may start and end with: a letter, a
// digit, or _.
func idEdge(r rune) bool {
	return r < utf8.RuneSelf && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_')
}

// rewriteMentions rewrites each @id in text that mentions lists to
// mapUser(id), by the grammar of Appendix A.3: the prefix is not preceded
// by a letter or digit, and the ID is the longest run of ID characters
// that starts and ends with a letter, a digit, or _. A role (@&) and an
// @id that mentions does not list stay as they are.
func rewriteMentions(text string, mentions map[string]bool, mapUser func(string) string) string {
	if len(mentions) == 0 || !strings.Contains(text, "@") {
		return text
	}
	var out strings.Builder
	previous := rune(-1)
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if r != '@' || previous >= 0 && (unicode.IsLetter(previous) || unicode.IsDigit(previous)) {
			out.WriteString(text[i : i+size])
			previous = r
			i += size
			continue
		}
		start := i + size
		end := start
		for end < len(text) && idChar(rune(text[end])) {
			end++
		}
		for end > start && !idEdge(rune(text[end-1])) {
			end--
		}
		id := text[start:end]
		if id == "" || !idEdge(rune(id[0])) || !mentions[id] {
			out.WriteString("@")
			previous = '@'
			i = start
			continue
		}
		out.WriteString("@" + mapUser(id))
		previous = rune(text[end-1])
		i = end
	}
	return out.String()
}

// mapBody is a message body for the other server: its text with the @ids
// it mentions rewritten, its mentions mapped, and its format. Embeds are
// not copied (uploads, live text, and previews load only from their own
// server's origins); each is named in a line after the text instead, with
// its link when it has an http one.
func mapBody(body map[string]any, mapUser func(string) string) map[string]any {
	text, _ := body["text"].(string)
	mapped := make(map[string]any)
	mentions := make(map[string]bool)
	if list, ok := body["mentions"].([]any); ok {
		ids := make([]any, 0, len(list))
		for _, value := range list {
			if id, ok := value.(string); ok && !mentions[id] {
				mentions[id] = true
				ids = append(ids, mapUser(id))
			}
		}
		if len(ids) > 0 {
			mapped["mentions"] = ids
		}
	}
	text = rewriteMentions(text, mentions, mapUser)
	if embeds, ok := body["embeds"].([]any); ok && len(embeds) > 0 {
		for _, value := range embeds {
			embed, _ := value.(map[string]any)
			text = strings.TrimRight(text, "\n") + "\n" + embedFallback(embed)
		}
		text = strings.TrimLeft(text, "\n")
	}
	mapped["text"] = text
	if format, ok := body["format"].(string); ok {
		mapped["format"] = format
	}
	return mapped
}

// embedFallback names an embed that is not copied (§4.8): its title or
// kind, and its link when it has an http one.
func embedFallback(embed map[string]any) string {
	og, _ := embed["og"].(map[string]any)
	title := firstString(embed["title"], og["title"], embed["kind"])
	link := firstString(og["url"], embed["url"])
	if parsed, err := url.Parse(link); err != nil || parsed.Scheme != "https" && parsed.Scheme != "http" {
		link = ""
	}
	if link != "" {
		return fmt.Sprintf("[%s: %s]", title, link)
	}
	return fmt.Sprintf("[%s, not bridged]", title)
}

func firstString(values ...any) string {
	for _, value := range values {
		if s, ok := value.(string); ok && s != "" {
			return s
		}
	}
	return "attachment"
}
