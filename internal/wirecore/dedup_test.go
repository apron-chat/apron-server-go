package wirecore

import "testing"

// A request still running is kept past Size, in its place, and forgotten
// once it is the oldest finished.
func TestDedupKeepsRunning(t *testing.T) {
	d := Dedup[string, int]{Size: 2}
	running, _ := d.Claim("a", [32]byte{})
	for _, key := range []string{"b", "c", "d"} {
		entry, prior := d.Claim(key, [32]byte{})
		if prior != nil {
			t.Fatalf("%s: a prior entry", key)
		}
		d.Finish(key, entry, 1, false)
	}
	if !d.Kept("a") || d.Kept("b") || d.Kept("c") || !d.Kept("d") {
		t.Fatalf("kept a %v, b %v, c %v, d %v; want a, running, and d", d.Kept("a"), d.Kept("b"), d.Kept("c"), d.Kept("d"))
	}
	if _, prior := d.Claim("a", [32]byte{}); prior != running {
		t.Fatal("a duplicate did not find the request still running")
	}
	d.Finish("a", running, 2, false)
	entry, _ := d.Claim("e", [32]byte{})
	d.Finish("e", entry, 3, false)
	if d.Kept("a") || !d.Kept("d") || !d.Kept("e") || len(d.entries) != 2 {
		t.Fatalf("kept a %v, d %v, e %v, %d in all; want the finished a forgotten", d.Kept("a"), d.Kept("d"), d.Kept("e"), len(d.entries))
	}
}
