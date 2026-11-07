package apron

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/apron-chat/apron-server-go/internal/auth/external"
	"github.com/apron-chat/apron-server-go/internal/authn"
)

// External sign-in (PROTOCOL.md §4.11.5): a client proposes it and opens
// the page the result names, at /external/<proposal>. The server hands the
// page to the extension that offers the sign-in (Registrar.ExternalSignIn),
// which signs the person in however it likes, such as with OAuth or OpenID
// Connect, and completes the proposal with the identity it proved
// (ExternalSignIn.Complete). The server then gives the person the
// proposal's temporary token, as a link back to the client or a code to
// type, which the client presents to sign in.

// ExternalSignIn is the external sign-in an extension offers
// (Registrar.ExternalSignIn), which completes the proposals it was given.
type ExternalSignIn struct{ scheme *external.Scheme }

// ExternalProposal is an external sign-in a client proposed, whose page a
// person opened.
type ExternalProposal struct {
	// ID names the proposal to ExternalSignIn.Complete. It is unguessable,
	// so it may serve as the state of an OAuth request.
	ID string
	// Add reports a proposal to add the identity to the account that is
	// signed in where it was proposed, rather than to sign in with it.
	Add bool
	// ReturnHost is the host of the client the person returns to, if any.
	ReturnHost string
}

// Identity is who an external sign-in proved someone is: the account's
// identity of the external scheme.
type Identity struct {
	// Subject names the identity, uniquely and for good, such as
	// "github:583231": the same Subject signs in to the same account.
	Subject string
	// Name and UserID are the display name and the user_id requested for
	// a new account, if the client requested none.
	Name, UserID string
	// Roles are the server roles (§3.3.4) the account holds, as of each
	// sign-in, besides those in Config.Roles.
	Roles []string
}

// ExternalSignIn offers external sign-in (§4.11.5), which start begins: it
// is called with the page a person opened for a proposal, and typically
// redirects to an identity provider, whose answer comes back to a route of
// the extension's (Handle) that calls the returned ExternalSignIn's
// Complete. One extension may offer it. Account creation is the extension's
// policy: an identity it completes signs in, making an account if it has
// none.
func (r *Registrar) ExternalSignIn(start func(w http.ResponseWriter, r *http.Request, p ExternalProposal)) *ExternalSignIn {
	scheme := external.New(func(w http.ResponseWriter, req *http.Request, p external.Proposal) {
		start(w, req, ExternalProposal{ID: p.ID, Add: p.Add, ReturnHost: p.ReturnHost})
	})
	r.register("external sign-in", func() error {
		switch {
		case start == nil:
			return errors.New("external sign-in needs a start function")
		case r.s.schemeLocked(external.Name) != nil:
			return fmt.Errorf("extension %s offers external sign-in already", r.s.externalBy)
		}
		if err := r.s.addSchemeLocked(scheme); err != nil {
			return err
		}
		r.s.externalBy = r.name
		return nil
	})
	return &ExternalSignIn{scheme: scheme}
}

// ErrUnknownProposal is returned by Complete for a proposal that is
// unknown, expired, or completed.
var ErrUnknownProposal = errors.New("apron: the sign-in proposal is unknown, expired, or completed")

// Complete completes the proposal named id with the identity the
// extension proved, and answers the person's browser: with the client's
// return_url, a page that asks them to continue to it with the sign-in
// link (§4.11.5), and without, a page that shows the code to type in the
// client. For a proposal that is no longer open, it answers with a page
// that says so, and returns ErrUnknownProposal.
func (x *ExternalSignIn) Complete(w http.ResponseWriter, _ *http.Request, id string, identity Identity) error {
	err := x.scheme.Complete(w, id, authn.Proof{
		Subject: identity.Subject, Name: identity.Name, UserID: identity.UserID, Roles: identity.Roles,
	})
	switch {
	case errors.Is(err, external.ErrUnknownProposal):
		return ErrUnknownProposal
	case errors.Is(err, external.ErrNoSubject):
		return errors.New("apron: an external Identity needs a Subject")
	}
	return err
}
