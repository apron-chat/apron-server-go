package email

import "testing"

// Normalize lowers an address, and refuses what is no address of a public
// domain.
func TestNormalize(t *testing.T) {
	for value, want := range map[string]string{
		" Ada@Example.COM ":      "ada@example.com",
		"a.b+c@mail.example.org": "a.b+c@mail.example.org",
		"Ada <ada@example.com>":  "",
		"ada@localhost":          "",
		"ada@[10.0.0.5]":         "",
		"ada@10.0.0.5":           "",
		"ada@-bad.example":       "",
		"ada@exa_mple.com":       "",
		"ada":                    "",
		"":                       "",
	} {
		if got := Normalize(value); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", value, got, want)
		}
	}
}
