package aprond

import (
	"crypto/sha256"
	"strings"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/server"
	"github.com/apron-chat/apron-server-go/aprond/internal/devices"
	"github.com/apron-chat/apron-server-go/aprond/internal/sessions"
)

// session is a bearer token's sign-in (§3.2), which a sessions.Table
// keeps: the account it resumes, bound to the frontend origin that signed
// in, or, for a token from a code (§4.12.4), to no origin (AnyOrigin), and
// the credential the sign-in proved, if any (account.go).
type session = sessions.Session[*userState]

// signInLocked makes an account the connection's identity after a scheme
// proved it (applyProvenLocked), with a new bearer token for later
// connections in the result (§3.2). The connection's previous token
// is forgotten. signIn is whether the auth signed the connection in as a
// user it was not signed in as, rather than adding a passkey to it or
// authenticating it again as the same user, which alone sends what follows
// a sign-in (§4.5). joinDefault joins a new account to the default room.
// The token resumes from the connection's origin, or with anyOrigin from
// any.
func (s *Server) signInLocked(c *client, reply server.Reply[apron.AuthResult], user *userState, now time.Time, signIn, joinDefault, anyOrigin bool) *Error {
	if err := s.admitLocked(user); err != nil {
		return err
	}
	s.sessions.Delete(c.token)
	token, key := s.sessions.Issue(user, c.origin, anyOrigin, now)
	c.token = key
	c.setService([32]byte{}, false)
	// A sign-in that proved a credential notes it after (provedLocked). A
	// device token the connection signed in with stays its own, so that
	// replacing that token still closes it.
	c.credential = ""
	s.switchUserLocked(c, reply, user, token, signIn, joinDefault)
	return nil
}

// parseRequested parses the name and user_id that an auth request may request
// for a new account.
func parseRequested(reply server.Reply[apron.AuthResult]) (name, requested string, err *Error) {
	d := authParams(reply).Decoder()
	name = d.String("name", optional)
	requested = d.String("user_id", optional)
	if err := paramError(d.Err()); err != nil {
		return "", "", err
	}
	return name, requested, nil
}

// authenticateToken resumes a sign-in through the protocol's token scheme
// (§3.2), signs in with a device token (devices.go), or approves a
// proposal with its temporary token (§4.11.2). Called holding s.mu.
func (s *Server) authenticateToken(c *client, reply server.Reply[apron.AuthResult]) *Error {
	// Any token auth ends the schemes' pending work on the connection, such
	// as a passkey ceremony, whether or not it signs in.
	c.authStates = nil
	now := time.Now()
	d := authParams(reply).Decoder()
	token := d.String("token", required)
	if d.Err() == nil && token == "" {
		d.Fail(invalidParams("token must be a non-empty string"))
	}
	if err := paramError(d.Err()); err != nil {
		return err
	}
	if strings.HasPrefix(token, devices.Prefix) {
		return s.authenticateDeviceLocked(c, reply, token)
	}
	if strings.HasPrefix(token, serviceTokenPrefix) {
		return s.authenticateServiceLocked(c, reply, token)
	}
	s.sessions.Prune(now)
	key := sha256.Sum256([]byte(token))
	session, ok := s.sessions.Get(key)
	if !ok || !session.AnyOrigin && session.Origin != c.origin || !now.Before(session.Expires) {
		// A temporary token approves its proposal with this scheme too, as
		// a client presents the token of a link that isn't its own
		// (§4.11.2), with the proposal's state or without (§4.11.3).
		state, err := parseString(authParams(reply), "state", optional)
		if err != nil {
			return err
		}
		if s.mayApproveLocked(c, token, state) {
			name, requested, err := parseRequested(reply)
			if err != nil {
				return err
			}
			return s.approveLocked(c, reply, token, state, name, requested)
		}
		return &Error{Code: CodeDenied, Message: "Session expired; sign in again"}
	}
	// Each successful resume renews the session for a full lifetime, so an
	// active user is never forced back through a ceremony. The token itself is
	// not rotated, since several tabs may share one persisted token: the
	// result carries the presented token, which clients keep as the latest.
	// A refused sign-in changes nothing (§3.2.2).
	if err := s.admitLocked(session.User); err != nil {
		return err
	}
	session.Expires = now.Add(sessions.Lifetime)
	s.sessions.Put(key, session)
	c.token = key
	c.credential = session.Credential
	c.setService([32]byte{}, false)
	// Resuming the user the connection is signed in as is no sign-in
	// (§4.5), and sends nothing after its result.
	s.switchUserLocked(c, reply, session.User, token, c.user != session.User, false)
	return nil
}
