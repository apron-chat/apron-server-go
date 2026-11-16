package external

import (
	"crypto/ed25519"
	"crypto/rsa"
	"errors"
	"strings"

	jose "github.com/go-jose/go-jose/v4"
)

// errToken is any problem with a token's form, type, or signature.
var errToken = errors.New("external: the token is not a valid access token from the issuer")

// algEd25519 is EdDSA with Ed25519 by its fully-specified name (RFC 9864),
// which go-jose does not know: the same signature.
const algEd25519 = jose.SignatureAlgorithm("Ed25519")

// accessTokenAlgs are the signatures accepted: never none or HMAC.
var accessTokenAlgs = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512, jose.PS256, jose.PS384, jose.PS512,
	jose.ES256, jose.ES384, jose.EdDSA, algEd25519,
}

// verifyAccessToken checks that a compact JWS is a JWT access token (RFC
// 9068), by its typ, signed with one of the keys that keys returns for its
// kid, and returns its payload.
//
// The typ must be at+jwt, or application/at+jwt, in any case (§2.1). An ID
// token, which an issuer signs with the same keys and may address to the
// server too, has another, or none, and is no access token.
func verifyAccessToken(token string, keys func(kid string) []jose.JSONWebKey) ([]byte, error) {
	parsed, err := jose.ParseSignedCompact(token, accessTokenAlgs)
	if err != nil {
		return nil, errToken
	}
	signature := parsed.Signatures[0]
	header := signature.Protected.ExtraHeaders
	typ, _ := header[jose.HeaderType].(string)
	typ = strings.ToLower(typ)
	// No crit: go-jose would take b64 (RFC 7797) in it.
	if _, crit := header["crit"]; crit || (typ != "at+jwt" && typ != "application/at+jwt") {
		return nil, errToken
	}
	for _, key := range keys(signature.Header.KeyID) {
		if jose.SignatureAlgorithm(signature.Header.Algorithm) == algEd25519 {
			public, ok := key.Key.(ed25519.PublicKey)
			if ok && ed25519.Verify(public, []byte(token[:strings.LastIndexByte(token, '.')]), signature.Signature) {
				return parsed.UnsafePayloadWithoutVerification(), nil
			}
			continue
		}
		if payload, err := parsed.Verify(key); err == nil {
			return payload, nil
		}
	}
	return nil, errToken
}

// signingKey reports whether k, from the issuer's key set, is a public key
// for signatures this server takes: not one for encryption, and no RSA key
// under 2048 bits or with an exponent under 3.
func signingKey(k jose.JSONWebKey) bool {
	if !k.IsPublic() || (k.Use != "" && k.Use != "sig") {
		return false
	}
	key, isRSA := k.Key.(*rsa.PublicKey)
	return !isRSA || (key.N.BitLen() >= 2048 && key.E >= 3)
}
