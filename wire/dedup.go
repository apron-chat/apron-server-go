package wire

import (
	"encoding/json/jsontext"

	"github.com/apron-chat/apron-server-go/internal/wirecore"
)

// dedupKey is a request across all users (§1.2): a user_id and a request
// id.
type dedupKey struct{ userID, id string }

// dedupOutcome is a request's encoded result, or its error.
type dedupOutcome struct {
	result jsontext.Value
	err    *Error
}

// dedupCache keeps the outcomes of the latest requests, oldest forgotten
// first. Guarded by the Server's mu.
type dedupCache = wirecore.Dedup[dedupKey, dedupOutcome]
