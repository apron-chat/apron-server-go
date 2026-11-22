package apron

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"reflect"
	"testing"

	"github.com/apron-chat/apron-server-go/wire"
)

// Every record the server logs is one of wire's types, field for field:
// history and room_list send records as wire.Room, wire.Message,
// wire.Reactions, and wire.Membership, which keep a member they have no
// field for (Unknown), so a record that had one would be sent without
// anything noticing that the server and the types drifted apart.
func TestRecordsAreWireTypes(t *testing.T) {
	s, httpServer := newTestServer(t, DefaultConfig())
	a := dialGroup(t, httpServer, 1)[0]
	_, ops := a.request(t, "room_set", "ops", map[string]any{"title": "Ops", "description": "Plans", "private": true, "ext": map[string]any{"x": map[string]any{"y": 1}}})
	_, thread := a.request(t, "room_set", "thread", map[string]any{"parent_room_id": "general"})
	_, first := a.request(t, "message", "first", map[string]any{
		"body": map[string]any{"text": "hi @guest_1", "format": "markdown", "mentions": []any{"guest_1"}, "future": true},
		"ext":  map[string]any{"k": "v"},
	})
	a.request(t, "message", "reply", map[string]any{"body": map[string]any{"text": "re", "mentions": []any{}}, "reply_to": map[string]any{"message_id": first["message_id"]}})
	a.request(t, "reactions", "react", map[string]any{"message_id": first["message_id"], "emojis": []any{"👍"}})
	a.request(t, "message", "move", map[string]any{"message_id": first["message_id"], "room_id": thread["room_id"], "body": map[string]any{"text": "moved"}})
	a.request(t, "message", "delete", map[string]any{"message_id": first["message_id"], "room_id": thread["room_id"], "deleted": true})
	a.request(t, "room_set", "edit", map[string]any{"room_id": ops["room_id"], "title": "Ops 2"})
	a.request(t, "room_leave", "leave", map[string]any{"room_id": ops["room_id"]})

	s.mu.RLock()
	defer s.mu.RUnlock()
	kinds := map[recordKind]int{}
	for _, record := range s.records {
		kinds[record.kind]++
		var typed any
		var unknown []jsontext.Value
		switch record.kind {
		case kindRoom:
			room := decodeRecord[wire.Room](record.raw)
			typed, unknown = room, []jsontext.Value{room.Unknown}
		case kindMessage:
			m := decodeRecord[wire.Message](record.raw)
			typed, unknown = m, []jsontext.Value{m.Unknown, m.From.Unknown}
		case kindReactions:
			r := decodeRecord[wire.Reactions](record.raw)
			typed = r
			for _, reaction := range r.Reactions {
				unknown = append(unknown, reaction.From.Unknown)
			}
		case kindMembership:
			m := decodeRecord[wire.Membership](record.raw)
			typed = m
			for _, member := range m.Members {
				unknown = append(unknown, member.User.Unknown)
			}
		}
		for _, value := range unknown {
			if value != nil {
				t.Errorf("record %s has members wire's types do not: %s", record.raw, value)
			}
		}
		var want, got any
		if json.Unmarshal(record.raw, &want) != nil || json.Unmarshal(encodeJSON(typed), &got) != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("record %s encodes as %s", record.raw, encodeJSON(typed))
		}
	}
	if len(kinds) != 4 {
		t.Fatalf("records of kinds %v, want every kind", kinds)
	}
}
