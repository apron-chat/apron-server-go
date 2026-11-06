// Command ops is an Apron server for an engineering team: GitHub webhooks
// post pushes, pull requests, and workflow runs into a room, and people
// with the deployer role run /deploy <service>, whose output streams into
// the room as a terminal (PROTOCOL.md §4.8.6).
//
//	go run ./examples/ops --role deployer=ada \
//	  --ops.webhook-secret ... --ops.service api=./scripts/deploy-api.sh
//
// Point a GitHub webhook (content type application/json) at
// https://chat.example/ops/github with the same secret.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/aprond"
)

func main() { aprond.Main(&Ops{}) }

// Ops posts webhooks and runs deploys.
type Ops struct {
	Settings struct {
		Room          string        `long:"room" default:"general" description:"The room webhooks post to"`
		WebhookSecret string        `long:"webhook-secret" description:"The GitHub webhook's secret; empty refuses webhooks"`
		Services      []string      `long:"service" description:"A service /deploy runs, as name=command, split into words without a shell; repeat for more"`
		Timeout       time.Duration `long:"timeout" default:"30m" description:"How long a deploy may run"`
	}
	host     *apron.Host
	bot      *apron.Session
	services map[string][]string

	mu        sync.Mutex
	deploying map[string]bool
}

func (*Ops) Name() string { return "ops" }
func (o *Ops) Flags() any { return &o.Settings }

func (o *Ops) Init(h *apron.Host) error {
	o.host = h
	o.services = make(map[string][]string)
	o.deploying = make(map[string]bool)
	for _, service := range o.Settings.Services {
		name, command, ok := strings.Cut(service, "=")
		if !ok || name == "" || len(strings.Fields(command)) == 0 {
			return fmt.Errorf("--ops.service %q: use name=command", service)
		}
		o.services[name] = strings.Fields(command)
	}
	var err error
	if o.bot, err = h.Session(h.Context(), "ops", apron.SessionOptions{Name: "Ops"}); err != nil {
		return err
	}
	if err := o.bot.Join(h.Context(), o.Settings.Room); err != nil {
		return fmt.Errorf("joining %s: %w", o.Settings.Room, err)
	}
	h.Handle("POST /ops/github", http.HandlerFunc(o.webhook))
	h.Command(apron.Command{
		Name: "deploy", Usage: "/deploy <service>", Help: "deploy a service and stream its output here",
		Available: func(u apron.User, _ apron.Room) bool { return u.HasRole("deployer") || u.HasRole("admin") },
		Run:       o.deploy,
	})
	return nil
}

// webhook posts a GitHub event, once its signature checks out.
func (o *Ops) webhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	if !o.signed(body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	text := summarize(r.Header.Get("X-GitHub-Event"), body)
	if text == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if _, err := o.bot.Post(r.Context(), o.Settings.Room, text, apron.Markdown()); err != nil {
		o.host.Logger().Error("posting a webhook", "error", err)
		http.Error(w, "could not post", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (o *Ops) signed(body []byte, signature string) bool {
	if o.Settings.WebhookSecret == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(o.Settings.WebhookSecret))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(signature))
}

// summarize renders the events worth a message as Markdown, and others as
// "".
func summarize(event string, body []byte) string {
	var payload struct {
		Action     string `json:"action"`
		Ref        string `json:"ref"`
		Compare    string `json:"compare"`
		Commits    []any  `json:"commits"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
		PullRequest struct {
			Number  int    `json:"number"`
			Title   string `json:"title"`
			HTMLURL string `json:"html_url"`
			Merged  bool   `json:"merged"`
		} `json:"pull_request"`
		WorkflowRun struct {
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
			HTMLURL    string `json:"html_url"`
			HeadBranch string `json:"head_branch"`
		} `json:"workflow_run"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	repo, who := payload.Repository.FullName, payload.Sender.Login
	switch event {
	case "push":
		if len(payload.Commits) == 0 {
			return ""
		}
		return fmt.Sprintf("**%s** pushed %d commit(s) to `%s` in %s: [compare](%s)",
			who, len(payload.Commits), strings.TrimPrefix(payload.Ref, "refs/heads/"), repo, payload.Compare)
	case "pull_request":
		pr := payload.PullRequest
		action := payload.Action
		if action == "closed" && pr.Merged {
			action = "merged"
		}
		if !slices.Contains([]string{"opened", "reopened", "merged", "closed", "ready_for_review"}, action) {
			return ""
		}
		return fmt.Sprintf("**%s** %s [%s#%d %s](%s)", who, strings.ReplaceAll(action, "_", " "), repo, pr.Number, pr.Title, pr.HTMLURL)
	case "workflow_run":
		run := payload.WorkflowRun
		if payload.Action != "completed" || run.Conclusion == "success" {
			return "" // only what needs attention
		}
		return fmt.Sprintf("⚠️ [%s](%s) on `%s` in %s: **%s**", run.Name, run.HTMLURL, run.HeadBranch, repo, run.Conclusion)
	}
	return ""
}

// deploy runs a service's deploy command, one at a time per service, with
// its output in a terminal in the room the command was run in.
func (o *Ops) deploy(ctx context.Context, call *apron.CommandCall) (map[string]any, error) {
	argv, ok := o.services[call.Args]
	if !ok {
		names := make([]string, 0, len(o.services))
		for name := range o.services {
			names = append(names, name)
		}
		slices.Sort(names)
		return nil, apron.InvalidParams("Usage: /deploy <service>, one of: " + strings.Join(names, ", "))
	}
	o.mu.Lock()
	busy := o.deploying[call.Args]
	o.deploying[call.Args] = true
	o.mu.Unlock()
	if busy {
		return nil, apron.Denied(call.Args + " is deploying already")
	}
	term, err := o.bot.StreamTerminal(ctx, call.Room.ID, "deploy "+call.Args+" by "+call.User.ID, apron.ReplyTo(call.ReplyTo))
	if err != nil {
		o.done(call.Args)
		return nil, err
	}
	o.host.Go(func(ctx context.Context) {
		defer o.done(call.Args)
		ctx, cancel := context.WithTimeout(ctx, o.Settings.Timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Stdout, cmd.Stderr = term, term
		err := cmd.Run()
		_ = term.Close()
		outcome := "✅ deployed " + call.Args
		if err != nil {
			outcome = fmt.Sprintf("❌ deploying %s failed: %s", call.Args, err)
		}
		if _, postErr := o.bot.Post(ctx, call.Room.ID, outcome, apron.ReplyTo(term.MessageID())); postErr != nil && !errors.Is(postErr, apron.ErrSessionClosed) {
			o.host.Logger().Error("posting a deploy's outcome", "error", postErr)
		}
	})
	call.Notice("Deploying " + call.Args + "…")
	return nil, nil
}

func (o *Ops) done(service string) {
	o.mu.Lock()
	delete(o.deploying, service)
	o.mu.Unlock()
}
