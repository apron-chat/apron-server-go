package wirecore

import "testing"

// A request still running is kept past Size, in its place, and forgotten
// once it is the oldest finished.
func TestDedupKeepsRunning(t *testing.T) {
	d := Dedup[string, int]{Size: 2}
	running, _ := d.Claim("a", [32]byte{}, 0)
	for _, key := range []string{"b", "c", "d"} {
		entry, prior := d.Claim(key, [32]byte{}, 0)
		if prior != nil {
			t.Fatalf("%s: a prior entry", key)
		}
		d.Finish(key, entry, 1, 0, false)
	}
	if !d.Kept("a") || d.Kept("b") || d.Kept("c") || !d.Kept("d") {
		t.Fatalf("kept a %v, b %v, c %v, d %v; want a, running, and d", d.Kept("a"), d.Kept("b"), d.Kept("c"), d.Kept("d"))
	}
	if _, prior := d.Claim("a", [32]byte{}, 0); prior != running {
		t.Fatal("a duplicate did not find the request still running")
	}
	d.Finish("a", running, 2, 0, false)
	entry, _ := d.Claim("e", [32]byte{}, 0)
	d.Finish("e", entry, 3, 0, false)
	if d.Kept("a") || !d.Kept("d") || !d.Kept("e") || d.Len() != 2 {
		t.Fatalf("kept a %v, d %v, e %v, %d in all; want the finished a forgotten", d.Kept("a"), d.Kept("d"), d.Kept("e"), len(d.entries))
	}
}

// MaxBytes bounds what the requests kept cost, oldest finished forgotten
// first; an outcome that does not fit is not kept, and its waiters still
// get it.
func TestDedupMaxBytes(t *testing.T) {
	d := Dedup[string, int]{Size: 100, MaxBytes: 3*DedupEntryBytes + 300}
	finish := func(key string, bytes int) *DedupEntry[int] {
		t.Helper()
		entry, prior := d.Claim(key, [32]byte{}, len(key))
		if prior != nil {
			t.Fatalf("%s: a prior entry", key)
		}
		d.Finish(key, entry, bytes, bytes, false)
		return entry
	}
	finish("a", 99)
	finish("b", 99)
	finish("c", 99)
	if d.Len() != 3 || d.Bytes() != 3*DedupEntryBytes+300 {
		t.Fatalf("%d kept, %d bytes; want 3, %d", d.Len(), d.Bytes(), 3*DedupEntryBytes+300)
	}
	finish("d", 1)
	if d.Kept("a") || !d.Kept("b") || !d.Kept("d") {
		t.Fatalf("kept a %v, b %v, d %v; want a forgotten", d.Kept("a"), d.Kept("b"), d.Kept("d"))
	}
	big := finish("e", 4*DedupEntryBytes)
	if d.Kept("e") || big.Value != 4*DedupEntryBytes || !isClosed(big.done) {
		t.Fatal("an outcome over MaxBytes was kept, or not finished")
	}
	if d.Bytes() > d.MaxBytes {
		t.Fatalf("%d bytes kept, over %d", d.Bytes(), d.MaxBytes)
	}

	// A request forgotten and claimed again is counted once.
	entry, _ := d.Claim("f", [32]byte{}, 1)
	d.Forget("f", entry)
	again, _ := d.Claim("f", [32]byte{}, 1)
	d.Finish("f", entry, 0, 1000, false) // The forgotten one: not counted.
	d.Finish("f", again, 0, 1, false)
	total := 0
	for _, key := range []string{"a", "b", "c", "d", "e", "f"} {
		if element := d.entries[key]; element != nil {
			total += element.Value.(*dedupNode[string, int]).bytes
		}
	}
	if total != d.Bytes() {
		t.Fatalf("Bytes is %d, the entries cost %d", d.Bytes(), total)
	}
	for d.EvictOldest() {
	}
	if d.Len() != 0 || d.Bytes() != 0 {
		t.Fatalf("%d kept, %d bytes once all are evicted", d.Len(), d.Bytes())
	}
}
