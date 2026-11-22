package durable

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/apron-chat/apron-server-go/store"
)

// failing is a store whose Apply fails from the failAt-th batch on.
type failing struct {
	mu      sync.Mutex
	applied [][]store.Entry
	failAt  int
}

func (f *failing) Load(func(store.Entry) error) error { return nil }
func (f *failing) Close() error                       { return nil }
func (f *failing) Apply(batch []store.Entry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAt > 0 && len(f.applied)+1 >= f.failAt {
		return errors.New("disk full")
	}
	f.applied = append(f.applied, batch)
	return nil
}

var errClosed = errors.New("closed")

func entry(id string) []store.Entry {
	return []store.Entry{{Kind: "k", ID: id, Value: []byte(`true`)}}
}

func TestFencesWaitForEarlierBatches(t *testing.T) {
	st := store.NewMemory()
	w := Start(st, nil, errClosed)
	w.Queue(entry("a"))
	f := w.Fence()
	w.Queue(entry("b"))
	if err := f.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	var ids []string
	_ = st.Load(func(e store.Entry) error { ids = append(ids, e.ID); return nil })
	if len(ids) != 2 {
		t.Fatalf("stored %v, want a and b", ids)
	}
	// A fence with nothing to write still waits for what came before.
	f = w.Fence()
	w.Queue(nil)
	if err := f.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.Stop()
	f = w.Fence()
	w.Queue(entry("c"))
	if err := f.Wait(context.Background()); !errors.Is(err, errClosed) {
		t.Fatalf("a fence after Stop: %v", err)
	}
}

func TestFailureIsFinal(t *testing.T) {
	st := &failing{failAt: 2}
	reported := make(chan error, 1)
	w := Start(st, func(err error) { reported <- err }, errClosed)
	w.Queue(entry("a"))
	w.Queue(entry("b"))
	f := w.Fence()
	w.Queue(entry("c"))
	if err := f.Wait(context.Background()); !errors.Is(err, ErrFailed) {
		t.Fatalf("fence after the failure: %v", err)
	}
	if err := <-reported; !errors.Is(err, ErrFailed) || !errors.Is(w.Err(), ErrFailed) {
		t.Fatalf("reported %v, Err %v", err, w.Err())
	}
	w.Stop()
	if len(st.applied) != 1 {
		t.Fatalf("applied %d batches, want only the one before the failure", len(st.applied))
	}
}
