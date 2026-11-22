// Package emoji puts reaction entries in their fully-qualified form
// (PROTOCOL.md §4.7, UTS #51), so that entries that differ only in U+FE0F
// are the same entry.
package emoji

import (
	"strings"
	"unicode"
)

const (
	variationSelector16 = '\uFE0F'
	combiningKeycap     = '\u20E3'
)

// Qualify returns a reaction entry in its fully-qualified form (§4.7,
// UTS #51 ED-18): without U+FE0F, then with U+FE0F after each emoji
// character of text presentation that does not start an emoji modifier
// sequence. #, *, and the digits take one only as a keycap's base. Entries
// that differ only in U+FE0F so become the same entry, and other text keeps
// every character but U+FE0F.
func Qualify(entry string) string {
	runes := []rune(strings.ReplaceAll(entry, string(variationSelector16), ""))
	var b strings.Builder
	b.Grow(len(entry))
	for i, r := range runes {
		b.WriteRune(r)
		var next rune
		if i+1 < len(runes) {
			next = runes[i+1]
		}
		if !unicode.Is(textDefaultEmoji, r) || emojiModifier(next) {
			continue
		}
		if r < 0x80 && next != combiningKeycap {
			continue
		}
		b.WriteRune(variationSelector16)
	}
	return b.String()
}

// emojiModifier reports whether r is a skin tone modifier (U+1F3FB to
// U+1F3FF).
func emojiModifier(r rune) bool {
	return r >= 0x1F3FB && r <= 0x1F3FF
}
