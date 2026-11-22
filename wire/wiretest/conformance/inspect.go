package conformance

import (
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Every frame from the server passes through inspect, with conn.mu held,
// before a check can see it.
func (c *conn) inspect(frame obj, raw []byte) {
	r := c.r
	if frame == nil {
		return // core.shape.schema, which is not ported
	}
	if len(c.frames) == 0 && frame["method"] != "server" {
		r.violate("server-first", fmt.Sprintf("first frame was %.80s", raw))
	}
	_, isNotification := frame["method"]
	if id, ok := frame["id"].(string); ok && !isNotification {
		c.answered[id]++
		if c.answered[id] > c.asked[id] {
			r.violate("replies", fmt.Sprintf("unexpected reply to id %q", id))
		}
	}
	checkIDs(r, frame, "")
	params, _ := frame["params"].(obj)
	if frame["method"] == "message" && params != nil {
		if logID, ok := parseID(params["log_id"]); ok {
			room, _ := params["room_id"].(string)
			key := orderKey{c, room}
			r.mu.Lock()
			last, seen := r.order[key]
			r.mu.Unlock()
			if seen && logID <= last {
				r.violate("order", fmt.Sprintf("room %s: %d after %d", room, logID, last))
			}
			r.mu.Lock()
			r.order[key] = max(last, logID)
			r.mu.Unlock()
		}
	}
	if r.has("history") {
		checkRoomRecords(r, frame)
	}
	c.checkSeen(frame)
}

// checkSeen checks that seen changes only when the shown status changes to
// offline (§4.5.2).
func (c *conn) checkSeen(frame obj) {
	type entry struct {
		user         obj
		notification bool
	}
	var users []entry
	params, _ := frame["params"].(obj)
	if frame["method"] == "user" && params != nil {
		if user, ok := params["new"].(obj); ok {
			users = append(users, entry{user, true})
		}
	}
	containers := []any{frame["result"]}
	if frame["method"] == "room_update" {
		containers = append(containers, frame["params"])
	}
	for _, container := range containers {
		if container, ok := container.(obj); ok {
			list, _ := container["users"].([]any)
			for _, u := range list {
				if u, ok := u.(obj); ok {
					users = append(users, entry{u, false})
				}
			}
		}
	}
	anySeen := func() bool {
		for _, s := range c.shown {
			if truthy(s.seen) {
				return true
			}
		}
		return false
	}
	for _, e := range users {
		user := e.user
		status, hasStatus := user["status"]
		userID, ok := user["user_id"].(string)
		if !hasStatus || !ok {
			continue
		}
		before, known := c.shown[userID]
		seen := user["seen"]
		switch {
		case status == "offline" && known && before.status == "offline":
			if seen != nil && before.seen != nil && !reflect.DeepEqual(seen, before.seen) {
				c.r.violate("shown-seen", fmt.Sprintf("seen of %s changed from %v to %v while the shown status stayed offline", userID, before.seen, seen))
			}
			if !truthy(seen) {
				seen = before.seen
			}
		case status == "offline" && e.notification && seen == nil && anySeen():
			c.r.violate("shown-seen", fmt.Sprintf("the user notification that changed %s to offline carries no seen", userID))
		}
		if _, has := user["seen"]; status == "offline" && !e.notification && !has && anySeen() {
			c.r.violate("shown-seen", fmt.Sprintf("the complete user object of %s has a shown offline and no seen", userID))
		}
		c.shown[userID] = shown{status, seen}
	}
}

var decimal = regexp.MustCompile(`^[1-9][0-9]*$`)

// checkIDs checks the IDs in a frame (§2).
func checkIDs(r *run, value any, key string) {
	switch value := value.(type) {
	case []any:
		for _, item := range value {
			checkIDs(r, item, key)
		}
	case obj:
		for k, v := range value {
			if k != "ext" && k != "og" && k != "push" {
				checkIDs(r, v, k)
			}
		}
	case string:
		switch {
		case key == "":
		case strings.HasSuffix(key, "room_id") || key == "user_id":
			if value == "" || strings.ContainsFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
				r.violate("ids", fmt.Sprintf("%s %q is empty or has whitespace or control characters", key, value))
			}
		case strings.HasSuffix(key, "log_id") || strings.HasSuffix(key, "message_id"):
			if n, err := strconv.ParseInt(value, 10, 64); !decimal.MatchString(value) || err != nil || n >= 1<<53 {
				r.violate("ids", fmt.Sprintf("%s %q is not a positive decimal below 2^53", key, value))
			}
		}
	}
}

// checkRoomRecords checks that room_list and room_update records carry
// log_id and the delivery fields of §4.3.3.
func checkRoomRecords(r *run, frame obj) {
	var rooms []any
	if result, ok := frame["result"].(obj); ok {
		rooms = append(rooms, list(result["joined"])...)
		rooms = append(rooms, list(result["not_joined"])...)
	}
	if params, ok := frame["params"].(obj); ok && frame["method"] == "room_update" {
		rooms = append(rooms, list(params["joined"])...)
		rooms = append(rooms, list(params["updated"])...)
	}
	for _, room := range rooms {
		room, ok := room.(obj)
		if !ok {
			continue
		}
		var missing []string
		for _, k := range []string{"log_id", "latest_log_id", "history_log_id"} {
			if _, ok := room[k]; !ok {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			r.violate("listed-rooms", fmt.Sprintf("room %v lacks %s", room["room_id"], strings.Join(missing, ", ")))
		}
	}
}
