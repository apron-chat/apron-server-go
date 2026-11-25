// Package testhook lets package aprondtest do in a test what no client or
// extension can, without adding to package aprond's API: wait for the
// extensions' followers, make an account with roles, and build a message
// request from aprond's post options. Package aprond sets each hook as it
// is initialized, so a program that imports aprondtest imports aprond, and
// finds them set. A hook takes the *aprond.Server as server, which this
// package cannot name.
package testhook

import (
	"context"

	"github.com/apron-chat/apron-server-go/apron"
)

var (
	// Settle waits until every follower of the server's log has handled
	// every event logged before it returns (aprond's settle).
	Settle func(server any, ctx context.Context) error

	// Member makes an account as one an invitation with options makes
	// (aprond.InvitationOptions), with address as its email identity, and
	// returns its user_id and a session token that signs it in from any
	// origin, once the account is stored.
	Member func(server any, ctx context.Context, options any, address string) (userID, token string, err error)

	// MessageParams are the params of a message request that posts text to
	// the room with options ([]aprond.PostOption), as a Session's Post sends
	// them, with aprond.WithExt's value under extension's name.
	MessageParams func(roomID, text, extension string, options any) (apron.MessageParams, error)
)
