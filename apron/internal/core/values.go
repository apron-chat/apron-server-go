package core

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
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
func ParseLogID[S string | []byte](s S) (int64, error) {
	if len(s) == 0 || s[0] == '0' && len(s) > 1 {
		return 0, ErrNotLogID
	}
	var n int64
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, ErrNotLogID
		}
		// MaxLogID has 16 digits: a number of more is past it, and is
		// not added up, where it could overflow.
		if i < 16 {
			n = 10*n + int64(s[i]-'0')
		}
	}
	if len(s) > 16 || n > MaxLogID {
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
