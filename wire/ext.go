package wire

import (
	"encoding/json/jsontext"
	"fmt"
	"maps"
)

// Ext is an ext object (§4.13), keyed by extension name, each value kept
// as the JSON it arrived as, so that a merge or a store keeps it byte for
// byte. Every ext in this package is one: the params' and the frames'.
type Ext map[string]jsontext.Value

// With returns a copy of e with key set to v encoded as JSON; e is not
// modified, and may be nil, as in Ext{}.With("irc", ...) built in one
// expression. A jsontext.Value is kept as it is, compacted. It panics if v
// cannot be encoded, as a value the program builds itself, such as a map
// of strings, always can.
func (e Ext) With(key string, v any) Ext {
	raw, err := marshal(v)
	if err != nil {
		panic(fmt.Sprintf("wire: ext %q: %v", key, err))
	}
	ext := make(Ext, len(e)+1)
	maps.Copy(ext, e)
	ext[key] = raw
	return ext
}

// clone returns a copy of e that shares nothing with it, or nil.
func (e Ext) clone() Ext {
	if e == nil {
		return nil
	}
	ext := make(Ext, len(e))
	for key, value := range e {
		ext[key] = cloneRaw(value)
	}
	return ext
}

// cloneRaw copies a raw value, keeping nil nil.
func cloneRaw(v jsontext.Value) jsontext.Value {
	if v == nil {
		return nil
	}
	return append(jsontext.Value{}, v...)
}

// MergeExt merges a write's ext into a kept one, one level deep (§4.13):
// each key the write carries replaces the kept value, a key whose value is
// empty ("", [], {}) is removed, and keys the write leaves out stay. null
// is an ordinary value. The result is a new object, or nil when no key is
// left; kept is not modified.
func MergeExt(kept, write Ext) Ext {
	merged := make(Ext, len(kept)+len(write))
	maps.Copy(merged, kept)
	for key, value := range write {
		if emptyJSON(value) {
			delete(merged, key)
		} else {
			merged[key] = value
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// emptyJSON reports whether a compact JSON value is an empty value, which
// clears what it is merged into (§3.3.1).
func emptyJSON(value jsontext.Value) bool {
	switch string(value) {
	case `""`, `[]`, `{}`:
		return true
	}
	return false
}
