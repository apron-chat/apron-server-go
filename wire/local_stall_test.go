package wire

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// stallBackend is a Backend whose Hello waits for release, as one would for
// a lock its caller holds.
type stallBackend struct{ release chan struct{} }

func (b stallBackend) Hello(context.Context, *Conn) ServerInfo {
	if b.release != nil {
		<-b.release
	}
	return ServerInfo{}
}

func (stallBackend) Auth(context.Context, *Conn, AuthParams, Reply[AuthResult]) error {
	return Denied("no")
}

// syncBuffer is a log that tests read while the server writes it.
type syncBuffer struct {
	mu  sync.Mutex
	out bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.out.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.out.String()
}

// stallWarning is what Server.Local logs once it has waited localOpenStall.
const stallWarning = "still waiting for OnConnect and Hello"

// Server.Local logs that it waits for OnConnect and Hello once it has
// waited localOpenStall, while it still does, and not when they return in
// time.
func TestLocalStallWarning(t *testing.T) {
	defer func(d time.Duration) { localOpenStall = d }(localOpenStall)
	localOpenStall = 20 * time.Millisecond

	var log syncBuffer
	release := make(chan struct{})
	s := &Server{Backend: stallBackend{release}, Logger: slog.New(slog.NewTextHandler(&log, nil))}
	opened := make(chan error, 1)
	go func() {
		l, err := s.Local(LocalOptions{User: User{UserID: "ada"}})
		if err == nil {
			err = l.Close()
		}
		opened <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(log.String(), stallWarning) {
		if time.Now().After(deadline) {
			close(release)
			t.Fatalf("no warning after 5s of a stalled Local: %q", log.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case err := <-opened:
		t.Fatalf("Local returned %v while Hello waited", err)
	default:
	}
	close(release)
	if err := <-opened; err != nil {
		t.Fatal(err)
	}
	if out := log.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "user_id=ada") || !strings.Contains(out, "waited=20ms") {
		t.Errorf("the warning: %q", out)
	}
}

func TestLocalNoStallWarning(t *testing.T) {
	defer func(d time.Duration) { localOpenStall = d }(localOpenStall)
	localOpenStall = 200 * time.Millisecond

	var log syncBuffer
	s := &Server{Backend: stallBackend{}, Logger: slog.New(slog.NewTextHandler(&log, nil))}
	start := time.Now()
	l, err := s.Local(LocalOptions{User: User{UserID: "ada"}})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if took := time.Since(start); took >= localOpenStall {
		t.Skipf("Local took %v, not a fast connect", took)
	}
	// Past when the warning would be logged, had its timer not been
	// stopped.
	time.Sleep(2 * localOpenStall)
	if strings.Contains(log.String(), stallWarning) {
		t.Errorf("a fast Local logged the stall warning: %q", log.String())
	}
}
