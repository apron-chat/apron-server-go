// Package passkey is the WebAuthn sign-in scheme (PROTOCOL.md §4.10):
// registering a passkey adds it to the connection's account, a guest's
// making it an account, or, on a connection that presents an invitation
// and is not signed in, makes a new account with it; logging in with one
// signs in to its account.
// The scheme keeps its credentials, which the server stores with each
// account (authn.Restorer), and proves identities named by the passkey
// user handle.
package passkey

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/apron-chat/apron-server-go/internal/authn"
)

// Name is the scheme's name, and its identities'.
const Name = "webauthn"

// Disabled says why the scheme may be off.
const Disabled = "Passkeys are disabled"

const (
	ceremonyLifetime = 2 * time.Minute
	// maxPasskeys bounds the passkeys of one account.
	maxPasskeys = 10
)

// Scheme is passkey sign-in.
type Scheme struct {
	w *webauthn.WebAuthn

	// mu guards the credentials of each account's passkey holder, by user
	// handle, and the holders by credential ID. Commit changes them under
	// the server's lock; lookups for a login take only mu.
	mu           sync.Mutex
	byHandle     map[string]*holder
	byCredential map[string]*holder
}

// holder is one account's passkeys: its user handle and credentials, which
// are its record (Restorer).
type holder struct {
	Handle      []byte                `json:"handle"`
	Credentials []webauthn.Credential `json:"credentials"`
	// name is the display name a registration shows, not kept.
	name string
}

func (h *holder) WebAuthnID() []byte                         { return h.Handle }
func (h *holder) WebAuthnName() string                       { return h.name }
func (h *holder) WebAuthnDisplayName() string                { return h.name }
func (h *holder) WebAuthnCredentials() []webauthn.Credential { return h.Credentials }

// New is passkey sign-in with w's relying party and origins.
func New(w *webauthn.WebAuthn) *Scheme {
	return &Scheme{w: w, byHandle: make(map[string]*holder), byCredential: make(map[string]*holder)}
}

func (*Scheme) Name() string  { return Name }
func (*Scheme) Label() string { return "passkey" }

// Subject names a user handle's identity.
func Subject(handle []byte) string { return base64.RawURLEncoding.EncodeToString(handle) }

// Restore keeps an account's passkeys, as the server stored them.
func (s *Scheme) Restore(_ string, record jsontext.Value) error {
	var h holder
	if err := json.Unmarshal(record, &h); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byHandle[string(h.Handle)] = &h
	for _, credential := range h.Credentials {
		s.byCredential[string(credential.ID)] = &h
	}
	return nil
}

// Routes serves the WebAuthn related origins document
// (https://www.w3.org/TR/webauthn-3/#sctn-related-origins), listing every
// configured frontend origin, so a browser on a frontend outside the RP
// ID's domain may use it. Browsers fetch it only from the RP ID's host.
func (s *Scheme) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/.well-known/webauthn", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Read with GET", http.StatusMethodNotAllowed)
			return
		}
		body, _ := json.Marshal(map[string][]string{"origins": s.w.Config.RPOrigins})
		header := w.Header()
		header.Set("Access-Control-Allow-Origin", "*")
		header.Set("Content-Type", "application/json")
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Cache-Control", "public, max-age=300")
		_, _ = w.Write(body)
	})
}

// ceremony is a connection's pending registration or login: one at a
// time, which finishing consumes even on failure.
type ceremony struct {
	challengeID string
	action      string
	// userID and handle are the account a registration adds to, and the
	// handle its passkey gets: the account's own, or a new one.
	userID string
	handle []byte
	rpID   string
	origin string
	// name is the display name a guest's registration asked for, applied
	// when it finishes; empty for a login, or a passkey added to an
	// account.
	name string
	// invitation is the invitation a registration that makes a new account
	// presented; empty for one that adds to the connection's account.
	invitation string
	data       *webauthn.SessionData
	expires    time.Time
}

// Expire makes a connection's pending ceremony expire, for tests.
func Expire(state *authn.State) {
	if c, ok := state.Load().(*ceremony); ok {
		c.expires = time.Now().Add(-time.Second)
	}
}

// Auth runs a registration or a login, a begin or a finish step (§4.10).
// Auth responses are never deduplicated, so replay cannot resurrect a
// consumed challenge.
func (s *Scheme) Auth(_ context.Context, r *authn.Request) (authn.Result, error) {
	action, err := authn.String(r.Params, "action", true)
	if err != nil {
		return nil, err
	}
	if action == "" {
		return nil, authn.InvalidParams("action must be a non-empty string")
	}
	if !protocol.IsOriginInHaystack(r.Origin, s.w.Config.RPOrigins) {
		return nil, authn.Denied("Frontend origin is not configured for passkeys")
	}
	switch action {
	case "register", "login":
		step, err := authn.String(r.Params, "step", true)
		if err != nil {
			return nil, err
		}
		switch step {
		case "":
			return nil, authn.InvalidParams("step must be a non-empty string")
		case "begin":
			return s.begin(r, action)
		case "finish":
			return s.finish(r, action)
		}
		return nil, authn.InvalidParams("Passkey step must be begin or finish")
	}
	return nil, authn.InvalidParams("Unknown passkey action")
}

func (s *Scheme) begin(r *authn.Request, action string) (authn.Result, error) {
	// Starting any new ceremony replaces the previous pending one. The
	// replacement happens before policy and library errors so a stale
	// pending challenge can never be resumed after a new begin was
	// attempted.
	r.State.Store(nil)
	c := &ceremony{challengeID: rand.Text(), action: action, rpID: s.w.Config.RPID, origin: r.Origin}
	var publicKey any
	if action == "register" {
		account := r.Account
		if account == nil && r.Invitation != nil {
			// A new account, which the invitation makes when the
			// registration finishes: nothing is made before.
			account = &authn.Account{Name: r.Invitation.Name, Guest: true}
			c.invitation = r.Invitation.ID
		}
		if account == nil {
			return nil, authn.Denied("Sign in as a guest, or present an invitation, before adding a passkey")
		}
		c.userID = account.UserID
		h := &holder{name: account.Name}
		if h.name == "" {
			h.name = account.UserID
		}
		if h.name == "" {
			h.name = "Apron"
		}
		if subject := account.Identities[Name]; subject != "" {
			handle, err := base64.RawURLEncoding.DecodeString(subject)
			if err != nil {
				return nil, authn.Internal("Unable to begin passkey registration")
			}
			s.mu.Lock()
			if existing := s.byHandle[string(handle)]; existing != nil {
				h.Credentials = append([]webauthn.Credential(nil), existing.Credentials...)
			}
			s.mu.Unlock()
			h.Handle = handle
		} else {
			h.Handle = []byte(rand.Text())
		}
		c.handle = h.Handle
		// A guest becoming an account may ask for its name with the begin
		// (§3.2); a passkey added to an account keeps the account's name.
		if account.Guest {
			c.name = r.Name
			if c.name != "" {
				h.name = c.name
			}
		}
		if c.invitation != "" && c.name == "" {
			c.name = r.Invitation.Name
		}
		if len(h.Credentials) >= maxPasskeys {
			return nil, authn.Denied("This identity already has ten passkeys")
		}
		exclusions := make([]protocol.CredentialDescriptor, 0, len(h.Credentials))
		for _, credential := range h.Credentials {
			exclusions = append(exclusions, credential.Descriptor())
		}
		creation, data, err := s.w.BeginRegistration(h,
			webauthn.WithRegistrationOrigin(r.Origin),
			webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
			webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
				ResidentKey:      protocol.ResidentKeyRequirementRequired,
				UserVerification: protocol.VerificationRequired,
			}), webauthn.WithExclusions(exclusions))
		if err != nil {
			return nil, authn.Denied("Unable to begin passkey registration")
		}
		creation.Response.Timeout = int(ceremonyLifetime / time.Millisecond)
		publicKey, c.data = creation.Response, data
	} else {
		assertion, data, err := s.w.BeginDiscoverableLogin(webauthn.WithLoginOrigin(r.Origin), webauthn.WithUserVerification(protocol.VerificationRequired))
		if err != nil {
			return nil, authn.Denied("Unable to begin passkey sign-in")
		}
		assertion.Response.Timeout = int(ceremonyLifetime / time.Millisecond)
		publicKey, c.data = assertion.Response, data
	}
	c.data.Expires = r.Now.Add(ceremonyLifetime)
	c.expires = c.data.Expires
	r.State.Store(c)
	return &authn.Reply{Result: map[string]any{"challenge_id": c.challengeID, "public_key": publicKey}}, nil
}

func (s *Scheme) finish(r *authn.Request, action string) (authn.Result, error) {
	challengeID, err := authn.String(r.Params, "challenge_id", true)
	if err != nil {
		return nil, err
	}
	if challengeID == "" {
		return nil, authn.InvalidParams("challenge_id must be a non-empty string")
	}
	c, _ := r.State.Load().(*ceremony)
	// An ID from another connection or a superseded begin does not identify
	// the current ceremony and therefore must not consume the current one.
	// A finish carrying the current ID consumes it before parsing or
	// verifying the proof, including malformed proofs and policy failures.
	if c == nil || c.challengeID != challengeID {
		return nil, authn.Denied("Passkey challenge is missing or expired; try again")
	}
	r.State.Store(nil)
	if c.action != action || c.rpID != s.w.Config.RPID || c.origin != r.Origin || !r.Now.Before(c.expires) {
		return nil, authn.Denied("Passkey challenge is missing or expired; try again")
	}
	if c.action == "register" && c.invitation != "" {
		return s.finishInvited(r, c)
	}
	if c.action == "register" {
		return s.finishRegistration(r, c)
	}
	return s.finishLogin(r, c)
}

// finishRegistration verifies a new passkey, which the server adds to the
// connection's account; Commit adds it to the account's holder.
func (s *Scheme) finishRegistration(r *authn.Request, c *ceremony) (authn.Result, error) {
	account := r.Account
	subject := ""
	if account != nil {
		subject = account.Identities[Name]
	}
	switch {
	case account == nil || account.UserID != c.userID:
		return nil, authn.Denied("Identity changed; try again")
	case subject != "" && subject != Subject(c.handle):
		return nil, authn.Denied("Registration changed; try again")
	}
	raw, err := parseCredential(r.Params, c.action)
	if err != nil {
		return nil, err
	}
	parsed, parseErr := protocol.ParseCredentialCreationResponseBytes(raw)
	if parseErr != nil {
		return nil, authn.Denied("Passkey verification failed")
	}
	registering := &holder{Handle: c.handle, name: account.Name}
	credential, createErr := s.w.CreateCredential(registering, *c.data, parsed)
	if createErr != nil || credential == nil {
		return nil, authn.Denied("Passkey verification failed")
	}
	return &authn.Add{Session: true, Proof: authn.Proof{
		Subject: Subject(c.handle), Name: c.name,
		Commit: s.commitCredential(c.handle, credential),
	}}, nil
}

// finishInvited verifies a passkey registered with an invitation, which
// signs in to a new account that the invitation makes. The connection must
// still be signed in to no account, and present the same invitation, which
// the server checks again, still unused, as it makes the account. A
// registration that fails here leaves the invitation for another try.
func (s *Scheme) finishInvited(r *authn.Request, c *ceremony) (authn.Result, error) {
	if r.Account != nil || r.Invitation == nil || r.Invitation.ID != c.invitation {
		return nil, authn.Denied("Registration changed; try again")
	}
	raw, err := parseCredential(r.Params, c.action)
	if err != nil {
		return nil, err
	}
	parsed, parseErr := protocol.ParseCredentialCreationResponseBytes(raw)
	if parseErr != nil {
		return nil, authn.Denied("Passkey verification failed")
	}
	credential, createErr := s.w.CreateCredential(&holder{Handle: c.handle, name: c.name}, *c.data, parsed)
	if createErr != nil || credential == nil {
		return nil, authn.Denied("Passkey verification failed")
	}
	return &authn.SignIn{Create: true, Invitation: c.invitation, Proof: authn.Proof{
		Subject: Subject(c.handle), Name: c.name, UserID: r.Invitation.UserID,
		Commit: s.commitCredential(c.handle, credential),
	}}, nil
}

// commitCredential adds a verified credential to the passkeys of the user
// handle, a new one's included, and returns them as the account's record.
func (s *Scheme) commitCredential(handle []byte, credential *webauthn.Credential) func() (jsontext.Value, error) {
	return func() (jsontext.Value, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.byCredential[string(credential.ID)] != nil {
			return nil, authn.Denied("Passkey is already registered")
		}
		h := s.byHandle[string(handle)]
		if h == nil {
			h = &holder{Handle: handle}
		}
		if len(h.Credentials) >= maxPasskeys {
			return nil, authn.Denied("Registration changed; try again")
		}
		h.Credentials = append(h.Credentials, *credential)
		s.byHandle[string(handle)] = h
		s.byCredential[string(credential.ID)] = h
		return json.Marshal(h)
	}
}

// finishLogin verifies a passkey, which signs in to its account; Commit
// keeps its advanced signature counter, which detects a cloned
// authenticator only if it survives a restart. Logins are verified against
// a copy of the credentials, outside any lock, so Commit checks the counter
// again against the one kept: of two logins with the same counter, as from
// a clone, or of a later counter committed first, only the first commits.
func (s *Scheme) finishLogin(r *authn.Request, c *ceremony) (authn.Result, error) {
	raw, err := parseCredential(r.Params, c.action)
	if err != nil {
		return nil, err
	}
	parsed, parseErr := protocol.ParseCredentialRequestResponseBytes(raw)
	if parseErr != nil {
		return nil, authn.Denied("Passkey verification failed")
	}
	var handle []byte
	verified, credential, verifyErr := s.w.ValidatePasskeyLogin(func(id, userHandle []byte) (webauthn.User, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		h := s.byCredential[string(id)]
		if h == nil || !bytes.Equal(h.Handle, userHandle) {
			return nil, errors.New("unknown credential")
		}
		handle = h.Handle
		return &holder{Handle: h.Handle, Credentials: append([]webauthn.Credential(nil), h.Credentials...)}, nil
	}, *c.data, parsed)
	if verifyErr != nil || verified == nil || credential == nil || credential.Authenticator.CloneWarning {
		return nil, authn.Denied("Passkey verification failed")
	}
	return &authn.SignIn{Proof: authn.Proof{
		Subject: Subject(handle),
		Commit: func() (jsontext.Value, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			h := s.byCredential[string(credential.ID)]
			if h == nil || !bytes.Equal(h.Handle, handle) {
				return nil, authn.Denied("Passkey verification failed")
			}
			i := slices.IndexFunc(h.Credentials, func(kept webauthn.Credential) bool { return bytes.Equal(kept.ID, credential.ID) })
			if i < 0 {
				return nil, authn.Denied("Passkey verification failed")
			}
			kept, next := h.Credentials[i].Authenticator.SignCount, credential.Authenticator.SignCount
			if (kept != 0 || next != 0) && next <= kept {
				return nil, authn.Denied("Passkey verification failed")
			}
			h.Credentials[i] = *credential
			return json.Marshal(h)
		},
	}}, nil
}

// Count counts the passkeys the scheme keeps, for tests.
func (s *Scheme) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byCredential)
}

// parseCredential performs only wire-shape validation. A syntactically
// shaped credential with invalid base64, authenticator data, origin, RP, UV,
// or signature remains a failed proof and is reported as denied.
func parseCredential(params map[string]jsontext.Value, action string) ([]byte, *authn.Error) {
	raw, ok := params["credential"]
	if !ok {
		return nil, authn.InvalidParams("Missing credential")
	}
	var credential map[string]jsontext.Value
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &credential) != nil || credential == nil {
		return nil, authn.InvalidParams("credential must be an object")
	}
	for _, name := range []string{"id", "rawId", "type"} {
		value, ok := credential[name]
		var text string
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &text) != nil || text == "" {
			return nil, authn.InvalidParams("credential.%s must be a non-empty string", name)
		}
	}
	var credentialType string
	if json.Unmarshal(credential["type"], &credentialType) == nil && credentialType != "public-key" {
		return nil, authn.InvalidParams("credential.type must be public-key")
	}
	if value, ok := credential["clientExtensionResults"]; ok {
		var extensions map[string]jsontext.Value
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &extensions) != nil || extensions == nil {
			return nil, authn.InvalidParams("credential.clientExtensionResults must be an object")
		}
	}
	if value, ok := credential["authenticatorAttachment"]; ok {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			value = nil
		}
		var attachment string
		if value != nil && (json.Unmarshal(value, &attachment) != nil || attachment == "") {
			return nil, authn.InvalidParams("credential.authenticatorAttachment must be a non-empty string")
		}
	}
	rawResponse, ok := credential["response"]
	if !ok || bytes.Equal(bytes.TrimSpace(rawResponse), []byte("null")) {
		return nil, authn.InvalidParams("credential.response must be an object")
	}
	var response map[string]jsontext.Value
	if json.Unmarshal(rawResponse, &response) != nil || response == nil {
		return nil, authn.InvalidParams("credential.response must be an object")
	}
	fields := []string{"clientDataJSON"}
	if action == "register" {
		fields = append(fields, "attestationObject")
	} else {
		fields = append(fields, "authenticatorData", "signature")
	}
	for _, name := range fields {
		value, ok := response[name]
		var text string
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &text) != nil || text == "" {
			return nil, authn.InvalidParams("credential.response.%s must be a non-empty string", name)
		}
	}
	if value, ok := response["userHandle"]; ok {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			value = nil
		}
		var handle string
		if value != nil && (json.Unmarshal(value, &handle) != nil || handle == "") {
			return nil, authn.InvalidParams("credential.response.userHandle must be a non-empty string")
		}
	}
	if value, ok := response["transports"]; ok {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			value = nil
		}
		var transports []string
		if value != nil && json.Unmarshal(value, &transports) != nil {
			return nil, authn.InvalidParams("credential.response.transports must be an array")
		}
	}
	encoded, err := json.Marshal(credential)
	if err != nil {
		return nil, authn.InvalidParams("credential is invalid")
	}
	return encoded, nil
}
