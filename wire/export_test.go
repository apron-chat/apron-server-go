package wire

// What this package's external tests (package wire_test) use that its API
// leaves out.

// Conns returns the open connections, Local ones included.
func (s *Server) Conns() []*Conn { return s.openConns() }

// EncodeRaw renders a notification frame of any method and params.
func EncodeRaw(method string, params any) (Frame, error) { return encodeRaw(method, params) }

// NotifyRaw queues a notification of any method and params.
func (c *Conn) NotifyRaw(method string, params any) bool {
	frame, err := encodeRaw(method, params)
	return err == nil && c.Send(frame)
}
