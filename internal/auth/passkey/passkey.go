// Package passkey is the WebAuthn sign-in scheme (PROTOCOL.md §4.10):
// registering a passkey adds it to the connection's account, a guest's
// making it an account, or, on a connection that is not signed in, makes a
// new account with it: with an invitation the connection presents, or,
// where anyone may sign in as a guest, without one (signsUp); logging in
// with one signs in to its account. Server policy decides who can register
// (§4.10), and where anyone may be a guest, signing in as one first would
// add a round trip and no protection. GuestFirst keeps that step: a
// registration then makes an account only of a signed-in guest, or with an
// invitation.
// The scheme keeps its credentials, which the server stores with each
// account (authn.Restorer), and proves identities named by the passkey
// user handle.
//
// A client that cannot use the RP ID runs the ceremony on the passkey page
// instead (page.go), on the server's own origin, which returns a code that
// finishes the exchange on the connection.
package passkey

import (
	"bytes"
	"cmp"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/apron-chat/apron-server-go/internal/auth/oauth"
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
	w    *webauthn.WebAuthn
	core authn.Core

	// SignalUnknown has the page say that no account has a passkey, when
	// none has nor may yet have it, and its script ask the passkey
	// provider to hide it (signalUnknownCredential). Off, such a login is
	// the page's usual error. Set it before the server serves the page;
	// apron.Config's SignalUnknownPasskeys says when it is safe.
	SignalUnknown bool

	// GuestFirst has a registration make an account only of a signed-in
	// guest, or with an invitation, even where anyone may sign in as a
	// guest: one on a connection that is not signed in, without an
	// invitation, is refused, as on a server without guests. Set it before
	// the server serves; apron.Config's GuestFirstPasskeys sets it, which
	// also leaves webauthn out of server.signup (§3.2).
	GuestFirst bool

	// The passkey page's state: the pages people have open, the codes the
	// page issued, and the verify budgets of its clients; and sealer, which
	// seals the additions begun on signed-in connections into the page's
	// `request` (sealAddition).
	flows   oauth.Table[*pageFlow]
	codes   oauth.Codes
	clients pageClients
	sealer  cipher.AEAD

	// mu guards the credentials of each account's passkey holder, by user
	// handle, and the holders by credential ID. Commit changes them under
	// the server's lock; lookups for a login take only mu. It also guards
	// pending: the credentials of verified registrations that no Commit
	// has added or refused yet, a code from the page's included, until
	// when they count as pending (markPending).
	mu           sync.Mutex
	byHandle     map[string]*holder
	byCredential map[string]*holder
	pending      map[string]time.Time
}

// holder is one account's passkeys: its user handle and credentials, which
// are its record (Restorer).
type holder struct {
	Handle      []byte                `json:"handle"`
	Credentials []webauthn.Credential `json:"credentials"`
	// Meta is what the account keeps about each credential, by its ID in
	// base64url: absent for credentials stored before it was kept.
	Meta map[string]credentialMeta `json:"meta,omitzero"`
	// name is the display name a registration shows, not kept.
	name string
}

func (h *holder) WebAuthnID() []byte                         { return h.Handle }
func (h *holder) WebAuthnName() string                       { return h.name }
func (h *holder) WebAuthnDisplayName() string                { return h.name }
func (h *holder) WebAuthnCredentials() []webauthn.Credential { return h.Credentials }

// credentialMeta is what an account keeps about one of its passkeys.
type credentialMeta struct {
	Name     string    `json:"name,omitzero"`
	Created  time.Time `json:"created,omitzero"`
	LastUsed time.Time `json:"last_used,omitzero"`
}

// maxCredentialNameBytes bounds a passkey's name.
const maxCredentialNameBytes = 64

// CredentialID names a credential, as the server and clients do.
func CredentialID(id []byte) string { return base64.RawURLEncoding.EncodeToString(id) }

func (h *holder) touch(id []byte, now time.Time, created bool) {
	if h.Meta == nil {
		h.Meta = make(map[string]credentialMeta)
	}
	meta := h.Meta[CredentialID(id)]
	if created {
		meta.Created = now
	}
	meta.LastUsed = now
	h.Meta[CredentialID(id)] = meta
}

// New is passkey sign-in with w's relying party and origins.
func New(w *webauthn.WebAuthn) *Scheme {
	key := make([]byte, 32)
	_, _ = rand.Read(key) // never fails (crypto/rand)
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	sealer, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		panic(err)
	}
	return &Scheme{w: w, sealer: sealer, byHandle: make(map[string]*holder), byCredential: make(map[string]*holder), pending: make(map[string]time.Time)}
}

// Bind gives the scheme the server's side of sign-in, for its page.
func (s *Scheme) Bind(core authn.Core) { s.core = core }

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
	s.pageRoutes(mux)
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
	// name is the display name a guest's registration, or a new
	// account's, asked for, applied when it finishes; empty for a login,
	// or a passkey added to an account.
	name string
	// create is a registration that makes a new account, on a connection
	// that is not signed in: with invitation, the invitation it presented,
	// or, without one, where anyone may sign up (signsUp), with requested,
	// the user_id its begin asked for.
	create     bool
	invitation string
	requested  string
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
// consumed challenge. A finish with a code from the passkey page works on
// any connection, whatever its origin.
func (s *Scheme) Auth(_ context.Context, r *authn.Request) (authn.Result, error) {
	action, err := authn.String(r.Params, "action", true)
	if err != nil {
		return nil, err
	}
	if action == "" {
		return nil, authn.InvalidParams("action must be a non-empty string")
	}
	if action != "register" && action != "login" {
		return nil, authn.InvalidParams("Unknown passkey action")
	}
	step, err := authn.String(r.Params, "step", true)
	if err != nil {
		return nil, err
	}
	// A client on an origin the RP does not list cannot run the ceremony
	// itself, but may use the passkey page.
	local := protocol.IsOriginInHaystack(r.Origin, s.w.Config.RPOrigins)
	switch step {
	case "":
		return nil, authn.InvalidParams("step must be a non-empty string")
	case "begin":
		return s.begin(r, action, local)
	case "finish":
		if _, has := r.Params["code"]; has {
			return s.redeem(r, action)
		}
		if !local {
			return nil, s.refuseOrigin(r.Origin, "")
		}
		return s.finish(r, action)
	}
	return nil, authn.InvalidParams("Passkey step must be begin or finish")
}

// refuseOrigin refuses a connection that can run no passkey ceremony: its
// origin is not one the RP accepts, nor is pageOrigin, the passkey page's,
// when not empty. The refusal names both, so a client can tell which to
// fix.
func (s *Scheme) refuseOrigin(origin, pageOrigin string) error {
	if s.core != nil {
		s.core.Logger().Info("passkeys refused: the origin is not a --webauthn.origin", "origin", origin, "page_origin", pageOrigin)
	}
	switch {
	case pageOrigin == "" && origin == "":
		return authn.Denied("This connection sent no Origin, which passkeys need")
	case pageOrigin == "":
		return authn.Denied(fmt.Sprintf("Origin %.100q is not configured for passkeys", origin))
	case origin == "":
		return authn.Denied(fmt.Sprintf("This connection sent no Origin, and the passkey page's origin %.100q is not configured for passkeys", pageOrigin))
	}
	return authn.Denied(fmt.Sprintf("Neither Origin %.100q nor the passkey page's origin %.100q is configured for passkeys", origin, pageOrigin))
}

func (s *Scheme) begin(r *authn.Request, action string, local bool) (authn.Result, error) {
	// Starting any new ceremony replaces the previous pending one. The
	// replacement happens before policy and library errors so a stale
	// pending challenge can never be resumed after a new begin was
	// attempted.
	r.State.Store(nil)
	if action == "register" && r.Account == nil && r.Invitation == nil && !s.signsUp(r) {
		if !r.Guests {
			return nil, authn.Denied(InviteOnly)
		}
		return nil, authn.Denied("Sign in as a guest, or present an invitation, before adding a passkey")
	}
	page, err := s.pageURL(r, action)
	if err != nil {
		if !local {
			return nil, err
		}
		// The client can run the ceremony itself: the page is only another
		// way, so whatever it lacks never stops the ceremony.
		page = ""
	}
	if !local {
		if page == "" {
			pageOrigin := ""
			if s.core != nil {
				// The page's origin is not one the RP accepts either.
				pageOrigin = oauth.Origin(r.BaseURL)
			}
			return nil, s.refuseOrigin(r.Origin, pageOrigin)
		}
		// Only the page can run the ceremony for this client.
		return &authn.Reply{Result: map[string]any{"external": map[string]any{"url": page}}}, nil
	}
	c := &ceremony{challengeID: rand.Text(), action: action, rpID: s.w.Config.RPID, origin: r.Origin}
	// The library binds a ceremony to an origin with a host only. An app's
	// origin has none, such as android:apk-key-hash:…, so the scheme binds
	// it after the begin, and the library compares it as a string when the
	// ceremony finishes.
	opaque := protocol.IsOpaqueOrigin(r.Origin)
	var publicKey any
	if action == "register" {
		account := r.Account
		if account == nil {
			// A new account, which the registration makes when it
			// finishes, with the invitation if it presents one: nothing
			// is made before, no guest included.
			account = &authn.Account{Guest: true}
			c.create, c.requested = true, r.UserID
			if r.Invitation != nil {
				account.Name, c.invitation = r.Invitation.Name, r.Invitation.ID
			}
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
		// A guest becoming an account, or a new account, may ask for its
		// name with the begin (§3.2); a passkey added to an account keeps
		// the account's name.
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
		options := []webauthn.RegistrationOption{
			webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
			webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
				ResidentKey:      protocol.ResidentKeyRequirementRequired,
				UserVerification: protocol.VerificationRequired,
			}), webauthn.WithExclusions(exclusions),
		}
		if !opaque {
			options = append(options, webauthn.WithRegistrationOrigin(r.Origin))
		}
		creation, data, err := s.w.BeginRegistration(h, options...)
		if err != nil {
			return nil, authn.Denied("Unable to begin passkey registration")
		}
		creation.Response.Timeout = int(ceremonyLifetime / time.Millisecond)
		publicKey, c.data = creation.Response, data
	} else {
		options := []webauthn.LoginOption{webauthn.WithUserVerification(protocol.VerificationRequired)}
		if !opaque {
			options = append(options, webauthn.WithLoginOrigin(r.Origin))
		}
		assertion, data, err := s.w.BeginDiscoverableLogin(options...)
		if err != nil {
			return nil, authn.Denied("Unable to begin passkey sign-in")
		}
		assertion.Response.Timeout = int(ceremonyLifetime / time.Millisecond)
		publicKey, c.data = assertion.Response, data
	}
	c.data.Origin = r.Origin
	c.data.Expires = r.Now.Add(ceremonyLifetime)
	c.expires = c.data.Expires
	r.State.Store(c)
	result := map[string]any{"challenge_id": c.challengeID, "public_key": publicKey}
	if page != "" {
		result["external"] = map[string]any{"url": page}
	}
	return &authn.Reply{Result: result}, nil
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
	if c.action == "register" && c.create {
		return s.finishNew(r, c)
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
	credential, err := s.verifyRegistration(raw, c.data, c.handle, account.Name)
	if err != nil {
		return nil, err
	}
	return &authn.Add{Session: true, Proof: authn.Proof{
		Subject: Subject(c.handle), Name: c.name, Credential: CredentialID(credential.ID),
		Commit: s.commitCredential(c.handle, credential),
	}}, nil
}

// finishNew verifies a passkey registered on a connection that is not
// signed in, which signs in to a new account (signUp). The connection must
// still be signed in to no account, and present the invitation its begin
// presented, or none if that presented none; the server checks an
// invitation again, still unused, as it makes the account. A registration
// that fails here leaves the invitation for another try.
func (s *Scheme) finishNew(r *authn.Request, c *ceremony) (authn.Result, error) {
	presented := ""
	if r.Invitation != nil {
		presented = r.Invitation.ID
	}
	if r.Account != nil || presented != c.invitation || presented == "" && !s.signsUp(r) {
		return nil, authn.Denied("Registration changed; try again")
	}
	raw, err := parseCredential(r.Params, c.action)
	if err != nil {
		return nil, err
	}
	credential, err := s.verifyRegistration(raw, c.data, c.handle, c.name)
	if err != nil {
		return nil, err
	}
	return s.signUp(r, c.handle, credential, c.name, cmp.Or(c.requested, r.UserID), false), nil
}

// InviteOnly refuses a new account's registration without an invitation on
// a server without guests, where only an invitation makes one.
const InviteOnly = "This server is invite-only; ask for an invite link"

// signsUp reports whether a registration on a connection that is not
// signed in makes a new account without an invitation: where anyone may
// sign in as a guest (authn.Request.Guests), unless GuestFirst.
func (s *Scheme) signsUp(r *authn.Request) bool { return r.Guests && !s.GuestFirst }

// signUp signs in to a new account with a verified passkey, on a
// connection that is not signed in: one the request's invitation makes,
// with the name asked for or the invitation's, and the invitation's
// user_id; or, without one, one the server makes as for any new identity,
// with the name and user_id asked for (requested), after the same checks
// (§3.2). The token resumes from the connection's origin, or with
// anyOrigin from any, as a token from a code does (§4.10.1).
func (s *Scheme) signUp(r *authn.Request, handle []byte, credential *webauthn.Credential, name, requested string, anyOrigin bool) *authn.SignIn {
	result := &authn.SignIn{Create: true, AnyOrigin: anyOrigin, Proof: authn.Proof{
		Subject: Subject(handle), Name: name, UserID: requested, Credential: CredentialID(credential.ID),
		Commit: s.commitCredential(handle, credential),
	}}
	if inv := r.Invitation; inv != nil {
		result.Invitation = inv.ID
		result.Name, result.UserID = cmp.Or(name, inv.Name), inv.UserID
	}
	return result
}

// verifyRegistration verifies a new credential for a user handle against
// a registration's session data.
func (s *Scheme) verifyRegistration(raw []byte, data *webauthn.SessionData, handle []byte, name string) (*webauthn.Credential, *authn.Error) {
	parsed, parseErr := protocol.ParseCredentialCreationResponseBytes(raw)
	if parseErr != nil {
		return nil, authn.Denied("Passkey verification failed")
	}
	credential, createErr := s.w.CreateCredential(&holder{Handle: handle, name: name}, *data, parsed)
	if createErr != nil || credential == nil {
		return nil, authn.Denied("Passkey verification failed")
	}
	return credential, nil
}

// markPending makes a verified registration's credential pending
// (registering) for at least ttl, until a Commit adds or refuses it.
func (s *Scheme) markPending(id []byte, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for pending, until := range s.pending {
		if !now.Before(until) {
			delete(s.pending, pending)
		}
	}
	if until := now.Add(ttl); until.After(s.pending[string(id)]) {
		s.pending[string(id)] = until
	}
}

// commitCredential adds a verified credential to the passkeys of the user
// handle, a new one's included, and returns them as the account's record.
// Until the server calls it, or for a ceremony's lifetime if it never does,
// the credential is pending (registering).
func (s *Scheme) commitCredential(handle []byte, credential *webauthn.Credential) func() (jsontext.Value, error) {
	s.markPending(credential.ID, ceremonyLifetime)
	return func() (jsontext.Value, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.pending, string(credential.ID))
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
		h.touch(credential.ID, time.Now(), true)
		s.byHandle[string(handle)] = h
		s.byCredential[string(credential.ID)] = h
		return json.Marshal(h)
	}
}

// Credentials lists the passkeys of the user handle subject names.
func (s *Scheme) Credentials(subject string) []authn.Credential {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.holderOf(subject)
	if h == nil {
		return nil
	}
	list := make([]authn.Credential, 0, len(h.Credentials))
	for _, credential := range h.Credentials {
		id := CredentialID(credential.ID)
		meta := h.Meta[id]
		list = append(list, authn.Credential{ID: id, Name: meta.Name, Created: meta.Created, LastUsed: meta.LastUsed})
	}
	return list
}

// RenameCredential names one of the user handle's passkeys.
func (s *Scheme) RenameCredential(subject, id, name string) (jsontext.Value, error) {
	if len(name) > maxCredentialNameBytes {
		return nil, authn.InvalidParams("A passkey's name is at most %d bytes", maxCredentialNameBytes)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.holderOf(subject)
	if h == nil || !slices.ContainsFunc(h.Credentials, func(c webauthn.Credential) bool { return CredentialID(c.ID) == id }) {
		return nil, authn.InvalidParams("Unknown passkey %q", id)
	}
	if h.Meta == nil {
		h.Meta = make(map[string]credentialMeta)
	}
	meta := h.Meta[id]
	meta.Name = name
	h.Meta[id] = meta
	return json.Marshal(h)
}

// RemoveCredential removes one of the user handle's passkeys, which signs
// in no more. Removing the last forgets the user handle, and returns no
// record.
func (s *Scheme) RemoveCredential(subject, id string) (jsontext.Value, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.holderOf(subject)
	if h == nil {
		return nil, authn.InvalidParams("Unknown passkey %q", id)
	}
	i := slices.IndexFunc(h.Credentials, func(c webauthn.Credential) bool { return CredentialID(c.ID) == id })
	if i < 0 {
		return nil, authn.InvalidParams("Unknown passkey %q", id)
	}
	delete(s.byCredential, string(h.Credentials[i].ID))
	h.Credentials = slices.Delete(h.Credentials, i, i+1)
	delete(h.Meta, id)
	if len(h.Credentials) == 0 {
		delete(s.byHandle, string(h.Handle))
		return nil, nil
	}
	return json.Marshal(h)
}

// holderOf returns the passkeys of the user handle subject names. Called
// holding s.mu.
func (s *Scheme) holderOf(subject string) *holder {
	handle, err := base64.RawURLEncoding.DecodeString(subject)
	if err != nil {
		return nil
	}
	return s.byHandle[string(handle)]
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
	handle, credential, _, err := s.verifyLogin(raw, c.data)
	if err != nil {
		return nil, err
	}
	return &authn.SignIn{Proof: s.loginProof(handle, credential)}, nil
}

// verifyLogin verifies an assertion against a login's session data, and
// returns the user handle and the credential, with its new counter. A
// login that fails only because no account has its credential returns the
// credential's ID as unknown: one whose lookup found nothing, in an
// assertion made for this ceremony (forCeremony). Any other failure,
// a known credential's included, returns none.
func (s *Scheme) verifyLogin(raw []byte, data *webauthn.SessionData) (handle []byte, credential *webauthn.Credential, unknown []byte, err *authn.Error) {
	failed := authn.Denied("Passkey verification failed")
	parsed, parseErr := protocol.ParseCredentialRequestResponseBytes(raw)
	if parseErr != nil {
		return nil, nil, nil, failed
	}
	absent := false
	verified, credential, verifyErr := s.w.ValidatePasskeyLogin(func(id, userHandle []byte) (webauthn.User, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		h := s.byCredential[string(id)]
		if h == nil {
			absent = true
			return nil, errors.New("unknown credential")
		}
		if !bytes.Equal(h.Handle, userHandle) {
			return nil, errors.New("credential of another user handle")
		}
		handle = h.Handle
		return &holder{Handle: h.Handle, Credentials: append([]webauthn.Credential(nil), h.Credentials...)}, nil
	}, *data, parsed)
	if verifyErr != nil || verified == nil || credential == nil || credential.Authenticator.CloneWarning {
		if absent && s.forCeremony(parsed, data) {
			return nil, nil, parsed.RawID, failed
		}
		return nil, nil, nil, failed
	}
	return handle, credential, nil, nil
}

// forCeremony reports whether an assertion was made for a login's ceremony,
// as far as it can be told without the credential's public key: its type,
// challenge, RP ID, user presence and verification, and the ceremony's time.
// The library looks a credential up before it checks these, so a lookup
// that found nothing says nothing of them. The page checks the origin
// itself (sameOrigin).
func (s *Scheme) forCeremony(parsed *protocol.ParsedCredentialAssertionData, data *webauthn.SessionData) bool {
	client, authData := parsed.Response.CollectedClientData, parsed.Response.AuthenticatorData
	rpIDHash := sha256.Sum256([]byte(s.w.Config.RPID))
	return client.Type == protocol.AssertCeremony && data.Challenge != "" && client.Challenge == data.Challenge &&
		bytes.Equal(authData.RPIDHash, rpIDHash[:]) && authData.Flags.UserPresent() && authData.Flags.UserVerified() &&
		len(parsed.RawID) > 0 && time.Now().Before(data.Expires)
}

// registering reports whether a credential may yet be an account's: a
// registration verified it and the server has not committed it yet, or has
// since. One verified on the page is pending from before its code is
// issued until a Commit, or until the code and then a ceremony's lifetime
// have passed (redirectWithCode), so a redemption, which takes the code
// before it reaches Commit, leaves no moment when it is neither.
func (s *Scheme) registering(id []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, pending := s.pending[string(id)]
	return pending && time.Now().Before(until) || s.byCredential[string(id)] != nil
}

// loginProof proves the identity of a verified login. Its Commit keeps
// the credential's advanced signature counter, checked again against the
// one kept (finishLogin).
func (s *Scheme) loginProof(handle []byte, credential *webauthn.Credential) authn.Proof {
	return authn.Proof{
		Subject: Subject(handle), Credential: CredentialID(credential.ID),
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
			h.touch(credential.ID, time.Now(), false)
			return json.Marshal(h)
		},
	}
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
