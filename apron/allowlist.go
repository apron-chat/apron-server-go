package apron

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// A server whose sign-in an extension restricts (Registrar.RestrictSignIn)
// admits only accounts on the extension's Allowlist, and no guests. The
// server checks the list itself, under its lock, wherever a connection
// signs in or an account is made, so no extension code runs there: an
// email sign-in, an external sign-in, a passkey, a session token's resume,
// and a device token. Taking an entry off the list signs its accounts out
// everywhere.

// Principal names who may sign in: an email address, an external sign-in's
// identity, or an existing account's user_id.
type Principal struct {
	kind  string
	value string
}

// Email names whoever proves they hold an email address (§4.11.4), and the
// account with that address.
func Email(address string) Principal {
	return Principal{"email", strings.ToLower(strings.TrimSpace(address))}
}

// ExternalID names the identity an external sign-in proves, as
// Identity.Subject, and the account with it.
func ExternalID(subject string) Principal { return Principal{"external", subject} }

// UserID names an existing account by its user_id. It never admits a new
// account: anyone may request a user_id.
func UserID(id string) Principal { return Principal{"user", id} }

// String writes the principal as ParsePrincipal reads it: email:<address>,
// external:<subject>, or user:<user_id>.
func (p Principal) String() string { return p.kind + ":" + p.value }

// ParsePrincipal reads email:<address>, external:<subject>, or
// user:<user_id>; text with an @ and no such prefix is an address.
func ParsePrincipal(text string) (Principal, error) {
	text = strings.TrimSpace(text)
	kind, value, found := strings.Cut(text, ":")
	switch {
	case found && kind == "email":
		return Email(value), Principal.check(Email(value))
	case found && kind == "external":
		return ExternalID(value), Principal.check(ExternalID(value))
	case found && kind == "user":
		return UserID(value), Principal.check(UserID(value))
	case strings.Contains(text, "@"):
		return Email(text), Principal.check(Email(text))
	}
	return Principal{}, fmt.Errorf("apron: %q is not email:<address>, external:<subject>, or user:<user_id>", text)
}

func (p Principal) check() error {
	switch {
	case p.kind == "email" && normalizeEmail(p.value) == "":
		return fmt.Errorf("apron: %q is not an email address", p.value)
	case p.kind == "external" && p.value == "":
		return errors.New("apron: an external identity is not empty")
	case p.kind == "user" && !validID(p.value):
		return fmt.Errorf("apron: %q is not a user_id", p.value)
	case p.kind == "":
		return errors.New("apron: the zero Principal names nobody")
	}
	return nil
}

// Allowlist is who may sign in to a server whose sign-in an extension
// restricted. Only that extension holds it.
type Allowlist struct{ s *Server }

// allowlist is the server's: the principals that may sign in.
type allowlist struct {
	host       *Host
	principals map[Principal]bool
}

// RestrictSignIn makes the server admit only the accounts on the returned
// Allowlist, which starts empty, and no guests; one extension may. Before a
// connection signs in, its server frame then holds only what signing in
// needs, and the full one follows the sign-in (§3.1). Bots are the
// extensions' own, and not restricted.
func (r *Registrar) RestrictSignIn() *Allowlist {
	r.register("restricted sign-in", func() error {
		if r.s.allowlist != nil {
			return fmt.Errorf("extension %s restricts sign-in already", r.s.allowlist.host.name)
		}
		r.s.allowlist = &allowlist{host: r.Host, principals: make(map[Principal]bool)}
		return nil
	})
	return &Allowlist{s: r.s}
}

// Allow lets the principals sign in.
func (a *Allowlist) Allow(principals ...Principal) error {
	for _, p := range principals {
		if err := p.check(); err != nil {
			return err
		}
	}
	s := a.s
	s.mu.Lock()
	defer s.unlock()
	for _, p := range principals {
		s.allowlist.principals[p] = true
	}
	return nil
}

// Disallow takes the principals off the list, and signs out the accounts
// they let in that nothing else on it does: their connections close, and
// their session tokens, device tokens, and push registrations are
// forgotten.
func (a *Allowlist) Disallow(principals ...Principal) {
	s := a.s
	s.mu.Lock()
	defer s.unlock()
	for _, p := range principals {
		delete(s.allowlist.principals, p)
	}
	for _, u := range s.users {
		if u.account() && !s.admittedLocked(u) {
			s.signOutLocked(u)
		}
	}
}

// Principals lists who is on the list, sorted.
func (a *Allowlist) Principals() []Principal {
	s := a.s
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.SortedFunc(maps.Keys(s.allowlist.principals), func(x, y Principal) int {
		return cmp.Or(cmp.Compare(x.kind, y.kind), cmp.Compare(x.value, y.value))
	})
}

// guestsAllowedLocked reports whether guests may sign in.
func (s *Server) guestsAllowedLocked() bool {
	return !s.config.DisableGuests && s.allowlist == nil
}

// admittedLocked reports whether the account may sign in: always, unless
// sign-in is restricted, and then when the list names it. An extension's
// bot always may; a guest never may on a restricted server.
func (s *Server) admittedLocked(u *userState) bool {
	switch {
	case s.allowlist == nil, u.botOf != "":
		return true
	case !u.account():
		return false
	}
	allowed := s.allowlist.principals
	return allowed[UserID(u.id)] || (u.email != "" && allowed[Email(u.email)]) || (u.external != "" && allowed[ExternalID(u.external)])
}

// admitLocked refuses a sign-in to an account the list does not name.
func (s *Server) admitLocked(u *userState) *Error {
	if s.admittedLocked(u) {
		return nil
	}
	return notAdmitted()
}

// admitsNewLocked reports whether a new account may be made for an email
// address or an external identity, whichever is set.
func (s *Server) admitsNewLocked(email, subject string) bool {
	switch {
	case s.allowlist == nil:
		return true
	case email != "":
		return s.allowlist.principals[Email(email)]
	case subject != "":
		return s.allowlist.principals[ExternalID(subject)]
	}
	return false
}

func notAdmitted() *Error {
	return &Error{Code: CodeDenied, Message: "This account may not sign in to this server"}
}

// signOutLocked signs an account out everywhere: its connections close,
// and its session tokens, device tokens, and push registrations, which
// would go on delivering its messages, are forgotten.
func (s *Server) signOutLocked(u *userState) {
	for c := range u.clients {
		c.closeWithError(&Error{Code: CodeDenied, Message: "This account may no longer sign in to this server"})
	}
	for key, session := range s.sessions {
		if session.user == u {
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
	for _, registration := range u.pushes {
		s.removePushLocked(registration)
	}
}
