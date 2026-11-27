package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// Error codes of a frame that is not a valid request (§1.1).
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeInvalidParams  = -32602
)

// ParseError is why a frame is not a valid request: a protocol error code
// and its message, which the caller turns into its error reply.
type ParseError struct {
	Code    int
	Message string
}

// InvalidParams reports whether the frame is a request whose params are
// not an object.
func (e *ParseError) InvalidParams() bool { return e.Code == codeInvalidParams }

// Request is a parsed frame.
type Request struct {
	Method string
	// ID is the request id, when HasID. The id of a notification-only
	// method is ignored (NotificationOnly).
	ID    string
	HasID bool
	// Params are the params as they arrived, {} when absent; Fields are
	// the same decoded, never nil once parsed.
	Params jsontext.Value
	Fields map[string]jsontext.Value
}

// NotificationOnly are the methods clients send only as notifications
// (§1.1). A server may ignore the id of one and handle the frame as a
// notification, and both servers here do: nothing about it is answered.
var NotificationOnly = map[string]bool{"ping": true, "activity": true}

// MaxRequestIDBytes bounds a request id, which is kept for deduplication
// (§1.2).
const MaxRequestIDBytes = 256

// ParseRequest parses a frame (§1.1). On failure the Request still has the
// method and id it could read, so that the caller can tell whether and how
// to answer.
func ParseRequest(payload []byte) (Request, *ParseError) {
	if !jsontext.Value(payload).IsValid() {
		return Request{}, &ParseError{codeParseError, "Parse error"}
	}
	if req, ok := parseWellFormed(payload); ok {
		return req, nil
	}
	return parseAny(payload)
}

// parseAny parses a valid frame, whatever it holds.
func parseAny(payload []byte) (Request, *ParseError) {
	var object map[string]jsontext.Value
	if err := json.Unmarshal(payload, &object); err != nil || object == nil {
		return Request{}, &ParseError{codeInvalidRequest, "Invalid request"}
	}

	// method comes first: the id of a notification method is ignored,
	// whatever it is.
	var req Request
	rawMethod, hasMethod := object["method"]
	methodOK := hasMethod && json.Unmarshal(rawMethod, &req.Method) == nil && req.Method != ""
	if !methodOK {
		req.Method = ""
	}
	if rawID, ok := object["id"]; ok && !NotificationOnly[req.Method] {
		req.HasID = true
		if bytes.Equal(bytes.TrimSpace(rawID), []byte("null")) || json.Unmarshal(rawID, &req.ID) != nil {
			req.HasID = false
			return req, &ParseError{codeInvalidRequest, "Request id must be a string"}
		}
	}
	if !methodOK {
		return req, &ParseError{codeInvalidRequest, "Invalid request"}
	}

	req.Params = jsontext.Value("{}")
	req.Fields = make(map[string]jsontext.Value)
	if raw, ok := object["params"]; ok {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &req.Fields) != nil || req.Fields == nil {
			req.Fields = nil
			return req, &ParseError{codeInvalidParams, "Params must be an object"}
		}
		req.Params = raw
	}
	return req, nil
}

// Fingerprint identifies a request for deduplication (§1.2): a hash of its
// method and params, whatever the order of their object keys, and however
// their strings and numbers are written, as RFC 8785 canonicalizes them.
func Fingerprint(method string, params jsontext.Value) [32]byte {
	// One buffer holds the method, a NUL, and the params, canonicalized
	// in place.
	buf := make([]byte, 0, len(method)+1+len(params))
	buf = append(append(buf, method...), 0)
	canonical := jsontext.Value(append(buf[len(buf):], params...))
	if canonical.Canonicalize() != nil {
		canonical = append(canonical[:0], params...)
	}
	return sha256.Sum256(append(buf, canonical...))
}

// envelope is a frame's members that a request has, as they arrived.
type envelope struct {
	ID     jsontext.Value `json:"id"`
	Method string         `json:"method"`
	Params jsontext.Value `json:"params"`
}

// parseWellFormed parses a valid frame that is a well-formed request, or
// notification, as ParseRequest does, but in one pass, as most frames
// are: a JSON object with a method, an id that is a string, if any, and
// params that are an object, if any. For any other frame it reports
// false, and ParseRequest tells what is wrong with it.
func parseWellFormed(payload []byte) (Request, bool) {
	var env envelope
	if jsontext.Value(payload).Kind() != '{' || json.Unmarshal(payload, &env) != nil || env.Method == "" {
		return Request{}, false
	}
	req := Request{Method: env.Method, Params: jsontext.Value("{}")}
	if env.ID != nil && !NotificationOnly[req.Method] {
		if env.ID.Kind() != '"' || json.Unmarshal(env.ID, &req.ID) != nil {
			return Request{}, false
		}
		req.HasID = true
	}
	if env.Params != nil {
		if env.Params.Kind() != '{' || json.Unmarshal(env.Params, &req.Fields) != nil {
			return Request{}, false
		}
		req.Params = env.Params
	} else {
		req.Fields = make(map[string]jsontext.Value)
	}
	return req, true
}
