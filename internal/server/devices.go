package server

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"time"
)

// `/invite-device <name>` follows apron-server-cloudflare's command of that
// name: an account mints a bearer token that signs it in on another device
// through the token scheme (§3.2), from any origin, until the same name
// mints a new one.
const (
	maxDeviceTokens    = 3
	deviceTokenPrefix  = "apron_device_"
	maxDeviceNameBytes = 32
)

// deviceToken is a token /invite-device minted, kept by the SHA-256 of the
// token: the account it signs in as, and the name it was minted under.
// Unlike a session it is bound to no origin and does not expire.
type deviceToken struct {
	user    *userState
	name    string
	created time.Time
}

// validDeviceName reports whether name is 1 to 32 letters, digits, `_`, `.`
// or `-`, starting with a letter or digit.
func validDeviceName(name string) bool {
	if len(name) == 0 || len(name) > maxDeviceNameBytes || !isASCIIAlphanumeric(name[0]) {
		return false
	}
	for i := range len(name) {
		if b := name[i]; !isASCIIAlphanumeric(b) && b != '_' && b != '.' && b != '-' {
			return false
		}
	}
	return true
}

func isASCIIAlphanumeric(b byte) bool {
	return 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9'
}

// devicesOfLocked lists u's device tokens oldest first, with their keys.
func (s *Server) devicesOfLocked(u *userState) ([][32]byte, []deviceToken) {
	var keys [][32]byte
	for key, device := range s.devices {
		if device.user == u {
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, func(a, b [32]byte) int {
		if order := s.devices[a].created.Compare(s.devices[b].created); order != 0 {
			return order
		}
		return bytes.Compare(a[:], b[:])
	})
	devices := make([]deviceToken, len(keys))
	for i, key := range keys {
		devices[i] = s.devices[key]
	}
	return keys, devices
}

// inviteDeviceCommand mints a device token for the sender's account under
// the one name in args. The same name again replaces its token: the old one
// no longer signs in, and the account's other connections that signed in
// with it close and its push registrations go. A passkey or session token
// someone got while signed in with the old token stays. The token goes to
// this connection only, in a ~private notice before the result (§1).
func (s *Server) inviteDeviceCommand(c *client, r *roomState, _ map[string]any, args string) (map[string]any, *rpcError) {
	u := c.user
	if !u.account() {
		return nil, &rpcError{Code: codeDenied, Message: "Sign in with a passkey or email to use /invite-device"}
	}
	words := strings.Fields(args)
	if len(words) != 1 {
		return nil, invalidParams("Usage: /invite-device <name>")
	}
	name := words[0]
	if !validDeviceName(name) {
		return nil, invalidParams("A device name is 1 to 32 letters, digits, _, . or -, starting with a letter or digit")
	}
	keys, devices := s.devicesOfLocked(u)
	previous := slices.IndexFunc(devices, func(d deviceToken) bool { return d.name == name })
	if previous < 0 && len(devices) >= maxDeviceTokens {
		names := make([]string, len(devices))
		for i, d := range devices {
			names[i] = "`" + d.name + "`"
		}
		return nil, &rpcError{Code: codeDenied, Message: fmt.Sprintf("You have %d device tokens (%s); run /invite-device with one of those names to replace its token", maxDeviceTokens, strings.Join(names, ", "))}
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, &rpcError{Code: codeInternalError, Message: "Unable to create a device token"}
	}
	token := deviceTokenPrefix + base64.RawURLEncoding.EncodeToString(secret)
	key := sha256.Sum256([]byte(token))
	s.devices[key] = deviceToken{user: u, name: name, created: time.Now()}
	s.touchDevice(key)
	replaced := ""
	if previous >= 0 {
		replaced = ", and replaces its earlier token"
		s.revokeDeviceLocked(c, keys[previous], key)
	}
	lines := []string{
		fmt.Sprintf("This token signs in as you (`%s`) on your device **%s**%s. It does not expire, and anyone who has it can sign in as you, so keep it secret. `/invite-device %s` again replaces it.", u.id, name, replaced, name),
		"```\n" + token + "\n```",
	}
	if server := webSocketURL(c.baseURL); server != "" {
		lines = append(lines, fmt.Sprintf("Or open this link on **%s**, which signs you in there with any Apron app: <%s>", name, apronLink(server, token)))
	}
	lines = append(lines, fmt.Sprintf("To sign in, authenticate with the token scheme: `{\"method\": \"auth\", \"params\": {\"scheme\": \"token\", \"token\": \"%s\"}}`", token))
	c.enqueue(privateNotice(r, strings.Join(lines, "\n\n")))
	return map[string]any{}, nil
}

// revokeDeviceLocked forgets the token under old and closes the account's
// connections, other than the asking one, that signed in with it; the
// asking connection now counts as signed in with the replacement, so a
// later replacement closes it. The push registrations made by connections
// signed in with old go too, the asking connection's among them.
func (s *Server) revokeDeviceLocked(c *client, old, replacement [32]byte) {
	delete(s.devices, old)
	s.touchDevice(old)
	if c.device == old {
		c.device = replacement
	}
	for peer := range c.user.clients {
		if peer != c && peer.device == old {
			peer.closeWithError(&rpcError{Code: codeDenied, Message: "Device token replaced; sign in with the new one"})
		}
	}
	for _, registration := range c.user.pushes {
		if registration.device == old {
			s.removePushLocked(registration)
		}
	}
}

// authenticateDeviceLocked signs the connection in with a device token,
// from any origin or none. The result carries no session token: the device
// keeps its own.
func (s *Server) authenticateDeviceLocked(c *client, req request, token string) (any, *rpcError) {
	key := sha256.Sum256([]byte(token))
	device, ok := s.devices[key]
	if !ok {
		return nil, &rpcError{Code: codeDenied, Message: "This device token is not valid; get a new one with /invite-device on a signed-in device"}
	}
	c.device = key
	return s.switchUserLocked(c, req, device.user, nil, c.user != device.user, false), nil
}

// apronLink is a link that any Apron app opens to sign in to server with
// token (link.apron.chat SPEC.md §2.1, PROTOCOL.md Appendix A.4), spelled
// as apron-server-cloudflare's apronLink spells it: the keys go in the
// fragment, so link.apron.chat never receives them, and every byte other
// than a letter, a digit, or one of `-._~:/` is percent-encoded.
func apronLink(server, token string) string {
	return "https://link.apron.chat/#server=" + linkEscape(server) + "&token=" + linkEscape(token)
}

func linkEscape(value string) string {
	var escaped strings.Builder
	for i := range len(value) {
		b := value[i]
		if isASCIIAlphanumeric(b) || strings.IndexByte("-._~:/", b) >= 0 {
			escaped.WriteByte(b)
		} else {
			fmt.Fprintf(&escaped, "%%%02X", b)
		}
	}
	return escaped.String()
}
