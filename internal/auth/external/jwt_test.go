package external

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"testing"

	jose "github.com/go-jose/go-jose/v4"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// compact is a JWT access token signed with sign under alg.
func compact(t *testing.T, alg string, sign func([]byte) []byte) string {
	return compactTyped(t, alg, "at+jwt", sign)
}

// compactTyped is a JWT whose header's typ is typ, none if empty.
func compactTyped(t *testing.T, alg, typ string, sign func([]byte) []byte) string {
	t.Helper()
	fields := map[string]any{"alg": alg, "kid": "k"}
	if typ != "" {
		fields["typ"] = typ
	}
	return compactHeader(t, fields, sign)
}

// compactHeader is a JWT with the header fields, signed with sign.
func compactHeader(t *testing.T, fields map[string]any, sign func([]byte) []byte) string {
	t.Helper()
	header, _ := json.Marshal(fields)
	payload, _ := json.Marshal(map[string]any{"sub": "s"})
	signed := b64(header) + "." + b64(payload)
	return signed + "." + b64(sign([]byte(signed)))
}

func TestVerifyAccessToken(t *testing.T) {
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	_, edKey, _ := ed25519.GenerateKey(rand.Reader)
	only := func(key crypto.PublicKey) func(string) []jose.JSONWebKey {
		return func(string) []jose.JSONWebKey { return []jose.JSONWebKey{{Key: key}} }
	}
	rs256 := compact(t, "RS256", func(signed []byte) []byte {
		sum := sha256.Sum256(signed)
		signature, _ := rsa.SignPKCS1v15(rand.Reader, rsaKey, crypto.SHA256, sum[:])
		return signature
	})
	if payload, err := verifyAccessToken(rs256, only(&rsaKey.PublicKey)); err != nil || string(payload) != `{"sub":"s"}` {
		t.Fatalf("RS256: %s %v", payload, err)
	}
	ed := func(signed []byte) []byte { return ed25519.Sign(edKey, signed) }
	eddsa := compact(t, "EdDSA", ed)
	if _, err := verifyAccessToken(eddsa, only(edKey.Public())); err != nil {
		t.Fatalf("EdDSA: %v", err)
	}
	// Ed25519 is EdDSA by its fully-specified name (RFC 9864), which go-jose
	// does not know.
	ed25519Named := compact(t, "Ed25519", ed)
	if payload, err := verifyAccessToken(ed25519Named, only(edKey.Public())); err != nil || string(payload) != `{"sub":"s"}` {
		t.Fatalf("Ed25519: %s %v", payload, err)
	}
	// No none, no HMAC with a public key, no key of another kind, and no
	// header the server does not understand.
	for name, test := range map[string]struct {
		token string
		key   crypto.PublicKey
	}{
		"none":              {compact(t, "none", func([]byte) []byte { return nil }), &rsaKey.PublicKey},
		"HS256":             {compact(t, "HS256", func([]byte) []byte { return []byte("mac") }), &rsaKey.PublicKey},
		"wrong key":         {eddsa, &rsaKey.PublicKey},
		"Ed25519, RSA key":  {ed25519Named, &rsaKey.PublicKey},
		"Ed25519, tampered": {ed25519Named[:len(ed25519Named)-4] + "AAAA", edKey.Public()},
		"crit":              {compactHeader(t, map[string]any{"alg": "EdDSA", "typ": "at+jwt", "crit": []string{"b64"}, "b64": true}, ed), edKey.Public()},
		"JSON serialization": {func() string {
			signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: edKey}, (&jose.SignerOptions{}).WithType("at+jwt"))
			signed, _ := signer.Sign([]byte(`{"sub":"s"}`))
			return signed.FullSerialize()
		}(), edKey.Public()},
	} {
		if _, err := verifyAccessToken(test.token, only(test.key)); err == nil {
			t.Errorf("%s verified", name)
		}
	}
	// Only an access token (RFC 9068 §2.1), in any case: not an ID token,
	// which the issuer signs with the same keys.
	for typ, ok := range map[string]bool{
		"at+jwt": true, "AT+JWT": true, "application/at+jwt": true, "Application/AT+JWT": true,
		"": false, "JWT": false, "jwt": false, "application/jwt": false, "id_token+jwt": false, "at+jwt; x": false,
	} {
		for _, alg := range []string{"EdDSA", "Ed25519"} {
			if _, err := verifyAccessToken(compactTyped(t, alg, typ, ed), only(edKey.Public())); (err == nil) != ok {
				t.Errorf("%s typ %q: %v", alg, typ, err)
			}
		}
	}
}

// Only public signing keys are taken from the issuer's key set, and no
// weak RSA key.
func TestSigningKey(t *testing.T) {
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rsa2048, _ := rsa.GenerateKey(rand.Reader, 2048)
	// Go's crypto/rsa verifies with a 1024-bit key; the server does not.
	rsa1024, _ := rsa.GenerateKey(rand.Reader, 1024)
	for name, test := range map[string]struct {
		key jose.JSONWebKey
		ok  bool
	}{
		"EC":              {jose.JSONWebKey{Key: &ecKey.PublicKey}, true},
		"EC, use sig":     {jose.JSONWebKey{Key: &ecKey.PublicKey, Use: "sig"}, true},
		"EC, use enc":     {jose.JSONWebKey{Key: &ecKey.PublicKey, Use: "enc"}, false},
		"EC, private":     {jose.JSONWebKey{Key: ecKey}, false},
		"RSA 2048":        {jose.JSONWebKey{Key: &rsa2048.PublicKey}, true},
		"RSA 1024":        {jose.JSONWebKey{Key: &rsa1024.PublicKey}, false},
		"RSA, exponent 2": {jose.JSONWebKey{Key: &rsa.PublicKey{N: rsa2048.N, E: 2}}, false},
		"HMAC":            {jose.JSONWebKey{Key: []byte("secret")}, false},
	} {
		if got := signingKey(test.key); got != test.ok {
			t.Errorf("%s: %v, want %v", name, got, test.ok)
		}
	}
}
