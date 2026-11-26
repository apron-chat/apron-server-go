package apron_test

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
)

func TestRetryAfterRoundsUpToWholeSeconds(t *testing.T) {
	for wait, want := range map[time.Duration]int64{
		0:                       1,
		-time.Second:            1,
		time.Millisecond:        1,
		time.Second:             1,
		time.Second + 1:         2,
		30 * time.Second:        30,
		90*time.Second + 999999: 91,
	} {
		e := apron.RetryAfter("Slow down", wait)
		data, _ := e.Data.(map[string]any)
		if e.Code != apron.CodeRetryAfter || e.Message != "Slow down" || data["retry_after"] != want {
			t.Errorf("RetryAfter(%v) = %#v, want retry_after %d", wait, e, want)
		}
	}
}

func TestRetryDelay(t *testing.T) {
	for _, c := range []struct {
		name     string
		err      error
		delay    time.Duration
		category string
		ok       bool
	}{
		{"constructed", apron.RetryAfter("Slow down", 3*time.Second), 3 * time.Second, "", true},
		{"wrapped", fmt.Errorf("posting: %w", apron.RetryAfter("Slow down", time.Second)), time.Second, "", true},
		{"decoded", decodeError(t, `{"code":-32002,"message":"Posting limit reached","data":{"retry_after":40,"ext":{"limits":"write"}}}`), 40 * time.Second, apron.LimitWrite, true},
		{"fraction", decodeError(t, `{"code":-32002,"message":"m","data":{"retry_after":0.5}}`), time.Second / 2, "", true},
		{"category of the wrong type", decodeError(t, `{"code":-32002,"message":"m","data":{"retry_after":2,"ext":{"limits":7}}}`), 2 * time.Second, "", true},
		{"ext of the wrong type", decodeError(t, `{"code":-32002,"message":"m","data":{"retry_after":2,"ext":"write"}}`), 2 * time.Second, "", true},
		{"no delay", decodeError(t, `{"code":-32002,"message":"m"}`), 0, "", false},
		{"zero delay", decodeError(t, `{"code":-32002,"message":"m","data":{"retry_after":0}}`), 0, "", false},
		{"another code", decodeError(t, `{"code":-32001,"message":"m","data":{"retry_after":2}}`), 0, "", false},
		{"not an apron error", errors.New("m"), 0, "", false},
		{"nil", nil, 0, "", false},
	} {
		delay, category, ok := apron.RetryDelay(c.err)
		if delay != c.delay || category != c.category || ok != c.ok {
			t.Errorf("%s: RetryDelay = %v, %q, %v; want %v, %q, %v", c.name, delay, category, ok, c.delay, c.category, c.ok)
		}
	}
}

func decodeError(t *testing.T, s string) error {
	t.Helper()
	var e apron.Error
	if err := json.Unmarshal([]byte(s), &e); err != nil {
		t.Fatal(err)
	}
	return &e
}
