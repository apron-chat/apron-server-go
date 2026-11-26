package kv

import (
	"encoding/json/jsontext"
	"slices"
	"testing"
)

func TestValues(t *testing.T) {
	v := New()
	v.Put("ext/a", "x1", jsontext.Value(`1`))
	v.Put("ext/a", "x2", jsontext.Value(`2`))
	v.Put("ext/b", "x1", jsontext.Value(`3`))
	v.Delete("ext/a", "x2")
	v.Delete("ext/a", "none")
	if raw, ok := v.Get("ext/a", "x1"); !ok || string(raw) != "1" {
		t.Fatalf("Get = %s, %v", raw, ok)
	}
	if keys := v.Keys("ext/a", "x"); !slices.Equal(keys, []string{"x1"}) {
		t.Fatalf("Keys = %v", keys)
	}
	entries := v.Entries()
	stored := map[string]string{}
	for _, e := range entries {
		stored[e.Kind+" "+e.ID] = string(e.Value)
	}
	if len(entries) != 3 || stored["ext/a x1"] != "1" || stored["ext/a x2"] != "" || stored["ext/b x1"] != "3" {
		t.Fatalf("entries %v", stored)
	}
	if len(v.Entries()) != 0 {
		t.Fatal("entries taken twice")
	}
	v.Restore("ext/c", map[string]jsontext.Value{"k": jsontext.Value(`true`)})
	if _, ok := v.Get("ext/c", "k"); !ok || len(v.Entries()) != 0 {
		t.Fatal("Restore")
	}
}
