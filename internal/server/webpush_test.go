package server

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json/v2"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"
)

// rfc8291 is the example of RFC 8291 Appendix A.
var rfc8291 = struct {
	plaintext, asPublic, asPrivate, uaPublic, uaPrivate, salt, auth, body string
}{
	plaintext: "When I grow up, I want to be a watermelon",
	asPublic:  "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8",
	asPrivate: "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw",
	uaPublic:  "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4",
	uaPrivate: "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94",
	salt:      "DGv6ra1nlYgDCS1FRnbzlw",
	auth:      "BTBZMqHH6r4Tts7J_aSIgg",
	body: "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27ml" +
		"mlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPT" +
		"pK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN",
}

func mustDecode(t *testing.T, text string) []byte {
	t.Helper()
	data, err := decodeBase64URL(text)
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
	return private, map[string]any{"p256dh": encodeBase64URL(private.PublicKey().Bytes()), "auth": encodeBase64URL(auth)}
}

func TestWebPushEncryptionMatchesRFC8291(t *testing.T) {
	v := rfc8291
	local, err := ecdh.P256().NewPrivateKey(mustDecode(t, v.asPrivate))
	if err != nil {
		t.Fatal(err)
	}
	if got := encodeBase64URL(local.PublicKey().Bytes()); got != v.asPublic {
		t.Fatalf("server public key %s", got)
	}
	keys, err := parsePushKeys(v.uaPublic, v.auth)
	if err != nil {
		t.Fatal(err)
	}
	body, err := encryptPushWith([]byte(v.plaintext), keys, local, mustDecode(t, v.salt))
	if err != nil {
		t.Fatal(err)
	}
	if got := encodeBase64URL(body); got != v.body {
		t.Fatalf("body\n got %s\nwant %s", got, v.body)
	}
	// The receiver's private key recovers it.
	receiver, err := ecdh.P256().NewPrivateKey(mustDecode(t, v.uaPrivate))
	if err != nil {
		t.Fatal(err)
	}
	if got := decryptPush(t, body, receiver, keys.auth); string(got) != v.plaintext {
		t.Fatalf("decrypted %q", got)
	}

	// Each message has a fresh salt and server key.
	first, _ := encryptPush([]byte("same"), keys)
	second, _ := encryptPush([]byte("same"), keys)
	if bytes.Equal(first[:pushHeaderBytes], second[:pushHeaderBytes]) {
		t.Fatal("two messages share a salt and key")
	}
	for _, body := range [][]byte{first, second} {
		if got := decryptPush(t, body, receiver, keys.auth); string(got) != "same" {
			t.Fatalf("decrypted %q", got)
		}
	}
	// The largest payload fills the 4096 bytes a push service must accept.
	largest, err := encryptPush(make([]byte, maxPushPlaintext), keys)
	if err != nil || len(largest) != pushRecordSize {
		t.Fatalf("largest payload: %d bytes, %v", len(largest), err)
	}
	if _, err := encryptPush(make([]byte, maxPushPlaintext+1), keys); err == nil {
		t.Fatal("an oversized payload was encrypted")
	}
}

func TestParsePushKeys(t *testing.T) {
	v := rfc8291
	if _, err := parsePushKeys(v.uaPublic+"=", v.auth+"=="); err != nil {
		t.Fatalf("padded keys: %v", err)
	}
	offCurve := mustDecode(t, v.uaPublic)
	offCurve[64] ^= 1
	compressed := mustDecode(t, v.uaPublic)[:33]
	compressed[0] = 2
	for name, keys := range map[string][2]string{
		"off the curve": {encodeBase64URL(offCurve), v.auth},
		"compressed":    {encodeBase64URL(compressed), v.auth},
		"zero point":    {encodeBase64URL(make([]byte, 65)), v.auth},
		"standard b64":  {strings.ReplaceAll(strings.ReplaceAll(v.uaPublic, "_", "/"), "-", "+"), v.auth},
		"short auth":    {v.uaPublic, encodeBase64URL(make([]byte, 15))},
		"long auth":     {v.uaPublic, encodeBase64URL(make([]byte, 17))},
		"empty":         {"", ""},
	} {
		if _, err := parsePushKeys(keys[0], keys[1]); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestVAPIDAuthorization(t *testing.T) {
	key, err := newVAPIDKey()
	if err != nil {
		t.Fatal(err)
	}
	// The stored form round-trips to the same key.
	again, err := parseVAPIDKey(key.encoded())
	if err != nil || again.public != key.public {
		t.Fatalf("parsed key: %v %v", again, err)
	}
	for _, bad := range []string{"", "not base64!", encodeBase64URL(make([]byte, 31)), encodeBase64URL(make([]byte, 32))} {
		if _, err := parseVAPIDKey(bad); err == nil {
			t.Errorf("VAPID key %q accepted", bad)
		}
	}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	header, err := key.authorization("https://push.example.net:8443/send/abc?x=1", "mailto:ops@example.com", now)
	if err != nil {
		t.Fatal(err)
	}
	token, k, ok := strings.Cut(strings.TrimPrefix(header, "vapid t="), ", k=")
	if !ok || !strings.HasPrefix(header, "vapid t=") || k != key.public {
		t.Fatalf("header %q", header)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token %q", token)
	}
	decode := func(part string) map[string]any {
		var value map[string]any
		if err := json.Unmarshal(mustDecode(t, part), &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if got := decode(parts[0]); !reflect.DeepEqual(got, map[string]any{"typ": "JWT", "alg": "ES256"}) {
		t.Fatalf("JWT header %#v", got)
	}
	want := map[string]any{"aud": "https://push.example.net:8443", "exp": float64(now.Add(12 * time.Hour).Unix()), "sub": "mailto:ops@example.com"}
	if got := decode(parts[1]); !reflect.DeepEqual(got, want) {
		t.Fatalf("JWT claims %#v", got)
	}
	// The advertised public key verifies the ES256 signature, r || s.
	public, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), mustDecode(t, k))
	if err != nil {
		t.Fatal(err)
	}
	signature := mustDecode(t, parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if len(signature) != 64 || !ecdsa.Verify(public, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
		t.Fatal("the VAPID signature does not verify")
	}
	// The audience is the origin, lowercased, without a default port.
	for endpoint, want := range map[string]string{
		"HTTPS://Push.Example.NET:443/x": "https://push.example.net",
		"https://push.example.net:8443/": "https://push.example.net:8443",
		"http://[::1]:80/x":              "http://[::1]",
	} {
		header, _ := key.authorization(endpoint, "", now)
		token, _, _ := strings.Cut(strings.TrimPrefix(header, "vapid t="), ", k=")
		if got := decode(strings.Split(token, ".")[1])["aud"]; got != want {
			t.Errorf("aud for %s = %v, want %s", endpoint, got, want)
		}
	}
	// Without a subject the token has no sub.
	header, _ = key.authorization("https://push.example.net/x", "", now)
	token, _, _ = strings.Cut(strings.TrimPrefix(header, "vapid t="), ", k=")
	if _, has := decode(strings.Split(token, ".")[1])["sub"]; has {
		t.Fatal("sub without a subject")
	}
}
