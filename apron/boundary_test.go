package apron_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The packages both sides use, which a client imports, import nothing of
// the server's: only the standard library, package apron, this module's
// internal packages, and the WebSocket package.
var clientPackages = []string{".", "./bot"}

func TestBoundary(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command to list the dependencies with")
	}
	const module = "github.com/apron-chat/apron-server-go/apron"
	allowed := func(path string) bool {
		return path == module ||
			strings.HasPrefix(path, module+"/internal/") ||
			path == "github.com/coder/websocket" || strings.HasPrefix(path, "github.com/coder/websocket/")
	}
	for _, pkg := range clientPackages {
		cmd := exec.Command(gobin, "list", "-deps", "-f", "{{.ImportPath}} {{.Standard}}", pkg)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list -deps %s: %v", pkg, err)
		}
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		// The last one is the package itself.
		self, _, _ := strings.Cut(lines[len(lines)-1], " ")
		for _, line := range lines[:len(lines)-1] {
			path, standard, _ := strings.Cut(line, " ")
			if standard != "true" && !allowed(path) {
				t.Errorf("%s imports %s", self, path)
			}
		}
	}
}
