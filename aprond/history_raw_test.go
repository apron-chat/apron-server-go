package aprond

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/aprond/internal/roomlog"
)

// rawVsTyped reports how a record's raw bytes compare with its typed
// encoding, as history would send it decoded.
func rawVsTyped(t *testing.T, kind roomlog.Kind, raw jsontext.Value) (typed []byte) {
	t.Helper()
	var err error
	switch kind {
	case roomlog.KindRoom:
		typed, err = apron.Marshal(decodeRecord[apron.Room](raw))
	case roomlog.KindMessage:
		typed, err = apron.Marshal(decodeRecord[apron.Message](raw))
	case roomlog.KindReactions:
		typed, err = apron.Marshal(decodeRecord[apron.Reactions](raw))
	case roomlog.KindMembership:
		typed, err = apron.Marshal(decodeRecord[apron.Membership](raw))
	}
	if err != nil {
		t.Fatal(err)
	}
	return typed
}

// History sends records as the log keeps them (apron.RawRecords), which
// must be what their typed encoding is but for the order of their
// members: so for each record a server stored (testdata/stored-log.jsonl),
// the raw bytes and the typed encoding are equal as JSON.
func TestHistoryRawRecords(t *testing.T) {
	kinds := map[roomlog.Kind]int{}
	for _, e := range loadStoredLog(t) {
		if e.Kind != roomlog.EntryKind {
			continue
		}
		var s struct {
			Kind roomlog.Kind   `json:"kind"`
			Raw  jsontext.Value `json:"raw"`
		}
		if err := json.Unmarshal(e.Value, &s); err != nil {
			t.Fatal(err)
		}
		kinds[s.Kind]++
		typed := rawVsTyped(t, s.Kind, s.Raw)
		if !sameJSON(t, s.Raw, typed) {
			t.Errorf("record %s:\n raw   %s\n typed %s", e.ID, s.Raw, typed)
		}
	}
	if len(kinds) != 4 {
		t.Fatalf("the stored records have kinds %v, want all four", kinds)
	}
}

// sameJSON reports whether a and b are the same JSON value, with object
// members in any order.
func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	canonical := func(raw []byte) []byte {
		value := jsontext.Value(bytes.Clone(raw))
		if err := value.Canonicalize(); err != nil {
			t.Fatal(err)
		}
		return value
	}
	return bytes.Equal(canonical(a), canonical(b))
}
