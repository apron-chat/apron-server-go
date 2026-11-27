package core

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
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

// The envelopes of frames, as Marshal encodes them: their members in the
// order of their names, as Marshal sorts a map's, so that they encode as
// the maps of the same members do, without what encoding a map costs
// (each key and value copied, then sorted).
type (
	// NotificationFrame is a notification (§1.1).
	NotificationFrame struct {
		Method string `json:"method"`
		Params any    `json:"params"`
	}
	// RequestFrame is a request, or with ID nil a notification, that a
	// client sends; nil Params are left out.
	RequestFrame struct {
		ID     *string `json:"id,omitzero"`
		Method string  `json:"method"`
		Params any     `json:"params,omitzero"`
	}
	// ReplyFrame is a reply (§1.1): Error, or Result, and the ID of the
	// request it answers, which an error about a frame that has none
	// leaves out.
	ReplyFrame struct {
		Error  any            `json:"error,omitzero"`
		ID     *string        `json:"id,omitzero"`
		Result jsontext.Value `json:"result,omitzero"`
	}
)
