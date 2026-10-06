package server

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
)

const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeUnsupported    = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
	codeDenied         = -32001
	codeRetryAfter     = -32002
	codeTooLarge       = -32003
)

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitzero"`
}

type rpcResponse struct {
	ID     any       `json:"id,omitzero"`
	Result any       `json:"result,omitzero"`
	Error  *rpcError `json:"error,omitzero"`
}

type request struct {
	method string
	params map[string]jsontext.Value
	id     string
	hasID  bool
}

func parseRequest(payload []byte) (request, *rpcError) {
	if !jsontext.Value(payload).IsValid() {
		return request{}, &rpcError{Code: codeParseError, Message: "Parse error"}
	}

	var object map[string]jsontext.Value
	if err := json.Unmarshal(payload, &object); err != nil || object == nil {
		return request{}, &rpcError{Code: codeInvalidRequest, Message: "Invalid request"}
	}

	// method comes first: an id on a notification-only method is ignored,
	// whatever it is, and so are its params' errors (§1).
	var req request
	rawMethod, hasMethod := object["method"]
	methodOK := hasMethod && json.Unmarshal(rawMethod, &req.method) == nil && req.method != ""
	if !methodOK {
		req.method = ""
	}
	if rawID, ok := object["id"]; ok && !notificationOnly[req.method] {
		req.hasID = true
		if bytes.Equal(bytes.TrimSpace(rawID), []byte("null")) || json.Unmarshal(rawID, &req.id) != nil {
			req.hasID = false
			return req, &rpcError{Code: codeInvalidRequest, Message: "Request id must be a string"}
		}
	}
	if !methodOK {
		return req, &rpcError{Code: codeInvalidRequest, Message: "Invalid request"}
	}

	req.params = make(map[string]jsontext.Value)
	if rawParams, ok := object["params"]; ok {
		if bytes.Equal(bytes.TrimSpace(rawParams), []byte("null")) || json.Unmarshal(rawParams, &req.params) != nil || req.params == nil {
			return req, invalidParams("Params must be an object")
		}
	}
	return req, nil
}

// notificationOnly are the methods clients send only as notifications. An
// id on one is ignored, and nothing about it is answered (§1).
var notificationOnly = map[string]bool{"ping": true, "activity": true}

func canonicalParams(params map[string]jsontext.Value) string {
	if params == nil {
		return "{}"
	}
	var value any
	raw := encodeJSON(params)
	if raw == nil || json.Unmarshal(raw, &value) != nil {
		return string(raw)
	}
	if canonical := encodeJSON(value); canonical != nil {
		return string(canonical)
	}
	return string(raw)
}

func requestFingerprint(req request) string {
	return req.method + "\x00" + canonicalParams(req.params)
}

func response(id string, result any) rpcResponse {
	return rpcResponse{ID: id, Result: result}
}

// errorResponse builds an error reply. A nil id omits "id", as for errors not
// tied to a request (PROTOCOL.md §1.1).
func errorResponse(id any, e *rpcError) rpcResponse {
	return rpcResponse{ID: id, Error: e}
}

func invalidParams(format string, args ...any) *rpcError {
	return &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf(format, args...)}
}

func parseString(params map[string]jsontext.Value, name string, required bool) (string, *rpcError) {
	raw, ok := params[name]
	if !ok {
		if required {
			return "", invalidParams("Missing %s", name)
		}
		return "", nil
	}
	var value string
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
		return "", invalidParams("%s must be a string", name)
	}
	return value, nil
}

func parseBool(params map[string]jsontext.Value, name string, required bool) (bool, *rpcError) {
	raw, ok := params[name]
	if !ok {
		if required {
			return false, invalidParams("Missing %s", name)
		}
		return false, nil
	}
	var value bool
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
		return false, invalidParams("%s must be a boolean", name)
	}
	return value, nil
}

func parseObject(params map[string]jsontext.Value, name string, required bool) (map[string]any, *rpcError) {
	raw, ok := params[name]
	if !ok {
		if required {
			return nil, invalidParams("Missing %s", name)
		}
		return nil, nil
	}
	var value map[string]any
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return nil, invalidParams("%s must be an object", name)
	}
	return value, nil
}

// extObject is an ext object (§3.5) keyed by extension name, each value
// kept as the JSON it arrived as, so a merge or a store keeps it byte for
// byte, numbers beyond 2^53 included.
type extObject = map[string]jsontext.Value

// parseExt reads an optional ext object of a write (§3.5). present reports
// whether the request carried it.
func parseExt(params map[string]jsontext.Value, name string) (ext extObject, present bool, err *rpcError) {
	raw, ok := params[name]
	if !ok {
		return nil, false, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &ext) != nil || ext == nil {
		return nil, true, invalidParams("%s must be an object", name)
	}
	for key, value := range ext {
		value = slices.Clone(value)
		if value.Compact() != nil {
			return nil, true, invalidParams("%s.%s is not valid JSON", name, key)
		}
		ext[key] = value
	}
	return ext, true, nil
}

// emptyJSON reports whether a compact JSON value is an empty value ("", [],
// or {}), which clears what it is merged into (§3.3).
func emptyJSON(value jsontext.Value) bool {
	switch string(value) {
	case `""`, `[]`, `{}`:
		return true
	}
	return false
}

// mergeExt merges a write's ext into the kept one, one level deep (§3.5):
// each key the write carries replaces the kept value, a key whose value is
// empty is removed, and keys the write leaves out stay. null is an ordinary
// value, and an empty write changes nothing. The result is a new object, or
// nil when no key is left; kept is not modified.
func mergeExt(kept, write extObject) extObject {
	merged := make(extObject, len(kept)+len(write))
	maps.Copy(merged, kept)
	for key, value := range write {
		if emptyJSON(value) {
			delete(merged, key)
		} else {
			merged[key] = value
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// clearedExt lists, as their empty values, the keys of a write's ext that
// cleared a key that was kept, for a notification that carries the change
// (§3.3).
func clearedExt(kept, write extObject) extObject {
	var cleared extObject
	for key, value := range write {
		if _, had := kept[key]; had && emptyJSON(value) {
			if cleared == nil {
				cleared = make(extObject)
			}
			cleared[key] = value
		}
	}
	return cleared
}

// extOf returns a decoded object's ext as an extObject: kept as is when it
// already is one, re-encoded when it was decoded into plain values.
func extOf(value any) extObject {
	switch value := value.(type) {
	case extObject:
		return value
	case map[string]any:
		ext := make(extObject, len(value))
		for key, child := range value {
			if raw := encodeJSON(child); raw != nil {
				ext[key] = raw
			}
		}
		return ext
	}
	return nil
}

// decodeObject decodes a JSON object, keeping the values of a top-level
// ext as raw JSON so that re-encoding the object keeps them exactly.
func decodeObject(raw []byte) map[string]any {
	var fields map[string]jsontext.Value
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil
	}
	value := make(map[string]any, len(fields))
	for key, field := range fields {
		if key == "ext" {
			var ext extObject
			if json.Unmarshal(field, &ext) == nil && ext != nil {
				value[key] = ext
				continue
			}
		}
		var decoded any
		if json.Unmarshal(field, &decoded) == nil {
			value[key] = decoded
		}
	}
	return value
}

// encodedSize is the size of a value encoded as JSON.
func encodedSize(value any) int {
	return len(encodeJSON(value))
}

func cloneValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		copyValue := make(map[string]any, len(value))
		for key, child := range value {
			copyValue[key] = cloneValue(child)
		}
		return copyValue
	case []any:
		copyValue := make([]any, len(value))
		for i, child := range value {
			copyValue[i] = cloneValue(child)
		}
		return copyValue
	default:
		return value
	}
}

func cloneObject(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	return cloneValue(value).(map[string]any)
}
