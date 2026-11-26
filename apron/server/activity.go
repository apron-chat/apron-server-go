package server

import (
	"context"

	"github.com/apron-chat/apron-server-go/apron"
)

// ReactionHandler handles reactions, and adds capability reactions (§4.7).
// It broadcasts the reaction set, including to c, before it replies.
type ReactionHandler interface {
	React(ctx context.Context, c *Conn, p apron.ReactParams, reply Reply[apron.OK]) error
}

// ActivityHandler handles activity notifications, and adds capability
// activity (§4.6). They get no reply: params that are not valid are
// ignored. A backend relays the activity it applies as Activity
// notifications.
type ActivityHandler interface {
	Activity(ctx context.Context, c *Conn, p apron.ActivityParams)
}
