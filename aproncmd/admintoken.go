package aproncmd

import (
	"cmp"
	"fmt"
	"os"
	"slices"

	"github.com/apron-chat/apron-server-go/apron"
)

// adminTokenUser is the account --admin-token-file signs in as.
const adminTokenUser = "admin"

// adminToken is --admin-token-file: an account, admin, which a test
// harness signs in as, with a service token written to a file at each
// start. The account is a bot of this extension's, which Config.Roles
// makes an admin.
type adminToken struct{ path string }

func (*adminToken) Name() string { return "admin-token" }

func (a *adminToken) Init(r *apron.Registrar) error {
	bot, err := r.Bot(adminTokenUser, apron.BotOptions{Name: "Admin"})
	if err != nil {
		return fmt.Errorf("--admin-token-file: %w", err)
	}
	// A bot holds at most ten tokens: each start replaces the oldest.
	tokens := bot.ServiceTokens()
	slices.SortFunc(tokens, func(x, y apron.ServiceToken) int { return cmp.Compare(x.Created.UnixNano(), y.Created.UnixNano()) })
	for len(tokens) >= 9 {
		if _, err := bot.RevokeToken(r.Context(), tokens[0].ID); err != nil {
			return err
		}
		tokens = tokens[1:]
	}
	token, err := bot.IssueToken(r.Context(), apron.ServiceTokenOptions{Name: "--admin-token-file"})
	if err != nil {
		return fmt.Errorf("--admin-token-file: %w", err)
	}
	if err := os.WriteFile(a.path, []byte(token.Token+"\n"), 0o600); err != nil {
		return fmt.Errorf("--admin-token-file: %w", err)
	}
	r.Logger().Info("wrote a token that signs in as admin", "file", a.path)
	return nil
}
