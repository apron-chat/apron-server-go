package apron

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
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
// applies to them as to its in-process connections.

const (
	serviceTokenPrefix = "apron_service_"
	entryService       = "service"
	// DefaultServiceTokenLifetime is a service token's lifetime unless
	// ServiceTokenOptions.Lifetime says otherwise.
	DefaultServiceTokenLifetime = 30 * 24 * time.Hour
	// maxServiceTokens bounds a bot's service tokens.
	maxServiceTokens    = 10
	maxServiceNameBytes = 64
)

// serviceToken is an issued service token, kept by the SHA-256 of its
// token. Guarded by s.mu.
type serviceToken struct {
	id      string
	user    *userState
	name    string
	created time.Time
	expires time.Time
	manage  bool
	timer   *time.Timer
}

type storedService struct {
	ID      string    `json:"id"`
	User    string    `json:"user"`
	Name    string    `json:"name,omitzero"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
	Manage  bool      `json:"manage,omitzero"`
}

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

func (t *serviceToken) describe() ServiceToken {
	return ServiceToken{ID: t.id, UserID: t.user.id, Name: t.name, Created: t.created, Expires: t.expires, ManageAccount: t.manage}
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
	for _, t := range s.services {
		if t.user.id == b.userID {
			list = append(list, t.describe())
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
	for key, t := range s.services {
		if t.id == id && t.user.id == b.userID {
			s.revokeServiceLocked(key, "This service token was revoked")
			found = true
		}
	}
	f := s.newFenceLocked()
	s.unlock()
	return found, waitFence(ctx, f)
}

func (s *Server) issueServiceToken(ctx context.Context, userID string, options ServiceTokenOptions, replaces string, overlap time.Duration) (ServiceToken, error) {
	lifetime := cmp.Or(options.Lifetime, DefaultServiceTokenLifetime)
	if lifetime < 0 {
		return ServiceToken{}, errors.New("apron: a service token's lifetime is positive")
	}
	if len(options.Name) > maxServiceNameBytes {
		return ServiceToken{}, fmt.Errorf("apron: a service token's name is at most %d bytes", maxServiceNameBytes)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return ServiceToken{}, err
	}
	token := serviceTokenPrefix + base64.RawURLEncoding.EncodeToString(secret)
	key := sha256.Sum256([]byte(token))
	now := time.Now()
	s.mu.Lock()
	if s.storeClosed {
		s.unlock()
		return ServiceToken{}, ErrServerClosed
	}
	u := s.users[userID]
	if u == nil {
		s.unlock()
		return ServiceToken{}, errors.New("apron: no such user")
	}
	if replaces != "" {
		found := false
		for oldKey, old := range s.services {
			if old.id != replaces || old.user != u {
				continue
			}
			found = true
			if overlap == 0 {
				s.revokeServiceLocked(oldKey, "This service token was replaced")
			} else if until := now.Add(overlap); until.Before(old.expires) {
				old.expires = until
				s.scheduleServiceLocked(oldKey, old)
				s.dirty.services[oldKey] = true
			}
		}
		if !found {
			s.unlock()
			return ServiceToken{}, fmt.Errorf("apron: no service token %q", replaces)
		}
	}
	held := 0
	for _, t := range s.services {
		if t.user == u {
			held++
		}
	}
	if held >= maxServiceTokens {
		s.unlock()
		return ServiceToken{}, fmt.Errorf("apron: a bot holds at most %d service tokens; revoke one", maxServiceTokens)
	}
	t := &serviceToken{
		id: "svc_" + strings.ToLower(rand.Text()[:16]), user: u, name: options.Name,
		created: now, expires: now.Add(lifetime), manage: options.ManageAccount,
	}
	s.services[key] = t
	s.scheduleServiceLocked(key, t)
	s.dirty.services[key] = true
	f := s.newFenceLocked()
	s.unlock()
	if err := waitFence(ctx, f); err != nil {
		return ServiceToken{}, err
	}
	described := t.describe()
	described.Token = token
	return described, nil
}

// scheduleServiceLocked revokes a token when it expires.
func (s *Server) scheduleServiceLocked(key [32]byte, t *serviceToken) {
	if t.timer != nil {
		t.timer.Stop()
	}
	t.timer = time.AfterFunc(time.Until(t.expires), func() {
		s.mu.Lock()
		defer s.unlock()
		if s.services[key] == t {
			s.revokeServiceLocked(key, "This service token expired")
		}
	})
}

// revokeServiceLocked forgets a service token and closes the connections it
// signed in.
func (s *Server) revokeServiceLocked(key [32]byte, why string) {
	t := s.services[key]
	if t == nil {
		return
	}
	if t.timer != nil {
		t.timer.Stop()
	}
	delete(s.services, key)
	s.dirty.services[key] = true
	for c := range t.user.clients {
		if c.service == key {
			c.closeWithError(&Error{Code: CodeDenied, Message: why})
		}
	}
}

// revokeServicesOfLocked revokes every service token of u's.
func (s *Server) revokeServicesOfLocked(u *userState) {
	for key, t := range s.services {
		if t.user == u {
			s.revokeServiceLocked(key, "This account was signed out")
		}
	}
}

// authenticateServiceLocked signs the connection in with a service token,
// from any origin.
func (s *Server) authenticateServiceLocked(c *client, req request, token string) (any, *Error) {
	key := sha256.Sum256([]byte(token))
	t := s.services[key]
	if t == nil || !time.Now().Before(t.expires) {
		return nil, &Error{Code: CodeDenied, Message: "This service token is not valid"}
	}
	if err := s.admitLocked(t.user); err != nil {
		return nil, err
	}
	c.service, c.serviceManage = key, t.manage
	return s.switchUserLocked(c, req, t.user, nil, c.user != t.user, false), nil
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

// serviceEntriesLocked renders the service tokens a dirty set names.
func (s *Server) serviceEntriesLocked(d dirtySet, put func(kind, id string, value any), del func(kind, id string)) {
	for key := range d.services {
		if t := s.services[key]; t != nil {
			put(entryService, hex.EncodeToString(key[:]), storedService{
				ID: t.id, User: t.user.id, Name: t.name, Created: t.created, Expires: t.expires, Manage: t.manage,
			})
		} else {
			del(entryService, hex.EncodeToString(key[:]))
		}
	}
}

// restoreServicesLocked keeps the stored service tokens that have not
// expired, of accounts that exist.
func (s *Server) restoreServicesLocked(entries map[string]jsontext.Value, now time.Time) error {
	for id, raw := range entries {
		key, err := hex.DecodeString(id)
		if err != nil || len(key) != 32 {
			continue
		}
		var stored storedService
		if err := json.Unmarshal(raw, &stored, decodeOptions); err != nil {
			return fmt.Errorf("stored service token %s: %w", id, err)
		}
		u := s.users[stored.User]
		if u == nil || !now.Before(stored.Expires) {
			s.dirty.services[[32]byte(key)] = true
			continue
		}
		t := &serviceToken{id: stored.ID, user: u, name: stored.Name, created: stored.Created, expires: stored.Expires, manage: stored.Manage}
		s.services[[32]byte(key)] = t
		s.scheduleServiceLocked([32]byte(key), t)
	}
	return nil
}
