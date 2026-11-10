package apron

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/store"
)

// failingStore is a memory store whose Apply fails once failing is set,
// and which counts the batches it was asked to apply.
type failingStore struct {
	*store.Memory
	failing atomic.Bool
	applied atomic.Int64
	// gate, while held, blocks Apply.
	gate sync.Mutex
}

func (f *failingStore) Apply(batch []store.Entry) error {
	f.gate.Lock()
	defer f.gate.Unlock()
	f.applied.Add(1)
	if f.failing.Load() {
		return errors.New("disk full")
	}
	return f.Memory.Apply(batch)
}

func TestSyncWaitsForTheStore(t *testing.T) {
	backing := &failingStore{Memory: store.NewMemory()}
	var host *Host
	config := extensionConfig(testExtension{"durable", func(r *Registrar) error {
		host = r.Host
		return nil
	}})
	config.Store = backing
	_, _ = newTestServer(t, config)

	// A Put is stored once Sync returns, and not before while the store is
	// slow.
	backing.gate.Lock()
	if err := host.Store().Put("k", "v"); err != nil {
		t.Fatal(err)
	}
	synced := make(chan error, 1)
	go func() { synced <- host.Sync(context.Background()) }()
	select {
	case err := <-synced:
		t.Fatalf("Sync returned %v before the store applied the batch", err)
	case <-time.After(50 * time.Millisecond):
	}
	backing.gate.Unlock()
	if err := <-synced; err != nil {
		t.Fatal(err)
	}
	var found bool
	_ = backing.Load(func(e store.Entry) error {
		found = found || (e.Kind == "ext/durable" && e.ID == "k")
		return nil
	})
	if !found {
		t.Fatal("Sync returned before the value was stored")
	}

	// A context that ends first ends the wait.
	backing.gate.Lock()
	_ = host.Store().Put("k", "w")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	if err := host.Sync(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Sync with an ended context = %v", err)
	}
	cancel()
	backing.gate.Unlock()
}

func TestAFailedStoreStaysFailed(t *testing.T) {
	backing := &failingStore{Memory: store.NewMemory()}
	var host *Host
	var reported []error
	var reportedMu sync.Mutex
	config := extensionConfig(testExtension{"durable", func(r *Registrar) error {
		host = r.Host
		return nil
	}})
	config.Store = backing
	config.OnStoreError = func(err error) {
		reportedMu.Lock()
		reported = append(reported, err)
		reportedMu.Unlock()
	}
	app, httpServer := newTestServer(t, config)
	if err := app.Sync(context.Background()); err != nil || app.StoreErr() != nil {
		t.Fatalf("Sync = %v, StoreErr = %v", err, app.StoreErr())
	}

	backing.failing.Store(true)
	_ = host.Store().Put("k", "v")
	if err := host.Sync(context.Background()); !errors.Is(err, ErrStoreFailed) {
		t.Fatalf("Sync after a failure = %v", err)
	}
	applied := backing.applied.Load()
	// Nothing more is written, and every later Sync fails.
	backing.failing.Store(false)
	_ = host.Store().Put("k", "w")
	if err := host.Sync(context.Background()); !errors.Is(err, ErrStoreFailed) {
		t.Fatalf("Sync after a failure = %v", err)
	}
	if backing.applied.Load() != applied {
		t.Fatal("the server wrote to a store that failed")
	}
	// OnStoreError runs on a goroutine of its own.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		reportedMu.Lock()
		n := len(reported)
		reportedMu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
	}
	reportedMu.Lock()
	if len(reported) != 1 || !errors.Is(reported[0], ErrStoreFailed) || !errors.Is(app.StoreErr(), ErrStoreFailed) {
		t.Fatalf("reported %v, StoreErr %v", reported, app.StoreErr())
	}
	reportedMu.Unlock()
	if status, _, body := httpDo(t, http.MethodGet, httpServer.URL+"/healthz", nil, ""); status != http.StatusServiceUnavailable || string(body) != `{"status":"store_failed"}` {
		t.Fatalf("healthz: %d %s", status, body)
	}
}

// OnStoreError may shut the server down.
func TestOnStoreErrorMayShutDown(t *testing.T) {
	backing := &failingStore{Memory: store.NewMemory()}
	var host *Host
	done := make(chan error, 1)
	var app *Server
	config := extensionConfig(testExtension{"durable", func(r *Registrar) error {
		host = r.Host
		return nil
	}})
	config.Store = backing
	config.OnStoreError = func(error) { done <- app.Shutdown(context.Background()) }
	app, _ = newTestServer(t, config)
	backing.failing.Store(true)
	_ = host.Store().Put("k", "v")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown from OnStoreError did not return")
	}
}
