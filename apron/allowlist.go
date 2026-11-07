package apron

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/apron-chat/apron-server-go/internal/auth/email"
	"github.com/apron-chat/apron-server-go/internal/auth/external"
	"github.com/apron-chat/apron-server-go/internal/auth/passkey"
	"github.com/apron-chat/apron-server-go/internal/authn"
)

// A server whose sign-in an extension restricts (Registrar.RestrictSignIn)
// admits only accounts on the extension's Allowlist, and no guests. The
// server checks the list itself, under its lock, wherever a connection
// signs in or an account is made, so no extension code runs there: an
// email sign-in, an external sign-in, a passkey, a session token's resume,
// and a device token. Taking an entry off the list signs its accounts out
// everywhere.

// Principal names an account, or who may make one: by an identity of a
// scheme, such as an email address, or by an existing account's user_id.
// The allowlist and Config.Roles name accounts by principals.
type Principal struct {
	// kind is a scheme's name, or principalUser.
	kind  string
	value string
}

// principalUser is the kind of a Principal that names a user_id.
const principalUser = "user"

// principalKinds are the kinds a Principal may have: the schemes the
// server knows, and user_ids.
var principalKinds = []string{email.Name, external.Name, passkey.Name, principalUser}

// Email names whoever proves they hold an email address (§4.11.4), and the
// account with that address.
func Email(address string) Principal {
	return Principal{email.Name, strings.ToLower(strings.TrimSpace(address))}
}

// ExternalID names the identity an external sign-in proves, as
// Identity.Subject, and the account with it.
func ExternalID(subject string) Principal { return Principal{external.Name, subject} }

// UserID names an existing account by its user_id, ignoring case, as
// user_ids are unique. It never admits a new account: anyone may request a
// user_id.
func UserID(id string) Principal { return Principal{principalUser, strings.ToLower(id)} }

// principalOf names an identity, as the allowlist and Config.Roles match
// it.
func principalOf(id authn.Identity) Principal { return Principal{id.Scheme, id.Subject} }

// String writes the principal as ParsePrincipal reads it: email:<address>,
// external:<subject>, webauthn:<user handle>, or user:<user_id>.
func (p Principal) String() string { return p.kind + ":" + p.value }

// ParsePrincipal reads email:<address>, external:<subject>,
// webauthn:<user handle>, or user:<user_id>; text with an @ and no such
// prefix is an address.
func ParsePrincipal(text string) (Principal, error) {
	text = strings.TrimSpace(text)
	if kind, value, found := strings.Cut(text, ":"); found && slices.Contains(principalKinds, kind) {
		p := newPrincipal(kind, value)
		return p, p.check()
	}
	if strings.Contains(text, "@") {
		p := Email(text)
		return p, p.check()
	}
	return Principal{}, fmt.Errorf("apron: %q is not email:<address>, external:<subject>, webauthn:<user handle>, or user:<user_id>", text)
}

// newPrincipal names value of a kind, normalized as that kind's are.
func newPrincipal(kind, value string) Principal {
	switch kind {
	case email.Name:
		return Email(value)
	case principalUser:
		return UserID(value)
	}
	return Principal{kind, value}
}

// roleHolder reads a holder of a role in Config.Roles: a principal as
// ParsePrincipal reads it, or a bare user_id or email address, which names
// an account by its user_id, and by its address too when it is one.
func roleHolder(text string) ([]Principal, error) {
	text = strings.TrimSpace(text)
	if kind, _, found := strings.Cut(text, ":"); found && slices.Contains(principalKinds, kind) {
		p, err := ParsePrincipal(text)
		return []Principal{p}, err
	}
	var holders []Principal
	if address := Email(text); address.check() == nil {
		holders = append(holders, address)
	}
	if id := UserID(text); id.check() == nil {
		holders = append(holders, id)
	}
	if holders == nil {
		return nil, fmt.Errorf("apron: role holder %q is not a user_id, an email address, or kind:subject", text)
	}
	return holders, nil
}

func (p Principal) check() error {
	switch {
	case p.kind == email.Name && email.Normalize(p.value) == "":
		return fmt.Errorf("apron: %q is not an email address", p.value)
	case p.kind == principalUser && !validID(p.value):
		return fmt.Errorf("apron: %q is not a user_id", p.value)
	case p.kind == "":
		return errors.New("apron: the zero Principal names nobody")
	case p.value == "":
		return fmt.Errorf("apron: a %s principal is not empty", p.kind)
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
//
// The list lives in memory: the extension fills it during Init, from its
// settings or its Store, and keeps it as it changes. Once every extension's
// Init has returned, the accounts the list does not admit are signed out,
// as when an entry was taken off it while the server was down.
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
	return s.listedLocked(u)
}

// admittedWithLocked reports whether an account, a guest's included, may
// sign in once it has the identity id too: the list names it, or the
// identity.
func (s *Server) admittedWithLocked(u *userState, id authn.Identity) bool {
	switch {
	case s.allowlist == nil, u.botOf != "":
		return true
	}
	return s.allowlist.principals[principalOf(id)] || s.listedLocked(u)
}

// listedLocked reports whether the list names an account, by its user_id or
// an identity.
func (s *Server) listedLocked(u *userState) bool {
	allowed := s.allowlist.principals
	if allowed[UserID(u.id)] {
		return true
	}
	for scheme, subject := range u.identities {
		if allowed[principalOf(authn.Identity{Scheme: scheme, Subject: subject})] {
			return true
		}
	}
	return false
}

// admitLocked refuses a sign-in to an account the list does not name.
func (s *Server) admitLocked(u *userState) *Error {
	if s.admittedLocked(u) {
		return nil
	}
	return notAdmitted()
}

// admitsNewLocked reports whether a new account may be made for an
// identity: one the list names, such as an email address.
func (s *Server) admitsNewLocked(id authn.Identity) bool {
	return s.allowlist == nil || s.allowlist.principals[principalOf(id)]
}

// startSignIn starts sign-in once the extensions are started, and every
// scheme is added: it checks that someone can sign in, hands the schemes
// their records, and signs out the accounts that a restricted sign-in does
// not admit, as when an entry was taken off the list while the server was
// down: the list is the one the extensions made during Init.
func (s *Server) startSignIn() error {
	s.mu.Lock()
	defer s.unlock()
	if err := s.restoreSchemesLocked(); err != nil {
		return fmt.Errorf("restoring the stored state: %w", err)
	}
	if !s.guestsAllowedLocked() && len(s.schemes) == 0 {
		return errors.New("nobody can sign in: without guests, the server needs a sign-in scheme, such as passkeys, email sign-in, or an extension's external sign-in")
	}
	if s.allowlist != nil {
		for _, u := range s.users {
			if u.account() && !s.admittedLocked(u) {
				s.signOutLocked(u)
			}
		}
	}
	return nil
}

func notAdmitted() *Error {
	return &Error{Code: CodeDenied, Message: "This account may not sign in to this server"}
}

// connectionClosing refuses a request on a connection that is being
// closed, which its account may no longer use.
func connectionClosing() *Error {
	return &Error{Code: CodeDenied, Message: "This connection is closing"}
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
