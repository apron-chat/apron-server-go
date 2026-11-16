package passkey

import (
	"cmp"
	"crypto/rand"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/apron-chat/apron-server-go/internal/auth/oauth"
	"github.com/apron-chat/apron-server-go/internal/authn"
)

// The passkey page (PLAN_WEBAUTHN_EXTERNAL.md) runs a ceremony for a client
// that cannot use the RP ID: a web client on another domain, an app, or a
// terminal. A begin step names it in `external`, and the client opens it as
// an OAuth authorization endpoint (§4.12.3). The page asks once, naming who
// is asking, runs the passkey on the server's own origin, and redirects
// with a code, which finishes the webauthn exchange on a connection instead
// of a credential.
//
// A code from this page finishes only webauthn, with the action of its
// begin: the page keeps codes of its own. A login's code signs in to the
// passkey's account; a registration's makes a new account, as an invitation
// does; and an addition's, begun on a signed-in connection, adds the
// passkey to that account, only on a connection signed in to it.
//
// With SignalUnknown, a login with a passkey that no account has, nor may
// yet have, such as one made on the page whose code was never redeemed,
// stays on the page, which says so and whose script asks the passkey
// provider to hide it (WebAuthn's signalUnknownCredential). Every other
// failure, and that one without SignalUnknown, is the usual error.

// PagePath is the passkey page.
const PagePath = "/passkey"

const (
	// maxPostBytes bounds what the page's script and buttons post: a
	// credential is a few kilobytes.
	maxPostBytes = 64 << 10
	// maxLabelBytes bounds the name a registration's passkey is labelled
	// with.
	maxLabelBytes = 64
)

// label is a name to label a new passkey with: printable, and at most
// maxLabelBytes, cut at a character.
func label(name string) string {
	name = strings.Join(strings.FieldsFunc(name, func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsPrint(r) }), " ")
	for len(name) > maxLabelBytes {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return name
}

// pageFlow is one person's way through the page: the client's request,
// what they are doing, and the ceremony in progress.
type pageFlow struct {
	id     string
	req    *oauth.Request
	action string
	// origin is the page's, which the ceremony's client data must carry.
	origin string
	// name labels a new account's passkey; addition is the account a
	// passkey is added to, for a registration begun on a signed-in
	// connection.
	name     string
	addition *addition

	mu sync.Mutex
	// data is the pending ceremony; handle the user handle a new
	// account's passkey gets.
	data   *webauthn.SessionData
	handle []byte
	done   bool
}

// addition is a registration begun on a signed-in connection, which the
// page's `request` names: the account, and the user handle its passkey
// gets.
type addition struct {
	id     string
	userID string
	// name is the account's, as the page shows it; requested the name a
	// guest asked for with the begin, which a guest becoming an account
	// takes.
	name, requested string
	guest           bool
	handle          []byte
}

// pageGrant is what a code from the page proves.
type pageGrant struct {
	action     string
	handle     []byte
	credential *webauthn.Credential
	// name is a new account's, or a guest's that becomes one.
	name string
	// accountID is the account an addition adds to; empty otherwise.
	accountID string
}

// pageURL is the passkey page for a begin step, or empty when the page
// cannot serve this server: its origin, the server's, must be one the RP
// accepts. For an addition it records the account, which the page's
// `request` names.
func (s *Scheme) pageURL(r *authn.Request, action string) (string, error) {
	origin := oauth.Origin(r.BaseURL)
	if s.core == nil || !protocol.IsOriginInHaystack(origin, s.w.Config.RPOrigins) {
		return "", nil
	}
	query := url.Values{"action": {action}}
	if action == "register" {
		switch {
		case r.Account != nil:
			a := &addition{userID: r.Account.UserID, name: r.Account.Name, guest: r.Account.Guest}
			if a.guest {
				a.requested = r.Name
			}
			if subject := r.Account.Identities[Name]; subject != "" {
				handle, err := base64.RawURLEncoding.DecodeString(subject)
				if err != nil {
					return "", authn.Internal("Unable to begin passkey registration")
				}
				a.handle = handle
			} else {
				a.handle = []byte(rand.Text())
			}
			id, err := s.additions.Put(a, oauth.FlowLifetime, r.Now)
			if err != nil {
				return "", authn.RetryAfter("Too many passkeys are being added; try again shortly", 30*time.Second)
			}
			a.id = id
			query.Set("request", id)
		case r.Invitation != nil:
			if name := cmp.Or(r.Name, r.Invitation.Name); name != "" {
				query.Set("name", name)
			}
		}
	}
	return origin + PagePath + "?" + query.Encode(), nil
}

func (s *Scheme) pageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+PagePath, s.page)
	mux.HandleFunc("POST "+PagePath, s.pagePost)
}

// page opens the passkey page for an authorization request: its one
// screen, which says who is asking and what they get.
func (s *Scheme) page(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	origin := oauth.Origin(s.core.BaseURL(r))
	req, problem := oauth.Parse(query, origin)
	if problem != nil {
		oauth.RenderProblem(w, problem)
		return
	}
	if !protocol.IsOriginInHaystack(origin, s.w.Config.RPOrigins) {
		// A misconfiguration, not the client's doing.
		oauth.Render(w, http.StatusNotFound, oauth.Page{
			Title: "Passkeys don't work here",
			Lines: []template.HTML{oauth.Line("This server's passkeys don't work at this address. Go back to the app and sign in another way.")},
		})
		return
	}
	f := &pageFlow{req: req, action: query.Get("action"), origin: origin, name: label(query.Get("name"))}
	switch {
	case f.action == "login" && !query.Has("request") && !query.Has("name"):
	case f.action == "register" && query.Has("request"):
		a, ok := s.additions.Get(query.Get("request"), time.Now())
		if !ok {
			oauth.Expired(w)
			return
		}
		f.addition, f.name = a, ""
	case f.action == "register":
	default:
		oauth.RenderProblem(w, &oauth.Problem{Code: "invalid_request", Description: "The app that sent you here asked for something this page does not do (action must be login or register, with request or name only for register)."})
		return
	}
	id, err := s.flows.Put(f, oauth.FlowLifetime, time.Now())
	if err != nil {
		oauth.Render(w, http.StatusServiceUnavailable, oauth.Page{Title: "Too many sign-ins in progress", Lines: []template.HTML{oauth.Line("Wait a minute, then go back to the app and try again.")}})
		return
	}
	f.id = id
	oauth.Render(w, http.StatusOK, s.screen(f))
}

// screen is the page's one screen, before the passkey: who is asking, and
// what they get. A login redirects as soon as its passkey is verified.
func (s *Scheme) screen(f *pageFlow) oauth.Page {
	host := hostOf(f.origin)
	client := f.req.Client()
	page := oauth.Page{
		Title:   "Sign in to " + cmp.Or(s.core.Title(), host),
		Note:    host + " has not verified this app.",
		Warning: "Only continue if you just started signing in on " + f.req.Started() + ".",
		Action:  PagePath, Fields: map[string]string{"flow": f.id},
		Script: pageScript,
		Data:   map[string]string{"flow": f.id, "post": PagePath, "ceremony": "get"},
	}
	button := oauth.Button{ID: "passkey", Label: "Continue with passkey", Primary: true, Script: true}
	switch {
	case f.addition != nil:
		page.Title = "Add a passkey"
		page.Lines = []template.HTML{oauth.Line(
			oauth.Strong(client), " wants to add a passkey to ", oauth.Strong(oauth.AccountName(f.addition.name, f.addition.userID)),
			"’s account on ", oauth.Strong(host), ".",
		)}
		button.Label, page.Data["ceremony"] = "Create passkey", "create"
	case f.action == "register":
		page.Title = "Create an account on " + cmp.Or(s.core.Title(), host)
		page.Lines = []template.HTML{oauth.Line(
			oauth.Strong(client), " wants to create an account for you on ", oauth.Strong(host),
			", with a new passkey. It will be able to read and send messages as you.",
		)}
		button.Label, page.Data["ceremony"] = "Create passkey", "create"
	default:
		page.Title = "Sign in to " + cmp.Or(s.core.Title(), host) + " on " + f.req.ClientInline() + "?"
		page.Lines = []template.HTML{oauth.Line("It will be able to read and send messages as you.")}
	}
	page.Buttons = []oauth.Button{button, {Op: "cancel", Label: "Cancel"}}
	return page
}

// pagePost takes the page's requests: the script's options and verify,
// and the button cancel.
func (s *Scheme) pagePost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxPostBytes)
	op := r.PostFormValue("op")
	scripted := op == "options" || op == "verify"
	f, ok := s.flows.Get(r.PostFormValue("flow"), time.Now())
	if ok {
		f.mu.Lock()
		defer f.mu.Unlock()
		ok = !f.done
	}
	if !ok {
		if scripted {
			pageJSON(w, http.StatusNotFound, map[string]string{"error": "This sign-in expired. Go back to the app and start again."})
		} else {
			oauth.Expired(w)
		}
		return
	}
	switch op {
	case "options":
		s.pageOptions(w, f)
	case "verify":
		s.pageVerify(w, r, f)
	case "cancel":
		s.finishFlowLocked(f)
		f.req.Deny(w, r)
	default:
		http.Error(w, "Unknown action", http.StatusBadRequest)
	}
}

// finishFlowLocked forgets a flow whose page redirected. Called holding
// f.mu.
func (s *Scheme) finishFlowLocked(f *pageFlow) {
	f.done = true
	s.flows.Delete(f.id)
}

// pageOptions begins the page's ceremony, replacing any it had, and
// answers with its WebAuthn options, as public_key.
func (s *Scheme) pageOptions(w http.ResponseWriter, f *pageFlow) {
	f.data = nil
	var publicKey any
	if f.action == "login" {
		assertion, data, err := s.w.BeginDiscoverableLogin(webauthn.WithLoginOrigin(f.origin), webauthn.WithUserVerification(protocol.VerificationRequired))
		if err != nil {
			pageJSON(w, http.StatusInternalServerError, map[string]string{"error": "Unable to begin passkey sign-in"})
			return
		}
		assertion.Response.Timeout = int(ceremonyLifetime / time.Millisecond)
		publicKey, f.data = assertion.Response, data
	} else {
		h := &holder{name: cmp.Or(f.name, hostOf(f.origin))}
		if a := f.addition; a != nil {
			h.Handle, h.name = a.handle, cmp.Or(a.requested, a.name, a.userID)
			s.mu.Lock()
			if existing := s.byHandle[string(a.handle)]; existing != nil {
				h.Credentials = append([]webauthn.Credential(nil), existing.Credentials...)
			}
			s.mu.Unlock()
		} else {
			if f.handle == nil {
				f.handle = []byte(rand.Text())
			}
			h.Handle = f.handle
		}
		if len(h.Credentials) >= maxPasskeys {
			pageJSON(w, http.StatusForbidden, map[string]string{"error": "This account already has ten passkeys."})
			return
		}
		exclusions := make([]protocol.CredentialDescriptor, 0, len(h.Credentials))
		for _, credential := range h.Credentials {
			exclusions = append(exclusions, credential.Descriptor())
		}
		creation, data, err := s.w.BeginRegistration(h,
			webauthn.WithRegistrationOrigin(f.origin),
			webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
			webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
				ResidentKey:      protocol.ResidentKeyRequirementRequired,
				UserVerification: protocol.VerificationRequired,
			}), webauthn.WithExclusions(exclusions))
		if err != nil {
			pageJSON(w, http.StatusInternalServerError, map[string]string{"error": "Unable to begin passkey registration"})
			return
		}
		creation.Response.Timeout = int(ceremonyLifetime / time.Millisecond)
		publicKey, f.data = creation.Response, data
	}
	f.data.Expires = time.Now().Add(ceremonyLifetime)
	pageJSON(w, http.StatusOK, map[string]any{"public_key": publicKey})
}

// pageVerify verifies the page's ceremony, which it uses up, and answers
// with the redirect to the client with a code.
func (s *Scheme) pageVerify(w http.ResponseWriter, r *http.Request, f *pageFlow) {
	data := f.data
	f.data = nil
	fail := func(message string) {
		pageJSON(w, http.StatusBadRequest, map[string]string{"error": message})
	}
	if data == nil || !time.Now().Before(data.Expires) {
		fail("That took too long. Try again.")
		return
	}
	var credential map[string]jsontext.Value
	if err := json.Unmarshal([]byte(r.PostFormValue("credential")), &credential); err != nil {
		fail("Your browser sent no passkey. Try again.")
		return
	}
	raw, err := parseCredential(map[string]jsontext.Value{"credential": jsontext.Value(r.PostFormValue("credential"))}, f.action)
	if err != nil || !s.sameOrigin(raw, f.origin) {
		fail("That passkey didn't work. Try again.")
		return
	}
	if f.action == "login" {
		handle, verified, unknown, err := s.verifyLogin(raw, data)
		if unknown != nil && s.SignalUnknown && !s.registering(unknown) {
			// No account has this passkey, and no registration may yet
			// make it one's, such as one whose code was never redeemed:
			// the script asks the passkey provider to hide it
			// (signalUnknownCredential, with the ID of its own
			// assertion), and the person stays on the page. Without
			// SignalUnknown it is the usual error, which tells nobody
			// whether a credential ID is registered.
			pageJSON(w, http.StatusBadRequest, map[string]any{
				"error":              "This passkey isn't for an account on " + hostOf(f.origin) + ". Try another passkey, or cancel.",
				"unknown_credential": true,
			})
			return
		}
		if err != nil {
			fail("That passkey didn't work. Try again, or use another passkey.")
			return
		}
		subject := Subject(handle)
		_, known := s.core.Lookup(subject)
		if !known || !s.core.Admits(subject, false) {
			fail("This passkey's account can't sign in to " + hostOf(f.origin) + ".")
			return
		}
		s.redirectWithCode(w, f, &pageGrant{action: "login", handle: handle, credential: verified})
		return
	}
	g := &pageGrant{action: "register", name: f.name}
	if a := f.addition; a != nil {
		g.handle, g.accountID = a.handle, a.userID
		if a.guest {
			g.name = a.requested
		}
	} else {
		g.handle = f.handle
	}
	g.credential, err = s.verifyRegistration(raw, data, g.handle, g.name)
	if err != nil {
		fail("That passkey didn't work. Try again.")
		return
	}
	s.redirectWithCode(w, f, g)
}

// redirectWithCode answers the page's script with the redirect to the
// client with a code for g, and ends the flow. A registration's credential
// is pending before the code exists, and as long as redeeming it may yet
// reach Commit (registering). Called holding f.mu.
func (s *Scheme) redirectWithCode(w http.ResponseWriter, f *pageFlow, g *pageGrant) {
	if g.action == "register" {
		s.markPending(g.credential.ID, oauth.CodeLifetime+ceremonyLifetime)
	}
	code, err := s.codes.Issue(f.req.Grant(), g, time.Now())
	if err != nil {
		pageJSON(w, http.StatusBadRequest, map[string]string{"error": "Too many sign-ins in progress. Wait a minute, then try again."})
		return
	}
	if f.addition != nil {
		s.additions.Delete(f.addition.id)
	}
	s.finishFlowLocked(f)
	pageJSON(w, http.StatusOK, map[string]string{"redirect": f.req.ReturnURL(url.Values{"code": {code}})})
}

// sameOrigin checks a credential's client data the page needs besides
// WebAuthn verification: the page's own origin, and no frame (WebAuthn
// Level 3 §13.4.9), since a ceremony inside a frame on another site is
// clickjacking the page's consent.
func (s *Scheme) sameOrigin(raw []byte, origin string) bool {
	var credential struct {
		Response struct {
			ClientDataJSON string `json:"clientDataJSON"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &credential) != nil {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(credential.Response.ClientDataJSON)
	if err != nil {
		return false
	}
	var data map[string]jsontext.Value
	if json.Unmarshal(decoded, &data) != nil {
		return false
	}
	if _, has := data["topOrigin"]; has {
		return false
	}
	var crossOrigin bool
	if value, has := data["crossOrigin"]; has && (json.Unmarshal(value, &crossOrigin) != nil || crossOrigin) {
		return false
	}
	var got string
	return json.Unmarshal(data["origin"], &got) == nil && got == origin
}

// redeem finishes a webauthn exchange with a code from the page, on any
// connection: a login signs in, a new account's registration makes the
// account with the connection's invitation, and an addition adds the
// passkey to the account that began it, only on a connection signed in to
// it. Unlike a ceremony in the client's, the token resumes from any origin:
// it is a token from a code.
func (s *Scheme) redeem(r *authn.Request, action string) (authn.Result, error) {
	var p [4]string
	for i, name := range []string{"code", "code_verifier", "redirect_uri", "client_id"} {
		value, err := authn.String(r.Params, name, true)
		if err != nil {
			return nil, err
		}
		if value == "" {
			return nil, authn.InvalidParams("%s must be a non-empty string", name)
		}
		p[i] = value
	}
	invalid := authn.Denied("The passkey code is invalid, used, or expired; try again")
	value, grant, err := s.codes.Redeem(p[0], p[1], p[2], p[3], r.Now)
	if err != nil || !slices.Contains(oauth.Resources(oauth.Origin(r.BaseURL)), grant.Resource) {
		return nil, invalid
	}
	g := value.(*pageGrant)
	if g.action != action {
		return nil, invalid
	}
	switch {
	case g.action == "login":
		return &authn.SignIn{Proof: s.loginProof(g.handle, g.credential), AnyOrigin: true}, nil
	case g.accountID != "":
		if r.Account == nil || r.Account.UserID != g.accountID {
			return nil, authn.Denied("Finish adding this passkey where the account that asked for it is signed in")
		}
		if subject := r.Account.Identities[Name]; subject != "" && subject != Subject(g.handle) {
			return nil, authn.Denied("Registration changed; try again")
		}
		return &authn.Add{Session: true, AnyOrigin: true, Proof: authn.Proof{
			Subject: Subject(g.handle), Name: g.name, Credential: CredentialID(g.credential.ID),
			Commit: s.commitCredential(g.handle, g.credential),
		}}, nil
	}
	// A new account, which an invitation makes on this server, as a
	// registration in the client does.
	if r.Account != nil {
		return nil, authn.Denied("This passkey is for a new account; finish on a connection that is not signed in")
	}
	if r.Invitation == nil {
		return nil, authn.Denied("Present an invitation to make an account with this passkey")
	}
	return &authn.SignIn{Create: true, Invitation: r.Invitation.ID, AnyOrigin: true, Proof: authn.Proof{
		Subject: Subject(g.handle), Name: cmp.Or(r.Name, g.name, r.Invitation.Name), UserID: r.Invitation.UserID,
		Credential: CredentialID(g.credential.ID), Commit: s.commitCredential(g.handle, g.credential),
	}}, nil
}

func pageJSON(w http.ResponseWriter, status int, value any) {
	body, _ := json.Marshal(value)
	header := w.Header()
	header.Set("Content-Type", "application/json")
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// hostOf is a URL's host, as the page names the server.
func hostOf(base string) string {
	if parsed, err := url.Parse(base); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return base
}
