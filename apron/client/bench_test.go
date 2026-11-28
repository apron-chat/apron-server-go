package client_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/client"
	"github.com/apron-chat/apron-server-go/apron/internal/benchchat"
	"github.com/apron-chat/apron-server-go/apron/server"
)

// benchServer serves a benchchat over a WebSocket.
func benchServer(b *testing.B) (*benchchat.Chat, string) {
	chat := benchchat.New(server.Config{CheckOrigin: server.AnyOrigin})
	ts := httptest.NewServer(chat.Server)
	b.Cleanup(func() {
		_ = chat.Server.Shutdown(context.Background())
		ts.Close()
	})
	return chat, "ws" + strings.TrimPrefix(ts.URL, "http")
}

// BenchmarkClientHistoryPages pages back through a log of 100,000 messages, 100
// a page, over a WebSocket: an op is the whole log, and each record's
// cost is reported.
func BenchmarkClientHistoryPages(b *testing.B) {
	const records = 100_000
	chat, url := benchServer(b)
	chat.Seed("general", records)
	ctx := context.Background()
	conn, err := client.Dial(ctx, url, apron.AuthParams{Scheme: "guest", Name: "Ada"})
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	for b.Loop() {
		n := 0
		for page, err := range conn.HistoryPages(ctx, apron.HistoryParams{Limit: 100}) {
			if err != nil {
				b.Fatal(err)
			}
			n += len(page.Messages)
		}
		if n != records {
			b.Fatalf("paged through %d records, want %d", n, records)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*records), "ns/record")
}

// BenchmarkClientFrames has the server broadcast messages to a client
// over a WebSocket, which hands each to its OnFrame: an op is one frame,
// sent, read, and handed over.
func BenchmarkClientFrames(b *testing.B) {
	chat, url := benchServer(b)
	ctx := context.Background()
	var got atomic.Int64
	reached := make(chan struct{}, 1)
	var target atomic.Int64
	conn, err := client.Dial(ctx, url, apron.AuthParams{Scheme: "guest", Name: "Ada"}, client.OnFrame(func(apron.Frame) {
		if got.Add(1) >= target.Load() {
			select {
			case reached <- struct{}{}:
			default:
			}
		}
	}))
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	wait := func(n int64) {
		target.Store(n)
		for got.Load() < n {
			<-reached
		}
	}
	wait(1) // the server frame
	frame, _ := apron.Encode(benchchat.Message(1_700_000_000_000, "general", 1))
	sent := got.Load()
	for b.Loop() {
		chat.Server.Broadcast(frame, nil)
		sent++
		if sent%64 == 0 {
			wait(sent)
		}
	}
	wait(sent)
}

// BenchmarkClientPost posts a message over a WebSocket and waits for its
// result, one at a time: a request's round trip, both ends.
func BenchmarkClientPost(b *testing.B) {
	_, url := benchServer(b)
	ctx := context.Background()
	conn, err := client.Dial(ctx, url, apron.AuthParams{Scheme: "guest", Name: "Ada"})
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	p := apron.MessageParams{Body: apron.Body{Text: "Hello, everyone: this is a line of chat of a usual length."}}
	for b.Loop() {
		if _, err := conn.Post(ctx, p); err != nil {
			b.Fatal(err)
		}
	}
}
