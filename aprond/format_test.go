package aprond

import (
	"context"
	"strings"
	"testing"

	"github.com/apron-chat/apron-server-go/store"
)

func storedFormatOf(t *testing.T, st store.Store) string {
	t.Helper()
	var format string
	_ = st.Load(func(e store.Entry) error {
		if e.Kind == entryMeta && e.ID == entryFormat {
			format = string(e.Value)
		}
		return nil
	})
	return format
}

// A new store says its format; one without, as every server before
// format versions wrote, is format 1 and says it once opened; one of a
// newer format than the server knows is refused.
func TestStoreFormat(t *testing.T) {
	st := store.NewMemory()
	s, err := Open(Config{Store: st})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := storedFormatOf(t, st); got != `{"version":1}` {
		t.Fatalf("a new store's format is %q", got)
	}

	// The stored shapes of a server before format versions.
	old := store.NewMemory()
	_ = old.Apply(loadStoredLog(t))
	if storedFormatOf(t, old) != "" {
		t.Fatal("the fixture says a format")
	}
	s, err = Open(Config{Store: old})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := storedFormatOf(t, old); got != `{"version":1}` {
		t.Fatalf("an unversioned store's format is %q once opened", got)
	}

	newer := store.NewMemory()
	_ = newer.Apply([]store.Entry{{Kind: entryMeta, ID: entryFormat, Value: []byte(`{"version":2}`)}})
	if _, err := Open(Config{Store: newer}); err == nil || !strings.Contains(err.Error(), "format version 2, newer than this aprond's 1") {
		t.Fatalf("a newer store opened: %v", err)
	}
	broken := store.NewMemory()
	_ = broken.Apply([]store.Entry{{Kind: entryMeta, ID: entryFormat, Value: []byte(`"one"`)}})
	if _, err := Open(Config{Store: broken}); err == nil {
		t.Fatal("a store with no format version opened")
	}
}
