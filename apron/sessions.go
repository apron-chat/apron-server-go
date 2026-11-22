package apron

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"slices"
	"strings"
	"time"
)

const sessionLifetime = 30 * 24 * time.Hour

// maxSessionsPerUser bounds the sessions kept for one account: far more
// devices and browsers than one person signs in from.
const maxSessionsPerUser = 100

// session is a bearer token's sign-in (§3.2): the account it resumes, bound
// to the frontend origin that signed in, or, for a token from a code
// (§4.12.4), to no origin (anyOrigin).
type session struct {
	user      *userState
	origin    string
	anyOrigin bool
	expires   time.Time
	// credential names the credential the sign-in that made it proved, as
	// scheme:id, if any (account.go).
	credential string
}

// signInLocked makes an account the connection's identity after a scheme
// proved it (applyProvenLocked), with a new bearer token for later
// connections in the result (§3.2). The connection's previous token
// is forgotten. signIn is whether the auth signed the connection in as a
// user it was not signed in as, rather than adding a passkey to it or
// authenticating it again as the same user, which alone sends what follows
// a sign-in (§4.5). joinDefault joins a new account to the default room.
// The token resumes from the connection's origin, or with anyOrigin from
// any.
func (s *Server) signInLocked(c *client, req request, user *userState, now time.Time, signIn, joinDefault, anyOrigin bool) (any, *Error) {
	if err := s.admitLocked(user); err != nil {
		return nil, err
	}
	delete(s.sessions, c.token)
	s.touchSession(c.token)
	token, key, err := s.newSessionLocked(user, c.origin, anyOrigin, now)
	if err != nil {
		return nil, err
	}
	c.token = key
	c.service, c.serviceManage = [32]byte{}, false
	// A sign-in that proved a credential notes it after (provedLocked). A
	// device token the connection signed in with stays its own, so that
	// replacing that token still closes it.
	c.credential = ""
	return s.switchUserLocked(c, req, user, map[string]any{"token": token}, signIn, joinDefault), nil
}

// newSessionLocked makes a bearer token that resumes user's sign-in from
// origin, or with anyOrigin from any origin or none (§3.2.1), and returns it
// with the key it is kept under.
func (s *Server) newSessionLocked(user *userState, origin string, anyOrigin bool, now time.Time) (string, [32]byte, *Error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", [32]byte{}, &Error{Code: CodeInternalError, Message: "Unable to create a sign-in session"}
	}
	token := base64.RawURLEncoding.EncodeToString(secret)
	key := sha256.Sum256([]byte(token))
	if anyOrigin {
		origin = ""
	}
	s.sessions[key] = session{user: user, origin: origin, anyOrigin: anyOrigin, expires: now.Add(sessionLifetime)}
	s.touchSession(key)
	s.capSessionsLocked(user, key)
	return token, key, nil
}

// parseRequested parses the name and user_id that an auth request may request
// for a new account.
func parseRequested(req request) (name, requested string, err *Error) {
	d := req.params.Decoder()
	name = d.String("name", optional)
	requested = d.String("user_id", optional)
	if err := paramError(d.Err()); err != nil {
		return "", "", err
	}
	return name, requested, nil
}

// capSessionsLocked forgets user's sessions beyond maxSessionsPerUser, but
// never keep, the one just made: the others that expire soonest, which were
// used least recently, go first. Every sign-in adds a session, so an account
// signing in again and again in a loop cannot grow the sessions, and the
// store, without bound.
func (s *Server) capSessionsLocked(user *userState, keep [32]byte) {
	var keys [][32]byte
	for key, session := range s.sessions {
		if session.user == user && key != keep {
			keys = append(keys, key)
		}
	}
	excess := len(keys) - (maxSessionsPerUser - 1)
	if excess <= 0 {
		return
	}
	slices.SortFunc(keys, func(a, b [32]byte) int {
		return s.sessions[a].expires.Compare(s.sessions[b].expires)
	})
	for _, key := range keys[:excess] {
		delete(s.sessions, key)
		s.touchSession(key)
	}
}

// pruneSessionsLocked forgets expired sessions.
func (s *Server) pruneSessionsLocked(now time.Time) {
	for key, session := range s.sessions {
		if !now.Before(session.expires) {
			delete(s.sessions, key)
			s.touchSession(key)
		}
	}
}

// authenticateToken resumes a sign-in through the protocol's token scheme
// (§3.2), signs in with a device token (devices.go), or approves a
// proposal with its temporary token (§4.11.2). Called holding s.mu.
func (s *Server) authenticateToken(c *client, req request) (any, *Error) {
	// Any token auth ends the schemes' pending work on the connection, such
	// as a passkey ceremony, whether or not it signs in.
	c.authStates = nil
	now := time.Now()
	d := req.params.Decoder()
	token := d.String("token", required)
	if d.Err() == nil && token == "" {
		d.Fail(invalidParams("token must be a non-empty string"))
	}
	if err := paramError(d.Err()); err != nil {
		return nil, err
	}
	if strings.HasPrefix(token, deviceTokenPrefix) {
		return s.authenticateDeviceLocked(c, req, token)
	}
	if strings.HasPrefix(token, serviceTokenPrefix) {
		return s.authenticateServiceLocked(c, req, token)
	}
	s.pruneSessionsLocked(now)
	key := sha256.Sum256([]byte(token))
	session, ok := s.sessions[key]
	if !ok || !session.anyOrigin && session.origin != c.origin || !now.Before(session.expires) {
		// A temporary token approves its proposal with this scheme too, as
		// a client presents the token of a link that isn't its own
		// (§4.11.2), with the proposal's state or without (§4.11.3).
		state, err := parseString(req.params, "state", optional)
		if err != nil {
			return nil, err
		}
		if s.mayApproveLocked(c, token, state) {
			name, requested, err := parseRequested(req)
			if err != nil {
				return nil, err
			}
			return s.approveLocked(c, req, token, state, name, requested)
		}
		return nil, &Error{Code: CodeDenied, Message: "Session expired; sign in again"}
	}
	// Each successful resume renews the session for a full lifetime, so an
	// active user is never forced back through a ceremony. The token itself is
	// not rotated, since several tabs may share one persisted token: the
	// result carries the presented token, which clients keep as the latest.
	// A refused sign-in changes nothing (§3.2.2).
	if err := s.admitLocked(session.user); err != nil {
		return nil, err
	}
	session.expires = now.Add(sessionLifetime)
	s.sessions[key] = session
	s.touchSession(key)
	c.token = key
	c.credential = session.credential
	c.service, c.serviceManage = [32]byte{}, false
	// Resuming the user the connection is signed in as is no sign-in
	// (§4.5), and sends nothing after its result.
	return s.switchUserLocked(c, req, session.user, map[string]any{"token": token}, c.user != session.user, false), nil
}
