// Package authn is the boundary between the server and its sign-in schemes
// (PROTOCOL.md §3.2): passkeys, email, and external sign-in. A scheme proves
// who a connection is; the server owns everything that follows from that:
// accounts and their identities, bearer tokens, proposals (§4.11), the
// allowlist, and roles.
//
// A Scheme sees an auth request as a copy (Request), never the server's
// state, and runs outside the server's lock. It answers with a Result: a
// Reply that authenticates nothing, such as a WebAuthn challenge or an
// email proposal's {}; an Approve that presents a proposal's temporary
// token, which the server checks; or a Proven identity, which the server
// signs in, adds to the connection's account, or makes an account for.
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

// Identity is what a scheme proves: who someone is, in the scheme's terms.
// An account has at most one identity of each scheme.
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
	// Identities are the account's, by scheme.
	Identities map[string]string
}

// Scheme proves identities for one auth scheme.
type Scheme interface {
	// Name is the scheme, as auth requests and the server frame name it.
	Name() string
	// Auth answers an auth request of the scheme. A refusal is an *Error.
	Auth(ctx context.Context, r *Request) (Result, error)
}

// Restorer is a Scheme that keeps data with the accounts it proves, such as
// passkey credentials: the server stores what Proven.Commit returns with
// the account, and hands it back at each start.
type Restorer interface {
	Restore(id Identity, record jsontext.Value) error
}

// Router is a Scheme that serves HTTP routes, such as a sign-in page.
type Router interface {
	Routes(mux *http.ServeMux)
}

// Request is one auth request, as a scheme sees it.
type Request struct {
	Params map[string]jsontext.Value
	// Account is who the connection is signed in as; nil when it is not.
	Account *Account
	// Origin is the connection's Origin header; BaseURL the base of the
	// URLs it gets, such as https://chat.example; ClientKey its client
	// address, for rate limits.
	Origin, BaseURL, ClientKey string
	Now                        time.Time
	// State is the scheme's per-connection state, such as a pending
	// WebAuthn ceremony. Only the connection's own requests use it, one at a
	// time.
	State *State
	// Core is the server's side of sign-in.
	Core Core
	// Conn is the server's handle on the connection, for Core.
	Conn any
}

// State holds a scheme's value for one connection.
type State struct{ value any }

// Load returns the value, nil for none.
func (s *State) Load() any { return s.value }

// Store sets the value.
func (s *State) Store(value any) { s.value = value }

// Core is what the server offers schemes.
type Core interface {
	// Propose replaces the connection's proposal (§4.11.1) with a new one,
	// and returns its temporary tokens.
	Propose(r *Request, p Proposal) (Tokens, error)
	// Page returns the proposal a sign-in page names, while it waits for
	// its identity (Complete).
	Page(page string) (Pending, bool)
	// Complete gives the proposal a page names the identity an external
	// provider proved, and returns its temporary tokens.
	Complete(page string, proven Proven) (Completed, error)
	// CanProve reports whether proving id on r would sign in or add it:
	// whether an email code is worth mailing.
	CanProve(r *Request, id Identity) bool
	// Go runs fn on its own goroutine, which Shutdown waits for.
	Go(fn func(ctx context.Context))
	// NormalizeName prepares a display name as the server keeps names.
	NormalizeName(name string) string
	// Logger logs for the scheme.
	Logger() *slog.Logger
}

// Proposal is a proposal to make (§4.11.1): to sign in, or, on a signed-in
// connection, to add an identity to its account.
type Proposal struct {
	// Proven is what its token proves: an email proposal's address. Nil for
	// a proposal whose page waits for the identity (Page and Complete).
	Proven *Proven
	// Page gives the proposal a page, which names it until Complete.
	Page bool
	// State is the client's, returned in the link (§4.11.1); ReturnURL
	// the client's page that a link opens, which must be on a trusted
	// origin; Link the server's page that a link opens instead, if any.
	State, ReturnURL, Link string
	// Name and UserID are what the client requested for a new account.
	Name, UserID string
}

// Tokens are a proposal's temporary tokens (§4.11.2).
type Tokens struct {
	// Code is the short token to type, which works only on the proposing
	// connection; empty until the proposal has its identity.
	Code string
	// Link opens the proposal's link page with the long token, which
	// works on any connection that may approve it; empty without one.
	Link string
	// Page names the proposal's page, if it has one.
	Page    string
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
	// when the code is typed instead.
	ReturnHost string
}

// Result is a scheme's answer: Reply, Approve, or Proven.
type Result interface{ result() }

// Reply answers the request with a result, and authenticates nothing.
type Reply struct{ Result any }

// Approve approves a proposal with its temporary token (§4.11.3).
type Approve struct {
	Token string
	// Name and UserID are what the approval requested for a new account.
	Name, UserID string
}

// Proven is an identity the scheme proved.
type Proven struct {
	Identity Identity
	// Add adds the identity to the connection's account, a guest's
	// included, which then is an account; otherwise the connection signs
	// in to the identity's account.
	Add bool
	// Create makes an account for an identity no account has; otherwise
	// such an identity is refused.
	Create bool
	// Name and UserID suggest a new account's display name and user_id.
	// Name also names a guest that becomes an account by Add.
	Name, UserID string
	// Roles, when SetRoles, are the server roles the identity grants its
	// account, as of this proof.
	Roles    []string
	SetRoles bool
	// Session replies to an Add with a bearer token, as a sign-in does;
	// otherwise an Add replies {}.
	Session bool
	// Label names the identity in refusals, such as "email address".
	Label string
	// Commit, if set, is called once the server has checked the identity
	// and before it changes anything; it returns the record the server
	// keeps with the account for the scheme (Restorer), nil for none, or
	// refuses. The server calls it holding its lock, so it must not block.
	Commit func(userID string) (jsontext.Value, error)
}

func (Reply) result()   {}
func (Approve) result() {}
func (Proven) result()  {}

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
