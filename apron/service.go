package apron

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/apron-chat/apron-server-go/apron/internal/services"
	"github.com/apron-chat/apron-server-go/wire"
)

// Service tokens are a bot's credentials for a client outside the server's
// process, such as an agent in another language, which signs in as the bot
// with the token scheme. Unlike a session token, a service token's expiry
// is fixed when it is issued and never extended by use; unlike a device
// token, it expires. Its expiry also bounds the connections it signs in:
// they close when it expires, or when it is revoked, so revoking a token
// ends its access at once. Unless issued with ManageAccount, its
// connections may not manage the account (account.*, /invite-device) nor
// sign in again on the same connection. The bot's scope (BotOptions.Scope)
// applies to them as to its in-process connections. A services.Table
// keeps the tokens and their expiry; the server keeps who may hold one,
// what a sign-in with one does, and what revoking one closes.

const (
	serviceTokenPrefix = services.Prefix
	// DefaultServiceTokenLifetime is a service token's lifetime unless
	// ServiceTokenOptions.Lifetime says otherwise.
	DefaultServiceTokenLifetime = 30 * 24 * time.Hour
	// maxServiceTokens bounds a bot's service tokens.
	maxServiceTokens = services.MaxPerUser
)

// serviceToken is an issued service token, which a services.Table keeps by
// the SHA-256 of its token. Guarded by s.mu.
type serviceToken = services.Token[*userState]

// ServiceTokenOptions describe a service token.
type ServiceTokenOptions struct {
	// Name says what the token is for, such as the host it is deployed to.
	Name string
	// Lifetime is how long it signs in; zero is
	// DefaultServiceTokenLifetime. Use never extends it.
	Lifetime time.Duration
	// ManageAccount lets its connections manage the bot's account
	// (account.*, /invite-device); they may not otherwise.
	ManageAccount bool
}

// ServiceToken describes a service token.
type ServiceToken struct {
	// ID names the token, to rotate or revoke it; it is not the token.
	ID string
	// Token is what signs in: `{"scheme": "token", "token": "<Token>"}`.
	// IssueToken and RotateToken return it once, and the server keeps only
	// its hash, so ServiceTokens lists none.
	Token            string
	UserID, Name     string
	Created, Expires time.Time
	ManageAccount    bool
}

func describeService(t *serviceToken) ServiceToken {
	return ServiceToken{ID: t.ID, UserID: t.User.id, Name: t.Name, Created: t.Created, Expires: t.Expires, ManageAccount: t.Manage}
}

// IssueToken issues a service token that signs in as the bot from any
// origin, and returns it once it is stored. A bot holds at most ten.
func (b *Bot) IssueToken(ctx context.Context, options ServiceTokenOptions) (ServiceToken, error) {
	return b.s.issueServiceToken(ctx, b.userID, options, "", 0)
}

// RotateToken issues a token as IssueToken does that replaces the token
// named id, which goes on signing in for overlap more, if it would not
// expire first, so the client can move to the new one; zero overlap
// revokes it at once.
func (b *Bot) RotateToken(ctx context.Context, id string, overlap time.Duration, options ServiceTokenOptions) (ServiceToken, error) {
	if overlap < 0 {
		return ServiceToken{}, errors.New("apron: a rotation's overlap is not negative")
	}
	return b.s.issueServiceToken(ctx, b.userID, options, id, overlap)
}

// ServiceTokens lists the bot's service tokens, without the tokens.
func (b *Bot) ServiceTokens() []ServiceToken {
	s := b.s
	s.mu.RLock()
	defer s.mu.RUnlock()
	var list []ServiceToken
	for _, t := range s.services.All() {
		if t.User.id == b.userID {
			list = append(list, describeService(t))
		}
	}
	slices.SortFunc(list, func(a, b ServiceToken) int { return cmp.Or(a.Created.Compare(b.Created), cmp.Compare(a.ID, b.ID)) })
	return list
}

// RevokeToken revokes the bot's service token named id, and returns once
// that is stored: it signs in no more, and the connections it signed in
// close. It reports whether there was such a token.
func (b *Bot) RevokeToken(ctx context.Context, id string) (bool, error) {
	s := b.s
	s.mu.Lock()
	found := false
	for key, t := range s.services.All() {
		if t.ID == id && t.User.id == b.userID {
			s.revokeServiceLocked(key, "This service token was revoked")
			found = true
		}
	}
	f := s.newFenceLocked()
	s.unlock()
	return found, f.Wait(ctx)
}

func (s *Server) issueServiceToken(ctx context.Context, userID string, options ServiceTokenOptions, replaces string, overlap time.Duration) (ServiceToken, error) {
	lifetime := cmp.Or(options.Lifetime, DefaultServiceTokenLifetime)
	if lifetime < 0 {
		return ServiceToken{}, errors.New("apron: a service token's lifetime is positive")
	}
	if len(options.Name) > services.MaxNameBytes {
		return ServiceToken{}, fmt.Errorf("apron: a service token's name is at most %d bytes", services.MaxNameBytes)
	}
	token, key, err := services.NewToken()
	if err != nil {
		return ServiceToken{}, err
	}
	now := time.Now()
	s.mu.Lock()
	if s.store.Stopped() {
		s.unlock()
		return ServiceToken{}, ErrServerClosed
	}
	u := s.users.Get(userID)
	if u == nil {
		s.unlock()
		return ServiceToken{}, errors.New("apron: no such user")
	}
	// The limit counts the token being replaced as gone, and is checked
	// before anything changes.
	held, replacing := 0, false
	for _, t := range s.services.All() {
		switch {
		case t.User != u:
		case replaces != "" && t.ID == replaces:
			replacing = true
		default:
			held++
		}
	}
	if replaces != "" && !replacing {
		s.unlock()
		return ServiceToken{}, fmt.Errorf("apron: no service token %q", replaces)
	}
	if held >= maxServiceTokens {
		s.unlock()
		return ServiceToken{}, fmt.Errorf("apron: a bot holds at most %d service tokens; revoke one", maxServiceTokens)
	}
	if replaces != "" {
		for oldKey, old := range s.services.All() {
			if old.ID != replaces || old.User != u {
				continue
			}
			if overlap == 0 {
				s.revokeServiceLocked(oldKey, "This service token was replaced")
			} else {
				s.services.Shorten(oldKey, now.Add(overlap))
			}
		}
	}
	t := &serviceToken{
		ID: services.NewID(), User: u, Name: options.Name,
		Created: now, Expires: now.Add(lifetime), Manage: options.ManageAccount,
	}
	s.services.Add(key, t)
	f := s.newFenceLocked()
	s.unlock()
	if err := f.Wait(ctx); err != nil {
		return ServiceToken{}, err
	}
	described := describeService(t)
	described.Token = token
	return described, nil
}

// serviceExpired revokes a token when it expires (services.Table).
func (s *Server) serviceExpired(key services.Key, t *serviceToken) {
	s.mu.Lock()
	defer s.unlock()
	if s.services.Get(key) == t {
		s.revokeServiceLocked(key, "This service token expired")
	}
}

// revokeServiceLocked forgets a service token and closes the connections it
// signed in.
func (s *Server) revokeServiceLocked(key [32]byte, why string) {
	t := s.services.Remove(key)
	if t == nil {
		return
	}
	for c := range t.User.clients {
		if c.service == key {
			c.closeWithError(&Error{Code: CodeDenied, Message: why})
		}
	}
}

// revokeServicesOfLocked revokes every service token of u's.
func (s *Server) revokeServicesOfLocked(u *userState) {
	for key, t := range s.services.All() {
		if t.User == u {
			s.revokeServiceLocked(key, "This account was signed out")
		}
	}
}

// authenticateServiceLocked signs the connection in with a service token,
// from any origin.
func (s *Server) authenticateServiceLocked(c *client, reply wire.Reply[wire.AuthResult], token string) *Error {
	key := sha256.Sum256([]byte(token))
	t := s.services.Get(key)
	if t == nil || !time.Now().Before(t.Expires) {
		return &Error{Code: CodeDenied, Message: "This service token is not valid"}
	}
	if err := s.admitLocked(t.User); err != nil {
		return err
	}
	c.setService(key, t.Manage)
	// It signs in with no session, device token, or credential.
	c.token, c.device, c.credential = [32]byte{}, [32]byte{}, ""
	s.switchUserLocked(c, reply, t.User, "", c.user != t.User, false)
	return nil
}

// setService notes the service token the connection signed in with, zero
// for none, and whether it may manage its account. A restricted one has a
// deduplication scope of its own (wire.Conn.SetDedupScope): a retry of
// another connection's request (§1.2) runs again on it, so that it never
// gets the outcome of what the handler refuses it, such as an account
// method's.
func (c *client) setService(key [32]byte, manage bool) {
	c.service, c.serviceManage = key, manage
	scope := ""
	if c.restrictedService() {
		scope = "restricted-service"
	}
	c.conn.SetDedupScope(scope)
}

// restrictedService reports whether the connection signed in with a service
// token that may not manage its account.
func (c *client) restrictedService() bool {
	return c.service != [32]byte{} && !c.serviceManage
}

// serviceRefusal refuses what a restricted service connection may not do.
func serviceRefusal() *Error {
	return &Error{Code: CodeDenied, Message: "A service token may not manage its account"}
}
