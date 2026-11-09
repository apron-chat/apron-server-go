// Package authn is the boundary between the server and its sign-in schemes
// (PROTOCOL.md §3.2): passkeys, email, and external sign-in. A scheme proves
// who a connection is; the server owns everything that follows from that:
// accounts and their identities, bearer tokens, proposals (§4.11), the
// allowlist, and roles.
//
// A Scheme sees an auth request as a copy (Request), never the server's
// state, and runs outside the server's lock. It answers with a Result:
//   - a *Reply, which authenticates nothing, such as a WebAuthn challenge;
//   - an *Approve, which presents a proposal's temporary token, which the
//     server checks;
//   - a *SignIn, which signs in to the account of a subject it proved; or
//   - an *Add, which adds a subject it proved to the connection's account.
//
// A scheme proves only subjects: the server names the identity with the
// scheme's own Name, so no scheme can prove another's identities, and it
// normalizes what a Proof requests for an account (its name, user_id, and
// roles) as it does a client's.
//
// Lock order: the server calls Proof.Commit holding its own lock, so
// Commit may take the scheme's locks, and the scheme must never call the
// server (Conn, Core) while holding a lock that Commit takes.
package authn

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Identity is an account's identity of one scheme: who someone is, in the
// scheme's terms. An account has at most one identity of each scheme, and
// an identity at most one account.
type Identity struct {
	// Scheme is the auth scheme that proves it, such as "email".
	Scheme string
	// Subject names the identity within its scheme: an address, a
	// passkey's user handle, or an external provider's subject.
	Subject string
}

// String writes the identity as scheme:subject.
func (i Identity) String() string { return i.Scheme + ":" + i.Subject }

// Account is the account a connection is signed in as, as a scheme sees it:
// a copy.
type Account struct {
	UserID string
	Name   string
	// Guest is a connection signed in as a guest, which no identity proves.
	Guest bool
	// Identities are the account's subjects, by scheme.
	Identities map[string]string
}

// Scheme proves identities for one auth scheme.
type Scheme interface {
	// Name is the scheme, as auth requests and the server frame name it,
	// and as its identities' Scheme.
	Name() string
	// Label names the scheme's identities in refusals, such as "email
	// address".
	Label() string
	// Auth answers an auth request of the scheme. A refusal is an *Error.
	Auth(ctx context.Context, r *Request) (Result, error)
}

// Binder is a Scheme that uses the server outside its auth requests, such
// as for the pages of its proposals. The server binds it once, as it adds
// the scheme, before any request.
type Binder interface {
	Bind(core Core)
}

// Restorer is a Scheme that keeps data with the identities it proves, such
// as passkey credentials: the server stores what Proof.Commit returns with
// the account, and hands it back once the server's schemes are all added,
// before any request.
type Restorer interface {
	Restore(subject string, record jsontext.Value) error
}

// Router is a Scheme that serves HTTP routes, such as a sign-in page.
type Router interface {
	Routes(mux *http.ServeMux)
}

// Request is one auth request, as a scheme sees it.
type Request struct {
	// Params are the request's params: a copy.
	Params map[string]jsontext.Value
	// Name and UserID are what the request asks for a new account, as the
	// server keeps them: the name normalized, and the user_id one that
	// could be a user_id, or empty.
	Name, UserID string
	// Account is who the connection is signed in as; nil when it is not.
	Account *Account
	// Origin is the connection's Origin header; BaseURL the base of the
	// URLs it gets, such as https://chat.example; ClientKey its client
	// address, for rate limits.
	Origin, BaseURL, ClientKey string
	Now                        time.Time
	// State is the scheme's state for the connection, such as a pending
	// WebAuthn ceremony, which the connection's sign-in to another account
	// clears. Only the connection's own requests use it, one at a time.
	State *State
	// Conn is the server's side of the request's connection.
	Conn Conn
	// Invitation is the invitation the request presents, on a connection
	// that is not signed in, as the server checked it: valid and unused. A
	// scheme may make a new account with it (SignIn.Invitation) where it
	// otherwise needs one to add to, such as a passkey registration. Nil
	// when the request presents none.
	Invitation *Invitation
}

// Invitation is an invitation to make one account, as a scheme sees it.
type Invitation struct {
	// ID names the invitation; it is not the token that presents it.
	ID string
	// Name and UserID are what the invitation suggests for the account.
	Name, UserID string
}

// State holds a scheme's value for one connection.
type State struct{ value any }

// Load returns the value, nil for none.
func (s *State) Load() any { return s.value }

// Store sets the value.
func (s *State) Store(value any) { s.value = value }

// Conn is what the server offers a scheme for one request's connection.
// Proposals (§4.11.1) are the connection's: a new one replaces any it has,
// and on a signed-in connection, guests included, a proposal adds its
// identity to the connection's account rather than signing in with it.
// The connection's proposals share one budget, whatever their scheme.
type Conn interface {
	// ProposeCode proposes what the scheme proved already, such as an
	// email address, whose temporary tokens prove it once delivered.
	ProposeCode(p CodeProposal) (Tokens, error)
	// ProposePage proposes an identity a page will prove, which the scheme
	// then completes (Core.Complete), and returns the page's name.
	ProposePage(p PageProposal) (page string, err error)
	// CanProve reports whether proving subject would sign in or add it:
	// whether an email code is worth mailing.
	CanProve(subject string) bool
}

// Core is what the server offers a scheme outside its requests.
type Core interface {
	// Page returns the scheme's proposal a page names, while it waits for
	// its identity.
	Page(page string) (Pending, bool)
	// Complete gives the scheme's proposal a page names the identity a
	// page proved, and returns its temporary tokens. Approving them signs
	// in with the identity, making an account for one that has none, or
	// adds it to the proposing account.
	Complete(page string, proof Proof) (Completed, error)
	// Go runs fn on its own goroutine, which the server's shutdown waits
	// for. Once the server is shutting down, it runs nothing and reports
	// false.
	Go(fn func(ctx context.Context)) bool
	// Logger logs for the scheme.
	Logger() *slog.Logger
}

// CodeProposal is a proposal of what the scheme proved already.
type CodeProposal struct {
	// Proof is what approving it proves. Approving a sign-in makes an
	// account for an identity that has none.
	Proof Proof
	// State is the client's, returned in the link (§4.11.1).
	State string
	// LinkPage is the server's page that a link opens, empty for no link.
	LinkPage string
}

// PageProposal is a proposal of an identity a page will prove.
type PageProposal struct {
	// State is the client's, returned in the link (§4.11.1); ReturnURL is
	// the client's page the link opens, empty for a code to type, which
	// must be an http or https URL on the connection's origin or the
	// server's.
	State, ReturnURL string
}

// Tokens are a proposal's temporary tokens (§4.11.2).
type Tokens struct {
	// Code is the short token to type, which works only on the proposing
	// connection.
	Code string
	// Link opens the proposal's link page with the long token, which
	// works on any connection that may approve it; empty without one.
	Link    string
	Expires time.Time
	// Add reports a proposal to add an identity to an account.
	Add bool
}

// Pending is a proposal whose page waits for its identity.
type Pending struct {
	Add bool
	// ReturnHost is the host of the client a link returns to, if any.
	ReturnHost string
}

// Completed is a proposal Complete gave its identity.
type Completed struct {
	Tokens
	// ReturnHost is the host of the client the link returns to; empty
	// when the code is typed instead, and then Link is too.
	ReturnHost string
}

// Proof is what a scheme proved: one of its subjects.
type Proof struct {
	// Subject names the identity within the scheme; never empty.
	Subject string
	// Name and UserID suggest a new account's display name and user_id,
	// for a sign-in that makes one, or a guest's that an Add makes an
	// account.
	Name, UserID string
	// Roles are the server roles the identity grants its account, as of
	// this proof, replacing those it granted before.
	Roles []string
	// Commit, if set, is called once the server has checked the proof and
	// before it changes anything; it returns the record the server keeps
	// with the identity (Restorer), nil for none, or refuses. The server
	// calls it holding its lock, so it must not block.
	Commit func() (jsontext.Value, error)
}

// Result is a scheme's answer: *Reply, *Approve, *SignIn, or *Add.
type Result interface{ result() }

// Reply answers the request with a result, and authenticates nothing.
type Reply struct{ Result any }

// Approve approves a proposal with its temporary token (§4.11.3).
type Approve struct{ Token string }

// SignIn signs the connection in to the account of the identity proven.
type SignIn struct {
	Proof
	// Create makes an account for an identity no account has; otherwise
	// such an identity is refused.
	Create bool
	// Invitation, the ID of the request's Invitation, makes the account
	// with that invitation, which the server uses up in the same change:
	// the identity must be one no account has. Empty for none.
	Invitation string
}

// Add adds the identity proven to the connection's account, a guest's
// included, which then is an account.
type Add struct {
	Proof
	// Session replies with a bearer token, as a sign-in does; otherwise
	// the reply is {}.
	Session bool
}

func (*Reply) result()   {}
func (*Approve) result() {}
func (*SignIn) result()  {}
func (*Add) result()     {}

// Error codes (PROTOCOL.md §1.1), as the server replies them.
const (
	CodeUnsupported   = -32601
	CodeInvalidParams = -32602
	CodeInternalError = -32603
	CodeDenied        = -32001
	CodeRetryAfter    = -32002
	CodeTooLarge      = -32003
)

// Error is a refusal, which the server replies as a protocol error.
type Error struct {
	Code    int
	Message string
	Data    any
}

func (e *Error) Error() string { return fmt.Sprintf("authn error %d: %s", e.Code, e.Message) }

// Denied is a CodeDenied error.
func Denied(message string) *Error { return &Error{Code: CodeDenied, Message: message} }

// InvalidParams is a CodeInvalidParams error, formatted.
func InvalidParams(format string, args ...any) *Error {
	return &Error{Code: CodeInvalidParams, Message: fmt.Sprintf(format, args...)}
}

// TooLarge is a CodeTooLarge error.
func TooLarge(message string) *Error { return &Error{Code: CodeTooLarge, Message: message} }

// Internal is a CodeInternalError error.
func Internal(message string) *Error { return &Error{Code: CodeInternalError, Message: message} }

// RetryAfter is a CodeRetryAfter error suggesting wait, in whole seconds.
func RetryAfter(message string, wait time.Duration) *Error {
	return &Error{Code: CodeRetryAfter, Message: message, Data: map[string]any{"retry_after": Seconds(wait)}}
}

// Seconds rounds a wait up to whole seconds, at least one.
func Seconds(wait time.Duration) int64 {
	return max(1, int64((wait+time.Second-1)/time.Second))
}

// String parses a string param; a missing required one, or one that is
// not a string, is CodeInvalidParams.
func String(params map[string]jsontext.Value, name string, required bool) (string, *Error) {
	raw, ok := params[name]
	if !ok {
		if required {
			return "", InvalidParams("Missing %s", name)
		}
		return "", nil
	}
	var value string
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
		return "", InvalidParams("%s must be a string", name)
	}
	return value, nil
}
