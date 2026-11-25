package aprond

import (
	"maps"
	"slices"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/internal/auth/external"
	"github.com/apron-chat/apron-server-go/internal/auth/passkey"
	"github.com/apron-chat/apron-server-go/internal/authn"
)

// serverFrameLocked is the `server` frame (PROTOCOL.md §3.1) for c.
func (s *Server) serverFrameLocked(c *client) apron.ServerInfo {
	return s.serverParams(c.baseURL, s.revealedLocked(c))
}

// revealedLocked reports whether c gets the whole server frame: once it is
// signed in, or before when anyone may sign in as a guest anyway. Before a
// sign-in that only some may make, the frame holds what signing in needs:
// not the default room's description, the custom emoji, or what extensions
// advertise.
func (s *Server) revealedLocked(c *client) bool {
	return c.user != nil || s.guestsAllowedLocked()
}

// serverParams is the `server` frame (PROTOCOL.md §3.1) for a connection
// whose URLs start with baseURL, whole when revealed; package apron/server adds the
// protocol version and the ping interval. Callers hold s.mu.
func (s *Server) serverParams(baseURL string, revealed bool) apron.ServerInfo {
	var authSchemes []string
	for _, scheme := range s.schemes {
		authSchemes = append(authSchemes, scheme.Name())
	}
	if len(authSchemes) > 0 {
		authSchemes = append(authSchemes, "token")
	}
	guests := s.guestsAllowedLocked()
	if guests {
		authSchemes = append(authSchemes, "guest")
	}
	// signup (§3.2) lists the schemes that can create an account: every
	// one but token, which only resumes one. Email, external sign-in, and
	// guest start one, and so does a passkey registration where guests are
	// allowed, on a connection that is not signed in (§4.10). Without
	// guests a registration makes an account only with an invitation, so
	// webauthn is no way to sign up. An invitation does not put it back,
	// since signup tells every client what it can do, and an invitation is
	// a secret one person was given; an invited client registers anyway,
	// as webauthn stays in auth. With GuestFirstPasskeys a registration
	// makes an account only of a signed-in guest, so signup lists guest but
	// not webauthn, which tells clients to sign in as a guest first.
	signup := slices.DeleteFunc(slices.Clone(authSchemes), func(scheme string) bool {
		return scheme == "token" || scheme == passkey.Name && (!guests || s.config.GuestFirstPasskeys)
	})
	info := apron.ServerInfo{
		Agent: "apron-go/10",
		// Package apron/server adds the capability of each handler and method the
		// server has, such as rooms, status, and ext:command_list
		// (methodCapabilities), so this lists only what no method stands
		// for: what the server keeps and does beyond its requests, and the
		// extensions that need no method, or that only some connections
		// are told of.
		Capabilities: slices.Clone(featureCapabilities),
		Auth:         authSchemes,
		// The optional statuses accepted with `me` (§4.5); online and ""
		// always are, and are not listed.
		Status: optionalStatuses,
	}
	if slices.Contains(authSchemes, external.Name) {
		// ext:oauth: external sign-in's OAuth extras (SERVER.md), the
		// server's own authorization server metadata and token endpoint,
		// or its protected resource metadata naming another issuer. A
		// client needs it to sign in, so it is listed before.
		info.Capabilities = append(info.Capabilities, "ext:oauth")
	}
	if revealed && slices.ContainsFunc(s.schemes, func(scheme authn.Scheme) bool {
		_, ok := scheme.(authn.Credentials)
		return ok
	}) {
		// ext:account (account.go): credentials, such as passkeys, and
		// other sessions.
		info.Capabilities = append(info.Capabilities, "ext:"+accountExt)
	}
	var ext apron.Ext
	if emoji := s.emojiExt(baseURL); emoji != nil && revealed {
		info.Capabilities = append(info.Capabilities, "ext:emoji")
		ext = ext.With("emoji", emoji)
	}
	if !s.config.DisablePush {
		push := map[string]any{"relay": map[string]any{}, "wake": (urgentScopes | wakeJoined | wakeBadge).Names()}
		if s.push.VAPID != nil {
			push["webpush"] = map[string]any{"key": s.push.VAPID.Public}
		}
		info.Push = push
	}
	if s.config.Welcome != "" {
		info.Welcome = s.config.Welcome
	}
	for _, field := range profileFields {
		if value, source := s.profileValueLocked(field); value != "" && (revealed || source != fromRoom) {
			switch field {
			case "title":
				info.Title = value
			case "description":
				info.Description = value
			case "avatar":
				info.Avatar = value
			}
		}
	}
	if s.config.MaxPrivateRooms > 0 {
		// ext:settings (apron's EXTENSIONS.md): read_cursors is absent,
		// so true, since the server keeps read_message_id (§4.6).
		info.Capabilities = append(info.Capabilities, "ext:settings")
		ext = ext.With("settings", map[string]any{"private_rooms": s.config.MaxPrivateRooms})
	}
	// Each extension that advertises (Host.Advertise) adds its capability,
	// ext:<name>, and its data under its name in ext (EXTENSIONS.md).
	for _, name := range slices.Sorted(maps.Keys(s.advertised)) {
		if !revealed {
			break
		}
		info.Capabilities = append(info.Capabilities, "ext:"+name)
		if value := s.advertised[name]; len(value) > 0 {
			if ext == nil {
				ext = make(apron.Ext)
			}
			ext[name] = value
		}
	}
	info.Ext = ext
	if !slices.Equal(signup, authSchemes) {
		info.Signup = signup
	}
	return info
}
