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
// to the frontend origin that signed in.
type session struct {
	user    *userState
	origin  string
	expires time.Time
}

// signInLocked makes an account the connection's identity after a scheme
// proved it (applyProvenLocked), with a new bearer token for later
// connections in the result (§3.2). The connection's previous token
// is forgotten. signIn is whether the auth signed the connection in as a
// user it was not signed in as, rather than adding a passkey to it or
// authenticating it again as the same user, which alone sends what follows
// a sign-in (§4.5). joinDefault joins a new account to the default room.
func (s *Server) signInLocked(c *client, req request, user *userState, now time.Time, signIn, joinDefault bool) (any, *Error) {
	if err := s.admitLocked(user); err != nil {
		return nil, err
	}
	delete(s.sessions, c.token)
	s.touchSession(c.token)
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, &Error{Code: CodeInternalError, Message: "Unable to create a sign-in session"}
	}
	token := base64.RawURLEncoding.EncodeToString(secret)
	c.token = sha256.Sum256([]byte(token))
	s.sessions[c.token] = session{user: user, origin: c.origin, expires: now.Add(sessionLifetime)}
	s.touchSession(c.token)
	s.capSessionsLocked(user, c.token)
	return s.switchUserLocked(c, req, user, map[string]any{"token": token}, signIn, joinDefault), nil
}

// capSessionsLocked forgets user's sessions beyond maxSessionsPerUser, but
// never keep, the one just made: the others that expire soonest, which were
// used least recently, go first. Every sign-in adds a session, so an account
// signing in again and again in a loop cannot grow the sessions, and the
// store, without bound.
// parseRequested parses the name and user_id that an approval may request
// for a new account.
func parseRequested(req request) (name, requested string, err *Error) {
	if name, err = parseString(req.params, "name", false); err != nil {
		return "", "", err
	}
	if requested, err = parseString(req.params, "user_id", false); err != nil {
		return "", "", err
	}
	return name, requested, nil
}

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
	now := time.Now()
	token, err := parseString(req.params, "token", true)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, invalidParams("token must be a non-empty string")
	}
	if strings.HasPrefix(token, deviceTokenPrefix) {
		return s.authenticateDeviceLocked(c, req, token)
	}
	s.pruneSessionsLocked(now)
	key := sha256.Sum256([]byte(token))
	session, ok := s.sessions[key]
	if !ok || session.origin != c.origin || !now.Before(session.expires) {
		// A temporary token approves its proposal with this scheme too, as
		// a client presents the token of a link that isn't its own (§4.11.2).
		if s.mayApproveLocked(c, token) {
			name, requested, err := parseRequested(req)
			if err != nil {
				return nil, err
			}
			return s.approveLocked(c, req, token, name, requested)
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
	// Resuming the user the connection is signed in as is no sign-in
	// (§4.5), and sends nothing after its result.
	return s.switchUserLocked(c, req, session.user, map[string]any{"token": token}, c.user != session.user, false), nil
}
