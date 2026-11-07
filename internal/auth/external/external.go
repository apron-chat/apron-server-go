// Package external is the external sign-in scheme (PROTOCOL.md §4.11.5): a
// client proposes it and opens the page the result names. The page is an
// extension's, which signs the person in however it likes, such as with
// OAuth, and completes the proposal with the identity it proved. The
// server's proposals (authn.Core) make and check the temporary tokens; this
// package serves the pages around them.
package external

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/apron-chat/apron-server-go/internal/authn"
)

// Name is the scheme's name, and its identities'.
const Name = "external"

// Disabled says why the scheme may be off.
const Disabled = "External sign-in is not offered"

// Path serves the pages of proposals: /external/<page>.
const Path = "/external/"

// maxStateBytes bounds the state a proposal carries (§4.11.1).
const maxStateBytes = 256

// ErrUnknownProposal is returned by Complete for a proposal that is
// unknown, expired, or completed.
var ErrUnknownProposal = errors.New("external: the sign-in proposal is unknown, expired, or completed")

// ErrNoSubject is returned by Complete for an identity without a Subject.
var ErrNoSubject = errors.New("external: an identity needs a Subject")

// Proposal is a proposal whose page a person opened.
type Proposal struct {
	ID         string
	Add        bool
	ReturnHost string
}

// Scheme is external sign-in.
type Scheme struct {
	start func(w http.ResponseWriter, r *http.Request, p Proposal)
	core  authn.Core
}

// New is external sign-in whose pages start.
func New(start func(w http.ResponseWriter, r *http.Request, p Proposal)) *Scheme {
	return &Scheme{start: start}
}

// Bind gives the scheme the server's side of sign-in.
func (s *Scheme) Bind(core authn.Core) { s.core = core }

func (*Scheme) Name() string  { return Name }
func (*Scheme) Label() string { return "external sign-in" }

// Auth proposes, returning the page to open, or approves with a token.
func (s *Scheme) Auth(_ context.Context, r *authn.Request) (authn.Result, error) {
	if _, has := r.Params["token"]; has {
		token, err := authn.String(r.Params, "token", true)
		if err != nil {
			return nil, err
		}
		return &authn.Approve{Token: token}, nil
	}
	state, err := authn.String(r.Params, "state", false)
	if err != nil {
		return nil, err
	}
	if len(state) > maxStateBytes {
		return nil, authn.TooLarge("state is at most 256 bytes")
	}
	returnURL, err := authn.String(r.Params, "return_url", false)
	if err != nil {
		return nil, err
	}
	if returnURL != "" {
		parsed, parseErr := url.Parse(returnURL)
		if parseErr != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil {
			return nil, authn.InvalidParams("return_url must be an http or https URL")
		}
	}
	page, proposeErr := r.Conn.ProposePage(authn.PageProposal{State: state, ReturnURL: returnURL})
	if proposeErr != nil {
		return nil, proposeErr
	}
	return &authn.Reply{Result: map[string]any{"url": r.BaseURL + Path + page}}, nil
}

// Routes serves the proposals' pages, handing them to the extension.
func (s *Scheme) Routes(mux *http.ServeMux) {
	mux.HandleFunc(Path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		page := strings.TrimPrefix(r.URL.Path, Path)
		pending, ok := s.core.Page(page)
		if !ok {
			render(w, http.StatusNotFound, view{Title: "This sign-in expired", Text: "Start again in your app."})
			return
		}
		s.start(w, r, Proposal{ID: page, Add: pending.Add, ReturnHost: pending.ReturnHost})
	})
}

// Complete completes the proposal named id with the identity proven, and
// answers the person's browser: with the client's return_url, a page that
// asks them to continue to it with the sign-in link (§4.11.5), and without,
// a page that shows the code to type in the client. For a proposal that is
// no longer open, it answers with a page that says so, and returns
// ErrUnknownProposal.
func (s *Scheme) Complete(w http.ResponseWriter, id string, proof authn.Proof) error {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if proof.Subject == "" {
		render(w, http.StatusInternalServerError, view{Title: "Sign-in failed", Text: "Start again in your app."})
		return ErrNoSubject
	}
	completed, err := s.core.Complete(id, proof)
	if err != nil {
		render(w, http.StatusNotFound, view{Title: "This sign-in expired", Text: "Start again in your app."})
		return ErrUnknownProposal
	}
	page := view{Add: completed.Add}
	switch {
	case completed.ReturnHost != "" && completed.Add:
		page.Title, page.Text = "Add this sign-in to your account", "Continue to "+completed.ReturnHost+" to add it."
	case completed.ReturnHost != "":
		page.Title, page.Text = "Signed in", "Continue to "+completed.ReturnHost+" to finish signing in."
	default:
		page.Title, page.Text = "Your code", "Type this code in the app where you started, and nowhere else."
		page.Code = completed.Code
	}
	if completed.ReturnHost != "" {
		page.Host, page.Link = completed.ReturnHost, template.URL(completed.Link)
	}
	render(w, http.StatusOK, page)
	return nil
}

type view struct {
	Title, Text string
	Add         bool
	// Host and Link continue to the client; Code is typed in it.
	Host string
	Link template.URL
	Code string
}

var pageTemplate = template.Must(template.New("external").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
body{font:16px/1.5 system-ui,sans-serif;max-width:28rem;margin:4rem auto;padding:0 1rem;color:#1d1d1f;background:#fff}
@media (prefers-color-scheme:dark){body{color:#eee;background:#161616}a.button{background:#4f7cff}}
a.button{display:inline-block;padding:.6rem 1.2rem;border-radius:.4rem;background:#2f5be0;color:#fff;text-decoration:none}
.code{font:600 2rem/1 ui-monospace,monospace;letter-spacing:.2em}
</style></head><body>
<h1>{{.Title}}</h1>
<p>{{.Text}}</p>
{{if .Link}}<p><a class="button" href="{{.Link}}">Continue to {{.Host}}</a></p>{{end}}
{{if .Code}}<p class="code">{{.Code}}</p>{{end}}
</body></html>
`))

func render(w http.ResponseWriter, status int, page view) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.WriteHeader(status)
	_ = pageTemplate.Execute(w, page)
}
