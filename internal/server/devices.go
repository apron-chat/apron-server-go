package server

import (
	"time"

	"golang.org/x/time/rate"
)

// deviceAddMethod names both the extension method that issues a bearer token
// for another device and the capability that offers it. It is apron-web's
// extension (its EXT.md §9), the way §4.10 lets a server issue more bearer
// tokens for a client that cannot use the account's passkeys.
const deviceAddMethod = "ext:device_add"

const (
	// Each account may add deviceAddBurst devices at once, refilled one
	// every deviceAddRefill, across all of its connections.
	deviceAddBurst  = 5
	deviceAddRefill = 12 * time.Minute
)

// addDevice issues a new bearer token for the sender's account, bound to
// this connection's origin like the token of a sign-in there (§4.10), and
// answers with it and that origin. The token is a session of its own: the
// connection keeps its token, and the new one resumes with `scheme: "token"`
// from a client that sends the same Origin.
func (s *Server) addDevice(c *client, req request) (any, bool, *rpcError) {
	s.mu.Lock()
	defer s.unlock()
	u := c.user
	if !u.account() {
		return nil, false, &rpcError{Code: codeDenied, Message: "Sign in with a passkey or email before adding a device"}
	}
	now := time.Now()
	if u.deviceAdds == nil {
		u.deviceAdds = rate.NewLimiter(rate.Every(deviceAddRefill), deviceAddBurst)
	}
	if delay := wait(u.deviceAdds, now); delay > 0 {
		return nil, false, retryAfter("Too many devices added; try again later", delay)
	}
	token, _, err := s.newSessionLocked(u, c.origin, now)
	if err != nil {
		return nil, false, err
	}
	u.deviceAdds.AllowN(now, 1)
	return map[string]any{"token": token, "origin": c.origin}, false, nil
}
