package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"sync"
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

// decoders are the decoders parseWellFormed reads frames with, each
// reading one in place (jsontext.NewDecoder of a bytes.Buffer), so that
// what it returns of the frame are parts of it, not copies.
var decoders = sync.Pool{New: func() any { return new(frameDecoder) }}

// frameDecoder is a decoder, and the buffer of the frame it reads.
type frameDecoder struct {
	jsontext.Decoder
	frame bytes.Buffer
}

// noInput is what a decoder in the pool reads, so that it keeps no frame.
var noInput = new(bytes.Buffer)

// parseWellFormed parses a valid frame that is a well-formed request, or
// notification, as ParseRequest does, but in one pass, as most frames
// are: a JSON object with a method, an id that is a string, if any, and
// params that are an object, if any. For any other frame it reports
// false, and ParseRequest tells what is wrong with it.
//
// The request's Params, and its Fields' values, are parts of payload,
// which the caller keeps as it is (Transport.Read).
func parseWellFormed(payload []byte) (req Request, ok bool) {
	if jsontext.Value(payload).Kind() != '{' {
		return Request{}, false
	}
	fd := decoders.Get().(*frameDecoder)
	defer func() {
		fd.Reset(noInput)
		fd.frame = bytes.Buffer{}
		decoders.Put(fd)
	}()
	fd.frame = *bytes.NewBuffer(payload)
	fd.Reset(&fd.frame)
	dec := &fd.Decoder
	if _, err := dec.ReadToken(); err != nil {
		return Request{}, false
	}
	var rawID jsontext.Value
	for dec.PeekKind() != '}' {
		name, err := dec.ReadValue()
		if err != nil {
			return Request{}, false
		}
		switch unquotedName(name) {
		case "method":
			raw, err := dec.ReadValue()
			if err != nil || raw.Kind() != '"' {
				return Request{}, false
			}
			if req.Method = methodName(raw); req.Method == "" {
				return Request{}, false
			}
		case "id":
			if rawID, err = dec.ReadValue(); err != nil {
				return Request{}, false
			}
		case "params":
			if dec.PeekKind() != '{' {
				return Request{}, false
			}
			if req.Params, req.Fields, ok = members(dec, payload); !ok {
				return Request{}, false
			}
		default:
			if dec.SkipValue() != nil {
				return Request{}, false
			}
		}
	}
	if req.Method == "" {
		return Request{}, false
	}
	if rawID != nil && !NotificationOnly[req.Method] {
		if rawID.Kind() != '"' || json.Unmarshal(rawID, &req.ID) != nil {
			return Request{}, false
		}
		req.HasID = true
	}
	if req.Params == nil {
		req.Params, req.Fields = jsontext.Value("{}"), make(map[string]jsontext.Value)
	}
	return req, true
}

// members reads the object dec is at, a part of payload, and returns it,
// and its members by name, each a part of payload too.
func members(dec *jsontext.Decoder, payload []byte) (object jsontext.Value, fields map[string]jsontext.Value, ok bool) {
	if _, err := dec.ReadToken(); err != nil {
		return nil, nil, false
	}
	start := dec.InputOffset() - 1
	fields = make(map[string]jsontext.Value)
	for dec.PeekKind() != '}' {
		tok, err := dec.ReadToken()
		if err != nil {
			return nil, nil, false
		}
		name := tok.String()
		value, err := dec.ReadValue()
		if err != nil {
			return nil, nil, false
		}
		// The decoder reads payload in place, so the value is part of it.
		end := dec.InputOffset()
		fields[name] = jsontext.Value(payload[end-int64(len(value)) : end : end])
	}
	if _, err := dec.ReadToken(); err != nil {
		return nil, nil, false
	}
	end := dec.InputOffset()
	return jsontext.Value(payload[start:end:end]), fields, true
}

// unquotedName returns a member's name of a request's envelope, quoted as
// it arrived, unquoted when it is one a request has, and else "".
func unquotedName(quoted jsontext.Value) string {
	switch string(quoted) {
	case `"method"`:
		return "method"
	case `"id"`:
		return "id"
	case `"params"`:
		return "params"
	}
	if bytes.IndexByte(quoted, '\\') < 0 {
		return ""
	}
	name, err := jsontext.AppendUnquote(nil, quoted)
	if err != nil {
		return ""
	}
	return unquotedName(jsontext.Value(`"` + string(name) + `"`))
}

// methodName returns a method's name, quoted as it arrived, unquoted: one
// of the protocol's without allocating.
func methodName(quoted jsontext.Value) string {
	if bytes.IndexByte(quoted, '\\') < 0 {
		switch name := quoted[1 : len(quoted)-1]; string(name) {
		case "message":
			return "message"
		case "history":
			return "history"
		case "me":
			return "me"
		case "auth":
			return "auth"
		case "ping":
			return "ping"
		case "activity":
			return "activity"
		case "status":
			return "status"
		case "reactions":
			return "reactions"
		case "command":
			return "command"
		case "room_list":
			return "room_list"
		case "room_join":
			return "room_join"
		case "room_leave":
			return "room_leave"
		case "room_set":
			return "room_set"
		default:
			return string(name)
		}
	}
	var name string
	if json.Unmarshal(quoted, &name) != nil {
		return ""
	}
	return name
}
