package presence

import (
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestShown(t *testing.T) {
	// chosen → shown when attended, idle, and offline (§4.5.1).
	for chosen, want := range map[string][3]string{
		Online:    {Online, Idle, Offline},
		DND:       {DND, DND, Offline},
		Invisible: {Offline, Offline, Offline},
		None:      {None, None, None},
	} {
		st := New()
		st.Chosen = chosen
		got := [3]string{st.Shown(true, true), st.Shown(false, true), st.Shown(false, false)}
		if got != want {
			t.Errorf("%q shows %q, want %q", chosen, got, want)
		}
	}
	if !Settable(DND) || !Settable(None) || Settable(Idle) || Settable(Offline) || Settable("away") {
		t.Fatal("Settable")
	}
}

func TestPublish(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	st := New()
	if st.Pending() || st.Status != Offline {
		t.Fatalf("a new user's status: %+v", st)
	}
	// Connecting is stored, as a change out of offline.
	st.Note(true, true, now)
	if !st.Pending() {
		t.Fatal("a change noted is not pending")
	}
	if changed, stored := st.Publish(now); !changed || !stored || st.Status != Online {
		t.Fatalf("published %v %v: %+v", changed, stored, st)
	}
	st.Note(false, true, now)
	if changed, stored := st.Publish(now); !changed || stored || st.Status != Idle {
		t.Fatalf("idle: %v %v", changed, stored)
	}
	// Seen is when the shown status became offline, though it is
	// published later.
	st.Note(false, false, now.Add(time.Second))
	if changed, stored := st.Publish(now.Add(time.Minute)); !changed || !stored || st.Seen != now.Add(time.Second).UnixMilli() {
		t.Fatalf("offline: %v %v, seen %d", changed, stored, st.Seen)
	}
	if changed, _ := st.Publish(now.Add(time.Hour)); changed {
		t.Fatal("published nothing new")
	}
	// A flap undone before it is published moves no seen.
	st.Note(true, true, now.Add(2*time.Minute))
	st.Note(false, false, now.Add(3*time.Minute))
	if st.Pending() {
		t.Fatal("an undone flap is pending")
	}
	// Invisible connections never move seen.
	st.Chosen = Invisible
	st.Note(true, true, now.Add(time.Hour))
	if st.Pending() || st.Seen != now.Add(time.Second).UnixMilli() {
		t.Fatal("an invisible connection showed")
	}
}

func TestRestore(t *testing.T) {
	now := time.UnixMilli(2_000_000)
	st := New()
	st.Chosen = DND
	if !st.Restore(true, now) || st.Status != Offline || st.Seen != now.UnixMilli() || st.Pending() {
		t.Fatalf("restored after a crash: %+v", st)
	}
	st = New()
	st.Chosen = None
	if st.Restore(true, now) || st.Status != None {
		t.Fatalf("restored with no status: %+v", st)
	}
}

func TestLimits(t *testing.T) {
	now := time.Now()
	st := New()
	for range RequestBurst {
		if delay := st.Admit(now); delay != 0 {
			t.Fatalf("refused within the burst: %v", delay)
		}
	}
	if delay := st.Admit(now); delay <= 0 || delay > RequestRefill {
		t.Fatalf("admitted past the burst: %v", delay)
	}
	st.SetRequestLimit(rate.NewLimiter(rate.Every(time.Hour), 1))
	if st.Admit(now) != 0 || st.Admit(now) == 0 {
		t.Fatal("SetRequestLimit")
	}
	for range Burst {
		if !st.PublishNow() {
			t.Fatal("held back within the burst")
		}
	}
	if st.PublishNow() {
		t.Fatal("published at once past the burst")
	}
	st.SetPublishLimit(rate.NewLimiter(rate.Every(time.Hour), 1))
	if !st.PublishNow() || st.PublishNow() {
		t.Fatal("SetPublishLimit")
	}
}
