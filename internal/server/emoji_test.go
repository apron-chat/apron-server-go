package server

import (
	"reflect"
	"testing"
)

// Reaction entries are kept fully qualified, as Unicode's emoji-test.txt
// lists them (§4.7).
func TestQualifyEmoji(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"👍", "👍"},
		{"❤", "❤\uFE0F"},
		{"❤\uFE0F", "❤\uFE0F"},
		{"❤\uFE0F\uFE0F", "❤\uFE0F"},
		{"☺", "☺\uFE0F"},
		{"©", "©\uFE0F"},
		// A skin tone makes the emoji presentation: no U+FE0F before it.
		{"☝🏽", "☝🏽"},
		{"☝\uFE0F🏽", "☝🏽"},
		{"👍🏽", "👍🏽"},
		// ZWJ sequences qualify each element.
		{"🏳\u200D🌈", "🏳\uFE0F\u200D🌈"},
		{"🏴\u200D☠", "🏴\u200D☠\uFE0F"},
		{"👁\u200D🗨", "👁\uFE0F\u200D🗨\uFE0F"},
		{"⛹🏻\u200D♂", "⛹🏻\u200D♂\uFE0F"},
		{"👩🏽\u200D❤\u200D💋\u200D👨🏿", "👩🏽\u200D❤\uFE0F\u200D💋\u200D👨🏿"},
		{"👨\u200D👩\u200D👧", "👨\u200D👩\u200D👧"},
		// Keycaps take U+FE0F; a bare digit, #, or * is no emoji.
		{"#\u20E3", "#\uFE0F\u20E3"},
		{"1\uFE0F\u20E3", "1\uFE0F\u20E3"},
		{"1", "1"},
		{"12", "12"},
		{"#", "#"},
		// Flags and keys stay as they are.
		{"🇺🇦", "🇺🇦"},
		{"🏴\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067\U000E007F", "🏴\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067\U000E007F"},
		{":partyparrot:", ":partyparrot:"},
		{":party+1:", ":party+1:"},
		{"x\uFE0F", "x"},
		{"\uFE0F", ""},
	} {
		if got := qualifyEmoji(tc.in); got != tc.want {
			t.Errorf("qualifyEmoji(%+q) = %+q, want %+q", tc.in, got, tc.want)
		}
	}
}

// Entries that differ only in U+FE0F are the same entry: they collapse, and
// a set that differs from the kept one only in them is unchanged (§4.7).
func TestReactionsIgnoreVariationSelectors(t *testing.T) {
	_, httpServer := newTestServer(t, DefaultConfig())
	c := dialTestClient(t, httpServer)
	id, _ := save(t, c, "create", map[string]any{"body": map[string]any{"text": "react to me"}})

	set := react(t, c, "set", id, "❤", "❤\uFE0F", "🏳\u200D🌈")
	if got := set["reactions"].([]any)[0].(map[string]any)["emojis"]; !reflect.DeepEqual(got, []any{"❤\uFE0F", "🏳\uFE0F\u200D🌈"}) {
		t.Fatalf("emojis %+q, want them collapsed and fully qualified", got)
	}
	c.result(t, "reactions", "same", map[string]any{"message_id": id, "emojis": []any{"🏳\uFE0F\u200D🌈", "❤"}})
	c.expectQuiet(t)
}
