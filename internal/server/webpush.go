package server

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Web Push (§4.7, push kind webpush, and relay with keys): message
// encryption for the client (RFC 8291, the aes128gcm content coding of
// RFC 8188) and the server's VAPID identification to push services
// (RFC 8292), with the standard library only.

const (
	// p256PointBytes is an uncompressed P-256 point; authSecretBytes is a
	// subscription's auth secret (RFC 8291 section 3.2).
	p256PointBytes  = 65
	authSecretBytes = 16
	// pushRecordSize is the rs of the one aes128gcm record a push carries.
	pushRecordSize = 4096
	// pushHeaderBytes is the aes128gcm header: salt, record size, key ID
	// length, and the server's 65-byte ephemeral public key as key ID.
	pushHeaderBytes = 16 + 4 + 1 + p256PointBytes
	// maxPushPlaintext is the longest payload one record holds: a push
	// service accepts 4096 bytes of body (RFC 8030 section 7.2), which holds
	// the header, the payload with its padding delimiter, and the 16-byte
	// AES-GCM tag.
	maxPushPlaintext = pushRecordSize - pushHeaderBytes - 1 - 16
	// vapidTokenLifetime is how long a VAPID token is valid; RFC 8292
	// allows at most 24 hours.
	vapidTokenLifetime = 12 * time.Hour
	// vapidTokenReuse is how long a VAPID token is reused for its audience
	// and subject before another is signed, well within its lifetime;
	// maxVAPIDTokens bounds the tokens kept, one per push service origin.
	vapidTokenReuse = time.Hour
	maxVAPIDTokens  = 1024
)

// pushKeys are a subscription's keys (§4.7 keys): the client's P-256 public
// key and its auth secret.
type pushKeys struct {
	p256dh []byte
	auth   []byte
}

// decodeBase64URL decodes base64url, unpadded as §4.7 sends it, or padded.
func decodeBase64URL(text string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(text, "="))
}

func encodeBase64URL(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

// parsePushKeys checks a subscription's keys: p256dh an uncompressed point
// on P-256, which its length alone does not show, and auth 16 bytes.
func parsePushKeys(p256dh, auth string) (pushKeys, error) {
	point, err := decodeBase64URL(p256dh)
	if err != nil || len(point) != p256PointBytes || point[0] != 4 {
		return pushKeys{}, errors.New("keys.p256dh must be an uncompressed P-256 point in base64url")
	}
	if _, err := ecdh.P256().NewPublicKey(point); err != nil {
		return pushKeys{}, errors.New("keys.p256dh is not a point on P-256")
	}
	secret, err := decodeBase64URL(auth)
	if err != nil || len(secret) != authSecretBytes {
		return pushKeys{}, errors.New("keys.auth must be 16 bytes in base64url")
	}
	return pushKeys{p256dh: point, auth: secret}, nil
}

// encryptPush encrypts one push payload for a subscription (RFC 8291): a
// fresh ECDH key pair and salt per message, one aes128gcm record with no
// padding past its delimiter.
func encryptPush(plaintext []byte, keys pushKeys) ([]byte, error) {
	local, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return encryptPushWith(plaintext, keys, local, salt)
}

// encryptPushWith is encryptPush with the server's ephemeral key and the
// salt given, for RFC 8291's test vector.
func encryptPushWith(plaintext []byte, keys pushKeys, local *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	if len(plaintext) > maxPushPlaintext {
		return nil, errors.New("push payload is too large")
	}
	receiver, err := ecdh.P256().NewPublicKey(keys.p256dh)
	if err != nil {
		return nil, err
	}
	secret, err := local.ECDH(receiver)
	if err != nil {
		return nil, err
	}
	localPublic := local.PublicKey().Bytes()
	// key_info = "WebPush: info" || 0x00 || ua_public || as_public
	keyInfo := "WebPush: info\x00" + string(keys.p256dh) + string(localPublic)
	ikm, err := hkdf.Key(sha256.New, secret, keys.auth, keyInfo, 32)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	body := make([]byte, pushHeaderBytes, pushHeaderBytes+len(plaintext)+1+gcm.Overhead())
	copy(body, salt)
	binary.BigEndian.PutUint32(body[16:], pushRecordSize)
	body[20] = p256PointBytes
	copy(body[21:], localPublic)
	// The last (and only) record ends with the 0x02 delimiter (RFC 8188
	// section 2).
	record := append(append(make([]byte, 0, len(plaintext)+1), plaintext...), 2)
	return gcm.Seal(body, nonce, record, nil), nil
}

// vapidKey is the server's VAPID key pair (RFC 8292), which
// server.push.webpush.key advertises.
type vapidKey struct {
	private *ecdsa.PrivateKey
	// public is the uncompressed public key in unpadded base64url.
	public string

	// tokens holds the Authorization header last signed for each audience
	// and subject, reused for vapidTokenReuse.
	mu     sync.Mutex
	tokens map[string]vapidToken
}

// vapidToken is a signed Authorization header and when it was signed.
type vapidToken struct {
	header string
	signed time.Time
}

// newVAPIDKey generates a VAPID key pair.
func newVAPIDKey() (*vapidKey, error) {
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return vapidKeyOf(private)
}

// parseVAPIDKey reads a VAPID private key: the P-256 scalar in base64url,
// as web-push tools print it.
func parseVAPIDKey(text string) (*vapidKey, error) {
	scalar, err := decodeBase64URL(strings.TrimSpace(text))
	if err != nil {
		return nil, errors.New("a VAPID private key is base64url")
	}
	private, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), scalar)
	if err != nil {
		return nil, errors.New("a VAPID private key is a 32-byte P-256 scalar")
	}
	return vapidKeyOf(private)
}

func vapidKeyOf(private *ecdsa.PrivateKey) (*vapidKey, error) {
	public, err := private.PublicKey.Bytes()
	if err != nil {
		return nil, err
	}
	return &vapidKey{private: private, public: encodeBase64URL(public)}, nil
}

// encoded is the private scalar in unpadded base64url, as the store keeps it.
func (k *vapidKey) encoded() string {
	scalar, _ := k.private.Bytes()
	return encodeBase64URL(scalar)
}

// authorization is the Authorization header that identifies this server to
// the push service of endpoint (RFC 8292 section 3): vapid t=<ES256 JWT for
// the endpoint's origin>, k=<public key>. subject, a mailto: or https: URL,
// is the contact the push service may use; empty leaves sub out. A header
// signed for the same audience and subject within vapidTokenReuse before
// now is reused, so a burst of pushes to one push service signs once.
func (k *vapidKey) authorization(endpoint, subject string, now time.Time) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" {
		return "", errors.New("push endpoint is not an absolute URL")
	}
	audience := origin(parsed)
	key := audience + " " + subject
	k.mu.Lock()
	defer k.mu.Unlock()
	if token, ok := k.tokens[key]; ok && !now.Before(token.signed) && now.Sub(token.signed) < vapidTokenReuse {
		return token.header, nil
	}
	header, err := k.sign(audience, subject, now)
	if err != nil {
		return "", err
	}
	if k.tokens == nil || len(k.tokens) >= maxVAPIDTokens {
		for stale, token := range k.tokens {
			if now.Sub(token.signed) >= vapidTokenReuse || now.Before(token.signed) {
				delete(k.tokens, stale)
			}
		}
		if k.tokens == nil || len(k.tokens) >= maxVAPIDTokens {
			k.tokens = make(map[string]vapidToken)
		}
	}
	k.tokens[key] = vapidToken{header: header, signed: now}
	return header, nil
}

// sign signs a VAPID Authorization header for an audience, valid for
// vapidTokenLifetime from now.
func (k *vapidKey) sign(audience, subject string, now time.Time) (string, error) {
	claims := map[string]any{
		"aud": audience,
		"exp": now.Add(vapidTokenLifetime).Unix(),
	}
	if subject != "" {
		claims["sub"] = subject
	}
	unsigned := encodeBase64URL([]byte(`{"typ":"JWT","alg":"ES256"}`)) + "." + encodeBase64URL(encodeJSON(claims))
	digest := sha256.Sum256([]byte(unsigned))
	r, s, err := ecdsa.Sign(rand.Reader, k.private, digest[:])
	if err != nil {
		return "", err
	}
	// JWS ES256 signs as r || s, each 32 bytes (RFC 7518 section 3.4).
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return "vapid t=" + unsigned + "." + encodeBase64URL(signature) + ", k=" + k.public, nil
}

// origin is a URL's origin as push services compare a VAPID audience
// (RFC 6454): the scheme and host lowercased, without a default port.
func origin(u *url.URL) string {
	scheme, host := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" && !(scheme == "https" && port == "443") && !(scheme == "http" && port == "80") {
		host += ":" + port
	}
	return scheme + "://" + host
}

// pushTTL is how long a push service keeps an undelivered webpush message,
// in seconds (RFC 8030 section 5.2): a day, after which a chat
// notification is stale.
var pushTTL = strconv.Itoa(int((24 * time.Hour) / time.Second))
