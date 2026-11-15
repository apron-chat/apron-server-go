package oauth

import (
	"crypto/rand"
	"html/template"
	"net/http"
	"strings"
)

// Page is one screen of a sign-in page: a heading, a few lines, and a form
// whose buttons post back to the page.
type Page struct {
	// Title is the heading, and the document's title.
	Title string
	// Lines are the screen's paragraphs (Line).
	Lines []template.HTML
	// Note is a muted line, such as that the server has not verified the
	// app; Warning a line that stands out, such as "Only continue if …".
	Note, Warning string
	// Action is where the form posts, with Fields as hidden inputs and
	// the button pressed as op.
	Action  string
	Fields  map[string]string
	Buttons []Button
	// Alt is a secondary action, shown as a link under the lines, such as
	// "Not you? Use another passkey".
	Alt *Button
	// Script runs on the page, with Data as its <main>'s data-* attributes.
	Script template.JS
	Data   map[string]string
}

// Button is one of a form's buttons, which posts op=Op; with Script, it
// runs the page's script instead, which finds it by its ID.
type Button struct {
	Op, Label string
	Primary   bool
	Script    bool
	ID        string
}

// Strong is a part of a Line shown in bold: a host, an app, or an account.
type Strong string

// Line is a paragraph of plain text and Strong parts, escaped.
func Line(parts ...any) template.HTML {
	var b strings.Builder
	for _, part := range parts {
		switch part := part.(type) {
		case Strong:
			b.WriteString("<strong>" + template.HTMLEscapeString(string(part)) + "</strong>")
		case string:
			b.WriteString(template.HTMLEscapeString(part))
		}
	}
	return template.HTML(b.String())
}

var pageTemplate = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>{{.Page.Title}}</title>
<style>
:root{color-scheme:light dark;--fg:#1d1d1f;--muted:#6e6e73;--bg:#fff;--accent:#2f5be0;--line:#d2d2d7;--warn:#8a4b00}
@media (prefers-color-scheme:dark){:root{--fg:#eee;--muted:#a1a1a6;--bg:#161616;--accent:#4f7cff;--line:#3a3a3c;--warn:#ffb35c}}
body{font:16px/1.5 system-ui,sans-serif;max-width:28rem;margin:4rem auto;padding:0 1rem;color:var(--fg);background:var(--bg)}
h1{font-size:1.5rem;line-height:1.25}
.note{color:var(--muted)}
.warning{color:var(--warn);font-weight:500}
.buttons{display:flex;gap:.75rem;flex-wrap:wrap;margin-top:1.5rem}
button{font:inherit;padding:.6rem 1.2rem;border-radius:.4rem;border:1px solid var(--line);background:transparent;color:var(--fg);cursor:pointer}
button.primary{background:var(--accent);border-color:var(--accent);color:#fff}
button.link{border:0;padding:0;color:var(--accent);text-decoration:underline}
button:disabled{opacity:.6;cursor:default}
#status{min-height:1.5em;color:var(--warn)}
</style></head><body>
<main{{range $k, $v := .Page.Data}} data-{{$k}}="{{$v}}"{{end}}>
<h1>{{.Page.Title}}</h1>
{{range .Page.Lines}}<p>{{.}}</p>
{{end}}{{if .Page.Note}}<p class="note">{{.Page.Note}}</p>
{{end}}{{if .Page.Warning}}<p class="warning">{{.Page.Warning}}</p>
{{end}}{{if .Page.Action}}<form method="post" action="{{.Page.Action}}">
{{range $k, $v := .Page.Fields}}<input type="hidden" name="{{$k}}" value="{{$v}}">
{{end}}{{with .Page.Alt}}<p><button class="link" name="op" value="{{.Op}}">{{.Label}}</button></p>
{{end}}{{if .Page.Script}}<p id="status" role="status"></p>
{{end}}<div class="buttons">{{range .Page.Buttons}}{{if .Script}}<button type="button" id="{{.ID}}"{{if .Primary}} class="primary"{{end}}>{{.Label}}</button>{{else}}<button name="op" value="{{.Op}}"{{if .Primary}} class="primary"{{end}}>{{.Label}}</button>{{end}}
{{end}}</div>
</form>
{{end}}</main>
{{if .Page.Script}}<script nonce="{{.Nonce}}">{{.Page.Script}}</script>
{{end}}</body></html>
`))

// Render writes a page with the headers every sign-in page sends: no
// caching, no referrer, and never inside a frame, since its buttons are
// consent and "Continue with passkey" is the click a passkey prompt needs
// (clickjacking).
func Render(w http.ResponseWriter, status int, page Page) {
	nonce := rand.Text()
	header := w.Header()
	header.Set("Content-Type", "text/html; charset=utf-8")
	header.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'nonce-"+nonce+"'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
	header.Set("X-Frame-Options", "DENY")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Cache-Control", "no-store")
	header.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	_ = pageTemplate.Execute(w, struct {
		Page  Page
		Nonce string
	}{page, nonce})
}

// RenderProblem shows a problem with an authorization request on the page:
// one whose redirect_uri is not known to be good, which must not be
// redirected to.
func RenderProblem(w http.ResponseWriter, p *Problem) {
	Render(w, http.StatusBadRequest, Page{
		Title: "This sign-in link doesn't work",
		Lines: []template.HTML{Line(p.Description), Line("Go back to the app and try signing in again.")},
	})
}

// Expired shows that the page's own state is gone, such as after
// FlowLifetime, or once its code was issued.
func Expired(w http.ResponseWriter) {
	Render(w, http.StatusNotFound, Page{
		Title: "This sign-in expired",
		Lines: []template.HTML{Line("Go back to the app and start signing in again.")},
	})
}

// AccountName names an account as the pages show it: Ada (@ada).
func AccountName(name, userID string) string {
	if name == "" || name == userID {
		return "@" + userID
	}
	return name + " (@" + userID + ")"
}
