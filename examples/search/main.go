// Command search is an Apron server with full-text search and a public web
// archive. An extension indexes every message as it is logged, rebuilding
// its index from the whole log at each start, and offers:
//
//   - a search.query request (capability ext:search) and a /search command,
//     which find only messages in rooms the person may see;
//   - /search/archive/, web pages of the public rooms' messages, which
//     search engines can index.
//
// Run it with:
//
//	go run ./examples/search
package main

import (
	"cmp"
	"context"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/aproncmd"
)

func main() { aproncmd.Main(&Search{}) }

// Search is an in-memory inverted index of the messages' current text.
type Search struct {
	host *apron.Host

	mu       sync.RWMutex
	messages map[string]apron.Message   // by message_id, the latest snapshot
	postings map[string]map[string]bool // by token, the message_ids that have it
}

func (*Search) Name() string { return "search" }

func (s *Search) Init(r *apron.Registrar) error {
	s.host = r.Host
	s.messages = make(map[string]apron.Message)
	s.postings = make(map[string]map[string]bool)
	r.OnEventFrom(0, func(_ context.Context, e apron.Event) error {
		if m, ok := apron.DecodeMessage(e); ok {
			s.index(m)
		}
		return nil
	})
	r.Method("search.query", s.query)
	r.Command(apron.Command{
		Name: "search", Usage: "/search <words>", Help: "find messages with these words",
		Run: func(_ context.Context, call *apron.CommandCall) (map[string]any, error) {
			hits := s.search(call.User, call.Args, "", 10)
			if len(hits) == 0 {
				call.Notice("Nothing found for " + call.Args)
				return nil, nil
			}
			var b strings.Builder
			for _, m := range hits {
				room, _ := s.host.Room(m.RoomID)
				fmt.Fprintf(&b, "- **%s** in %s: %s\n", cmp.Or(m.From.Name, m.From.UserID), room.Title, excerpt(m.Text))
			}
			call.Notice(b.String())
			return nil, nil
		},
	})
	r.Handle("GET /search/archive/", http.HandlerFunc(s.archiveIndex))
	r.Handle("GET /search/archive/{room}", http.HandlerFunc(s.archiveRoom))
	return r.Advertise(map[string]any{"method": "search.query"})
}

// index replaces what the index holds for a message with its snapshot. A
// deletion's tombstone removes it.
func (s *Search) index(m apron.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.messages[m.ID]; ok {
		for _, token := range tokens(old.Text) {
			delete(s.postings[token], m.ID)
			if len(s.postings[token]) == 0 {
				delete(s.postings, token)
			}
		}
		delete(s.messages, m.ID)
	}
	if m.Deleted || strings.HasPrefix(m.From.UserID, "~") {
		return
	}
	s.messages[m.ID] = m
	for _, token := range tokens(m.Text) {
		if s.postings[token] == nil {
			s.postings[token] = make(map[string]bool)
		}
		s.postings[token][m.ID] = true
	}
}

// tokens are the distinct lowercased words of text.
func tokens(text string) []string {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	slices.Sort(words)
	return slices.DeleteFunc(slices.Compact(words), func(word string) bool { return len(word) < 2 })
}

// search returns the latest messages with every word of query that u may
// see, in roomID if it is set.
func (s *Search) search(u apron.User, query, roomID string, limit int) []apron.Message {
	words := tokens(query)
	if len(words) == 0 {
		return nil
	}
	s.mu.RLock()
	var hits []apron.Message
	for id := range s.postings[words[0]] {
		if !slices.ContainsFunc(words[1:], func(word string) bool { return !s.postings[word][id] }) {
			if m := s.messages[id]; roomID == "" || m.RoomID == roomID {
				hits = append(hits, m)
			}
		}
	}
	s.mu.RUnlock()
	slices.SortFunc(hits, func(a, b apron.Message) int { return cmp.Compare(b.LogID, a.LogID) })
	// Only rooms the person may see: a private room's messages never show.
	visible := make(map[string]bool)
	hits = slices.DeleteFunc(hits, func(m apron.Message) bool {
		ok, seen := visible[m.RoomID]
		if !seen {
			ok = s.host.Visible(u, m.RoomID)
			visible[m.RoomID] = ok
		}
		return !ok
	})
	return hits[:min(len(hits), limit)]
}

func (s *Search) query(_ context.Context, call *apron.MethodCall) (any, error) {
	var params struct {
		Q      string `json:"q"`
		RoomID string `json:"room_id"`
		Limit  int    `json:"limit"`
	}
	if err := call.Decode(&params); err != nil {
		return nil, err
	}
	if strings.TrimSpace(params.Q) == "" {
		return nil, apron.InvalidParams("q must name words to find")
	}
	if params.Limit <= 0 || params.Limit > 100 {
		params.Limit = 20
	}
	results := []any{}
	for _, m := range s.search(call.User, params.Q, params.RoomID, params.Limit) {
		results = append(results, map[string]any{
			"message_id": m.ID, "room_id": m.RoomID, "log_id": fmt.Sprint(m.LogID),
			"from": map[string]any{"user_id": m.From.UserID, "name": m.From.Name}, "text": m.Text,
		})
	}
	return map[string]any{"results": results}, nil
}

func excerpt(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > 120 {
		text = string(runes[:120]) + "…"
	}
	return text
}

var pages = template.Must(template.New("").Funcs(template.FuncMap{
	"time": func(logID int64) string { return time.UnixMilli(logID).UTC().Format("2006-01-02 15:04") },
}).Parse(`
{{define "head"}}<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1"><title>{{.}}</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:46rem;margin:2rem auto;padding:0 1rem}
@media (prefers-color-scheme:dark){body{background:#161616;color:#eee}a{color:#8ab4ff}}
.meta{color:#888;font-size:.85em}p{white-space:pre-wrap;margin:.2rem 0 1rem}</style></head><body>{{end}}
{{define "index"}}{{template "head" "Archive"}}<h1>Archive</h1><ul>
{{range .}}<li><a href="/search/archive/{{.ID}}">{{.Title}}</a></li>{{else}}<li>No public rooms yet.</li>{{end}}
</ul></body></html>{{end}}
{{define "room"}}{{template "head" .Room.Title}}<p><a href="/search/archive/">Archive</a></p><h1>{{.Room.Title}}</h1>
{{range .Messages}}<div class="meta">{{.From.Name}} · {{time .LogID}}</div><p>{{.Text}}</p>{{else}}<p>No messages yet.</p>{{end}}
</body></html>{{end}}`))

// archiveIndex lists the public rooms.
func (s *Search) archiveIndex(w http.ResponseWriter, _ *http.Request) {
	rooms := slices.DeleteFunc(s.host.Rooms(), func(r apron.Room) bool { return !s.host.Public(r.ID) })
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = pages.ExecuteTemplate(w, "index", rooms)
}

// archiveRoom shows a public room's latest messages, oldest first. A room
// that is not public is not found, whoever asks.
func (s *Search) archiveRoom(w http.ResponseWriter, r *http.Request) {
	room, ok := s.host.Room(r.PathValue("room"))
	if !ok || !s.host.Public(room.ID) {
		http.NotFound(w, r)
		return
	}
	s.mu.RLock()
	var messages []apron.Message
	for _, m := range s.messages {
		if m.RoomID == room.ID {
			messages = append(messages, m)
		}
	}
	s.mu.RUnlock()
	// A message_id is its first log_id, a decimal string.
	slices.SortFunc(messages, func(a, b apron.Message) int {
		return cmp.Or(cmp.Compare(len(a.ID), len(b.ID)), cmp.Compare(a.ID, b.ID))
	})
	messages = messages[max(0, len(messages)-200):]
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = pages.ExecuteTemplate(w, "room", map[string]any{"Room": room, "Messages": messages})
}
