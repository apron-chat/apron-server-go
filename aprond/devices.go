package aprond

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/aprond/internal/devices"
)

// `/invite-device <name>` follows apron-server-cloudflare's command of that
// name: an account mints a bearer token that signs it in on another device
// through the token scheme (§3.2), from any origin, until the same name
// mints a new one. A devices.Table keeps the tokens; the server keeps who
// may mint one, what a sign-in with one does, and what replacing one ends.

// maxDeviceTokens bounds an account's device tokens.
const maxDeviceTokens = devices.MaxPerUser

// deviceToken is a token /invite-device minted, which a devices.Table
// keeps by the SHA-256 of the token.
type deviceToken = devices.Device[*userState]

func isASCIIAlphanumeric(b byte) bool {
	return 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9'
}

// inviteDeviceCommand mints a device token for the sender's account under
// the one name in args. The same name again replaces its token: the old one
// no longer signs in, and the account's other connections that signed in
// with it close and its push registrations go. A passkey or session token
// someone got while signed in with the old token stays. The token goes to
// this connection only, in a ~private notice before the result (§1).
func (s *Server) inviteDeviceCommand(c *client, r *roomState, _ map[string]any, args, _ string) (apron.CommandResult, *Error) {
	u := c.user
	if !u.Account() {
		return apron.CommandResult{}, &Error{Code: CodeDenied, Message: "Sign in with an account to use /invite-device"}
	}
	if c.restrictedService() {
		return apron.CommandResult{}, serviceRefusal()
	}
	words := strings.Fields(args)
	if len(words) != 1 {
		return apron.CommandResult{}, invalidParams("Usage: /invite-device <name>")
	}
	name := words[0]
	if !devices.ValidName(name) {
		return apron.CommandResult{}, invalidParams("A device name is 1 to 32 letters, digits, _, . or -, starting with a letter or digit")
	}
	keys, held := s.devices.Of(u)
	previous := slices.IndexFunc(held, func(d deviceToken) bool { return d.Name == name })
	if previous < 0 && len(held) >= maxDeviceTokens {
		names := make([]string, len(held))
		for i, d := range held {
			names[i] = "`" + d.Name + "`"
		}
		return apron.CommandResult{}, &Error{Code: CodeDenied, Message: fmt.Sprintf("You have %d device tokens (%s); run /invite-device with one of those names to replace its token", maxDeviceTokens, strings.Join(names, ", "))}
	}
	token, key, err := s.devices.Mint(u, name, time.Now())
	if err != nil {
		return apron.CommandResult{}, &Error{Code: CodeInternalError, Message: "Unable to create a device token"}
	}
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
	c.enqueue(privateNotice(r.ID(), strings.Join(lines, "\n\n")))
	return apron.CommandResult{}, nil
}

// revokeDeviceLocked forgets the token under old and closes the account's
// connections, other than the asking one, that signed in with it; the
// asking connection now counts as signed in with the replacement, so a
// later replacement closes it. The push registrations made by connections
// signed in with old go too, the asking connection's among them.
func (s *Server) revokeDeviceLocked(c *client, old, replacement [32]byte) {
	s.devices.Delete(old)
	if c.device == old {
		c.device = replacement
	}
	for peer := range c.user.clients {
		if peer != c && peer.device == old {
			peer.closeWithError(&Error{Code: CodeDenied, Message: "Device token replaced; sign in with the new one"})
		}
	}
	for _, registration := range s.pushes.Of(c.user.id) {
		if registration.Device == old {
			s.removePushLocked(registration)
		}
	}
}

// authenticateDeviceLocked signs the connection in with a device token,
// from any origin or none. The result carries no session token: the device
// keeps its own.
func (s *Server) authenticateDeviceLocked(c *client, reply apron.Reply[apron.AuthResult], token string) *Error {
	key := sha256.Sum256([]byte(token))
	device, ok := s.devices.Get(key)
	if !ok {
		return &Error{Code: CodeDenied, Message: "This device token is not valid; get a new one with /invite-device on a signed-in device"}
	}
	// A refused sign-in changes nothing (§3.2.2).
	if err := s.admitLocked(device.User); err != nil {
		return err
	}
	c.device = key
	c.setService([32]byte{}, false)
	c.token, c.credential = [32]byte{}, ""
	s.switchUserLocked(c, reply, device.User, "", c.user != device.User, false)
	return nil
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
