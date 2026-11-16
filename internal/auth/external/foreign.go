package external

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/apron-chat/apron-server-go/internal/auth/oauth"
	"github.com/apron-chat/apron-server-go/internal/authn"
)

// Foreign is external sign-in with another authorization server (§4.12),
// such as an OpenID Connect provider: a begin names the issuer's
// authorization endpoint, and the server redeems a code at the issuer's
// token endpoint, or takes an access token the client redeemed itself, and
// accepts only a token the issuer signed for this server (§4.12.4).
//
// The begin's URL carries no client_id: each app names itself with its
// own, an https URL of its client ID metadata document, so the issuer asks
// for consent per app and names it. A client_id shared by every app would
// let any site that can navigate a person's browser get a code for this
// server, without a screen once the person consented to that client.
//
// The issuer must issue access tokens as JWTs (RFC 9068, typ at+jwt: an ID
// token is refused) whose aud names the server's origin, which it binds to the request's resource
// (RFC 8707); introspection is not supported.
type Foreign struct {
	issuer, scope string
	client        *http.Client
	core          authn.Core

	mu       sync.Mutex
	metadata *issuerMetadata
	fetched  time.Time
	// keys is the issuer's key set, nil until a fetch of it worked; keyed
	// is when it was last fetched.
	keys  *jose.JSONWebKeySet
	keyed time.Time
}

// issuerMetadata is what the server reads of the issuer's metadata.
type issuerMetadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
	// UserinfoEndpoint is OpenID Connect's, which names a new account; it
	// may be only in the OpenID Connect metadata.
	UserinfoEndpoint string `json:"userinfo_endpoint"`
}

const (
	// metadataLifetime is how long the issuer's metadata is kept.
	metadataLifetime = time.Hour
	// keysRefresh is the least time between fetches of the issuer's keys,
	// which a token with an unknown kid causes.
	keysRefresh = time.Minute
	// maxIssuerResponse bounds what the server reads from the issuer.
	maxIssuerResponse = 1 << 20
	// clockSkew is the leeway on exp and nbf.
	clockSkew = time.Minute
)

// NewForeign is external sign-in with the authorization server issuer, an
// https URL, asked for scope (empty for none), fetched with client (nil for
// one with a timeout).
func NewForeign(issuer, scope string, client *http.Client) (*Foreign, error) {
	parsed, err := url.Parse(issuer)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return nil, fmt.Errorf("external sign-in: the issuer %q is not an https URL without a query", issuer)
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Foreign{issuer: strings.TrimSuffix(issuer, "/"), scope: scope, client: client}, nil
}

// Bind gives the scheme the server's side of sign-in.
func (f *Foreign) Bind(core authn.Core) { f.core = core }

func (*Foreign) Name() string  { return Name }
func (*Foreign) Label() string { return "external sign-in" }

// Routes serves the server's protected resource metadata (RFC 9728),
// which names the issuer as its authorization server; the server serves
// no authorization server of its own.
func (f *Foreign) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+ResourcePath, func(w http.ResponseWriter, r *http.Request) {
		resourceMetadata(w, oauth.Origin(f.core.BaseURL(r)), f.issuer)
	})
}

// Auth begins, naming the issuer's page, or redeems a code at the issuer,
// or checks a token the client redeemed there.
func (f *Foreign) Auth(ctx context.Context, r *authn.Request) (authn.Result, error) {
	_, hasCode := r.Params["code"]
	_, hasToken := r.Params["token"]
	switch {
	case hasCode:
		params, err := redemption(r.Params)
		if err != nil {
			return nil, err
		}
		token, redeemErr := f.redeem(ctx, params, oauth.Origin(r.BaseURL))
		if redeemErr != nil {
			return nil, redeemErr
		}
		return f.signIn(ctx, r, token)
	case hasToken:
		token, err := authn.String(r.Params, "token", true)
		if err != nil {
			return nil, err
		}
		return f.signIn(ctx, r, token)
	}
	metadata, err := f.discover(ctx)
	if err != nil {
		return nil, err
	}
	page, _ := url.Parse(metadata.AuthorizationEndpoint)
	query := page.Query()
	if f.scope != "" {
		query.Set("scope", f.scope)
	}
	page.RawQuery = query.Encode()
	return &authn.Reply{Result: map[string]any{"external": map[string]any{"url": page.String(), "issuer": f.issuer}}}, nil
}

// signIn checks an access token and proves its identity: a sign-in, or an
// addition on a signed-in connection, as with the server's own codes, whose
// token resumes from any origin.
func (f *Foreign) signIn(ctx context.Context, r *authn.Request, token string) (authn.Result, error) {
	proof, sub, err := f.check(ctx, token, oauth.Origin(r.BaseURL), r.Now)
	if err != nil {
		return nil, err
	}
	if r.Account != nil {
		proof.Name, proof.UserID = "", ""
		return &authn.Add{Proof: proof, Session: r.Account.Guest, AnyOrigin: true}, nil
	}
	proof.Name, proof.UserID = cmp.Or(r.Name, proof.Name), cmp.Or(r.UserID, proof.UserID)
	if proof.Name == "" || proof.UserID == "" {
		if _, known := f.lookup(proof.Subject); !known {
			// A new account, which the access token may not name: Pocket
			// ID's carry no profile claims. Ask the issuer who it is.
			name, username := f.userinfo(ctx, token, sub)
			proof.Name, proof.UserID = cmp.Or(proof.Name, name, username), cmp.Or(proof.UserID, username)
		}
	}
	return &authn.SignIn{Proof: proof, Create: true, AnyOrigin: true}, nil
}

// redeem exchanges a code at the issuer's token endpoint for an access
// token for this server.
func (f *Foreign) redeem(ctx context.Context, p codeParams, origin string) (string, *authn.Error) {
	metadata, err := f.discover(ctx)
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {p.code}, "code_verifier": {p.verifier},
		"redirect_uri": {p.redirectURI}, "client_id": {p.clientID}, "resource": {origin},
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, metadata.TokenEndpoint, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	var answer struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	status, fetchErr := f.fetch(request, &answer)
	switch {
	case fetchErr != nil && status == 0:
		return "", authn.RetryAfter("The sign-in provider is unreachable; try again shortly", 30*time.Second)
	case status != http.StatusOK || answer.AccessToken == "":
		return "", authn.Denied("The sign-in code is invalid, used, or expired; sign in again")
	case !strings.EqualFold(answer.TokenType, "Bearer"):
		// A DPoP or other bound token needs a proof this protocol has none
		// of (§4.12.4).
		return "", authn.Denied("The sign-in provider issued a token bound to a key")
	}
	return answer.AccessToken, nil
}

// lookup is the account of an identity, if the scheme is bound.
func (f *Foreign) lookup(subject string) (authn.Account, bool) {
	if f.core == nil {
		return authn.Account{}, false
	}
	return f.core.Lookup(subject)
}

// userinfo asks the issuer's userinfo endpoint (OpenID Connect Core §5.3)
// with the access token for the person's name and preferred_username,
// which it returns only for the token's sub. A failure is no name, not an
// error: the account is made without one, as before.
func (f *Foreign) userinfo(ctx context.Context, token, sub string) (name, username string) {
	metadata, err := f.discover(ctx)
	if err != nil || metadata.UserinfoEndpoint == "" {
		return "", ""
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, metadata.UserinfoEndpoint, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	var claims struct {
		Sub               string `json:"sub"`
		Name              string `json:"name"`
		PreferredUsername string `json:"preferred_username"`
	}
	if status, err := f.fetch(request, &claims); err != nil || status != http.StatusOK || claims.Sub != sub {
		if f.core != nil {
			f.core.Logger().Warn("external sign-in: no name from the issuer's userinfo", "status", status, "error", err, "sub_matches", claims.Sub == sub)
		}
		return "", ""
	}
	return claims.Name, claims.PreferredUsername
}

// check verifies an access token (§4.12.4): signed by the issuer, iss the
// issuer, aud including this server's origin, unexpired, and bound to no
// key. It proves the identity of the issuer and sub, which it returns too.
func (f *Foreign) check(ctx context.Context, token, origin string, now time.Time) (authn.Proof, string, *authn.Error) {
	invalid := authn.Denied("The sign-in token is invalid, expired, or not for this server")
	if _, err := f.discover(ctx); err != nil {
		return authn.Proof{}, "", err
	}
	payload, err := verifyAccessToken(token, func(kid string) []jose.JSONWebKey { return f.keysFor(ctx, kid) })
	if err != nil {
		return authn.Proof{}, "", invalid
	}
	var c struct {
		jwt.Claims
		Name              string         `json:"name"`
		PreferredUsername string         `json:"preferred_username"`
		Cnf               jsontext.Value `json:"cnf"`
	}
	if json.Unmarshal(payload, &c, json.RejectUnknownMembers(false)) != nil || c.Subject == "" || c.Expiry == nil {
		return authn.Proof{}, "", invalid
	}
	// Only exp and nbf bound the token's time, not iat.
	c.IssuedAt = nil
	if c.ValidateWithLeeway(jwt.Expected{Issuer: f.issuer, Time: now}, clockSkew) != nil ||
		!slices.ContainsFunc(oauth.Resources(origin), c.Audience.Contains) {
		return authn.Proof{}, "", invalid
	}
	if c.Cnf != nil {
		return authn.Proof{}, "", authn.Denied("The sign-in token is bound to a key, which this server cannot check")
	}
	return authn.Proof{
		// The issuer and its subject name the person for good (§4.12.4); an
		// issuer URL has no "#".
		Subject: f.issuer + "#" + c.Subject,
		Name:    cmp.Or(c.Name, c.PreferredUsername), UserID: c.PreferredUsername,
	}, c.Subject, nil
}

// discover returns the issuer's metadata (RFC 8414, or OpenID Connect
// Discovery), fetched at most once an hour while it works.
func (f *Foreign) discover(ctx context.Context) (*issuerMetadata, *authn.Error) {
	f.mu.Lock()
	if f.metadata != nil && time.Since(f.fetched) < metadataLifetime {
		defer f.mu.Unlock()
		return f.metadata, nil
	}
	f.mu.Unlock()
	parsed, _ := url.Parse(f.issuer)
	candidates := []string{
		// RFC 8414 §3.1 puts the well-known part before the issuer's path.
		parsed.Scheme + "://" + parsed.Host + "/.well-known/oauth-authorization-server" + parsed.Path,
		f.issuer + "/.well-known/openid-configuration",
	}
	// The first document that fits is the metadata. An RFC 8414 one may
	// lack OpenID Connect's userinfo_endpoint, which the other then gives.
	var found *issuerMetadata
	for _, candidate := range candidates {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, candidate, nil)
		request.Header.Set("Accept", "application/json")
		var metadata issuerMetadata
		if status, err := f.fetch(request, &metadata); err != nil || status != http.StatusOK {
			continue
		}
		if metadata.Issuer != f.issuer || !isHTTPS(metadata.AuthorizationEndpoint) || !isHTTPS(metadata.TokenEndpoint) || !isHTTPS(metadata.JWKSURI) {
			continue
		}
		if !isHTTPS(metadata.UserinfoEndpoint) {
			metadata.UserinfoEndpoint = ""
		}
		if found == nil {
			found = &metadata
		} else {
			found.UserinfoEndpoint = metadata.UserinfoEndpoint
		}
		if found.UserinfoEndpoint != "" {
			break
		}
	}
	if found != nil {
		f.mu.Lock()
		f.metadata, f.fetched = found, time.Now()
		f.mu.Unlock()
		return found, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.metadata != nil {
		// Keep using what worked while the issuer is away.
		return f.metadata, nil
	}
	return nil, authn.RetryAfter("The sign-in provider is unreachable; try again shortly", 30*time.Second)
}

func isHTTPS(u string) bool {
	parsed, err := url.Parse(u)
	return err == nil && parsed.Scheme == "https" && parsed.Host != ""
}

// keysFor returns the issuer's signing keys for kid, all of them for an
// empty kid, fetching them again when kid is unknown, at most once a minute.
func (f *Foreign) keysFor(ctx context.Context, kid string) []jose.JSONWebKey {
	f.mu.Lock()
	known := f.keys != nil && (kid == "" || len(f.keys.Key(kid)) > 0)
	fresh := time.Since(f.keyed) < keysRefresh
	metadata := f.metadata
	f.mu.Unlock()
	if !known && !fresh && metadata != nil {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, metadata.JWKSURI, nil)
		request.Header.Set("Accept", "application/json")
		var set struct {
			Keys []jsontext.Value `json:"keys"`
		}
		if status, err := f.fetch(request, &set); err == nil && status == http.StatusOK {
			// A key this server cannot use is skipped, not the set.
			keys := new(jose.JSONWebKeySet)
			for _, raw := range set.Keys {
				var k jose.JSONWebKey
				if k.UnmarshalJSON(raw) == nil && signingKey(k) {
					keys.Keys = append(keys.Keys, k)
				}
			}
			f.mu.Lock()
			f.keys = keys
			f.mu.Unlock()
		}
		f.mu.Lock()
		f.keyed = time.Now()
		f.mu.Unlock()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.keys == nil:
		return nil
	case kid == "":
		return f.keys.Keys
	}
	return f.keys.Key(kid)
}

// fetch makes a request to the issuer and decodes its JSON answer into
// into, whatever the status, which it returns: 0 when there was none.
func (f *Foreign) fetch(request *http.Request, into any) (int, error) {
	response, err := f.client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxIssuerResponse))
	if err != nil {
		return response.StatusCode, err
	}
	if err := json.Unmarshal(body, into, json.RejectUnknownMembers(false)); err != nil {
		return response.StatusCode, errors.Join(errors.New("external: the issuer's answer is not JSON"), err)
	}
	return response.StatusCode, nil
}
