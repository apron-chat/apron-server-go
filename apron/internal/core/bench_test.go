package core

import (
	"strconv"
	"testing"
)

// The frame a client sends most: a new message.
var messageFrame = []byte(`{"id":"k3J9xQ2mZ8aB-17","method":"message","params":{"room_id":"general","body":{"text":"Hello, everyone: this is a line of chat of a usual length.","mentions":["guest_2"]}}}`)

func BenchmarkRequestParse(b *testing.B) {
	b.SetBytes(int64(len(messageFrame)))
	for b.Loop() {
		if _, err := ParseRequest(messageFrame); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRequestFingerprint(b *testing.B) {
	req, _ := ParseRequest(messageFrame)
	for b.Loop() {
		Fingerprint(req.Method, req.Params)
	}
}

// BenchmarkDedupCache claims and finishes requests of many users, each with
// new ids, so that every user's oldest are forgotten as Size bounds them.
func BenchmarkDedupCache(b *testing.B) {
	for _, size := range []int{16, 1024} {
		b.Run("size="+strconv.Itoa(size), func(b *testing.B) {
			d := Dedup[string, []byte]{Size: size, MaxBytes: 1 << 20}
			result := []byte(`{"message_id":"1700000000000"}`)
			ids := make([]string, 4*size)
			for i := range ids {
				ids[i] = "k3J9xQ2mZ8aB-" + strconv.Itoa(i)
			}
			i := 0
			for b.Loop() {
				id := ids[i%len(ids)]
				i++
				entry, prior := d.Claim(id, "m", nil, len(id))
				if prior != nil {
					continue
				}
				d.Finish(id, entry, result, len(result), false)
			}
		})
	}
}
