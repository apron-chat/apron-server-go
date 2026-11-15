package external

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"math/big"
	"testing"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func compact(t *testing.T, alg string, sign func([]byte) []byte) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": alg, "kid": "k"})
	payload, _ := json.Marshal(map[string]any{"sub": "s"})
	signed := b64(header) + "." + b64(payload)
	return signed + "." + b64(sign([]byte(signed)))
}

func TestVerifyJWT(t *testing.T) {
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	_, edKey, _ := ed25519.GenerateKey(rand.Reader)
	only := func(key crypto.PublicKey) func(string) []crypto.PublicKey {
		return func(string) []crypto.PublicKey { return []crypto.PublicKey{key} }
	}
	rs256 := compact(t, "RS256", func(signed []byte) []byte {
		sum := sha256.Sum256(signed)
		signature, _ := rsa.SignPKCS1v15(rand.Reader, rsaKey, crypto.SHA256, sum[:])
		return signature
	})
	if claims, err := verifyJWT(rs256, only(&rsaKey.PublicKey)); err != nil || string(claims["sub"]) != `"s"` {
		t.Fatalf("RS256: %v %v", claims, err)
	}
	eddsa := compact(t, "EdDSA", func(signed []byte) []byte { return ed25519.Sign(edKey, signed) })
	if _, err := verifyJWT(eddsa, only(edKey.Public())); err != nil {
		t.Fatalf("EdDSA: %v", err)
	}
	// No none, no HMAC with a public key, no key of another kind.
	for name, token := range map[string]string{
		"none":      compact(t, "none", func([]byte) []byte { return nil }),
		"HS256":     compact(t, "HS256", func([]byte) []byte { return []byte("mac") }),
		"wrong key": eddsa,
	} {
		if _, err := verifyJWT(token, only(&rsaKey.PublicKey)); err == nil {
			t.Errorf("%s verified", name)
		}
	}
	// A weak RSA key in a key set is not used.
	weak := jwk{Kty: "RSA", N: b64(big.NewInt(1<<62 - 57).Bytes()), E: "AQAB"}
	if _, err := weak.publicKey(); err == nil {
		t.Fatal("a weak RSA key was accepted")
	}
}
