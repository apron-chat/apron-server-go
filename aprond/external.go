package aprond

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"

	"github.com/apron-chat/apron-server-go/internal/auth/external"
	"github.com/apron-chat/apron-server-go/internal/authn"
)

// External sign-in (PROTOCOL.md §4.12) is a profile of OAuth 2.1 in which
// the server is its own authorization server. A client begins, and opens
// the server's page at /oauth/authorize with PKCE, which hands the person
// straight to the extension that offers the sign-in
// (Registrar.ExternalSignIn), which signs them in however it likes, such as
// with OAuth or OpenID Connect, and completes the sign-in with the identity
// it proved (ExternalSignIn.Complete). The page then asks once, naming the
// app and the account, and on Allow redirects to the app with a code; an
// account that allowed the same https redirect_uri before is not asked
// again, until it signs out everywhere. The app redeems the code on a
// connection, which signs in, or adds the identity to the connection's
// account, or at /oauth/token for a bearer token. The server also serves
// its authorization server metadata at
// /.well-known/oauth-authorization-server, and its protected resource
// metadata, which names itself as the authorization server, at
// /.well-known/oauth-protected-resource.

// ExternalIssuer is another OAuth authorization server that external
// sign-in names (Config.ExternalIssuer), such as an OpenID Connect
// provider like Pocket ID. A begin returns its authorization endpoint, with
// Scope in the query, and the issuer; the server redeems a code at its
// token endpoint, which it finds from the issuer's metadata, or takes an
// access token the app redeemed there itself (§4.12.4). The server's
// protected resource metadata (RFC 9728) names the issuer too.
//
// The begin names no client_id: each app identifies itself to the issuer
// with its own, the https URL of its client ID metadata document, so the
// issuer asks for consent per app and names it. The issuer must accept
// such clients, bind access tokens to the resource the app asks for (RFC
// 8707), and issue them as signed JWT access tokens (RFC 9068, typ
// at+jwt): the server accepts only a token whose iss is Issuer, whose aud
// includes the server's origin, which has not expired, and which is bound
// to no key (cnf). The account's identity is the issuer and the token's
// sub, as external:<issuer>#<sub>. A new account's name comes from the
// token, else from the issuer's userinfo endpoint, asked with the token.
type ExternalIssuer struct {
	// Issuer is the authorization server's issuer identifier, an https URL
	// such as https://id.example.
	Issuer string
	// Scope, if set, goes in the authorization URL, such as "openid
	// profile", which an issuer may need to answer userinfo.
	Scope string
	// HTTPClient fetches the issuer's metadata, keys, tokens, and userinfo;
	// nil uses one with a ten-second timeout.
	HTTPClient *http.Client
}

// ExternalSignIn is the external sign-in an extension offers
// (Registrar.ExternalSignIn), which completes the sign-ins it was handed.
type ExternalSignIn struct{ scheme *external.Scheme }

// ExternalRequest is a sign-in the server's page hands to the extension,
// as the app opens the page, or when the person chose to sign in as
// someone else.
type ExternalRequest struct {
	// ID names the sign-in to ExternalSignIn.Complete. It is unguessable
	// and works once, so it may serve as the state of an OAuth request.
	ID string
	// Client is who is asking, as the page's confirmation shows it, such
	// as chat.alice.dev or "An app on this device (com.example.app)".
	Client string
}

// Identity is who an external sign-in proved someone is: the account's
// identity of the external scheme.
type Identity struct {
	// Subject names the identity, uniquely and for good, such as
	// "github:583231": the same Subject signs in to the same account. It
	// should be the provider's own stable ID for the person, never a
	// name that can change (§4.12.4).
	Subject string
	// Name and UserID are the display name and the user_id requested for
	// a new account, if the client requested none.
	Name, UserID string
	// Roles are the server roles (§3.3.4) the account holds, as of each
	// sign-in, besides those in Config.Roles.
	Roles []string
}

// ExternalSignIn offers external sign-in (§4.12), which start begins: it
// is called as an app opens the server's page, and again when the person
// chooses to sign in as someone else, and typically redirects to an
// identity provider, whose answer comes back to a route of the extension's
// (Handle) that calls the returned ExternalSignIn's Complete, Deny, or
// Cancel. One extension may offer it. Account creation is the extension's
// policy: an identity it completes signs in, making an account if it has
// none, and one it refuses it denies.
func (r *Registrar) ExternalSignIn(start func(w http.ResponseWriter, r *http.Request, req ExternalRequest)) *ExternalSignIn {
	scheme := external.New(func(w http.ResponseWriter, req *http.Request, p external.Request) {
		start(w, req, ExternalRequest{ID: p.ID, Client: p.Client})
	})
	r.register("external sign-in", func() error {
		switch {
		case start == nil:
			return errors.New("external sign-in needs a start function")
		case r.s.schemeLocked(external.Name) != nil:
			return fmt.Errorf("%s offers external sign-in already", cmp.Or(r.s.externalBy, "the server"))
		}
		if err := r.s.addSchemeLocked(scheme); err != nil {
			return err
		}
		r.s.externalBy = "extension " + r.name
		return nil
	})
	return &ExternalSignIn{scheme: scheme}
}

// ErrUnknownSignIn is returned by Complete, Deny, and Cancel for a sign-in
// that is unknown, expired, or completed.
var ErrUnknownSignIn = errors.New("apron: the sign-in is unknown, expired, or completed")

// Complete completes the sign-in named id with the identity the extension
// proved, and answers the person's browser with the page's confirmation,
// which asks them to allow the app to use the account, or to sign in as
// them with a new identity; or, for an account that allowed the same https
// redirect_uri before, with the redirect to the app. For a sign-in that
// is no longer open, it answers with a page that says so, and returns
// ErrUnknownSignIn.
func (x *ExternalSignIn) Complete(w http.ResponseWriter, r *http.Request, id string, identity Identity) error {
	err := x.scheme.Complete(w, r, id, authn.Proof{
		Subject: identity.Subject, Name: identity.Name, UserID: identity.UserID, Roles: identity.Roles,
	})
	switch {
	case errors.Is(err, external.ErrUnknownSignIn):
		return ErrUnknownSignIn
	case errors.Is(err, external.ErrNoSubject):
		return errors.New("apron: an external Identity needs a Subject")
	}
	return err
}

// Cancel ends the sign-in named id, such as when the person declined at
// the identity provider, and sends them back to the app, which hears that
// they declined (error=access_denied). For a sign-in that is no longer
// open, it answers with a page that says so, and returns ErrUnknownSignIn.
func (x *ExternalSignIn) Cancel(w http.ResponseWriter, r *http.Request, id string) error {
	if errors.Is(x.scheme.Cancel(w, r, id), external.ErrUnknownSignIn) {
		return ErrUnknownSignIn
	}
	return nil
}

// Deny refuses the sign-in named id, as the extension's policy, such as
// for a person who is not a member of the organization it admits, and
// answers the person's browser with the server's page, which says they
// can't sign in with this account, then reason, such as "octocat is not a
// member of acme", and offers to sign in as someone else (the extension's
// start function again, with a new ExternalRequest) or to cancel. Cancel
// returns to the app with error=access_denied (RFC 6749 §4.1.2.1) and
// reason as its error_description, in printable ASCII. For a sign-in that
// is no longer open, it answers with a page that says so, and returns
// ErrUnknownSignIn.
func (x *ExternalSignIn) Deny(w http.ResponseWriter, r *http.Request, id, reason string) error {
	if errors.Is(x.scheme.Deny(w, r, id, reason), external.ErrUnknownSignIn) {
		return ErrUnknownSignIn
	}
	return nil
}

// BaseURL is the server's base URL as people reach it, such as
// https://chat.example: Config.PublicURL, else the scheme and host that r,
// a request to the server, arrived on, as the server's own pages and links
// have it. An extension makes the addresses of its routes with it, such as
// the callback URL of the OAuth app it signs people in with, which it
// sends from its external sign-in's start function and from that callback.
func (h *Host) BaseURL(r *http.Request) string { return h.s.baseURL(r) }
