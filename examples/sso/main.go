// Command sso is an Apron server for a team that signs in with GitHub: an
// extension offers external sign-in (PROTOCOL.md §4.11.5) through a GitHub
// OAuth app, lets in only the members of one organization, and makes some
// of them admins.
//
// Register an OAuth app at https://github.com/settings/developers with the
// callback URL https://chat.example/sso/callback, then run:
//
//	go run ./examples/sso --public-url https://chat.example \
//	  --origin https://chat.example --disable-guests \
//	  --sso.base-url https://chat.example \
//	  --sso.client-id ... --sso.client-secret ... \
//	  --sso.org my-team --sso.admin octocat
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/aprond"
)

func main() { aprond.Main(&SSO{}) }

// SSO signs people in with GitHub.
type SSO struct {
	Settings struct {
		ClientID     string   `long:"client-id" description:"The GitHub OAuth app's client ID"`
		ClientSecret string   `long:"client-secret" description:"The GitHub OAuth app's client secret"`
		BaseURL      string   `long:"base-url" description:"This server's public URL, which the OAuth app's callback URL starts with, such as https://chat.example"`
		Org          string   `long:"org" description:"Only members of this GitHub organization may sign in; empty lets in everyone"`
		Admins       []string `long:"admin" description:"A GitHub login that gets the admin role; repeat for more"`
	}
	host   *apron.Host
	client *http.Client
	// github is where GitHub's sign-in pages and API are, which tests
	// replace.
	github struct{ web, api string }
}

func (*SSO) Name() string       { return "sso" }
func (s *SSO) Flags() any       { return &s.Settings }
func (s *SSO) callback() string { return strings.TrimRight(s.Settings.BaseURL, "/") + "/sso/callback" }

func (s *SSO) Init(h *apron.Host) error {
	if s.Settings.ClientID == "" || s.Settings.ClientSecret == "" || s.Settings.BaseURL == "" {
		return errors.New("set --sso.client-id, --sso.client-secret, and --sso.base-url")
	}
	s.host = h
	s.client = &http.Client{Timeout: 15 * time.Second}
	if s.github.web == "" {
		s.github.web, s.github.api = "https://github.com", "https://api.github.com"
	}
	h.ExternalSignIn(s.start)
	h.Handle("GET /sso/callback", http.HandlerFunc(s.finish))
	return nil
}

// start sends the person to GitHub. The proposal's ID is unguessable and
// works once, so it serves as OAuth's state.
func (s *SSO) start(w http.ResponseWriter, r *http.Request, p apron.ExternalProposal) {
	query := url.Values{
		"client_id":    {s.Settings.ClientID},
		"redirect_uri": {s.callback()},
		"state":        {p.ID},
		"allow_signup": {"false"},
	}
	if s.Settings.Org != "" {
		query.Set("scope", "read:org")
	}
	http.Redirect(w, r, s.github.web+"/login/oauth/authorize?"+query.Encode(), http.StatusFound)
}

// finish exchanges GitHub's code for the person's identity and completes
// the proposal with it.
func (s *SSO) finish(w http.ResponseWriter, r *http.Request) {
	proposal := r.URL.Query().Get("state")
	identity, err := s.identify(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		s.host.Logger().Warn("GitHub sign-in refused", "error", err)
		http.Error(w, "Sign-in failed: "+err.Error(), http.StatusForbidden)
		return
	}
	if err := s.host.CompleteExternal(w, r, proposal, identity); err != nil {
		s.host.Logger().Info("sign-in not completed", "error", err)
	}
}

func (s *SSO) identify(ctx context.Context, code string) (apron.Identity, error) {
	if code == "" {
		return apron.Identity{}, errors.New("GitHub sent no code")
	}
	var token struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error_description"`
	}
	form := url.Values{"client_id": {s.Settings.ClientID}, "client_secret": {s.Settings.ClientSecret}, "code": {code}, "redirect_uri": {s.callback()}}
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, s.github.web+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := s.fetch(request, &token); err != nil {
		return apron.Identity{}, err
	}
	if token.AccessToken == "" {
		return apron.Identity{}, fmt.Errorf("GitHub refused the code: %s", token.Error)
	}

	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
	}
	if err := s.api(ctx, token.AccessToken, "/user", &user); err != nil {
		return apron.Identity{}, err
	}
	if org := s.Settings.Org; org != "" {
		var membership struct {
			State string `json:"state"`
		}
		if err := s.api(ctx, token.AccessToken, "/user/memberships/orgs/"+url.PathEscape(org), &membership); err != nil || membership.State != "active" {
			return apron.Identity{}, fmt.Errorf("%s is not a member of %s", user.Login, org)
		}
	}
	identity := apron.Identity{Subject: "github:" + strconv.FormatInt(user.ID, 10), Name: user.Name, UserID: user.Login}
	if identity.Name == "" {
		identity.Name = user.Login
	}
	if slices.ContainsFunc(s.Settings.Admins, func(login string) bool { return strings.EqualFold(login, user.Login) }) {
		identity.Roles = []string{"admin"}
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
	return json.NewDecoder(response.Body).Decode(into)
}
