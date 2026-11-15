package external

import (
	"cmp"
	"context"
	"crypto"
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

	"github.com/apron-chat/apron-server-go/internal/auth/oauth"
	"github.com/apron-chat/apron-server-go/internal/authn"
)

// Foreign is external sign-in with another authorization server (§4.12),
// such as an OpenID Connect provider: a begin names the issuer's
// authorization endpoint, with a client_id it accepts from any client, and
// the server redeems a code at the issuer's token endpoint, or takes an
// access token the client redeemed itself, and accepts only a token the
// issuer signed for this server (§4.12.4).
//
// The issuer must issue access tokens as JWTs (RFC 9068, typ at+jwt: an ID
// token is refused) whose aud names the server's origin, which it binds to the request's resource
// (RFC 8707); introspection is not supported.
type Foreign struct {
	issuer, clientID, scope string
	client                  *http.Client

	mu       sync.Mutex
	metadata *issuerMetadata
	fetched  time.Time
	keys     map[string][]crypto.PublicKey
	all      []crypto.PublicKey
	keyed    time.Time
}

// issuerMetadata is what the server reads of the issuer's metadata.
type issuerMetadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
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
// https URL, which accepts clientID from any client (empty for an issuer
// that needs none) and scope (empty for none), fetched with client (nil
// for one with a timeout).
func NewForeign(issuer, clientID, scope string, client *http.Client) (*Foreign, error) {
	parsed, err := url.Parse(issuer)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return nil, fmt.Errorf("external sign-in: the issuer %q is not an https URL without a query", issuer)
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Foreign{issuer: strings.TrimSuffix(issuer, "/"), clientID: clientID, scope: scope, client: client}, nil
}

func (*Foreign) Name() string  { return Name }
func (*Foreign) Label() string { return "external sign-in" }

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
	if f.clientID != "" {
		query.Set("client_id", f.clientID)
	}
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
	proof, err := f.check(ctx, token, oauth.Origin(r.BaseURL), r.Now)
	if err != nil {
		return nil, err
	}
	if r.Account != nil {
		proof.Name, proof.UserID = "", ""
		return &authn.Add{Proof: proof, Session: r.Account.Guest, AnyOrigin: true}, nil
	}
	proof.Name, proof.UserID = cmp.Or(r.Name, proof.Name), cmp.Or(r.UserID, proof.UserID)
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

// check verifies an access token (§4.12.4): signed by the issuer, iss the
// issuer, aud including this server's origin, unexpired, and bound to no
// key. It proves the identity of the issuer and sub.
func (f *Foreign) check(ctx context.Context, token, origin string, now time.Time) (authn.Proof, *authn.Error) {
	invalid := authn.Denied("The sign-in token is invalid, expired, or not for this server")
	if _, err := f.discover(ctx); err != nil {
		return authn.Proof{}, err
	}
	claims, err := verifyAccessToken(token, func(kid string) []crypto.PublicKey { return f.keysFor(ctx, kid) })
	if err != nil {
		return authn.Proof{}, invalid
	}
	var c struct {
		Iss               string `json:"iss"`
		Sub               string `json:"sub"`
		Exp               *int64 `json:"exp"`
		Nbf               *int64 `json:"nbf"`
		Name              string `json:"name"`
		PreferredUsername string `json:"preferred_username"`
	}
	raw, _ := json.Marshal(claims)
	if json.Unmarshal(raw, &c, json.RejectUnknownMembers(false)) != nil {
		return authn.Proof{}, invalid
	}
	switch {
	case c.Iss != f.issuer, c.Sub == "", c.Exp == nil:
		return authn.Proof{}, invalid
	case now.After(time.Unix(*c.Exp, 0).Add(clockSkew)):
		return authn.Proof{}, invalid
	case c.Nbf != nil && now.Add(clockSkew).Before(time.Unix(*c.Nbf, 0)):
		return authn.Proof{}, invalid
	case !audienceIncludes(claims["aud"], oauth.Resources(origin)):
		return authn.Proof{}, invalid
	}
	if _, bound := claims["cnf"]; bound {
		return authn.Proof{}, authn.Denied("The sign-in token is bound to a key, which this server cannot check")
	}
	return authn.Proof{
		// The issuer and its subject name the person for good (§4.12.4); an
		// issuer URL has no "#".
		Subject: f.issuer + "#" + c.Sub,
		Name:    cmp.Or(c.Name, c.PreferredUsername), UserID: c.PreferredUsername,
	}, nil
}

// audienceIncludes reports whether aud, a string or an array of them,
// names one of origins.
func audienceIncludes(aud jsontext.Value, origins []string) bool {
	var one string
	if json.Unmarshal(aud, &one) == nil {
		return slices.Contains(origins, one)
	}
	var many []string
	if json.Unmarshal(aud, &many) != nil {
		return false
	}
	return slices.ContainsFunc(many, func(a string) bool { return slices.Contains(origins, a) })
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
		f.mu.Lock()
		f.metadata, f.fetched = &metadata, time.Now()
		f.mu.Unlock()
		return &metadata, nil
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

// keysFor returns the issuer's keys for kid, all of them for an empty kid,
// fetching them again when kid is unknown, at most once a minute.
func (f *Foreign) keysFor(ctx context.Context, kid string) []crypto.PublicKey {
	f.mu.Lock()
	known := f.keys != nil && (kid == "" || f.keys[kid] != nil)
	fresh := time.Since(f.keyed) < keysRefresh
	metadata := f.metadata
	f.mu.Unlock()
	if !known && !fresh && metadata != nil {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, metadata.JWKSURI, nil)
		request.Header.Set("Accept", "application/json")
		var set struct {
			Keys []jwk `json:"keys"`
		}
		if status, err := f.fetch(request, &set); err == nil && status == http.StatusOK {
			keys := make(map[string][]crypto.PublicKey)
			var all []crypto.PublicKey
			for _, k := range set.Keys {
				if k.Use != "" && k.Use != "sig" {
					continue
				}
				if key, err := k.publicKey(); err == nil {
					keys[k.Kid] = append(keys[k.Kid], key)
					all = append(all, key)
				}
			}
			f.mu.Lock()
			f.keys, f.all = keys, all
			f.mu.Unlock()
		}
		f.mu.Lock()
		f.keyed = time.Now()
		f.mu.Unlock()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if kid == "" {
		return f.all
	}
	return f.keys[kid]
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
