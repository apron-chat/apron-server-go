package conformance

import (
	"encoding/json/v2"
	"fmt"
	"slices"
	"strconv"

	"github.com/apron-chat/apron-server-go/apron"
)

// The frames, as decoded JSON, and what the checks do with them.

// get returns the member at a path of keys, or nil.
func get(v any, keys ...string) any {
	for _, k := range keys {
		o, ok := v.(obj)
		if !ok {
			return nil
		}
		v = o[k]
	}
	return v
}

// str returns the string at a path of keys, or "".
func str(v any, keys ...string) string {
	s, _ := get(v, keys...).(string)
	return s
}

// list returns v as an array, or nil.
func list(v any) []any {
	l, _ := v.([]any)
	return l
}

// has reports whether an object has a member.
func has(v any, key string) bool {
	o, ok := v.(obj)
	if !ok {
		return false
	}
	_, ok = o[key]
	return ok
}

// hasString reports whether v is an array that holds s.
func hasString(v any, s string) bool {
	return slices.Contains(list(v), any(s))
}

// stringsOf returns the strings of an array.
func stringsOf(v any) []string {
	var out []string
	for _, item := range list(v) {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// truthy is Python's truth of a decoded JSON value.
func truthy(v any) bool {
	switch v := v.(type) {
	case nil:
		return false
	case bool:
		return v
	case float64:
		return v != 0
	case string:
		return v != ""
	case []any:
		return len(v) > 0
	case obj:
		return len(v) > 0
	}
	return true
}

// isEmptyObject reports whether v is {}.
func isEmptyObject(v any) bool {
	o, ok := v.(obj)
	return ok && len(o) == 0
}

// parseID parses a log_id or message_id string.
func parseID(v any) (int64, bool) {
	s, ok := v.(string)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// id parses a log_id or message_id, and fails the check when it is not
// one, as conform.py's int() does.
func (c *checker) id(v any) int64 {
	c.t.Helper()
	n, ok := parseID(v)
	c.must(ok, "%v is not a decimal log_id", v)
	return n
}

// Error codes, by the names PROTOCOL.md gives them (§1.1).
var codes = map[string]int{
	"parse_error": apron.CodeParseError, "invalid_request": apron.CodeInvalidRequest,
	"unsupported": apron.CodeUnsupported, "invalid_params": apron.CodeInvalidParams,
	"internal_error": apron.CodeInternalError, "denied": apron.CodeDenied,
	"retry_after": apron.CodeRetryAfter, "too_large": apron.CodeTooLarge,
}

// code returns the error code of a reply, or 0.
func code(reply obj) int {
	n, ok := get(reply, "error", "code").(float64)
	if !ok {
		return 0
	}
	return int(n)
}

// describe describes a reply for a failure.
func describe(reply obj) string {
	if e, ok := reply["error"].(obj); ok {
		name := fmt.Sprint(e["code"])
		for k, v := range codes {
			if float64(v) == e["code"] {
				name = k
			}
		}
		return fmt.Sprintf("error/%s (%q)", name, fmt.Sprint(e["message"]))
	}
	result, _ := json.Marshal(reply["result"])
	return fmt.Sprintf("result %.120s", result)
}

// expectError fails the check unless the reply is the error of name.
func (c *checker) expectError(reply obj, name, what string) {
	c.t.Helper()
	c.must(code(reply) == codes[name], "%s: expected error/%s, got %s", what, name, describe(reply))
}

// signedIn reports whether a reply signed in.
func signedIn(reply obj) bool {
	_, ok := get(reply, "result", "you").(obj)
	return ok
}

// policy skips the check when the server's policy refused a request with
// retry_after.
func (c *checker) policy(reply obj) {
	c.t.Helper()
	if code(reply) == apron.CodeRetryAfter {
		c.skip("the server's policy refused it: %s", describe(reply))
	}
}

// unlessPolicy skips the check when the server's policy refused the
// request: retry_after, or denied, which §1.1 allows for a well-formed
// request that the user or the server does not allow, such as a guest who
// may only read.
//
// control, if not nil, sends a request that the same policy treats alike,
// but that the rule under test allows, such as the same request for a room
// that exists. Then denied is policy only if control is denied too;
// otherwise the check goes on, and fails on the denied reply.
func (c *checker) unlessPolicy(reply obj, what string, control func() obj) {
	c.t.Helper()
	c.policy(reply)
	if code(reply) != apron.CodeDenied {
		return
	}
	if control != nil && code(control()) != apron.CodeDenied {
		return
	}
	c.skip("the server's policy refused %s: %s", what, describe(reply))
}

// Frame matchers.

// method matches the notifications of a method.
func method(name string) func(obj) bool {
	return func(f obj) bool { return f["method"] == name }
}

// snapshotOf matches the snapshots of a message, in any room when room is
// "".
func snapshotOf(messageID any, room string) func(obj) bool {
	return func(f obj) bool {
		return f["method"] == "message" && get(f, "params", "message_id") == messageID &&
			(room == "" || get(f, "params", "room_id") == room)
	}
}

// roomUpdate matches a room_update with a record of room under key.
func roomUpdate(key, room string) func(obj) bool {
	return func(f obj) bool {
		if f["method"] != "room_update" {
			return false
		}
		for _, r := range list(get(f, "params", key)) {
			if get(r, "room_id") == room {
				return true
			}
		}
		return false
	}
}

// records returns the records of room under key in a room_update.
func records(frame obj, key, room string) []obj {
	var found []obj
	for _, r := range list(get(frame, "params", key)) {
		if r, ok := r.(obj); ok && r["room_id"] == room {
			found = append(found, r)
		}
	}
	return found
}

// isError matches an error that is no reply to a request.
func isError(f obj) bool {
	_, hasID := f["id"]
	_, hasError := f["error"]
	return hasError && !hasID
}
