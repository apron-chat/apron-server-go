package livetext

import (
	"bytes"
	"strings"
	"testing"

	"golang.org/x/text/encoding/unicode"
)

// keepLastReference is §4.8.6's keep_last, transcribed from its Python.
func keepLastReference(output []byte, limit int) []byte {
	if len(output) <= limit {
		return output
	}
	tail := output[len(output)-limit:]
	for i, b := range tail {
		if b < 0x20 {
			return tail[i:]
		}
	}
	// tail.lstrip(bytes(range(0x80, 0xC0)))
	i := 0
	for i < len(tail) && tail[i] >= 0x80 && tail[i] < 0xC0 {
		i++
	}
	return tail[i:]
}

func TestKeepLastCutsAtAControlCharacterElseAWholeCharacter(t *testing.T) {
	for _, tc := range []struct {
		output string
		limit  int
		want   string
	}{
		{"short", 16, "short"},
		{"0123456789", 10, "0123456789"},
		{"line one\nline two", 12, "\nline two"},
		{"a\x1b[31mred\x1b[0m", 9, "\x1b[0m"},
		{"\r\n" + strings.Repeat("y", 12), 8, "yyyyyyyy"},
		{"ééé", 5, "éé"},
		{"ééé", 4, "éé"},
		{"ééé", 3, "é"},
		{"日本語", 7, "本語"},
		{"日本語", 8, "本語"},
		{"x日本語", 7, "本語"},
		{strings.Repeat("\x80", 6), 4, ""},
	} {
		got := KeepLast([]byte(tc.output), tc.limit)
		if string(got) != tc.want {
			t.Errorf("KeepLast(%q, %d) = %q, want %q", tc.output, tc.limit, got, tc.want)
		}
		if reference := keepLastReference([]byte(tc.output), tc.limit); !bytes.Equal(got, reference) {
			t.Errorf("KeepLast(%q, %d) = %q, the reference %q", tc.output, tc.limit, got, reference)
		}
	}
}

func TestWholeRunesDropsAPartialCharacter(t *testing.T) {
	for in, want := range map[string]string{
		"":                   "",
		"abc":                "abc",
		"abé":                "abé",
		"ab\xc3":             "ab",
		"ab\xe6\x97":         "ab",
		"ab日":                "ab日",
		"ab\xf0\x9f\x98":     "ab",
		"ab\xf0\x9f\x98\x80": "ab\xf0\x9f\x98\x80",
	} {
		if got := string(WholeRunes([]byte(in))); got != want {
			t.Errorf("WholeRunes(%q) = %q, want %q", in, got, want)
		}
	}
}

// The live buffer serves what a terminal keeps from its start, both to a
// new reader and as the finished output.
func TestTerminalBufferServesKeepLast(t *testing.T) {
	b := New(true, 8, 1<<20)
	var all []byte
	for _, chunk := range []string{"build ok\r\n", "\x1b[32m", "passed", " 12 tests\n", "ééé", "x"} {
		b.Write([]byte(chunk))
		all = append(all, chunk...)
		want := KeepLast(all, 8)
		got, _, _, _, _ := b.Read(-1)
		if !bytes.Equal(got, want) {
			t.Fatalf("after %q a new reader got %q, want %q", chunk, got, want)
		}
		if text := b.String(); text != strings.ToValidUTF8(string(want), "\uFFFD") {
			t.Fatalf("after %q the output is %q, want %q", chunk, text, want)
		}
	}
}

// A reader goes on from where it is while the terminal still holds the
// bytes after it, and is told when it fell behind them.
func TestTerminalBufferTellsAReaderThatFellBehind(t *testing.T) {
	b := New(true, 8, 1<<20)
	b.Write([]byte("12345678"))
	_, next, _, _, _ := b.Read(-1)
	b.Write([]byte("abcdefgh"))
	if got, _, _, _, behind := b.Read(next); string(got) != "abcdefgh" || behind {
		t.Fatalf("a reader still within the text got %q, behind %v", got, behind)
	}
	b.Write([]byte("ij"))
	if got, _, _, _, behind := b.Read(next); got != nil || !behind {
		t.Fatalf("a reader past what the terminal holds got %q, behind %v", got, behind)
	}
}

func TestAppendBufferKeepsTheFirstPartUpToAWholeCharacter(t *testing.T) {
	b := New(false, 5, 1<<20)
	if !b.Write([]byte("abc")) {
		t.Fatal("ended early")
	}
	if b.Write([]byte("dé")) || !b.Limited() {
		t.Fatal("did not end at the limit")
	}
	if text := b.String(); text != "abcd" {
		t.Fatalf("kept %q", text)
	}
	got, _, _, ended, _ := b.Read(-1)
	if string(got) != "abcd" || !ended {
		t.Fatalf("reader got %q, ended %v", got, ended)
	}
}

// editAppend is §4.8.5's edits, for reference: the whole text decoded,
// then each backspace removes the code point before it and each form feed
// all the text before it.
func editAppend(raw []byte) string {
	decoded, _ := unicode.UTF8.NewDecoder().Bytes(raw)
	var kept []rune
	for _, r := range string(decoded) {
		switch r {
		case '\b':
			kept = kept[:max(0, len(kept)-1)]
		case '\f':
			kept = kept[:0]
		default:
			kept = append(kept, r)
		}
	}
	return string(kept)
}

// An append applies backspaces and form feeds as the text arrives, after
// decoding, however the writer splits it (§4.8.5).
func TestAppendBufferAppliesEdits(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"hel\b\bello", "hello"},
		{"caf\u00e9\be", "cafe"},
		{"\U0001F600\b!", "!"},
		{"draft\fHello", "Hello"},
		{"\b\bstart", "start"},
		{"one\ftwo\fthree", "three"},
		{"ab\xff\bc", "abc"},
		{"ab\xE2\x82\bc", "abc"},
		{"end \xE2\x82", "end \uFFFD"},
		{"no edits\r\n\x1b[0m", "no edits\r\n\x1b[0m"},
	} {
		if want := editAppend([]byte(c.in)); want != c.want {
			t.Fatalf("%q: the reference edits it to %q, not %q", c.in, want, c.want)
		}
		for split := 0; split <= len(c.in); split++ {
			b := New(false, 1<<10, 1<<20)
			b.Write([]byte(c.in[:split]))
			b.Write([]byte(c.in[split:]))
			b.End()
			if got := b.String(); got != c.want {
				t.Errorf("%q split at %d: %q, want %q", c.in, split, got, c.want)
			}
			if got, _, _, _, _ := b.Read(-1); string(got) != c.want {
				t.Errorf("%q split at %d: a new reader got %q, want %q", c.in, split, got, c.want)
			}
			// A reader from the start gets the decoded text with its edits.
			got, _, _, _, _ := b.Read(0)
			if decoded, _ := unicode.UTF8.NewDecoder().Bytes([]byte(c.in)); string(got) != string(decoded) {
				t.Errorf("%q split at %d: a reader from the start got %q", c.in, split, got)
			}
		}
	}
}

// An append decodes invalid UTF-8 as a browser's TextDecoder('utf-8') does
// (WHATWG), independently of x/text: these outputs are TextDecoder's.
func TestAppendDecodesInvalidUTF8AsBrowsersDo(t *testing.T) {
	for _, c := range []struct{ in, decoded, want string }{
		{"a\xE2\x82A\b", "a\uFFFDA\b", "a\uFFFD"},
		{"a\xF0\x80\x80\b", "a\uFFFD\uFFFD\uFFFD\b", "a\uFFFD\uFFFD"},
		{"a\xED\xA0\x80\b", "a\uFFFD\uFFFD\uFFFD\b", "a\uFFFD\uFFFD"},
		{"a\xC0\xAF\b", "a\uFFFD\uFFFD\b", "a\uFFFD"},
		{"a\xFF\b", "a\uFFFD\b", "a"},
		{"a\x80\x80\b", "a\uFFFD\uFFFD\b", "a\uFFFD"},
		{"a\xF0\x9F\x9A\bb", "a\uFFFD\bb", "ab"},
		{"a\xF0\x9F\x9A", "a\uFFFD", "a\uFFFD"},
	} {
		if got := editAppend([]byte(c.in)); got != c.want {
			t.Errorf("%q: the reference edits it to %q, want %q", c.in, got, c.want)
		}
		for split := 0; split <= len(c.in); split++ {
			b := New(false, 1<<10, 1<<20)
			b.Write([]byte(c.in[:split]))
			b.Write([]byte(c.in[split:]))
			b.End()
			if got := b.String(); got != c.want {
				t.Errorf("%q split at %d: %q, want %q", c.in, split, got, c.want)
			}
			if got, _, _, _, _ := b.Read(0); string(got) != c.decoded {
				t.Errorf("%q split at %d: a reader from the start got %q, want %q", c.in, split, got, c.decoded)
			}
		}
	}
}

// A new reader of an append gets its edited text so far, then what follows;
// one already reading gets the edits to apply. The start of a character
// that has not all arrived waits for the rest.
func TestAppendBufferServesEditedTextToANewReader(t *testing.T) {
	b := New(false, 1<<10, 1<<20)
	b.Write([]byte("secret draft\fcaf\xc3"))
	early, next, _, _, _ := b.Read(-1)
	if string(early) != "caf" {
		t.Fatalf("a new reader got %q", early)
	}
	b.Write([]byte("\xa9\b\be!"))
	late, _, _, _, _ := b.Read(-1)
	more, _, _, _, _ := b.Read(next)
	if string(late) != "cae!" || string(more) != "\u00e9\b\be!" {
		t.Fatalf("a new reader got %q, the reader before it %q", late, more)
	}
}

// An append's size limit counts its edited text, while its writer may send
// up to maxBytes; readers that fall behind the edits it holds fail.
func TestAppendBufferLimitsItsEditedText(t *testing.T) {
	b := New(false, 4, 1<<20)
	_, start, _, _, _ := b.Read(-1)
	for range 4 << 10 {
		if !b.Write([]byte("abcd\f")) {
			t.Fatal("ended while the edited text fit")
		}
	}
	if b.String() != "" || b.total != 20<<10 {
		t.Fatalf("kept %q of %d bytes", b.String(), b.total)
	}
	if _, _, _, _, behind := b.Read(start); !behind {
		t.Fatal("a reader from the start is not behind")
	}
	if b.Write([]byte("wxyz!")) || !b.Limited() || b.Limit() != 4 || b.String() != "wxyz" {
		t.Fatalf("kept %q, limited %v to %d", b.String(), b.Limited(), b.Limit())
	}

	b = New(false, 4, 8)
	if b.Write([]byte("ab\b\b\bcd\xc3\xa9")) || !b.Limited() || b.Limit() != 8 || b.String() != "cd" {
		t.Fatalf("kept %q, limited %v to %d", b.String(), b.Limited(), b.Limit())
	}
}

// The kept text is converted with WHATWG's UTF-8 decode without BOM
// (§4.8.3): one U+FFFD per maximal invalid subsequence, and for an
// incomplete sequence at the end; a BOM is kept.
func TestLiveTextDecodesAsWHATWG(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"ok \u00e9", "ok \u00e9"},
		{"a\xF1\x80\x80\xE1\x80\xC2b", "a\uFFFD\uFFFD\uFFFDb"},
		{"\xC0\xAF", "\uFFFD\uFFFD"},
		{"\xED\xA0\x80", "\uFFFD\uFFFD\uFFFD"},
		{"\xE1\x80\xE2\xF0\x91\x92\xF1\xBF\x41", "\uFFFD\uFFFD\uFFFD\uFFFDA"},
		{"end \xE2\x82", "end \uFFFD"},
		{"\uFEFFbom", "\uFEFFbom"},
	} {
		live := New(true, 1<<10, 1<<20)
		live.Write([]byte(c.in))
		if got := live.String(); got != c.want {
			t.Errorf("%q: %q, want %q", c.in, got, c.want)
		}
	}
}
