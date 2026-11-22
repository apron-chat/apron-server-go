package wire

import (
	"net/http"
	"net/url"
	"path"
	"strings"
)

// OriginPatterns returns a Server.CheckOrigin that accepts the server's own
// origin, a request without an Origin header, and the origins that match
// one of patterns, as in github.com/coder/websocket's AcceptOptions: a
// pattern is matched with path.Match, case-insensitively, against the
// origin's host, such as "*.example.com" or "localhost:*", or, when it has
// "://", against the scheme and host, such as "https://*.example.com". A
// pattern that is not well formed matches nothing.
func OriginPatterns(patterns ...string) func(r *http.Request) bool {
	patterns = append([]string(nil), patterns...)
	return func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		if strings.EqualFold(r.Host, u.Host) {
			return true
		}
		for _, pattern := range patterns {
			target := u.Host
			if strings.Contains(pattern, "://") {
				target = u.Scheme + "://" + u.Host
			}
			if matched, err := path.Match(strings.ToLower(pattern), strings.ToLower(target)); err == nil && matched {
				return true
			}
		}
		return false
	}
}

// AnyOrigin is a Server.CheckOrigin that accepts every origin, such as for a
// server whose clients are not browsers, or a trusted deployment: it lets
// any web page open a WebSocket with its visitors' cookies.
func AnyOrigin(*http.Request) bool { return true }
