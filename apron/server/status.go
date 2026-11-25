package server

import (
	"context"

	"github.com/apron-chat/apron-server-go/apron"
)

// StatusHandler handles the status request, and adds capability status
// (§4.5). A backend sends the user's mutes to its connections as status
// notifications, and statuses as user notifications. The statuses it
// accepts with me, other than online and "", are ServerInfo.Status.
type StatusHandler interface {
	Status(ctx context.Context, c *Conn, p apron.StatusParams, reply Reply[apron.OK]) error
}
