package messages

import (
	"encoding/json/jsontext"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/apron-chat/apron-server-go/apron"
)

// code is an error's code, 0 for none.
func code(err *apron.Error) int {
	if err == nil {
		return 0
	}
	return err.Code
}

func TestCheckBody(t *testing.T) {
	mentions := make([]string, MaxMentions+1)
	for i := range mentions {
		mentions[i] = fmt.Sprint("user", i)
	}
	embeds := slices.Repeat([]jsontext.Value{jsontext.Value(`{"kind":"upload"}`)}, MaxEmbeds+1)
	for _, c := range []struct {
		name string
		body apron.Body
		want int
	}{
		{"empty", apron.Body{}, 0},
		{"at the limits", apron.Body{Mentions: mentions[:MaxMentions], Embeds: embeds[:MaxEmbeds]}, 0},
		// A count limit the server sets is denied (§1.1).
		{"mentions", apron.Body{Mentions: mentions}, apron.CodeDenied},
		{"embeds", apron.Body{Embeds: embeds}, apron.CodeDenied},
	} {
		if got := code(CheckBody(&c.body)); got != c.want {
			t.Errorf("%s: code %d, want %d", c.name, got, c.want)
		}
	}
}

// A body from elsewhere than a client is checked as apron/server checks a
// client's, then against the limits.
func TestCheckRaw(t *testing.T) {
	many := `{"mentions":["` + strings.Repeat(`a","`, MaxMentions) + `a"]}`
	for raw, want := range map[string]int{
		`{"text":"hi","format":"markdown","mentions":["ada"],"color":"red"}`: 0,
		`{"text":5}`:              apron.CodeInvalidParams,
		`{"format":"html"}`:       apron.CodeInvalidParams,
		`{"embeds":[{"kind":5}]}`: apron.CodeInvalidParams,
		`[]`:                      apron.CodeInvalidParams,
		many:                      apron.CodeDenied,
	} {
		if got := code(CheckRaw(jsontext.Value(raw))); got != want {
			t.Errorf("CheckRaw(%.40s) = %d, want %d", raw, got, want)
		}
	}
}

func TestCheckExt(t *testing.T) {
	if err := CheckExt(apron.Ext{}.With("x", strings.Repeat("a", MaxExtBytes/2))); err != nil {
		t.Fatalf("half the limit: %v", err)
	}
	if code(CheckExt(apron.Ext{}.With("x", strings.Repeat("a", MaxExtBytes)))) != apron.CodeTooLarge {
		t.Fatal("an ext over the limit was taken")
	}
}

func TestEmojis(t *testing.T) {
	// Entries that differ only in U+FE0F are one, kept fully qualified,
	// in the order they first came.
	got, err := Emojis([]string{"❤", "👍", "❤️", "👍"})
	if err != nil || !slices.Equal(got, []string{"❤️", "👍"}) {
		t.Fatalf("Emojis = %q, %v", got, err)
	}
	if got, err := Emojis(nil); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("an empty set = %#v, %v", got, err)
	}
	many := make([]string, MaxDistinctEmoji+1)
	for i := range many {
		many[i] = fmt.Sprint(i)
	}
	for name, c := range map[string]struct {
		in   []string
		want int
	}{
		"only U+FE0F": {[]string{"️"}, apron.CodeInvalidParams},
		"too long":    {[]string{strings.Repeat("x", MaxEmojiBytes+1)}, apron.CodeTooLarge},
		"too many":    {many, apron.CodeDenied},
		"repeated":    {slices.Repeat([]string{"👍"}, MaxDistinctEmoji+1), 0},
	} {
		if _, err := Emojis(c.in); code(err) != c.want {
			t.Errorf("%s: %v, want code %d", name, err, c.want)
		}
	}
}
