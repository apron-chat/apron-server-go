package passkey

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/apron-chat/apron-server-go/internal/authn"
)

const origin = "http://localhost:5173"

// authenticator is a software authenticator with real ES256 credentials.
type authenticator struct {
	key    *ecdsa.PrivateKey
	id     []byte
	handle string
}

func b64(value []byte) string { return base64.RawURLEncoding.EncodeToString(value) }

func clientData(kind, challenge string) []byte {
	data, _ := json.Marshal(map[string]any{"type": kind, "challenge": challenge, "origin": origin, "crossOrigin": false})
	return data
}

// publicKey reads the public_key of a begin's reply.
func publicKey(t *testing.T, result authn.Result) (string, map[string]any) {
	t.Helper()
	reply, ok := result.(*authn.Reply)
	if !ok {
		t.Fatalf("begin = %#v", result)
	}
	encoded, err := json.Marshal(reply.Result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		ChallengeID string         `json:"challenge_id"`
		PublicKey   map[string]any `json:"public_key"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded.ChallengeID, decoded.PublicKey
}

func (a *authenticator) registration(t *testing.T, key map[string]any) map[string]any {
	t.Helper()
	a.handle = key["user"].(map[string]any)["id"].(string)
	cose, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: a.key.X.FillBytes(make([]byte, 32)), -3: a.key.Y.FillBytes(make([]byte, 32))})
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("localhost"))
	data := append(hash[:], 0x45, 0, 0, 0, 0)
	data = append(data, make([]byte, 16)...)
	data = binary.BigEndian.AppendUint16(data, uint16(len(a.id)))
	data = append(data, a.id...)
	data = append(data, cose...)
	attestation, err := cbor.Marshal(map[string]any{"fmt": "none", "authData": data, "attStmt": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"id": b64(a.id), "rawId": b64(a.id), "type": "public-key",
		"response": map[string]any{"clientDataJSON": b64(clientData("webauthn.create", key["challenge"].(string))), "attestationObject": b64(attestation)},
	}
}

func (a *authenticator) assertion(t *testing.T, key map[string]any, counter uint32) map[string]any {
	t.Helper()
	client := clientData("webauthn.get", key["challenge"].(string))
	hash := sha256.Sum256([]byte("localhost"))
	data := append(hash[:], 0x05)
	data = binary.BigEndian.AppendUint32(data, counter)
	clientHash := sha256.Sum256(client)
	signed := sha256.Sum256(append(append([]byte{}, data...), clientHash[:]...))
	signature, err := ecdsa.SignASN1(rand.Reader, a.key, signed[:])
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"id": b64(a.id), "rawId": b64(a.id), "type": "public-key",
		"response": map[string]any{"clientDataJSON": b64(client), "authenticatorData": b64(data), "signature": b64(signature), "userHandle": a.handle},
	}
}

func request(t *testing.T, state *authn.State, account *authn.Account, params map[string]any) *authn.Request {
	t.Helper()
	encoded := make(map[string]jsontext.Value, len(params))
	for name, value := range params {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		encoded[name] = raw
	}
	return &authn.Request{Params: encoded, Account: account, Origin: origin, Now: time.Now(), State: state}
}

// verifiedLogin runs a login up to the proof, which it does not commit.
func verifiedLogin(t *testing.T, s *Scheme, a *authenticator, counter uint32) *authn.SignIn {
	t.Helper()
	state := new(authn.State)
	begun, err := s.Auth(context.Background(), request(t, state, nil, map[string]any{"action": "login", "step": "begin"}))
	if err != nil {
		t.Fatal(err)
	}
	challenge, key := publicKey(t, begun)
	finished, err := s.Auth(context.Background(), request(t, state, nil, map[string]any{
		"action": "login", "step": "finish", "challenge_id": challenge, "credential": a.assertion(t, key, counter),
	}))
	if err != nil {
		t.Fatal(err)
	}
	return finished.(*authn.SignIn)
}

// Logins are verified outside any lock, so two may be verified against the
// same counter: of those, only the first to commit signs in, and the
// counter kept never goes back.
func TestCommitKeepsTheSignCounterAdvancing(t *testing.T) {
	w, err := webauthn.New(&webauthn.Config{RPID: "localhost", RPDisplayName: "Apron", RPOrigins: []string{origin}})
	if err != nil {
		t.Fatal(err)
	}
	s := New(w)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	a := &authenticator{key: key, id: []byte(rand.Text())}
	account := &authn.Account{UserID: "ada", Name: "Ada", Guest: true}
	state := new(authn.State)
	begun, err := s.Auth(context.Background(), request(t, state, account, map[string]any{"action": "register", "step": "begin"}))
	if err != nil {
		t.Fatal(err)
	}
	challenge, options := publicKey(t, begun)
	registered, err := s.Auth(context.Background(), request(t, state, account, map[string]any{
		"action": "register", "step": "finish", "challenge_id": challenge, "credential": a.registration(t, options),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registered.(*authn.Add).Commit(); err != nil {
		t.Fatal(err)
	}

	// A clone: two logins with the same counter.
	first, second := verifiedLogin(t, s, a, 1), verifiedLogin(t, s, a, 1)
	if _, err := first.Commit(); err != nil {
		t.Fatalf("first login: %v", err)
	}
	if _, err := second.Commit(); err == nil {
		t.Fatal("a second login with the same counter committed")
	}

	// Two logins committed out of order: the later counter stays.
	earlier, later := verifiedLogin(t, s, a, 2), verifiedLogin(t, s, a, 3)
	if _, err := later.Commit(); err != nil {
		t.Fatalf("later login: %v", err)
	}
	if _, err := earlier.Commit(); err == nil {
		t.Fatal("an earlier counter committed after a later one")
	}
	record, err := verifiedLogin(t, s, a, 4).Commit()
	if err != nil {
		t.Fatal(err)
	}
	var kept holder
	if err := json.Unmarshal(record, &kept); err != nil {
		t.Fatal(err)
	}
	if count := kept.Credentials[0].Authenticator.SignCount; count != 4 {
		t.Fatalf("kept counter %d, want 4", count)
	}
}
