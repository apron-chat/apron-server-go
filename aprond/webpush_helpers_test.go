package aprond

// Helpers for tests of pushes to webpush subscriptions, as in
// internal/webpush's tests.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"testing"

	"github.com/apron-chat/apron-server-go/aprond/internal/webpush"
)

// The aes128gcm framing of a push (internal/webpush).
const (
	p256PointBytes  = webpush.P256PointBytes
	authSecretBytes = 16
	pushRecordSize  = 4096
	pushHeaderBytes = 16 + 4 + 1 + p256PointBytes
)

func mustDecode(t *testing.T, text string) []byte {
	t.Helper()
	data, err := webpush.DecodeBase64URL(text)
	if err != nil {
		t.Fatalf("decode %q: %v", text, err)
	}
	return data
}

// decryptPush is the user agent's side of RFC 8291: it recovers the
// payload of one aes128gcm record with the subscription's private key.
func decryptPush(t *testing.T, body []byte, private *ecdh.PrivateKey, auth []byte) []byte {
	t.Helper()
	if len(body) < pushHeaderBytes || body[20] != p256PointBytes {
		t.Fatalf("aes128gcm header: %x", body[:min(len(body), pushHeaderBytes)])
	}
	salt, serverKey := body[:16], body[21:pushHeaderBytes]
	if rs := binary.BigEndian.Uint32(body[16:]); rs != pushRecordSize {
		t.Fatalf("record size %d", rs)
	}
	server, err := ecdh.P256().NewPublicKey(serverKey)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := private.ECDH(server)
	if err != nil {
		t.Fatal(err)
	}
	info := "WebPush: info\x00" + string(private.PublicKey().Bytes()) + string(serverKey)
	ikm, _ := hkdf.Key(sha256.New, secret, auth, info, 32)
	cek, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	record, err := gcm.Open(nil, nonce, body[pushHeaderBytes:], nil)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if len(record) == 0 || record[len(record)-1] != 2 {
		t.Fatalf("record does not end with the last-record delimiter: %x", record)
	}
	return record[:len(record)-1]
}

// testSubscription is a browser's push subscription: its private key and
// the keys it registers.
func testSubscription(t *testing.T) (*ecdh.PrivateKey, map[string]any) {
	t.Helper()
	private, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, authSecretBytes)
	_, _ = rand.Read(auth)
	return private, map[string]any{"p256dh": webpush.EncodeBase64URL(private.PublicKey().Bytes()), "auth": webpush.EncodeBase64URL(auth)}
}
