package client

import (
	"testing"

	"github.com/apron-chat/apron-server-go/apron"
)

// BenchmarkClientReceive is what the client does with each frame it reads, but
// the read: tell a reply from a notification, note what it says of the
// connection, and queue it for OnFrame.
func BenchmarkClientReceive(b *testing.B) {
	frames := map[string][]byte{}
	message, _ := apron.Encode(apron.Message{
		MessageID: 1_700_000_000_000, LogID: 1_700_000_000_000, RoomID: "general",
		From: apron.User{UserID: "guest_1", Name: "Ada"}, Body: apron.Body{Text: "Hello, everyone: this is a line of chat of a usual length."},
	})
	frames["message"] = message
	frames["reply"] = []byte(`{"id":"k3J9xQ2mZ8aB-17","result":{"message_id":"1700000000000"}}`)
	for name, frame := range frames {
		b.Run(name, func(b *testing.B) {
			c := &Conn{frames: make(chan apron.Frame, queue), calls: map[string]*pending{}, done: make(chan struct{})}
			for b.Loop() {
				if name == "reply" {
					c.mu.Lock()
					c.calls["k3J9xQ2mZ8aB-17"] = &pending{method: "message", reply: make(chan reply, 1)}
					c.mu.Unlock()
				}
				c.receive(frame)
				select {
				case <-c.frames:
				default:
				}
			}
		})
	}
}
