package apron

import (
	"time"

	"github.com/apron-chat/apron-server-go/internal/authn"
)

// Remembered consent. The external sign-in page (§4.12.3) may skip its
// confirmation, and return at once, when the account it signs in to
// allowed an app at the same https redirect host before. An account keeps
// those hosts, with when each was last allowed, with its other data
// (storedUser.Consents), at most maxConsents of them, and forgets them all
// when it signs out everywhere: its other sessions (account.sign_out with
// others), or Server.SignOut and anything else that signs it out
// (signOutLocked). The page never remembers an app's scheme or a loopback
// redirect, which any app on the device can claim.

// maxConsents bounds the hosts an account keeps; allowing another forgets
// the one allowed longest ago.
const maxConsents = 32

// Consented reports whether the account of an identity of the scheme
// allowed an app at host before.
func (sc schemeCore) Consented(subject, host string) bool {
	s := sc.s
	s.mu.RLock()
	defer s.mu.RUnlock()
	u := s.accounts[authn.Identity{Scheme: sc.scheme.Name(), Subject: subject}]
	if u == nil || host == "" {
		return false
	}
	_, ok := u.consents[host]
	return ok
}

// Consent remembers that the account of an identity of the scheme allowed
// an app at host.
func (sc schemeCore) Consent(subject, host string) {
	s := sc.s
	s.mu.Lock()
	defer s.unlock()
	u := s.accounts[authn.Identity{Scheme: sc.scheme.Name(), Subject: subject}]
	if u == nil || host == "" {
		return
	}
	if u.consents == nil {
		u.consents = make(map[string]int64)
	}
	u.consents[host] = time.Now().UnixMilli()
	for len(u.consents) > maxConsents {
		oldest := ""
		for h, at := range u.consents {
			if oldest == "" || at < u.consents[oldest] {
				oldest = h
			}
		}
		delete(u.consents, oldest)
	}
	s.touchUser(u.id)
}

// forgetConsentsLocked forgets the apps u allowed.
func (s *Server) forgetConsentsLocked(u *userState) {
	if len(u.consents) > 0 {
		u.consents = nil
		s.touchUser(u.id)
	}
}
