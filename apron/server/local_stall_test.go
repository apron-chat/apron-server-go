package server

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
)

// stallBackend is a Backend whose Hello waits for release, as one would for
// a lock its caller holds.
type stallBackend struct{ release chan struct{} }

func (b stallBackend) Hello(context.Context, *Conn) apron.ServerInfo {
	if b.release != nil {
		<-b.release
	}
	return apron.ServerInfo{}
}

func (stallBackend) Auth(context.Context, *Conn, apron.AuthParams, Reply[apron.AuthResult]) error {
	return apron.Denied("no")
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
// time. The bubble's clock is fake, so the waits are exact and take no time.
func TestLocalStallWarning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var log syncBuffer
		release := make(chan struct{})
		s := New(stallBackend{release}, Config{Logger: slog.New(slog.NewTextHandler(&log, nil))})
		opened := make(chan error, 1)
		go func() {
			l, err := s.Local(LocalOptions{User: apron.User{UserID: "ada"}})
			if err == nil {
				err = l.Close()
			}
			opened <- err
		}()
		time.Sleep(localOpenStall - time.Nanosecond)
		synctest.Wait()
		if strings.Contains(log.String(), stallWarning) {
			t.Fatalf("warned before localOpenStall: %q", log.String())
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if !strings.Contains(log.String(), stallWarning) {
			t.Fatalf("no warning after localOpenStall of a stalled Local: %q", log.String())
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
		if out := log.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "user_id=ada") || !strings.Contains(out, "waited="+localOpenStall.String()) {
			t.Errorf("the warning: %q", out)
		}
		if err := s.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestLocalNoStallWarning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var log syncBuffer
		s := New(stallBackend{}, Config{Logger: slog.New(slog.NewTextHandler(&log, nil))})
		l, err := s.Local(LocalOptions{User: apron.User{UserID: "ada"}})
		if err != nil {
			t.Fatal(err)
		}
		// Past when the warning would be logged, had its timer not been
		// stopped.
		time.Sleep(2 * localOpenStall)
		synctest.Wait()
		if strings.Contains(log.String(), stallWarning) {
			t.Errorf("a fast Local logged the stall warning: %q", log.String())
		}
		_ = l.Close()
		if err := s.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}
