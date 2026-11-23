package push

import (
	"encoding/json/jsontext"
	"slices"
	"testing"
	"time"
)

func TestWake(t *testing.T) {
	if got := (Urgent | Badge).Names(); !slices.Equal(got, []string{"mentions", "replies", "private", "badge"}) {
		t.Fatalf("Names = %v", got)
	}
	if got := Wake(0).Names(); got == nil || len(got) != 0 {
		t.Fatalf("no scopes = %#v, want []", got)
	}
	if ParseWake([]string{"joined", "ext:later", "replies"}) != Joined|Replies {
		t.Fatal("ParseWake")
	}
}

func TestTable(t *testing.T) {
	now := time.Now()
	table := NewTable()
	a := New(Registration{User: "ada", Kind: "relay", URL: "https://b.example", Renewed: now})
	b := New(Registration{User: "ada", Kind: "relay", URL: "https://a.example", Renewed: now.Add(-Expiry)})
	c := New(Registration{User: "bob", Kind: "webpush", URL: "https://a.example", Wake: Badge, Renewed: now})
	for _, p := range []*Registration{a, b, c} {
		table.Add(p)
	}
	if table.Len() != 3 || table.Count("ada") != 2 || table.Get("bob", "https://a.example") != c {
		t.Fatal("lookups")
	}
	if got := table.Of("ada"); !slices.Equal(got, []*Registration{b, a}) {
		t.Fatalf("Of sorts by url: %v", got)
	}
	if table.Oldest("ada") != b || !slices.Equal(table.Expired(now), []*Registration{b}) {
		t.Fatal("Oldest, Expired")
	}
	if c.TakesBadges() {
		t.Fatal("webpush takes badges")
	}
	again := New(Registration{User: "ada", Kind: "relay", URL: "https://b.example", Renewed: now})
	table.Add(again)
	if table.Remove(a) || table.Get("ada", a.URL) != again {
		t.Fatal("a replaced registration is removed")
	}
	if !table.Remove(c) || table.Count("bob") != 0 {
		t.Fatal("Remove")
	}
	entries := table.Entries()
	if len(entries) != 3 {
		t.Fatalf("entries %v", entries)
	}
	for _, e := range entries {
		if (e.Value == nil) != (e.ID == Key("bob", "https://a.example")) {
			t.Fatalf("entry %s = %s", e.ID, e.Value)
		}
	}
}

// A badge push goes out for a count the endpoint did not accept last, and
// not for the count of a push in flight; a push accepted late records no
// older count.
func TestDeliveries(t *testing.T) {
	p := New(Registration{User: "ada", Kind: "relay", URL: "https://a.example", Wake: Badge})
	if !p.WantsBadge(0) {
		t.Fatal("a new registration wants its count")
	}
	first := p.Sending(1)
	if p.WantsBadge(1) || !p.WantsBadge(2) {
		t.Fatal("in flight")
	}
	second := p.Sending(2)
	p.Delivered(second, 2, true)
	p.Delivered(first, 1, true)
	if p.WantsBadge(2) || !p.WantsBadge(1) {
		t.Fatal("a push accepted late recorded its older count")
	}
	third := p.Sending(3)
	p.Delivered(third, 3, false)
	if !p.WantsBadge(3) {
		t.Fatal("a refused push recorded its count")
	}
}

func TestDay(t *testing.T) {
	var d Day
	now := time.Date(2026, 1, 1, 23, 0, 0, 0, time.UTC)
	d.Accepted(now)
	d.Accepted(now)
	if d.Count(now) != 2 || d.Count(now.Add(2*time.Hour)) != 0 {
		t.Fatal("Day")
	}
}

// Restore keeps what keep accepts, deletes the others from the store, and
// keeps a registration stored by its url alone by its key.
func TestRestore(t *testing.T) {
	table := NewTable()
	err := table.Restore(map[string]jsontext.Value{
		"https://old.example":     jsontext.Value(`{"user":"ada","kind":"relay","url":"https://old.example","wake":["mentions"]}`),
		"gone https://x.example":  jsontext.Value(`{"user":"gone","kind":"relay","url":"https://x.example","wake":[]}`),
		"ada https://web.example": jsontext.Value(`{"user":"ada","kind":"webpush","url":"https://web.example","device":"zz","wake":[]}`),
	}, func(p *Registration) bool { return p.User == "ada" })
	if err != nil {
		t.Fatal(err)
	}
	if p := table.Get("ada", "https://old.example"); p == nil || p.Wake != Mentions || !p.WantsBadge(0) {
		t.Fatalf("restored %#v", p)
	}
	var put, deleted []string
	for _, e := range table.Entries() {
		if e.Value == nil {
			deleted = append(deleted, e.ID)
		} else {
			put = append(put, e.ID)
		}
	}
	slices.Sort(put)
	if !slices.Equal(put, []string{"ada https://old.example", "ada https://web.example"}) || !slices.Equal(deleted, []string{"gone https://x.example"}) {
		t.Fatalf("put %v, deleted %v", put, deleted)
	}
	if table.Restore(map[string]jsontext.Value{"k": jsontext.Value(`{"user":"ada","p256dh":"x","auth":"y","wake":[]}`)}, func(*Registration) bool { return true }) == nil {
		t.Fatal("bad keys restore")
	}
}
