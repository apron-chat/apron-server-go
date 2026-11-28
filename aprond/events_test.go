package aprond

import (
	"reflect"
	"testing"

	"github.com/apron-chat/apron-server-go/aprond/internal/roomlog"
)

// A decoded message's Body holds the members of its snapshot's body, those
// package apron has no field for too, as the snapshot encodes them.
func TestDecodeMessageBody(t *testing.T) {
	for _, raw := range []string{
		`{"body":{"text":"hi"},"from":{"user_id":"ada"},"log_id":"2","message_id":"2","room_id":"general"}`,
		`{"body":{"format":"markdown","mentions":["bob"],"text":"**hi** @bob","x":{"y":[1,2.5,"z",null]}},"from":{"user_id":"ada"},"log_id":"3","message_id":"2","prev_log_id":"2","room_id":"general"}`,
		`{"body":{"embeds":[{"embed_id":"e1","type":"upload","url":"https://example.com/f"}],"text":""},"from":{"user_id":"ada"},"log_id":"4","message_id":"4","room_id":"general"}`,
		`{"deleted":true,"from":{"user_id":"ada"},"log_id":"5","message_id":"5","room_id":"general"}`,
	} {
		m, ok := decodeMessageRecord([]byte(raw))
		if !ok {
			t.Fatalf("%s is not a message", raw)
		}
		var want map[string]any
		if snapshot := roomlog.DecodeMessage([]byte(raw)); snapshot.Body != nil {
			want = bodyObject(&snapshot.Body.Body)
		}
		if !reflect.DeepEqual(m.Body(), want) {
			t.Errorf("%s: Body %#v, want %#v", raw, m.Body(), want)
		}
	}
}

// A decoded message has the fields of its snapshot.
func TestDecodeMessageFields(t *testing.T) {
	raw := `{"body":{"format":"markdown","mentions":["bob","cy"],"text":"**hi**"},"ext":{"k":{"a":1}},"from":{"name":"Ada","user_id":"ada"},"log_id":"9","message_id":"7","prev_log_id":"7","prev_room_id":"ops","reply_to":{"message_id":"3"},"room_id":"general"}`
	m, ok := decodeMessageRecord([]byte(raw))
	if !ok || m.ID != "7" || m.LogID != 9 || m.PrevLogID != 7 || m.PrevRoomID != "ops" || m.RoomID != "general" ||
		m.From != (Sender{UserID: "ada", Name: "Ada"}) || m.Text != "**hi**" || m.Format != "markdown" ||
		!reflect.DeepEqual(m.Mentions, []string{"bob", "cy"}) || m.ReplyTo != "3" || string(m.Ext["k"]) != `{"a":1}` || m.Deleted || m.New() {
		t.Fatalf("decoded %+v", m)
	}
	if m, ok := decodeMessageRecord([]byte(`{"body":{"text":"x"},"from":{"user_id":"ada"},"log_id":"1","message_id":"1","room_id":"general"}`)); !ok || m.Format != "plain" || !m.New() || m.Mentions != nil {
		t.Fatalf("decoded %+v", m)
	}
	if _, ok := decodeMessageRecord([]byte(`{"log_id":"1","room_id":"general"}`)); ok {
		t.Fatal("a room record decoded as a message")
	}
}
