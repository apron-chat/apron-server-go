package apron

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/apron-chat/apron-server-go/internal/authn"
)

// Account management (capability ext:account): an account lists its
// credentials, such as passkeys, names them, removes one it lost, and signs
// out its other sessions. Removing a credential and signing out others need
// a recent reauthentication on the asking connection (reauthWindow): a
// passkey login as the same account, which a stolen session token cannot
// make. An account always keeps one identity to sign in with. The changes
// that revoke are stored before their result (fenceLocked).

const (
	// accountExt is the capability, and the prefix of the methods.
	accountExt = "account"
	// reauthWindow is how recent a connection's reauthentication must be
	// for the changes that need one.
	reauthWindow = 5 * time.Minute
)

// verification is a connection's latest proof of its account with a
// credential, such as a passkey login, which counts as a reauthentication
// for reauthWindow.
type verification struct {
	user       string
	credential string
	at         time.Time
}

// credentialKey names a credential across schemes: scheme:id.
func credentialKey(scheme, id string) string {
	if id == "" {
		return ""
	}
	return scheme + ":" + id
}

// provedLocked notes the credential a proof that signed the connection in,
// or added to its account, was made with: the connection's session token
// remembers it, so removing the credential ends that session, and it
// counts as a reauthentication of the connection's account.
func (s *Server) provedLocked(c *client, scheme authn.Scheme, proof authn.Proof, now time.Time) {
	key := credentialKey(scheme.Name(), proof.Credential)
	if key == "" || c.user == nil {
		return
	}
	c.credential = key
	c.verified = verification{user: c.user.id, credential: key, at: now}
	if session, ok := s.sessions[c.token]; ok && session.user == c.user {
		session.credential = key
		s.sessions[c.token] = session
		s.touchSession(c.token)
	}
}

// reauthenticatedLocked refuses a change that needs the connection to have
// proven its account with a credential recently.
func (s *Server) reauthenticatedLocked(c *client) *Error {
	v := c.verified
	if v.user == c.user.id && time.Since(v.at) < reauthWindow {
		return nil
	}
	return &Error{
		Code:    CodeDenied,
		Message: "Sign in again with a passkey on this connection, then try again",
		Data:    map[string]any{"reauthenticate": true},
	}
}

// Credential is one of an account's credentials, such as a passkey.
type Credential struct {
	// Scheme is the sign-in scheme it proves, such as "webauthn", and ID
	// names it within the account's identity of that scheme.
	Scheme, ID string
	// Name is what its owner calls it, if anything.
	Name              string
	Created, LastUsed time.Time
}

// credentialsLocked lists u's credentials, by scheme then creation.
func (s *Server) credentialsLocked(u *userState) []Credential {
	var list []Credential
	for _, scheme := range slices.Sorted(maps.Keys(u.identities)) {
		manager, ok := s.schemeLocked(scheme).(authn.Credentials)
		if !ok {
			continue
		}
		for _, c := range manager.Credentials(u.identities[scheme]) {
			list = append(list, Credential{Scheme: scheme, ID: c.ID, Name: c.Name, Created: c.Created, LastUsed: c.LastUsed})
		}
	}
	slices.SortStableFunc(list, func(a, b Credential) int {
		return cmp.Or(cmp.Compare(a.Scheme, b.Scheme), a.Created.Compare(b.Created))
	})
	return list
}

// removeCredentialLocked removes one of u's credentials. It refuses to
// remove the last way u signs in: the identity a last credential proves
// goes with it, and an account keeps at least one identity. The sessions
// made with the credential end, and the connections signed in with it
// close, except except, which closes after its result.
func (s *Server) removeCredentialLocked(u *userState, scheme, id string, except *client) *Error {
	manager, ok := s.schemeLocked(scheme).(authn.Credentials)
	subject := u.identities[scheme]
	if !ok || subject == "" {
		return invalidParams("This account has no %s credentials", scheme)
	}
	credentials := manager.Credentials(subject)
	if !slices.ContainsFunc(credentials, func(c authn.Credential) bool { return c.ID == id }) {
		return invalidParams("Unknown credential %q", id)
	}
	if len(credentials) == 1 && len(u.identities) == 1 {
		return lastCredential
	}
	record, err := manager.RemoveCredential(subject, id)
	if err != nil {
		return authError(err)
	}
	if record == nil {
		// The identity proves nothing more, and leaves the account.
		delete(u.identities, scheme)
		delete(u.records, scheme)
		delete(s.accounts, authn.Identity{Scheme: scheme, Subject: subject})
		if s.setGrantLocked(u, scheme, nil) && s.grantRolesLocked(u) {
			s.notifyProfileLocked(u, nil, nil)
		}
	} else {
		if u.records == nil {
			u.records = make(map[string]jsontext.Value)
		}
		u.records[scheme] = record
	}
	s.touchUser(u.id)
	key := credentialKey(scheme, id)
	for token, session := range s.sessions {
		if session.user == u && session.credential == key {
			delete(s.sessions, token)
			s.touchSession(token)
		}
	}
	for c := range u.clients {
		if c.credential != key {
			continue
		}
		if c == except {
			// Its result goes first.
			continue
		}
		c.closeWithError(&Error{Code: CodeDenied, Message: "The passkey this connection signed in with was removed"})
	}
	return nil
}

// signOutOthersLocked ends every session and device token of u's but the
// connection's own session, and closes u's other connections.
func (s *Server) signOutOthersLocked(u *userState, keep *client) {
	for c := range u.clients {
		if c != keep {
			c.closeWithError(&Error{Code: CodeDenied, Message: "This account signed out its other sessions"})
		}
	}
	for key, session := range s.sessions {
		if session.user == u && (keep == nil || key != keep.token) {
			delete(s.sessions, key)
			s.touchSession(key)
		}
	}
	for key, device := range s.devices {
		if device.user == u {
			delete(s.devices, key)
			s.touchDevice(key)
		}
	}
	for key, t := range s.services {
		if t.user == u && (keep == nil || key != keep.service) {
			s.revokeServiceLocked(key, "This account signed out its other sessions")
		}
	}
}

// accountOps are the account's methods (ext:account).
func accountOps(ops map[string]operation) {
	ops[accountExt+".credentials"] = (*Server).listCredentials
	ops[accountExt+".credential_rename"] = (*Server).renameCredential
	ops[accountExt+".credential_remove"] = (*Server).removeCredential
	ops[accountExt+".sign_out"] = (*Server).signOut
}

// accountLocked refuses the account methods to a guest.
func accountLocked(c *client) *Error {
	if c.restrictedService() {
		return serviceRefusal()
	}
	if !c.user.account() {
		return &Error{Code: CodeDenied, Message: "Sign in with an account to manage it"}
	}
	return nil
}

func (s *Server) listCredentials(c *client, req request) (any, bool, *Error) {
	s.mu.Lock()
	defer s.unlock()
	if err := accountLocked(c); err != nil {
		return nil, false, err
	}
	list := []any{}
	for _, credential := range s.credentialsLocked(c.user) {
		entry := map[string]any{"scheme": credential.Scheme, "id": credential.ID}
		if credential.Name != "" {
			entry["name"] = credential.Name
		}
		if !credential.Created.IsZero() {
			entry["created"] = credential.Created.UnixMilli()
		}
		if !credential.LastUsed.IsZero() {
			entry["last_used"] = credential.LastUsed.UnixMilli()
		}
		if c.credential == credentialKey(credential.Scheme, credential.ID) {
			entry["current"] = true
		}
		list = append(list, entry)
	}
	result := map[string]any{"credentials": list}
	if v := c.verified; v.user == c.user.id && time.Since(v.at) < reauthWindow {
		result["reauthenticated_until"] = v.at.Add(reauthWindow).UnixMilli()
	}
	c.sendResult(req, result)
	return result, true, nil
}

// credentialParams parses a credential's scheme, webauthn by default, and
// id.
func credentialParams(req request) (string, string, *Error) {
	scheme, err := parseString(req.params, "scheme", false)
	if err != nil {
		return "", "", err
	}
	id, err := parseString(req.params, "id", true)
	if err != nil {
		return "", "", err
	}
	if id == "" {
		return "", "", invalidParams("id must be a non-empty string")
	}
	return cmp.Or(scheme, "webauthn"), id, nil
}

func (s *Server) renameCredential(c *client, req request) (any, bool, *Error) {
	scheme, id, err := credentialParams(req)
	if err != nil {
		return nil, false, err
	}
	name, err := parseString(req.params, "name", true)
	if err != nil {
		return nil, false, err
	}
	name = strings.TrimSpace(name)
	s.mu.Lock()
	defer s.unlock()
	if err := accountLocked(c); err != nil {
		return nil, false, err
	}
	u := c.user
	manager, ok := s.schemeLocked(scheme).(authn.Credentials)
	if !ok || u.identities[scheme] == "" {
		return nil, false, invalidParams("This account has no %s credentials", scheme)
	}
	record, schemeErr := manager.RenameCredential(u.identities[scheme], id, name)
	if schemeErr != nil {
		return nil, false, authError(schemeErr)
	}
	u.records[scheme] = record
	s.touchUser(u.id)
	return map[string]any{}, false, nil
}

func (s *Server) removeCredential(c *client, req request) (any, bool, *Error) {
	scheme, id, err := credentialParams(req)
	if err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.unlock()
	if err := accountLocked(c); err != nil {
		return nil, false, err
	}
	if err := s.reauthenticatedLocked(c); err != nil {
		return nil, false, err
	}
	if err := s.removeCredentialLocked(c.user, scheme, id, c); err != nil {
		return nil, false, err
	}
	c.fenceLocked()
	result := map[string]any{}
	c.sendResult(req, result)
	if c.credential == credentialKey(scheme, id) {
		c.closeWithError(&Error{Code: CodeDenied, Message: "The passkey this connection signed in with was removed"})
	}
	return result, true, nil
}

// signOut forgets the connection's session token and closes it, or, with
// `others`, ends every other session and device token of the account and
// closes its other connections, which needs a recent reauthentication.
func (s *Server) signOut(c *client, req request) (any, bool, *Error) {
	others, err := parseBool(req.params, "others", false)
	if err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.unlock()
	if err := accountLocked(c); err != nil {
		return nil, false, err
	}
	if others {
		if err := s.reauthenticatedLocked(c); err != nil {
			return nil, false, err
		}
		s.signOutOthersLocked(c.user, c)
		c.fenceLocked()
		result := map[string]any{}
		c.sendResult(req, result)
		return result, true, nil
	}
	delete(s.sessions, c.token)
	s.touchSession(c.token)
	c.fenceLocked()
	result := map[string]any{}
	c.sendResult(req, result)
	c.closeWithError(&Error{Code: CodeDenied, Message: "Signed out"})
	return result, true, nil
}

// Credentials lists an account's credentials, such as its passkeys.
func (s *Server) Credentials(userID string) []Credential {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u := s.users[userID]
	if u == nil {
		return nil
	}
	return s.credentialsLocked(u)
}

// Credentials lists an account's credentials; see Server.Credentials.
func (h *Host) Credentials(userID string) []Credential { return h.s.Credentials(userID) }

// lastCredential refuses to remove an account's last way to sign in.
var lastCredential = &Error{Code: CodeDenied, Message: "This is the account's last way to sign in; add another before removing it"}

// ErrLastCredential is returned by RemoveCredential for the credential an
// account last signs in with.
var ErrLastCredential = errors.New("apron: this is the account's last way to sign in")

// RemoveCredential removes one of an account's credentials, such as a lost
// passkey, and returns once that is stored: it signs in no more, the
// sessions it made end, and the connections signed in with it close. The
// account, its messages, and its other credentials stay. It refuses the
// account's last way to sign in (ErrLastCredential). Unlike the
// account.credential_remove method, it needs no reauthentication: the
// program or extension that calls it decides who may.
func (s *Server) RemoveCredential(ctx context.Context, userID, scheme, id string) error {
	s.mu.Lock()
	u := s.users[userID]
	if u == nil {
		s.unlock()
		return errors.New("apron: no such user")
	}
	if err := s.removeCredentialLocked(u, scheme, id, nil); err != nil {
		s.unlock()
		if err == lastCredential {
			return ErrLastCredential
		}
		return err
	}
	f := s.newFenceLocked()
	s.unlock()
	return waitFence(ctx, f)
}

// RemoveCredential removes an account's credential; see
// Server.RemoveCredential.
func (h *Host) RemoveCredential(ctx context.Context, userID, scheme, id string) error {
	return h.s.RemoveCredential(ctx, userID, scheme, id)
}

// SignOut signs an account out everywhere, and returns once that is
// stored: its connections close, and its session tokens, device tokens,
// push registrations, and writes that had not finished end. It may sign in
// again.
func (s *Server) SignOut(ctx context.Context, userID string) error {
	s.mu.Lock()
	if u := s.users[userID]; u != nil {
		s.signOutLocked(u)
	}
	f := s.newFenceLocked()
	s.unlock()
	return waitFence(ctx, f)
}

// SignOut signs an account out everywhere; see Server.SignOut.
func (h *Host) SignOut(ctx context.Context, userID string) error { return h.s.SignOut(ctx, userID) }

// waitFence waits for a fence, or for ctx to end.
func waitFence(ctx context.Context, f *storeFence) error {
	select {
	case <-f.done:
		return f.err
	case <-ctx.Done():
		return ctx.Err()
	}
}
