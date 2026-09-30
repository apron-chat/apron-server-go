package main

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/internal/server"
)

// fakeRelay is a minimal SMTP relay without STARTTLS that keeps the DATA it
// receives, or, when stall is set, never answers after connecting.
func fakeRelay(t *testing.T, stall bool) (string, chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	received := make(chan string, 1)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				if stall {
					_, _ = conn.Read(make([]byte, 1))
					return
				}
				r := bufio.NewReader(conn)
				write := func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }
				write("220 relay.test ESMTP")
				var data strings.Builder
				inData := false
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if inData {
						if line == ".\r\n" {
							inData = false
							received <- data.String()
							write("250 queued")
							continue
						}
						data.WriteString(line)
						continue
					}
					switch verb := strings.ToUpper(strings.Fields(line + " x")[0]); verb {
					case "EHLO", "HELO":
						write("250 relay.test")
					case "DATA":
						inData = true
						write("354 go ahead")
					case "QUIT":
						write("221 bye")
						return
					default:
						write("250 ok")
					}
				}
			}()
		}
	}()
	return listener.Addr().String(), received
}

func TestSMTPSenderRequiresSTARTTLS(t *testing.T) {
	addr, received := fakeRelay(t, false)
	message := server.SignInEmail{To: "ada@example.com", Code: "418092", Expires: time.Now().Add(10 * time.Minute)}
	sender, err := newSMTPSender(addr, "Apron <chat@example.com>", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.SendSignInCode(context.Background(), message); err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("cleartext relay without --email.smtp-insecure: %v", err)
	}
	sender.insecure = true
	if err := sender.SendSignInCode(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if data := <-received; !strings.Contains(data, "Subject: Your sign-in code is 418092") || !strings.Contains(data, "To: ada@example.com") {
		t.Fatalf("message: %q", data)
	}
}

func TestSMTPSenderStopsAtItsDeadline(t *testing.T) {
	addr, _ := fakeRelay(t, true)
	sender, err := newSMTPSender(addr, "chat@example.com", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := sender.SendSignInCode(ctx, server.SignInEmail{To: "ada@example.com", Code: "1"}); err == nil {
		t.Fatal("a stalled relay did not fail")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("took %v past a 200ms deadline", elapsed)
	}
}
