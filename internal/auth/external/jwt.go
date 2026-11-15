package external

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"hash"
	"math/big"
	"strings"
)

// A JSON Web Key (RFC 7517), as an issuer's jwks_uri lists them.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	// RSA
	N string `json:"n"`
	E string `json:"e"`
	// EC and OKP
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// publicKey is the key, or an error for one this server cannot use.
func (k jwk) publicKey() (crypto.PublicKey, error) {
	decode := base64.RawURLEncoding.DecodeString
	switch k.Kty {
	case "RSA":
		n, err1 := decode(k.N)
		e, err2 := decode(k.E)
		if err1 != nil || err2 != nil || len(e) > 4 {
			return nil, errors.New("bad RSA key")
		}
		exponent := int(new(big.Int).SetBytes(e).Int64())
		key := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exponent}
		if key.N.BitLen() < 2048 || exponent < 3 {
			return nil, errors.New("weak RSA key")
		}
		return key, nil
	case "EC":
		var curve elliptic.Curve
		switch k.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		default:
			return nil, errors.New("unsupported curve")
		}
		x, err1 := decode(k.X)
		y, err2 := decode(k.Y)
		if err1 != nil || err2 != nil {
			return nil, errors.New("bad EC key")
		}
		size := (curve.Params().BitSize + 7) / 8
		if len(x) != size || len(y) != size {
			return nil, errors.New("bad EC key")
		}
		uncompressed := append(append([]byte{4}, x...), y...)
		return ecdsa.ParseUncompressedPublicKey(curve, uncompressed)
	case "OKP":
		x, err := decode(k.X)
		if k.Crv != "Ed25519" || err != nil || len(x) != ed25519.PublicKeySize {
			return nil, errors.New("unsupported OKP key")
		}
		return ed25519.PublicKey(x), nil
	}
	return nil, errors.New("unsupported key type")
}

// errToken is any problem with a token's form or signature.
var errToken = errors.New("external: the token is not a valid JWT from the issuer")

// verifyJWT checks a compact JWS's signature with one of the keys that
// keys returns for its kid, and returns its claims.
func verifyJWT(token string, keys func(kid string) []crypto.PublicKey) (map[string]jsontext.Value, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errToken
	}
	var header struct {
		Alg  string   `json:"alg"`
		Kid  string   `json:"kid"`
		Crit []string `json:"crit"`
	}
	rawHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(rawHeader, &header) != nil || len(header.Crit) > 0 {
		return nil, errToken
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errToken
	}
	signed := []byte(parts[0] + "." + parts[1])
	verified := false
	for _, key := range keys(header.Kid) {
		if verifySignature(header.Alg, key, signed, signature) {
			verified = true
			break
		}
	}
	if !verified {
		return nil, errToken
	}
	rawClaims, err := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]jsontext.Value
	if err != nil || json.Unmarshal(rawClaims, &claims) != nil || claims == nil {
		return nil, errToken
	}
	return claims, nil
}

// verifySignature checks one signature by alg, which must suit the key:
// RS256/384/512, PS256/384/512, ES256/384, or EdDSA; never none or HMAC.
func verifySignature(alg string, key crypto.PublicKey, signed, signature []byte) bool {
	digest := func(h hash.Hash) []byte {
		h.Write(signed)
		return h.Sum(nil)
	}
	switch key := key.(type) {
	case *rsa.PublicKey:
		var h crypto.Hash
		switch alg[min(2, len(alg)):] {
		case "256":
			h = crypto.SHA256
		case "384":
			h = crypto.SHA384
		case "512":
			h = crypto.SHA512
		default:
			return false
		}
		sum := digest(h.New())
		switch {
		case strings.HasPrefix(alg, "RS"):
			return rsa.VerifyPKCS1v15(key, h, sum, signature) == nil
		case strings.HasPrefix(alg, "PS"):
			return rsa.VerifyPSS(key, h, sum, signature, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}) == nil
		}
	case *ecdsa.PublicKey:
		var sum []byte
		switch {
		case alg == "ES256" && key.Curve == elliptic.P256():
			sum = digest(sha256.New())
		case alg == "ES384" && key.Curve == elliptic.P384():
			sum = digest(sha512.New384())
		default:
			return false
		}
		size := (key.Curve.Params().BitSize + 7) / 8
		if len(signature) != 2*size {
			return false
		}
		r, s := new(big.Int).SetBytes(signature[:size]), new(big.Int).SetBytes(signature[size:])
		return ecdsa.Verify(key, sum, r, s)
	case ed25519.PublicKey:
		return (alg == "EdDSA" || alg == "Ed25519") && ed25519.Verify(key, signed, signature)
	}
	return false
}
