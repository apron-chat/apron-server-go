package aprond

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/aprond/internal/sessions"
	"github.com/apron-chat/apron-server-go/internal/auth/passkey"
	"github.com/apron-chat/apron-server-go/store"
	"github.com/coder/websocket"
	"github.com/fxamacker/cbor/v2"
	"github.com/go-webauthn/webauthn/webauthn"
)

const testPasskeyOrigin = "http://localhost:5173"

// testWebAuthn is the relying party the passkey tests use, on localhost.
func testWebAuthn(t *testing.T) *webauthn.WebAuthn {
	t.Helper()
	w, err := webauthn.New(&webauthn.Config{RPID: "localhost", RPDisplayName: "Apron", RPOrigins: []string{testPasskeyOrigin}})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func passkeyTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	config := DefaultConfig()
	config.WebAuthn = testWebAuthn(t)
	return newTestServer(t, config)
}

func passkeyCall(t *testing.T, c *testClient, id, action, step string, extra map[string]any) map[string]any {
	t.Helper()
	params := map[string]any{"scheme": "webauthn", "action": action}
	if action == "token" {
		params = map[string]any{"scheme": "token"}
	} else if step != "" {
		params["step"] = step
		if step == "finish" && c.passkeyChallenge != "" {
			params["challenge_id"] = c.passkeyChallenge
		}
	}
	for key, value := range extra {
		params[key] = value
	}
	c.write(t, map[string]any{"method": "auth", "id": id, "params": params})
	frame := c.read(t)
	if frame["id"] != id {
		t.Fatalf("unexpected response: %#v", frame)
	}
	if action != "token" && step == "begin" {
		if result, ok := frame["result"].(map[string]any); ok {
			if challengeID, ok := result["challenge_id"].(string); ok {
				c.passkeyChallenge = challengeID
			}
		}
	}
	return frame
}

func passkeyResult(t *testing.T, frame map[string]any) map[string]any {
	t.Helper()
	result, ok := frame["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected success: %#v", frame)
	}
	return result
}

func passkeyPublicKey(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	publicKey, ok := result["public_key"].(map[string]any)
	if !ok {
		t.Fatalf("passkey result has no public_key: %#v", result)
	}
	return publicKey
}

func passkeyDenied(t *testing.T, frame map[string]any) {
	t.Helper()
	err, ok := frame["error"].(map[string]any)
	if !ok || err["code"] != float64(CodeDenied) {
		t.Fatalf("expected denial: %#v", frame)
	}
}

// A software authenticator produces real ES256 credentials and signatures;
// the server exercises the same verifier as browser-created credentials.
type testAuthenticator struct {
	key     *ecdsa.PrivateKey
	id      []byte
	handle  string
	counter uint32
	// clientData adds to or replaces the fields of the client data it
	// signs, such as crossOrigin.
	clientData map[string]any
}

func newTestAuthenticator(t *testing.T) *testAuthenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &testAuthenticator{key: key, id: []byte(rand.Text())}
}

func b64(value []byte) string { return base64.RawURLEncoding.EncodeToString(value) }

func clientData(kind, challenge, origin string, extra map[string]any) []byte {
	fields := map[string]any{"type": kind, "challenge": challenge, "origin": origin, "crossOrigin": false}
	for key, value := range extra {
		fields[key] = value
	}
	data, _ := json.Marshal(fields)
	return data
}

func (a *testAuthenticator) registration(t *testing.T, options map[string]any, origin string) map[string]any {
	t.Helper()
	publicKey := passkeyPublicKey(t, options)
	user, ok := publicKey["user"].(map[string]any)
	if !ok {
		t.Fatalf("registration public key: %#v", publicKey)
	}
	a.handle = user["id"].(string)
	cose, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: a.key.X.FillBytes(make([]byte, 32)), -3: a.key.Y.FillBytes(make([]byte, 32))})
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("localhost"))
	data := append(hash[:], 0x45, 0, 0, 0, 0) // UP, UV, attested credential data.
	data = append(data, make([]byte, 16)...)
	data = binary.BigEndian.AppendUint16(data, uint16(len(a.id)))
	data = append(data, a.id...)
	data = append(data, cose...)
	attestation, err := cbor.Marshal(map[string]any{"fmt": "none", "authData": data, "attStmt": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"id": b64(a.id), "rawId": b64(a.id), "type": "public-key", "clientExtensionResults": map[string]any{},
		"response": map[string]any{"clientDataJSON": b64(clientData("webauthn.create", publicKey["challenge"].(string), origin, a.clientData)), "attestationObject": b64(attestation)},
	}
}

func (a *testAuthenticator) assertion(t *testing.T, options map[string]any, origin, rpID string, flags byte) map[string]any {
	t.Helper()
	a.counter++
	publicKey := passkeyPublicKey(t, options)
	client := clientData("webauthn.get", publicKey["challenge"].(string), origin, a.clientData)
	hash := sha256.Sum256([]byte(rpID))
	data := append(hash[:], flags)
	data = binary.BigEndian.AppendUint32(data, a.counter)
	clientHash := sha256.Sum256(client)
	signed := sha256.Sum256(append(append([]byte{}, data...), clientHash[:]...))
	signature, err := ecdsa.SignASN1(rand.Reader, a.key, signed[:])
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"id": b64(a.id), "rawId": b64(a.id), "type": "public-key", "clientExtensionResults": map[string]any{},
		"response": map[string]any{"clientDataJSON": b64(client), "authenticatorData": b64(data), "signature": b64(signature), "userHandle": a.handle},
	}
}

func registerTestPasskey(t *testing.T, c *testClient, a *testAuthenticator) map[string]any {
	t.Helper()
	guest := guestAuth(t, c)["you"].(map[string]any)["user_id"]
	options := passkeyResult(t, passkeyCall(t, c, "register-start", "register", "begin", nil))
	publicKey := passkeyPublicKey(t, options)
	selection, ok := publicKey["authenticatorSelection"].(map[string]any)
	if !ok || selection["residentKey"] != "required" || selection["userVerification"] != "required" {
		t.Fatalf("registration did not require resident key and user verification: %#v", publicKey)
	}
	result := passkeyResult(t, passkeyCall(t, c, "register-end", "register", "finish", map[string]any{"credential": a.registration(t, options, testPasskeyOrigin)}))
	if result["you"].(map[string]any)["user_id"] != guest {
		t.Fatal("registration changed guest identity")
	}
	return result
}

func TestPasskeyRegistrationLoginAndSession(t *testing.T) {
	app, httpServer := passkeyTestServer(t)
	owner, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	a := newTestAuthenticator(t)
	registered := registerTestPasskey(t, owner, a)
	identity := registered["you"].(map[string]any)["user_id"]
	other, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	options := passkeyResult(t, passkeyCall(t, other, "begin", "login", "begin", nil))
	publicKey := passkeyPublicKey(t, options)
	if _, ok := publicKey["allowCredentials"]; ok {
		t.Fatalf("discoverable login unexpectedly constrained credentials: %#v", publicKey)
	}
	if publicKey["userVerification"] != "required" {
		t.Fatalf("login did not require user verification: %#v", publicKey)
	}
	credential := a.assertion(t, options, testPasskeyOrigin, "localhost", 0x05)
	finish := map[string]any{"credential": credential}
	loggedIn := passkeyResult(t, passkeyCall(t, other, "finish", "login", "finish", finish))
	if loggedIn["you"].(map[string]any)["user_id"] != identity {
		t.Fatal("login changed identity")
	}
	// Even the identical request ID cannot replay a completed ceremony.
	passkeyDenied(t, passkeyCall(t, other, "finish", "login", "finish", finish))
	resumed, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	token := loggedIn["token"].(string)
	result := passkeyResult(t, passkeyCall(t, resumed, "resume", "token", "", map[string]any{"token": token}))
	if result["you"].(map[string]any)["user_id"] != identity {
		t.Fatal("resume changed identity")
	}
	// Token sign-out is local to the client: drop the connection and reconnect.
	// The server retains the token until its normal expiry.
	_ = resumed.ws.Close(websocket.StatusNormalClosure, "signed out")
	reconnected, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	result = passkeyResult(t, passkeyCall(t, reconnected, "resume", "token", "", map[string]any{"token": token}))
	if result["you"].(map[string]any)["user_id"] != identity {
		t.Fatal("reconnect changed identity")
	}
	// A resume renews the session: a token close to expiry is good for a full
	// lifetime again after use.
	app.mu.Lock()
	for key, session := range app.sessions.All() {
		session.Expires = time.Now().Add(time.Minute)
		app.sessions.Put(key, session)
	}
	app.mu.Unlock()
	renewed, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	result = passkeyResult(t, passkeyCall(t, renewed, "resume", "token", "", map[string]any{"token": token}))
	if result["token"] != token {
		t.Fatalf("resume did not return the presented token: %#v", result["token"])
	}
	app.mu.Lock()
	renewed2, _ := app.sessions.Get(sha256.Sum256([]byte(token)))
	renewedExpiry := renewed2.Expires
	app.mu.Unlock()
	if renewedExpiry.Before(time.Now().Add(sessions.Lifetime - time.Minute)) {
		t.Fatalf("resume did not renew the session: expires %v", renewedExpiry)
	}
	app.mu.Lock()
	for key, session := range app.sessions.All() {
		session.Expires = time.Now().Add(-time.Second)
		app.sessions.Put(key, session)
	}
	app.mu.Unlock()
	passkeyDenied(t, passkeyCall(t, reconnected, "expired", "token", "", map[string]any{"token": registered["token"]}))
}

// relatedPasskeyOrigin is a frontend outside the RP ID's domain, which
// browsers allow through the related origins document.
const relatedPasskeyOrigin = "https://web.apron.chat"

func relatedOriginsTestServer(t *testing.T, staticDir string) (*Server, *httptest.Server) {
	t.Helper()
	config := DefaultConfig()
	config.AllowAnyOrigin = true
	config.StaticDir = staticDir
	w, err := webauthn.New(&webauthn.Config{RPID: "localhost", RPDisplayName: "Apron", RPOrigins: []string{testPasskeyOrigin, relatedPasskeyOrigin}})
	if err != nil {
		t.Fatalf("an origin outside the RP ID was refused: %v", err)
	}
	config.WebAuthn = w
	return newTestServer(t, config)
}

func TestRelatedOriginsDocument(t *testing.T) {
	// A static directory cannot shadow the document.
	dir := t.TempDir()
	path := filepath.Join(dir, ".well-known", "webauthn")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(`{"origins":["https://evil.example"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, httpServer := relatedOriginsTestServer(t, dir)
	url := httpServer.URL + "/.well-known/webauthn"
	status, header, body := httpDo(t, http.MethodGet, url, nil, "")
	if status != http.StatusOK || header.Get("Content-Type") != "application/json" || header.Get("Cache-Control") == "" {
		t.Fatalf("GET: %d %v %s", status, header, body)
	}
	var document struct {
		Origins []string `json:"origins"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	if want := []string{testPasskeyOrigin, relatedPasskeyOrigin}; !reflect.DeepEqual(document.Origins, want) {
		t.Fatalf("origins = %v, want %v", document.Origins, want)
	}
	if status, _, body := httpDo(t, http.MethodHead, url, nil, ""); status != http.StatusOK || len(body) != 0 {
		t.Fatalf("HEAD: %d %q", status, body)
	}
	if status, header, _ := httpDo(t, http.MethodPost, url, nil, ""); status != http.StatusMethodNotAllowed || header.Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST: %d %v", status, header)
	}

	// Without passkeys there is nothing to declare.
	config := DefaultConfig()
	config.StaticDir = dir
	_, plain := newTestServer(t, config)
	if status, _, body := httpDo(t, http.MethodGet, plain.URL+"/.well-known/webauthn", nil, ""); status != http.StatusNotFound {
		t.Fatalf("GET without passkeys: %d %s", status, body)
	}
}

// go-webauthn checks a ceremony's origin only against the configured list,
// so a related origin outside the RP ID's domain registers and signs in, and
// a passkey made on one frontend works from the other.
func TestPasskeyFromRelatedOrigin(t *testing.T) {
	_, httpServer := relatedOriginsTestServer(t, "")
	owner, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	a := newTestAuthenticator(t)
	identity := registerTestPasskey(t, owner, a)["you"].(map[string]any)["user_id"]
	related, _ := dialOrigin(t, httpServer, relatedPasskeyOrigin)
	options := passkeyResult(t, passkeyCall(t, related, "begin", "login", "begin", nil))
	finish := map[string]any{"credential": a.assertion(t, options, relatedPasskeyOrigin, "localhost", 0x05)}
	if result := passkeyResult(t, passkeyCall(t, related, "finish", "login", "finish", finish)); result["you"].(map[string]any)["user_id"] != identity {
		t.Fatal("login from the related origin changed identity")
	}

	b := newTestAuthenticator(t)
	fresh, _ := dialOrigin(t, httpServer, relatedPasskeyOrigin)
	guestAuth(t, fresh)
	options = passkeyResult(t, passkeyCall(t, fresh, "register-start", "register", "begin", nil))
	passkeyResult(t, passkeyCall(t, fresh, "register-end", "register", "finish", map[string]any{"credential": b.registration(t, options, relatedPasskeyOrigin)}))
}

func TestRegistrationTakesRequestedName(t *testing.T) {
	_, httpServer := passkeyTestServer(t)
	c, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	guest := guestAuth(t, c)["you"].(map[string]any)
	userOf := func(options map[string]any) map[string]any {
		t.Helper()
		user, ok := passkeyPublicKey(t, options)["user"].(map[string]any)
		if !ok {
			t.Fatalf("creation options have no user: %#v", options)
		}
		return user
	}

	// A begin that is superseded changes nothing: the name applies only to the
	// registration that finishes.
	abandoned := passkeyResult(t, passkeyCall(t, c, "begin-abandoned", "register", "begin", map[string]any{"name": "Abandoned"}))
	if userOf(abandoned)["name"] != "Abandoned" {
		t.Fatalf("passkey not labelled with the requested name: %#v", userOf(abandoned))
	}
	options := passkeyResult(t, passkeyCall(t, c, "begin", "register", "begin", map[string]any{"name": "  Ada   Lovelace "}))
	if user := userOf(options); user["name"] != "Ada Lovelace" || user["displayName"] != "Ada Lovelace" {
		t.Fatalf("passkey not labelled with the normalized requested name: %#v", user)
	}
	a := newTestAuthenticator(t)
	result := passkeyResult(t, passkeyCall(t, c, "finish", "register", "finish", map[string]any{"credential": a.registration(t, options, testPasskeyOrigin)}))
	you := result["you"].(map[string]any)
	if you["user_id"] != guest["user_id"] || you["name"] != "Ada Lovelace" {
		t.Fatalf("registration did not keep the guest's identity with the requested name: %#v", you)
	}

	// Adding a passkey to an account keeps the account's name, whatever is asked.
	other, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	passkeyResult(t, passkeyCall(t, other, "resume", "token", "", map[string]any{"token": result["token"]}))
	other.drain(t)
	// Adding another passkey takes a recent sign-in with one.
	loginTestPasskey(t, other, a)
	adding := passkeyResult(t, passkeyCall(t, other, "begin-add", "register", "begin", map[string]any{"name": "Someone else"}))
	if userOf(adding)["name"] != "Ada Lovelace" {
		t.Fatalf("added passkey not labelled with the account's name: %#v", userOf(adding))
	}
	added := passkeyResult(t, passkeyCall(t, other, "finish-add", "register", "finish", map[string]any{
		"credential": newTestAuthenticator(t).registration(t, adding, testPasskeyOrigin),
	}))
	if name := added["you"].(map[string]any)["name"]; name != "Ada Lovelace" {
		t.Fatalf("adding a passkey renamed the account: %#v", name)
	}
}

// Where guests are allowed, a registration on a connection that is not
// signed in makes a new account as it finishes (§4.10), as an invitation
// does, without signing in as a guest first: the begin makes nothing, and
// the finish makes the account with the name and user_id the begin asked
// for, by the rules of any new account, joins it to the default room, and
// signs in with a token. The account is stored, and its passkey signs in to
// it after a restart. A guest's registration still makes the guest an
// account.
func TestFreshRegistrationMakesAnAccount(t *testing.T) {
	kept := store.NewMemory()
	configure := func(config *Config) { config.Roles = map[string][]string{"admin": {"granted"}} }
	app, httpServer, stop := startWith(t, kept, t.TempDir(), configure)
	c, frame := dialOrigin(t, httpServer, testPasskeyOrigin)
	if params := frame["params"].(map[string]any); !reflect.DeepEqual(params["signup"], []any{"webauthn", "guest"}) {
		t.Fatalf("signup: %#v", params["signup"])
	}
	options := passkeyResult(t, passkeyCall(t, c, "begin", "register", "begin", map[string]any{"name": "  Ada   Lovelace ", "user_id": "ada"}))
	if user := passkeyPublicKey(t, options)["user"].(map[string]any); user["name"] != "Ada Lovelace" || user["displayName"] != "Ada Lovelace" {
		t.Fatalf("passkey not labelled with the normalized requested name: %#v", user)
	}
	app.mu.RLock()
	users := app.users.Len()
	app.mu.RUnlock()
	if users != 0 {
		t.Fatal("the begin made a user")
	}
	// A finish presenting an invitation the begin did not is another
	// registration, and uses the ceremony up.
	invitation, err := app.Invite(context.Background(), InvitationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a := newTestAuthenticator(t)
	passkeyDenied(t, passkeyCall(t, c, "changed", "register", "finish", map[string]any{"invitation": invitation.Token, "credential": a.registration(t, options, testPasskeyOrigin)}))
	options = passkeyResult(t, passkeyCall(t, c, "begin-again", "register", "begin", map[string]any{"name": "  Ada   Lovelace ", "user_id": "ada"}))
	result := passkeyResult(t, passkeyCall(t, c, "finish", "register", "finish", map[string]any{"credential": a.registration(t, options, testPasskeyOrigin)}))
	you := result["you"].(map[string]any)
	if you["user_id"] != "ada" || you["name"] != "Ada Lovelace" || result["token"] == nil || !reflect.DeepEqual(you["roles"], []any{}) {
		t.Fatalf("new account: %#v", result)
	}
	expectJoinedDefault(t, c, "ada")
	if len(app.Invitations()) != 1 {
		t.Fatal("a registration without an invitation used one")
	}

	// A user_id the server would not assign, or that is taken, or granted
	// a role, gets user_<n>, as any new account does.
	for i, requested := range []string{"guest_7", "ada", "granted"} {
		other, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
		options := passkeyResult(t, passkeyCall(t, other, "begin", "register", "begin", map[string]any{"user_id": requested}))
		result := passkeyResult(t, passkeyCall(t, other, "finish", "register", "finish", map[string]any{"credential": newTestAuthenticator(t).registration(t, options, testPasskeyOrigin)}))
		if id := result["you"].(map[string]any)["user_id"]; id != fmt.Sprintf("user_%d", i+1) {
			t.Fatalf("requested %q, assigned %v", requested, id)
		}
	}

	// The token resumes from the ceremony's origin.
	resumed, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	if you := passkeyResult(t, passkeyCall(t, resumed, "resume", "token", "", map[string]any{"token": result["token"]}))["you"].(map[string]any); you["user_id"] != "ada" {
		t.Fatalf("resumed as %#v", you)
	}

	// A guest's registration still adds the passkey to the guest, which
	// becomes an account.
	guest, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	registerTestPasskey(t, guest, newTestAuthenticator(t))

	// The account outlives a restart, and its passkey signs in to it.
	if err := app.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	stop()
	_, httpServer, _ = startWith(t, kept, t.TempDir(), configure)
	again, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	if you := loginTestPasskey(t, again, a)["you"].(map[string]any); you["user_id"] != "ada" || you["name"] != "Ada Lovelace" {
		t.Fatalf("signed in after a restart as %#v", you)
	}
}

// Without guests, a registration makes an account only with an invitation:
// on a connection that is not signed in, a registration without one is
// refused, as before.
func TestFreshRegistrationWithoutGuests(t *testing.T) {
	config := DefaultConfig()
	config.WebAuthn = testWebAuthn(t)
	config.DisableGuests = true
	app, httpServer := newTestServer(t, config)
	c, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	passkeyDenied(t, passkeyCall(t, c, "bare", "register", "begin", map[string]any{"name": "Ada"}))
	invitation, err := app.Invite(context.Background(), InvitationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a := newTestAuthenticator(t)
	options := passkeyResult(t, passkeyCall(t, c, "begin", "register", "begin", map[string]any{"invitation": invitation.Token, "name": "Ada"}))
	// A finish without the invitation is another registration.
	passkeyDenied(t, passkeyCall(t, c, "without", "register", "finish", map[string]any{"credential": a.registration(t, options, testPasskeyOrigin)}))
	result := passkeyResult(t, registerInvited(t, c, a, invitation.Token, map[string]any{"name": "Ada"}))
	if you := result["you"].(map[string]any); you["name"] != "Ada" || result["token"] == nil {
		t.Fatalf("invited account: %#v", result)
	}
}

// With GuestFirstPasskeys, a registration makes an account only of a
// signed-in guest, or with an invitation, even where guests are allowed:
// signup lists guest but not webauthn, which stays in auth, so clients sign
// in as a guest first, and a registration on a connection that is not
// signed in is refused, saying so.
func TestGuestFirstPasskeys(t *testing.T) {
	config := DefaultConfig()
	config.WebAuthn = testWebAuthn(t)
	config.GuestFirstPasskeys = true
	app, httpServer := newTestServer(t, config)
	c, frame := dialOrigin(t, httpServer, testPasskeyOrigin)
	if params := frame["params"].(map[string]any); !reflect.DeepEqual(params["auth"], []any{"webauthn", "token", "guest"}) || !reflect.DeepEqual(params["signup"], []any{"guest"}) {
		t.Fatalf("auth: %#v, signup: %#v", params["auth"], params["signup"])
	}
	refusal := passkeyCall(t, c, "bare", "register", "begin", map[string]any{"name": "Ada"})
	passkeyDenied(t, refusal)
	if message := refusal["error"].(map[string]any)["message"]; message != "Sign in as a guest, or present an invitation, before adding a passkey" {
		t.Fatalf("refusal: %#v", refusal)
	}
	// A guest registers, and so does an invited connection.
	registerTestPasskey(t, c, newTestAuthenticator(t))
	invitation, err := app.Invite(context.Background(), InvitationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	invited, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	passkeyResult(t, registerInvited(t, invited, newTestAuthenticator(t), invitation.Token, nil))
}

func TestAddingPasskeyPreservesStoredName(t *testing.T) {
	for _, renameDuringRegistration := range []bool{false, true} {
		name := "rename before registration"
		if renameDuringRegistration {
			name = "rename during registration"
		}
		t.Run(name, func(t *testing.T) {
			_, httpServer := passkeyTestServer(t)
			owner, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
			original := newTestAuthenticator(t)
			registered := registerTestPasskey(t, owner, original)
			other, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
			passkeyResult(t, passkeyCall(t, other, "resume", "token", "", map[string]any{"token": registered["token"]}))
			other.drain(t)
			// Adding another passkey takes a recent sign-in with one.
			loginTestPasskey(t, other, original)
			rename := func() {
				owner.write(t, map[string]any{"method": "me", "id": "rename", "params": map[string]any{"name": "Updated name"}})
				result := passkeyResult(t, owner.read(t))
				if result["you"].(map[string]any)["name"] != "Updated name" {
					t.Fatalf("rename was not accepted: %#v", result)
				}
				// The same user's other connection learns of the change.
				if you := other.notification(t, "user")["you"].(map[string]any); you["name"] != "Updated name" {
					t.Fatalf("other connection's user notification: %#v", you)
				}
			}
			if !renameDuringRegistration {
				rename()
			}
			options := passkeyResult(t, passkeyCall(t, other, "begin", "register", "begin", nil))
			if renameDuringRegistration {
				rename()
			}
			additional := newTestAuthenticator(t)
			result := passkeyResult(t, passkeyCall(t, other, "finish", "register", "finish", map[string]any{
				"credential": additional.registration(t, options, testPasskeyOrigin),
			}))
			assertIdentity := func(result map[string]any) {
				t.Helper()
				you := result["you"].(map[string]any)
				if you["user_id"] != registered["you"].(map[string]any)["user_id"] || you["name"] != "Updated name" {
					t.Fatalf("registration lost the stored identity: %#v", you)
				}
			}
			assertIdentity(result)
			// Both credentials must restore the accepted name on fresh connections.
			for _, authenticator := range []*testAuthenticator{original, additional} {
				fresh, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
				options := passkeyResult(t, passkeyCall(t, fresh, "login-begin", "login", "begin", nil))
				result := passkeyResult(t, passkeyCall(t, fresh, "login-finish", "login", "finish", map[string]any{
					"credential": authenticator.assertion(t, options, testPasskeyOrigin, "localhost", 0x05),
				}))
				assertIdentity(result)
			}
		})
	}
}

func TestPasskeyRejectsInvalidProofs(t *testing.T) {
	app, httpServer := passkeyTestServer(t)
	a := newTestAuthenticator(t)
	owner, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	registerTestPasskey(t, owner, a)
	for _, test := range []struct {
		name, origin, rpID string
		flags              byte
	}{
		{"wrong origin", "https://evil.example", "localhost", 0x05},
		{"wrong RP", testPasskeyOrigin, "evil.example", 0x05},
		{"no user verification", testPasskeyOrigin, "localhost", 0x01},
		{"no user presence", testPasskeyOrigin, "localhost", 0x04},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
			options := passkeyResult(t, passkeyCall(t, c, "begin", "login", "begin", nil))
			proof := a.assertion(t, options, test.origin, test.rpID, test.flags)
			passkeyDenied(t, passkeyCall(t, c, "finish", "login", "finish", map[string]any{"credential": proof}))
			passkeyDenied(t, passkeyCall(t, c, "retry", "login", "finish", map[string]any{"credential": proof}))
		})
	}
	c, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	first := passkeyResult(t, passkeyCall(t, c, "first", "login", "begin", nil))
	second := passkeyResult(t, passkeyCall(t, c, "second", "login", "begin", nil))
	proof := a.assertion(t, first, testPasskeyOrigin, "localhost", 0x05)
	passkeyDenied(t, passkeyCall(t, c, "superseded", "login", "finish", map[string]any{
		"challenge_id": first["challenge_id"], "credential": proof,
	}))
	current := a.assertion(t, second, testPasskeyOrigin, "localhost", 0x05)
	passkeyResult(t, passkeyCall(t, c, "current", "login", "finish", map[string]any{
		"challenge_id": second["challenge_id"], "credential": current,
	}))
	other, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	passkeyDenied(t, passkeyCall(t, other, "cross-connection", "login", "finish", map[string]any{
		"challenge_id": first["challenge_id"], "credential": proof,
	}))
	options := passkeyResult(t, passkeyCall(t, c, "expire", "login", "begin", nil))
	app.mu.Lock()
	for client := range app.clients {
		if state := client.authStates[passkey.Name]; state != nil {
			passkey.Expire(state)
		}
	}
	app.mu.Unlock()
	passkeyDenied(t, passkeyCall(t, c, "expired", "login", "finish", map[string]any{"credential": a.assertion(t, options, testPasskeyOrigin, "localhost", 0x05)}))
	wrongOrigin, _ := dialOrigin(t, httpServer, "http://localhost:9999")
	passkeyDenied(t, passkeyCall(t, wrongOrigin, "origin", "login", "begin", nil))
}

// A refusal for an origin the RP does not accept names the origin, and the
// passkey page's, from the request's Host, when that is not accepted
// either.
func TestPasskeyOriginRefusals(t *testing.T) {
	_, httpServer := passkeyTestServer(t)
	page := httpServer.URL
	refusal := func(frame map[string]any) string {
		t.Helper()
		passkeyDenied(t, frame)
		return frame["error"].(map[string]any)["message"].(string)
	}
	wrongOrigin, _ := dialOrigin(t, httpServer, "http://localhost:9999")
	if got, want := refusal(passkeyCall(t, wrongOrigin, "b", "login", "begin", nil)), `Neither Origin "http://localhost:9999" nor the passkey page's origin "`+page+`" is configured for passkeys`; got != want {
		t.Errorf("begin: %q, want %q", got, want)
	}
	if got, want := refusal(passkeyCall(t, wrongOrigin, "f", "login", "finish", map[string]any{"challenge_id": "x", "credential": map[string]any{}})), `Origin "http://localhost:9999" is not configured for passkeys`; got != want {
		t.Errorf("finish: %q, want %q", got, want)
	}
	none, _ := dialRaw(t, httpServer)
	if got, want := refusal(passkeyCall(t, none, "b", "login", "begin", nil)), `This connection sent no Origin, and the passkey page's origin "`+page+`" is not configured for passkeys`; got != want {
		t.Errorf("begin without Origin: %q, want %q", got, want)
	}
	if got, want := refusal(passkeyCall(t, none, "f", "login", "finish", map[string]any{"challenge_id": "x", "credential": map[string]any{}})), "This connection sent no Origin, which passkeys need"; got != want {
		t.Errorf("finish without Origin: %q, want %q", got, want)
	}
}

// A native app connects with the origin its platform writes in client
// data, which has no host, once the RP accepts it (§4.10), and runs the
// ceremony with it. Other origins, web ones the RP accepts among them,
// still need an origin pattern.
func TestNativeAppOrigin(t *testing.T) {
	const android = "android:apk-key-hash:AbC-_123"
	config := DefaultConfig()
	w, err := webauthn.New(&webauthn.Config{RPID: "localhost", RPDisplayName: "Apron", RPOrigins: []string{testPasskeyOrigin, "https://web.example", android}})
	if err != nil {
		t.Fatal(err)
	}
	config.WebAuthn = w
	_, httpServer := newTestServer(t, config)
	app, _ := dialOrigin(t, httpServer, android)
	guestAuth(t, app)
	a := newTestAuthenticator(t)
	options := passkeyResult(t, passkeyCall(t, app, "rb", "register", "begin", nil))
	passkeyResult(t, passkeyCall(t, app, "rf", "register", "finish", map[string]any{"credential": a.registration(t, options, android)}))
	// The ceremony is bound to the app's origin: an assertion made on
	// another origin the RP accepts fails.
	app, _ = dialOrigin(t, httpServer, android)
	options = passkeyResult(t, passkeyCall(t, app, "lb", "login", "begin", nil))
	passkeyDenied(t, passkeyCall(t, app, "lf", "login", "finish", map[string]any{"credential": a.assertion(t, options, testPasskeyOrigin, "localhost", 0x05)}))
	options = passkeyResult(t, passkeyCall(t, app, "lb", "login", "begin", nil))
	passkeyResult(t, passkeyCall(t, app, "lf", "login", "finish", map[string]any{"credential": a.assertion(t, options, android, "localhost", 0x05)}))
	for _, origin := range []string{"android:apk-key-hash:other", "android:apk-key-hash:abc-_123", "https://web.example", "https://evil.example", "null"} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		ws, _, err := websocket.Dial(ctx, wsURL(httpServer), &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {origin}}})
		cancel()
		if err == nil {
			_ = ws.CloseNow()
			t.Errorf("accepted Origin %q", origin)
		}
	}
}

func TestPasskeyRejectsInvalidRegistration(t *testing.T) {
	app, httpServer := passkeyTestServer(t)
	c, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	guestAuth(t, c)
	a := newTestAuthenticator(t)
	options := passkeyResult(t, passkeyCall(t, c, "begin", "register", "begin", nil))
	proof := a.registration(t, options, "https://evil.example")
	passkeyDenied(t, passkeyCall(t, c, "bad-origin", "register", "finish", map[string]any{"credential": proof}))
	proof = a.registration(t, options, testPasskeyOrigin)
	passkeyDenied(t, passkeyCall(t, c, "consumed", "register", "finish", map[string]any{"credential": proof}))
	for _, malformed := range []any{nil, "invalid", []any{}, map[string]any{}} {
		passkeyResult(t, passkeyCall(t, c, "begin", "register", "begin", nil))
		failure := passkeyCall(t, c, "malformed", "register", "finish", map[string]any{"credential": malformed})
		if failure["error"].(map[string]any)["code"] != float64(CodeInvalidParams) {
			t.Fatalf("malformed credential: %#v", failure)
		}
	}
	app.mu.RLock()
	defer app.mu.RUnlock()
	if app.schemeLocked(passkey.Name).(*passkey.Scheme).Count() != 0 || app.users.Identities() != 0 || app.sessions.Len() != 0 {
		t.Fatal("failed registration retained an account, credential, or session")
	}
}

// A WebAuthn begin step authenticates nothing, so requests pipelined behind
// it are denied (§3.2); a verified finish lets the ones behind it run.
func TestPasskeyBeginIsNoBarrierPass(t *testing.T) {
	_, httpServer := passkeyTestServer(t)
	owner, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	a := newTestAuthenticator(t)
	registerTestPasskey(t, owner, a)

	c, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	c.write(t, map[string]any{"method": "auth", "id": "begin", "params": map[string]any{"scheme": "webauthn", "action": "login", "step": "begin"}})
	c.write(t, map[string]any{"method": "room_list", "id": "list", "params": map[string]any{"filter": "joined"}})
	options := passkeyResult(t, c.read(t))
	if denied := c.read(t); denied["id"] != "list" || denied["error"].(map[string]any)["code"] != float64(CodeDenied) {
		t.Fatalf("room_list behind begin: %#v", denied)
	}
	c.write(t, map[string]any{"method": "auth", "id": "finish", "params": map[string]any{
		"scheme": "webauthn", "action": "login", "step": "finish",
		"challenge_id": options["challenge_id"], "credential": a.assertion(t, options, testPasskeyOrigin, "localhost", 0x05),
	}})
	c.write(t, map[string]any{"method": "room_list", "id": "listed", "params": map[string]any{"filter": "joined"}})
	passkeyResult(t, c.read(t))
	if listed := passkeyResult(t, c.read(t)); !reflect.DeepEqual(roomIDs(t, listed["joined"]), []string{"general"}) {
		t.Fatalf("room_list behind a verified finish: %#v", listed)
	}
}

func TestPasskeyNotificationsDoNotRunCeremonies(t *testing.T) {
	_, httpServer := passkeyTestServer(t)
	c, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	guestAuth(t, c)
	a := newTestAuthenticator(t)
	options := passkeyResult(t, passkeyCall(t, c, "begin", "register", "begin", nil))
	proof := a.registration(t, options, testPasskeyOrigin)
	challengeID := options["challenge_id"].(string)

	// This notification carries the current challenge and a malformed proof. It
	// must not consume the ceremony or produce a response.
	c.write(t, map[string]any{"method": "auth", "params": map[string]any{
		"scheme": "webauthn", "action": "register", "step": "finish",
		"challenge_id": challengeID, "credential": map[string]any{},
	}})
	result := passkeyResult(t, passkeyCall(t, c, "finish", "register", "finish", map[string]any{"credential": proof}))
	if result["you"].(map[string]any)["user_id"] == "" {
		t.Fatalf("registration result lost identity: %#v", result)
	}
}

func TestPasskeyMalformedFields(t *testing.T) {
	_, httpServer := passkeyTestServer(t)
	c, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	guestAuth(t, c)

	// Actions require a valid step and reject unknown action names.
	for id, params := range map[string]map[string]any{
		"unknown-action": {"scheme": "webauthn", "action": "other", "step": "begin"},
		"no-step":        {"scheme": "webauthn", "action": "register"},
		"bad-step":       {"scheme": "webauthn", "action": "register", "step": "middle"},
	} {
		c.write(t, map[string]any{"method": "auth", "id": id, "params": params})
		frame := c.read(t)
		if frame["error"].(map[string]any)["code"] != float64(CodeInvalidParams) {
			t.Fatalf("%s: %#v", id, frame)
		}
	}

	a := newTestAuthenticator(t)
	options := passkeyResult(t, passkeyCall(t, c, "begin", "register", "begin", nil))
	proof := a.registration(t, options, testPasskeyOrigin)
	// The opaque ID matches, but the action does not. The bound ceremony is
	// consumed before this policy failure and cannot be retried as register.
	passkeyDenied(t, passkeyCall(t, c, "wrong-action", "login", "finish", map[string]any{
		"challenge_id": options["challenge_id"], "credential": proof,
	}))
	passkeyDenied(t, passkeyCall(t, c, "wrong-action-retry", "register", "finish", map[string]any{
		"credential": proof,
	}))
	options = passkeyResult(t, passkeyCall(t, c, "begin-again", "register", "begin", nil))
	proof = a.registration(t, options, testPasskeyOrigin)
	// Missing challenge_id is malformed and leaves the pending challenge in
	// place because no challenge can be matched.
	c.write(t, map[string]any{"method": "auth", "id": "missing-challenge", "params": map[string]any{
		"scheme": "webauthn", "action": "register", "step": "finish", "credential": proof,
	}})
	missing := c.read(t)
	if missing["error"].(map[string]any)["code"] != float64(CodeInvalidParams) {
		t.Fatalf("missing challenge_id: %#v", missing)
	}
	// A matching malformed credential is invalid_params and consumes the
	// challenge, so its later retry is a denied missing challenge.
	failure := passkeyCall(t, c, "malformed", "register", "finish", map[string]any{"credential": map[string]any{}})
	if failure["error"].(map[string]any)["code"] != float64(CodeInvalidParams) {
		t.Fatalf("malformed credential: %#v", failure)
	}
	passkeyDenied(t, passkeyCall(t, c, "retry", "register", "finish", map[string]any{"credential": proof}))
}

func TestSignInReplacesGuestAndDeduplicatesPerUser(t *testing.T) {
	_, httpServer := passkeyTestServer(t)
	owner, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	registered := registerTestPasskey(t, owner, newTestAuthenticator(t))
	observer, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	guestAuth(t, observer)
	switcher, _ := dialOrigin(t, httpServer, testPasskeyOrigin)
	guest := guestAuth(t, switcher)["you"].(map[string]any)
	guestID := guest["user_id"].(string)
	// Each guest's join to general reaches general's members.
	expectMembership(t, owner, "general", observer.userID, true)
	expectMembership(t, owner, "general", guestID, true)
	expectMembership(t, observer, "general", guestID, true)

	// Signing in to an existing account replaces the guest identity, which
	// is retired as when it disconnects: its leave is logged in general and
	// reaches general's members, the switching connection included, which
	// now acts as the passkey user, after its result: every notification a
	// sign-in causes on its connection follows the result (§3.2). It is a
	// departure, not a user_id change: no `user` with `old` follows (§3.3).
	before, result := switcher.request(t, "auth", "resume", map[string]any{"scheme": "token", "token": registered["token"]})
	if !reflect.DeepEqual(result["you"], registered["you"]) || len(before) != 0 {
		t.Fatalf("sign-in %#v after %#v", result, before)
	}
	leave := membershipOnly(t, switcher.read(t))
	checkMembership(t, leave, "general", guestID, false)
	for _, member := range []*testClient{owner, observer} {
		if observed := expectMembership(t, member, "general", guestID, false); !reflect.DeepEqual(observed, leave) {
			t.Fatalf("member's copy %#v differs from %#v", observed, leave)
		}
	}
	observer.expectQuiet(t)
	owner.expectQuiet(t)
	switcher.expectQuiet(t)

	// Request IDs deduplicate per user, across that user's connections.
	params := map[string]any{"room_id": "general", "body": map[string]any{"text": "once"}}
	id, _ := save(t, owner, "same", params)
	switcher.notification(t, "message")
	observer.notification(t, "message")
	if again := switcher.result(t, "message", "same", params); again["message_id"] != id {
		t.Fatalf("duplicate result: %#v", again)
	}
	switcher.expectError(t, "message", "same", map[string]any{"room_id": "general", "body": map[string]any{"text": "different"}}, CodeInvalidParams)
	observer.expectQuiet(t)
}
