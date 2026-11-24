package wire_test

import (
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/wire"
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
		e := wire.RetryAfter("Slow down", wait)
		data, _ := e.Data.(map[string]any)
		if e.Code != wire.CodeRetryAfter || e.Message != "Slow down" || data["retry_after"] != want {
			t.Errorf("RetryAfter(%v) = %#v, want retry_after %d", wait, e, want)
		}
	}
}
