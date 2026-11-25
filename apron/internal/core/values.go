package core

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"strconv"
)

// IsNull reports whether raw is the JSON null.
func IsNull(raw jsontext.Value) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

// MaxLogID is the greatest log_id (§2.1): log_ids are below 2^53.
const MaxLogID = 1<<53 - 1

// The errors of ParseLogID, which its callers word.
var (
	ErrNotLogID   = errors.New("not a decimal string")
	ErrLogIDRange = errors.New("not below 2^53")
)

// ParseLogID reads a log_id (§2.1): a decimal string of a non-negative
// number, without a sign or leading zeros, of at most MaxLogID. It tells a
// string that is no number (ErrNotLogID) from a number past MaxLogID
// (ErrLogIDRange).
func ParseLogID(s string) (int64, error) {
	if s == "" || s[0] == '0' && len(s) > 1 {
		return 0, ErrNotLogID
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, ErrNotLogID
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n > MaxLogID {
		// Only a number too large for an int64 is not parsed.
		return 0, ErrLogIDRange
	}
	return n, nil
}

// CloneRaw copies a raw value, keeping nil nil.
func CloneRaw(v jsontext.Value) jsontext.Value {
	if v == nil {
		return nil
	}
	return append(jsontext.Value{}, v...)
}

// CloneExt returns a copy of an ext object that shares nothing with it, or
// nil.
func CloneExt[M ~map[string]jsontext.Value](e M) M {
	if e == nil {
		return nil
	}
	ext := make(M, len(e))
	for key, value := range e {
		ext[key] = CloneRaw(value)
	}
	return ext
}
