package server

import (
	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/internal/core"
)

// What this package's external tests (package server_test) use that its API
// leaves out.

// Conns returns the open connections, Local ones included.
func (s *Server) Conns() []*Conn { return s.openConns() }

// EncodeRaw renders a notification frame of any method and params.
func EncodeRaw(method string, params any) (apron.Frame, error) {
	return core.Marshal(map[string]any{"method": method, "params": params})
}

// NotifyRaw queues a notification of any method and params.
func (c *Conn) NotifyRaw(method string, params any) bool {
	frame, err := EncodeRaw(method, params)
	return err == nil && c.Send(frame)
}
