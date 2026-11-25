// Package kv keeps the values extensions store (aprond's Host.Store), by
// entry kind, one per extension, and key, as the JSON they were handed
// over as, and which of them changed since they were last written to the
// store. It knows nothing of extensions, their names, or reserved keys.
//
// A Values is not safe for concurrent use: the server calls it holding its
// lock.
package kv

import (
	"encoding/json/jsontext"
	"slices"
	"strings"

	"github.com/apron-chat/apron-server-go/store"
)

// Values are the values, by entry kind and key.
type Values struct {
	values map[string]map[string]jsontext.Value
	// dirty are the values put or deleted since Entries last took them.
	dirty map[entry]struct{}
}

// entry names one value: its entry kind and its key.
type entry struct{ kind, key string }

// New returns no values.
func New() *Values {
	return &Values{values: make(map[string]map[string]jsontext.Value), dirty: make(map[entry]struct{})}
}

// Get returns the value under kind and key, which the caller must not
// modify.
func (v *Values) Get(kind, key string) (jsontext.Value, bool) {
	raw, ok := v.values[kind][key]
	return raw, ok
}

// Put keeps raw under kind and key, which the caller must not modify
// after.
func (v *Values) Put(kind, key string, raw jsontext.Value) {
	if v.values[kind] == nil {
		v.values[kind] = make(map[string]jsontext.Value)
	}
	v.values[kind][key] = raw
	v.dirty[entry{kind, key}] = struct{}{}
}

// Delete forgets the value under kind and key, if any.
func (v *Values) Delete(kind, key string) {
	if _, ok := v.values[kind][key]; ok {
		delete(v.values[kind], key)
		v.dirty[entry{kind, key}] = struct{}{}
	}
}

// Keys lists the keys of kind that start with prefix, sorted.
func (v *Values) Keys(kind, prefix string) []string {
	var keys []string
	for key := range v.values[kind] {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

// Restore takes the stored values of kind, as the store holds them.
func (v *Values) Restore(kind string, values map[string]jsontext.Value) {
	v.values[kind] = values
}

// Entries returns the store entries of the values put or deleted since it
// was last called: a deleted value's entry has no Value.
func (v *Values) Entries() []store.Entry {
	if len(v.dirty) == 0 {
		return nil
	}
	entries := make([]store.Entry, 0, len(v.dirty))
	for e := range v.dirty {
		if raw, ok := v.values[e.kind][e.key]; ok {
			entries = append(entries, store.Entry{Kind: e.kind, ID: e.key, Value: raw})
		} else {
			entries = append(entries, store.Entry{Kind: e.kind, ID: e.key})
		}
	}
	clear(v.dirty)
	return entries
}
