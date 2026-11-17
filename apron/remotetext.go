package apron

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
// @id that mentions does not list stay as they are, and so does code in
// Markdown: a code span or a fenced block, from a run of backticks to the
// next run of as many.
func rewriteMentions(text string, markdown bool, mentions map[string]bool, mapUser func(string) string) string {
	if len(mentions) == 0 || !strings.Contains(text, "@") {
		return text
	}
	var out strings.Builder
	previous := rune(-1)
	for i := 0; i < len(text); {
		if markdown && text[i] == '`' {
			n := len(text[i:]) - len(strings.TrimLeft(text[i:], "`"))
			end := closingTicks(text, i+n, n)
			if end < 0 {
				end = i + n
			}
			out.WriteString(text[i:end])
			previous, i = '`', end
			continue
		}
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

// closingTicks is the end of the first run of exactly n backticks at or
// after from, or -1.
func closingTicks(text string, from, n int) int {
	for i := from; i < len(text); {
		if text[i] != '`' {
			i++
			continue
		}
		run := len(text[i:]) - len(strings.TrimLeft(text[i:], "`"))
		if run == n {
			return i + run
		}
		i += run
	}
	return -1
}

// remoteBody is a message body for a remote server (ext:remote): its text
// with each @id it mentions that mapped names rewritten, outside code in
// Markdown, its mentions those that mapped names, as mapped, and its
// format. Embeds are not copied (uploads, live text, and previews load only
// from their own server's origins); each is named in a line after the text
// instead, with its link when it has an http one.
func remoteBody(body map[string]any, mapped map[string]string) map[string]any {
	text, _ := body["text"].(string)
	out := make(map[string]any)
	var mentions []any
	listed := make(map[string]bool)
	for _, value := range asList(body["mentions"]) {
		if id, ok := value.(string); ok && mapped[id] != "" && !listed[id] {
			listed[id] = true
			mentions = append(mentions, mapped[id])
		}
	}
	if len(mentions) > 0 {
		out["mentions"] = mentions
	}
	text = rewriteMentions(text, body["format"] == "markdown", listed, func(id string) string { return mapped[id] })
	if embeds := asList(body["embeds"]); len(embeds) > 0 {
		for _, value := range embeds {
			embed, _ := value.(map[string]any)
			text = strings.TrimRight(text, "\n") + "\n" + embedFallback(embed)
		}
		text = strings.TrimLeft(text, "\n")
	}
	out["text"] = text
	if format, ok := body["format"].(string); ok {
		out["format"] = format
	}
	return out
}

// embedFallback names an embed that is not copied (§4.8): its title or
// kind, and its link when it has an http one.
func embedFallback(embed map[string]any) string {
	og, _ := embed["og"].(map[string]any)
	title := firstStringOf(embed["title"], og["title"], embed["kind"])
	link := firstStringOf(og["url"], embed["url"])
	if parsed, err := url.Parse(link); err != nil || parsed.Scheme != "https" && parsed.Scheme != "http" {
		link = ""
	}
	if link != "" {
		return fmt.Sprintf("[%s: %s]", title, link)
	}
	return fmt.Sprintf("[%s, not bridged]", title)
}

func firstStringOf(values ...any) string {
	for _, value := range values {
		if s, ok := value.(string); ok && s != "" {
			return s
		}
	}
	return "attachment"
}
