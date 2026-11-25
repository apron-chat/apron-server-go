package authn_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The sign-in schemes see the server only through this package's
// interfaces (Core, Conn): of this repository, they and this package
// import only this package, each other, and their shared OAuth pages,
// never the server (aprond and its internals), its store, or the protocol
// module, so that a scheme is tested, and changed, on its own.
func TestBoundary(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command to list the dependencies with")
	}
	const repository = "github.com/apron-chat/apron-server-go/"
	allowed := func(path string) bool {
		return !strings.HasPrefix(path, repository) ||
			path == repository+"internal/authn" ||
			strings.HasPrefix(path, repository+"internal/auth/")
	}
	cmd := exec.Command(gobin, "list", "-f", "{{.ImportPath}} {{join .Deps \" \"}}", ".", "../auth/...")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for line := range strings.Lines(strings.TrimSpace(string(out))) {
		fields := strings.Fields(line)
		for _, path := range fields[1:] {
			if !allowed(path) {
				t.Errorf("%s imports %s", fields[0], path)
			}
		}
	}
}
