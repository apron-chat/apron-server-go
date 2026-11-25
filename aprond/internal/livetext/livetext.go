// Package livetext holds live text (PROTOCOL.md §4.8.3) as its writer
// sends it, and wakes its readers as it grows: a terminal's output, which
// keeps its last bytes (§4.8.6), or an append's text, which keeps its first
// bytes with its edits applied (§4.8.5). A Text owns its own lock; it knows
// nothing of embeds, messages, or HTTP.
package livetext

import (
	"sync"
	"unicode/utf8"

	"golang.org/x/text/encoding/unicode"
)

// Text holds live text as it arrives and wakes its readers as it grows.
// A terminal keeps its last keep bytes, cut by KeepLast. An append keeps
// its first keep bytes of edited text, up to a whole character (§4.8.5).
// Past maxBytes from its writer, the text ends.
type Text struct {
	mu       sync.Mutex
	terminal bool
	keep     int
	maxBytes int64
	// data holds what readers read, from absolute offset start on, and at
	// most twice what it holds after a trim. A terminal's is the bytes as
	// they arrived, at least its last keep bytes. An append's is its text
	// as decoded, with backspaces and form feeds: readers that are reading
	// it apply them, while a new reader starts after it, with kept.
	data  []byte
	start int64
	// total counts the bytes the writer sent.
	total int64
	// An append's kept text: decoded, with its edits applied. pending holds
	// the start of a character the writer has not finished sending.
	kept    []byte
	pending []byte
	changed chan struct{}
	ended   bool
	atLimit bool
	// reached is the size limit the text reached.
	reached int64
	// finished closes when the text ends, for its writer.
	finished chan struct{}
}

// New returns the text of a terminal, if terminal, which keeps its last
// keep bytes, or of an append, which keeps its first keep bytes of edited
// text; it ends past maxBytes from its writer.
func New(terminal bool, keep int, maxBytes int64) *Text {
	return &Text{terminal: terminal, keep: keep, maxBytes: maxBytes, changed: make(chan struct{}), finished: make(chan struct{})}
}

// Write adds to the text and reports whether it still accepts more. Past
// a size limit it keeps what fits and ends.
func (b *Text) Write(p []byte) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ended {
		return false
	}
	if room := b.maxBytes - b.total; int64(len(p)) > room {
		p = p[:room]
		b.limitLocked(b.maxBytes)
	}
	b.total += int64(len(p))
	if b.terminal {
		b.data = append(b.data, p...)
	} else {
		b.appendLocked(b.decodeLocked(p, false))
	}
	hold := b.keep
	if !b.terminal {
		hold = max(hold, minLiveHold)
	}
	if len(b.data) > 2*hold {
		excess := len(b.data) - hold
		b.data = append([]byte(nil), b.data[excess:]...)
		b.start += int64(excess)
	}
	b.signalLocked()
	if b.atLimit {
		b.endLocked()
	}
	return !b.ended
}

// minLiveHold is the least that data holds after a trim, for an append that
// keeps little or nothing, whose edits still reach its readers.
const minLiveHold = 4 << 10

func (b *Text) limitLocked(limit int64) {
	if !b.atLimit {
		b.atLimit = true
		b.reached = limit
	}
}

// decodeLocked converts p, after what is pending, with WHATWG's UTF-8 decode
// without BOM (§4.8.3): each invalid sequence becomes one U+FFFD, as a
// browser's TextDecoder shows it. Until atEOF, it holds back the start of
// a character that p ends partway through; at the end that becomes U+FFFD.
func (b *Text) decodeLocked(p []byte, atEOF bool) []byte {
	src := append(b.pending, p...)
	// Each byte decodes to at most three, those of U+FFFD. x/text's UTF8
	// keeps a BOM, and fails only for want of more of src (ErrShortSrc).
	dst := make([]byte, 3*len(src))
	nDst, nSrc, _ := unicode.UTF8.NewDecoder().Transform(dst, src, atEOF)
	b.pending = append([]byte(nil), src[nSrc:]...)
	return dst[:nDst]
}

// appendLocked adds decoded text to an append (§4.8.5). A backspace
// (U+0008) removes the character before it, and a form feed (U+000C) all
// the text before it, from the kept text only, never from the message's
// opening body.text. The first character that the kept text has no room
// for fills it, and the text ends without it.
func (b *Text) appendLocked(decoded []byte) {
	n := 0
	for n < len(decoded) {
		r, size := utf8.DecodeRune(decoded[n:])
		switch r {
		case '\b':
			_, last := utf8.DecodeLastRune(b.kept)
			b.kept = b.kept[:len(b.kept)-last]
		case '\f':
			b.kept = b.kept[:0]
		default:
			if len(b.kept)+size > b.keep {
				b.limitLocked(int64(b.keep))
				b.data = append(b.data, decoded[:n]...)
				return
			}
			b.kept = append(b.kept, decoded[n:n+size]...)
		}
		n += size
	}
	b.data = append(b.data, decoded...)
}

// End ends the text, keeping what arrived.
func (b *Text) End() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.endLocked()
}

// endLocked ends the text. An append that ends partway through a character
// ends with U+FFFD for it, as a reader's decoder shows it, if the kept text
// has room for it. Without room it ends without it: the writer stayed
// within the limit, so the text is not limited.
func (b *Text) endLocked() {
	if b.ended {
		return
	}
	if !b.terminal && !b.atLimit && len(b.pending) > 0 {
		// The rest is U+FFFD alone, with no edits to apply.
		if rest := b.decodeLocked(nil, true); len(b.kept)+len(rest) <= b.keep {
			b.appendLocked(rest)
		}
	}
	b.ended = true
	close(b.finished)
	b.signalLocked()
}

func (b *Text) signalLocked() {
	close(b.changed)
	b.changed = make(chan struct{})
}

// Ended reports whether the text ended.
func (b *Text) Ended() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ended
}

// Limited reports whether the text reached a size limit.
func (b *Text) Limited() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.atLimit
}

// Limit is the size limit the text reached: what its writer may send, or
// what an append keeps.
func (b *Text) Limit() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reached
}

// tailLocked returns the offset in data of the last keep bytes a terminal
// keeps, and whether it kept less than it received.
func (b *Text) tailLocked() (int, bool) {
	if !b.terminal || b.total <= int64(b.keep) {
		return 0, false
	}
	return len(b.data) - b.keep, true
}

// keptStartLocked is the absolute offset of the kept text: for a terminal
// past its size, the start of KeepLast.
func (b *Text) keptStartLocked() int64 {
	tail, cut := b.tailLocked()
	if !cut {
		return b.start
	}
	return b.start + int64(tail+keepLastStart(b.data[tail:]))
}

// Read returns the bytes from position on, the next position, a channel
// closed on the next change, and whether the text has ended with nothing
// more to read. A new reader (position -1) reads the kept text: a
// terminal's from its start, and an append's edited text so far. For one
// that fell behind what the text holds, so that bytes it has not read are
// gone, read reports behind instead.
func (b *Text) Read(position int64) (chunk []byte, next int64, changed chan struct{}, ended, behind bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	end := b.start + int64(len(b.data))
	if position < 0 && !b.terminal {
		return append([]byte(nil), b.kept...), end, b.changed, b.ended, false
	}
	if position < 0 {
		position = b.keptStartLocked()
	} else if position < b.start {
		return nil, position, nil, false, true
	}
	position = min(position, end)
	chunk = append([]byte(nil), b.data[position-b.start:]...)
	return chunk, end, b.changed, b.ended, false
}

// String is the kept text. A terminal's is converted with WHATWG's UTF-8
// decode without BOM (§4.8.3): each invalid sequence, and an incomplete one
// at the end, becomes one U+FFFD, as a browser's TextDecoder shows it. An
// append's was converted the same way as it arrived, and edited.
func (b *Text) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.terminal {
		return string(b.kept)
	}
	kept := b.data[b.keptStartLocked()-b.start:]
	decoded, _ := unicode.UTF8.NewDecoder().Bytes(kept) // x/text's UTF8 never fails, and keeps a BOM
	return string(decoded)
}

// KeepLast is the last part of output of at most limit bytes that a
// terminal keeps (§4.8.6): it starts at its first byte below 0x20, or
// without one, at its first byte outside 0x80–0xBF.
func KeepLast(output []byte, limit int) []byte {
	if len(output) <= limit {
		return output
	}
	tail := output[len(output)-limit:]
	return tail[keepLastStart(tail):]
}

// keepLastStart is where KeepLast starts in tail.
func keepLastStart(tail []byte) int {
	for i, c := range tail {
		if c < 0x20 {
			return i
		}
	}
	for i, c := range tail {
		if c < 0x80 || c > 0xBF {
			return i
		}
	}
	return len(tail)
}

// WholeRunes drops a character that p ends partway through, as when an
// append is cut at its size limit (§4.8.5).
func WholeRunes(p []byte) []byte {
	for i := len(p) - 1; i >= 0 && i >= len(p)-utf8.UTFMax; i-- {
		if utf8.RuneStart(p[i]) {
			if !utf8.FullRune(p[i:]) {
				return p[:i]
			}
			return p
		}
	}
	return p
}

// Done is closed when the text ends, for its writer.
func (b *Text) Done() <-chan struct{} { return b.finished }

// Keep is what the text keeps: a terminal's last bytes, or an append's
// first bytes of edited text.
func (b *Text) Keep() int { return b.keep }

// Progress reports the bytes the writer sent, and the offset of the oldest
// byte the text still holds for readers.
func (b *Text) Progress() (received, start int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total, b.start
}
