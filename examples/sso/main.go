// Command sso is an Apron server for a team that signs in with GitHub: an
// extension offers external sign-in (PROTOCOL.md §4.12) through a GitHub
// OAuth app, lets in only the members of one organization, and makes some
// of them admins.
//
// The server is the authorization server apps sign in with: an app opens
// its page at /oauth/authorize, which hands the person to this extension.
// The extension sends them to GitHub and back, and completes the sign-in
// with their GitHub identity; the page then asks them to allow the app,
// naming it, and returns to it with a code.
//
// Register an OAuth app at https://github.com/settings/developers with the
// callback URL https://chat.example/sso/callback, then run:
//
//	go run ./examples/sso --public-url https://chat.example \
//	  --origin https://chat.example --disable-guests \
//	  --sso.client-id ... --sso.client-secret ... \
//	  --sso.org my-team --sso.admin octocat
//
// Set --public-url in a deployment: without it, the callback URL the
// extension sends to GitHub follows the Host header of the request that
// started the sign-in, which the client chooses.
package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/apron-chat/apron-server-go/aproncmd"
	"github.com/apron-chat/apron-server-go/aprond"
)

func main() { aproncmd.Main(&SSO{}) }

// SSO signs people in with GitHub.
type SSO struct {
	Settings struct {
		ClientID     string   `long:"client-id" description:"The GitHub OAuth app's client ID"`
		ClientSecret string   `long:"client-secret" description:"The GitHub OAuth app's client secret"`
		Org          string   `long:"org" description:"Only members of this GitHub organization may sign in; empty lets in everyone"`
		Admins       []string `long:"admin" description:"A GitHub login that gets the admin role; repeat for more"`
	}
	host   *aprond.Host
	signIn *aprond.ExternalSignIn
	client *http.Client
	// github is where GitHub's sign-in pages and API are, which tests
	// replace.
	github struct{ web, api string }
}

func (*SSO) Name() string { return "sso" }
func (s *SSO) Flags() any { return &s.Settings }

// callback is the OAuth app's callback URL: under --public-url, if set,
// and else under the scheme and Host header r arrived with, which only a
// development server should rely on.
func (s *SSO) callback(r *http.Request) string { return s.host.BaseURL(r) + "/sso/callback" }

func (s *SSO) Init(r *aprond.Registrar) error {
	if s.Settings.ClientID == "" || s.Settings.ClientSecret == "" {
		return errors.New("set --sso.client-id and --sso.client-secret")
	}
	s.host = r.Host
	s.client = &http.Client{Timeout: 15 * time.Second}
	if s.github.web == "" {
		s.github.web, s.github.api = "https://github.com", "https://api.github.com"
	}
	s.signIn = r.ExternalSignIn(s.start)
	r.Handle("GET /sso/callback", http.HandlerFunc(s.finish))
	return nil
}

// start sends the person to GitHub. The sign-in's ID is unguessable and
// works once, so it serves as OAuth's state.
func (s *SSO) start(w http.ResponseWriter, r *http.Request, req aprond.ExternalRequest) {
	query := url.Values{
		"client_id":    {s.Settings.ClientID},
		"redirect_uri": {s.callback(r)},
		"state":        {req.ID},
		"allow_signup": {"false"},
	}
	if s.Settings.Org != "" {
		query.Set("scope", "read:org")
	}
	http.Redirect(w, r, s.github.web+"/login/oauth/authorize?"+query.Encode(), http.StatusFound)
}

// finish exchanges GitHub's code for the person's identity and completes
// the sign-in with it; the server's page then asks them to allow the app.
// A person who declined at GitHub goes back to the app, which hears so; one
// who is not a member of the organization is refused.
func (s *SSO) finish(w http.ResponseWriter, r *http.Request) {
	signIn := r.URL.Query().Get("state")
	if r.URL.Query().Get("error") == "access_denied" {
		_ = s.signIn.Cancel(w, r, signIn)
		return
	}
	identity, err := s.identify(r.Context(), r.URL.Query().Get("code"), s.callback(r))
	if err != nil {
		// The server's page says why, and lets the person sign in as
		// someone else, or go back to the app, which hears why too.
		s.host.Logger().Warn("GitHub sign-in refused", "error", err)
		_ = s.signIn.Deny(w, r, signIn, err.Error())
		return
	}
	if err := s.signIn.Complete(w, r, signIn, identity); err != nil {
		s.host.Logger().Info("sign-in not completed", "error", err)
	}
}

func (s *SSO) identify(ctx context.Context, code, callback string) (aprond.Identity, error) {
	if code == "" {
		return aprond.Identity{}, errors.New("GitHub sent no code")
	}
	var token struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error_description"`
	}
	form := url.Values{"client_id": {s.Settings.ClientID}, "client_secret": {s.Settings.ClientSecret}, "code": {code}, "redirect_uri": {callback}}
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, s.github.web+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := s.fetch(request, &token); err != nil {
		return aprond.Identity{}, err
	}
	if token.AccessToken == "" {
		return aprond.Identity{}, fmt.Errorf("GitHub refused the code: %s", token.Error)
	}

	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
	}
	if err := s.api(ctx, token.AccessToken, "/user", &user); err != nil {
		return aprond.Identity{}, err
	}
	if org := s.Settings.Org; org != "" {
		var membership struct {
			State string `json:"state"`
		}
		if err := s.api(ctx, token.AccessToken, "/user/memberships/orgs/"+url.PathEscape(org), &membership); err != nil || membership.State != "active" {
			return aprond.Identity{}, fmt.Errorf("%s is not a member of %s", user.Login, org)
		}
	}
	identity := aprond.Identity{Subject: "github:" + strconv.FormatInt(user.ID, 10), Name: user.Name, UserID: user.Login}
	if identity.Name == "" {
		identity.Name = user.Login
	}
	if slices.ContainsFunc(s.Settings.Admins, func(login string) bool { return strings.EqualFold(login, user.Login) }) {
		identity.Roles = []string{aprond.RoleAdmin}
	}
	return identity, nil
}

func (s *SSO) api(ctx context.Context, token, path string, into any) error {
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.github.api+path, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	return s.fetch(request, into)
}

func (s *SSO) fetch(request *http.Request, into any) error {
	request.Header.Set("Accept", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", request.URL.Path, response.Status)
	}
	return json.UnmarshalRead(response.Body, into)
}
