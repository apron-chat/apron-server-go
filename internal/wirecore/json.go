package wirecore

import (
	"encoding/json/v2"
	"time"
)

// JSONOptions encode deterministically, sorting object keys, and keep
// encoding/json's null for nil slices and maps. encoding/json/v2 never
// escapes <, >, and &, which only matter for JSON embedded in HTML and would
// make stored text up to six times larger than it arrived.
var JSONOptions = json.JoinOptions(
	json.Deterministic(true),
	json.FormatNilSliceAsNull(true),
	json.FormatNilMapAsNull(true),
)

// Marshal encodes value with JSONOptions.
func Marshal(value any) ([]byte, error) { return json.Marshal(value, JSONOptions) }

// PingFrame is the exact client liveness ping (§1), which a server answers
// with PongFrame without parsing it or waiting for the frames before it.
// Neither is ever modified.
var (
	PingFrame = []byte(`{"method":"ping"}`)
	PongFrame = []byte(`{"method":"pong"}`)
)

// NextLogID returns the log_id after last (§2.1): the time in milliseconds,
// or last plus one, so that log_ids strictly increase.
func NextLogID(last int64) int64 {
	return max(time.Now().UnixMilli(), last+1)
}
