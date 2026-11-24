package apron

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/apron-chat/apron-server-go/apron/internal/testhook"
	"github.com/apron-chat/apron-server-go/internal/auth/email"
	"github.com/apron-chat/apron-server-go/internal/authn"
	"github.com/apron-chat/apron-server-go/wire"
)

// The hooks package aprontest calls, for what a test does in process that
// no client or extension can (apron/internal/testhook).
func init() {
	testhook.Settle = func(server any, ctx context.Context) error {
		return server.(*Server).settle(ctx)
	}
	testhook.Member = func(server any, ctx context.Context, options any, address string) (string, string, error) {
		return server.(*Server).member(ctx, options.(InvitationOptions), address)
	}
	testhook.MessageParams = func(roomID, text, extension string, options any) (wire.MessageParams, error) {
		p := newPost(roomID, text, extension)
		for _, option := range options.([]PostOption) {
			option.applyPost(p)
		}
		return p.params, p.check()
	}
}

// member makes an account as an invitation with options makes one, with
// the email address as its identity, as if it had signed in with it, and
// returns its user_id and a session token that resumes it from any origin,
// once the account is stored.
func (s *Server) member(ctx context.Context, options InvitationOptions, address string) (string, string, error) {
	invited, err := s.invite(ctx, "", options)
	if err != nil {
		return "", "", err
	}
	now := time.Now()
	id := authn.Identity{Scheme: email.Name, Subject: strings.ToLower(address)}
	s.mu.Lock()
	inv := s.invitations.Presented(invited.Token, now)
	switch {
	case inv == nil:
		s.unlock()
		return "", "", errors.New("apron: the invitation was used up")
	case s.users.ByIdentity(id) != nil:
		s.invitations.Use(invited.Token)
		s.unlock()
		return "", "", errors.New("apron: " + address + " belongs to an account")
	}
	user := s.enrollAccountLocked(inv, invited.Token, id, nil, authn.Proof{}, now)
	s.joinDefaultRoomLocked(user)
	s.joinInvitedLocked(user, inv)
	token, _, sessionErr := s.newSessionLocked(user, "", true, now)
	f := s.newFenceLocked()
	s.unlock()
	if sessionErr != nil {
		return "", "", sessionErr
	}
	if err := f.Wait(ctx); err != nil {
		return "", "", err
	}
	return user.id, token, nil
}
