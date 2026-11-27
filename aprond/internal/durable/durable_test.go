package durable

import (
	"context"
	"errors"
	"reflect"
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

var (
	errClosed = errors.New("closed")
	errFailed = errors.New("failed")
)

func entry(id string) []store.Entry {
	return []store.Entry{{Kind: "k", ID: id, Value: []byte(`true`)}}
}

func TestFencesWaitForEarlierBatches(t *testing.T) {
	st := store.NewMemory()
	w := Start(st, nil, errFailed, errClosed)
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
	w := Start(st, func(err error) { reported <- err }, errFailed, errClosed)
	f := w.Fence()
	w.Queue(entry("a"))
	if err := f.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.Queue(entry("b"))
	f = w.Fence()
	w.Queue(entry("c"))
	if err := f.Wait(context.Background()); !errors.Is(err, errFailed) {
		t.Fatalf("fence after the failure: %v", err)
	}
	if err := <-reported; !errors.Is(err, errFailed) || !errors.Is(w.Err(), errFailed) {
		t.Fatalf("reported %v, Err %v", err, w.Err())
	}
	w.Stop()
	if len(st.applied) != 1 {
		t.Fatalf("applied %d batches, want only the one before the failure", len(st.applied))
	}
}

// blocking is a store whose first Apply waits until release is closed.
type blocking struct {
	failing
	started, release chan struct{}
	once             sync.Once
}

func (b *blocking) Apply(batch []store.Entry) error {
	b.once.Do(func() {
		close(b.started)
		<-b.release
	})
	return b.failing.Apply(batch)
}

// The batches that wait while the store applies one are applied as one
// next, in order, keeping the last of the entries with one kind and ID, and
// their fences are released after it.
func TestWaitingBatchesMerge(t *testing.T) {
	st := &blocking{started: make(chan struct{}), release: make(chan struct{})}
	w := Start(st, nil, errFailed, errClosed)
	defer w.Stop()
	w.Queue(entry("a"))
	<-st.started
	var fences []*Fence
	for _, id := range []string{"b", "c", "b", "d"} {
		fences = append(fences, w.Fence())
		w.Queue(entry(id))
	}
	close(st.release)
	for _, f := range fences {
		if err := f.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	var got [][]string
	for _, batch := range st.applied {
		var ids []string
		for _, e := range batch {
			ids = append(ids, e.ID)
		}
		got = append(got, ids)
	}
	// The second b replaces the first.
	if want := [][]string{{"a"}, {"c", "b", "d"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("applied %v, want %v", got, want)
	}
}
