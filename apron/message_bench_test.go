package apron_test

import (
	"encoding/json/v2"
	"testing"

	"github.com/apron-chat/apron-server-go/apron"
)

// BenchmarkMessageCodec decodes and encodes a message snapshot of a line
// of chat, as a client decodes each it receives and a server encodes each
// it sends.
func BenchmarkMessageCodec(b *testing.B) {
	raw, err := apron.Marshal(benchMessages(1)[0])
	if err != nil {
		b.Fatal(err)
	}
	b.Run("decode", func(b *testing.B) {
		b.SetBytes(int64(len(raw)))
		for b.Loop() {
			var m apron.Message
			if err := json.Unmarshal(raw, &m); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("encode", func(b *testing.B) {
		m := benchMessages(1)[0]
		for b.Loop() {
			if _, err := apron.Marshal(&m); err != nil {
				b.Fatal(err)
			}
		}
	})
}
