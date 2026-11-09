// Command private is an Apron server that only invited people can use:
// nothing on it is visible to anyone else. An extension restricts sign-in to
// an allowlist, so there are no guests, a sign-in by email or another
// method succeeds only for an address or identity on the list, and
// connections that are not signed in learn only how to sign in. Admins
// manage the list from the chat with /allow, /disallow, and /allowed, and
// taking someone off it signs them out everywhere, as does a restart
// without their --private.allow entry. /invite makes an invitation, which
// lets one new person make an account with a passkey, without an address
// on the list; /disallow user:<user_id> takes that back too.
//
// Run it with email sign-in, and yourself on the list and an admin:
//
//	go run ./examples/private --public-url https://chat.example \
//	  --origin https://chat.example \
//	  --email.enable --email.from chat@example.com \
//	  --email.smtp-addr smtp.example.com:587 \
//	  --private.allow you@example.com --role admin=you@example.com
//
// An entry is an email address, user:<user_id> for an existing account (such
// as one that signs in with a passkey), or external:<subject> for an
// external sign-in's identity, such as the sso example's github:<id>.
package main

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/aproncmd"
)

func main() { aproncmd.Main(&Private{}) }

// Private keeps the server's allowlist: the entries given as flags, and
// those admins add with /allow, which the store keeps.
type Private struct {
	Settings struct {
		Allow []string `long:"allow" description:"Who may sign in: an email address, user:<user_id>, or external:<subject>; repeat for more"`
	}
	host      *apron.Host
	allowlist *apron.Allowlist
	flagged   map[apron.Principal]bool
	// mu keeps each change to the list and its stored copy together.
	mu sync.Mutex
}

func (*Private) Name() string { return "private" }
func (p *Private) Flags() any { return &p.Settings }

// allowedKeys keep the entries admins add: allowed/<principal>.
const allowedKeys = "allowed/"

func (p *Private) Init(r *apron.Registrar) error {
	p.host = r.Host
	p.allowlist = r.RestrictSignIn()
	p.flagged = make(map[apron.Principal]bool)
	var principals []apron.Principal
	for _, entry := range p.Settings.Allow {
		principal, err := apron.ParsePrincipal(entry)
		if err != nil {
			return fmt.Errorf("--private.allow: %w", err)
		}
		p.flagged[principal] = true
		principals = append(principals, principal)
	}
	for _, key := range r.Store().Keys(allowedKeys) {
		principal, err := apron.ParsePrincipal(strings.TrimPrefix(key, allowedKeys))
		if err != nil {
			return fmt.Errorf("stored entry %q: %w", key, err)
		}
		principals = append(principals, principal)
	}
	if err := p.allowlist.Allow(principals...); err != nil {
		return err
	}
	if len(principals) == 0 {
		r.Logger().Warn("nobody may sign in; add someone with --private.allow")
	}

	admin := func(u apron.User, _ apron.Room) bool { return u.HasRole(apron.RoleAdmin) }
	r.Command(apron.Command{
		Name: "allow", Usage: "/allow <address|user:id|external:subject>", Help: "let someone sign in",
		Available: admin, Run: p.allow,
	})
	r.Command(apron.Command{
		Name: "disallow", Usage: "/disallow <address|user:id|external:subject>", Help: "sign someone out, and keep them out",
		Available: admin, Run: p.disallow,
	})
	r.Command(apron.Command{
		Name: "allowed", Help: "list who may sign in",
		Available: admin, Run: p.list,
	})
	r.Command(apron.Command{
		Name: "invite", Usage: "/invite [user_id]", Help: "invite one person to make an account with a passkey",
		Available: admin, Run: p.invite,
	})
	return nil
}

func (p *Private) allow(_ context.Context, call *apron.CommandCall) (map[string]any, error) {
	principal, err := apron.ParsePrincipal(call.Args)
	if err != nil {
		return nil, apron.InvalidParams("Usage: /allow <address|user:id|external:subject>")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.host.Store().Put(allowedKeys+principal.String(), true); err != nil {
		return nil, err
	}
	if err := p.allowlist.Allow(principal); err != nil {
		return nil, err
	}
	call.Notice(fmt.Sprintf("`%s` may sign in.", principal))
	return nil, nil
}

func (p *Private) disallow(_ context.Context, call *apron.CommandCall) (map[string]any, error) {
	principal, err := apron.ParsePrincipal(call.Args)
	if err != nil {
		return nil, apron.InvalidParams("Usage: /disallow <address|user:id|external:subject>")
	}
	if p.flagged[principal] {
		return nil, apron.Denied(fmt.Sprintf("`%s` is allowed by --private.allow; remove it there, and restart", principal))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.host.Store().Delete(allowedKeys + principal.String()); err != nil {
		return nil, err
	}
	p.allowlist.Disallow(principal)
	call.Notice(fmt.Sprintf("`%s` may no longer sign in, and is signed out.", principal))
	return nil, nil
}

func (p *Private) list(_ context.Context, call *apron.CommandCall) (map[string]any, error) {
	var b strings.Builder
	b.WriteString("May sign in:\n")
	for _, principal := range p.allowlist.Principals() {
		note := ""
		if p.flagged[principal] {
			note = " (--private.allow)"
		}
		fmt.Fprintf(&b, "- `%s`%s\n", principal, note)
	}
	call.Notice(b.String())
	return nil, nil
}

func (p *Private) invite(ctx context.Context, call *apron.CommandCall) (map[string]any, error) {
	userID := strings.TrimSpace(call.Args)
	invitation, err := p.host.Invite(ctx, apron.InvitationOptions{UserID: userID})
	if err != nil && userID != "" {
		return nil, apron.InvalidParams("Usage: /invite [user_id]")
	}
	if err != nil {
		return nil, err
	}
	call.Notice(fmt.Sprintf("This invitation makes one account with a passkey until %s:\n\n`%s`",
		invitation.Expires.UTC().Format("2006-01-02 15:04 MST"), invitation.Token))
	return nil, nil
}
