package box

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/project"
)

func serveRepo(t *testing.T, portsYAML string) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, project.File), []byte("serve:\n  ports:\n"+portsYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

// TestAppendPublish: with egress open, a serve port becomes a localhost -p to its deterministic host
// port; egress off or no serve config publishes nothing.
func TestAppendPublish(t *testing.T) {
	repo := serveRepo(t, "    - 5173\n")
	host := project.HostPort(repo, 5173)

	// egress open + a free host port → -p 127.0.0.1:<host>:5173
	got := appendPublish(nil, &config.Config{Egress: "open"}, RunSpec{Repo: repo})
	if !hostPortFree(host) {
		t.Skipf("deterministic host port %d already in use on this machine", host)
	}
	if joined := strings.Join(got, " "); !strings.Contains(joined, "-p") || !strings.Contains(joined, fmt.Sprintf("127.0.0.1:%d:5173", host)) {
		t.Errorf("egress-open publish = %v, want a -p 127.0.0.1:%d:5173", got, host)
	}
	// the box learns its host-facing URL for the port.
	if !strings.Contains(strings.Join(got, " "), fmt.Sprintf("COOP_SERVE_URL_5173=http://localhost:%d", host)) {
		t.Errorf("publish should inject COOP_SERVE_URL_5173: %v", got)
	}

	// A fork inherits serve.ports from its PolicyRepo but allocates the host port from its OWN
	// workspace path — so it never collides with the parent (that's the fork-distinct-ports fix).
	forkWs := t.TempDir()
	forkHost := project.HostPort(forkWs, 5173)
	if forkHost == host {
		t.Skip("workspace-path hash collided (astronomically unlikely) — can't prove distinctness")
	}
	forkGot := strings.Join(appendPublish(nil, &config.Config{Egress: "open"}, RunSpec{Repo: forkWs, PolicyRepo: repo}), " ")
	if !strings.Contains(forkGot, fmt.Sprintf("127.0.0.1:%d:5173", forkHost)) {
		t.Errorf("a fork must publish on its own host port %d (from its workspace), got %v", forkHost, forkGot)
	}
	if strings.Contains(forkGot, fmt.Sprintf("127.0.0.1:%d:5173", host)) {
		t.Errorf("a fork must NOT reuse the policy repo's host port %d, got %v", host, forkGot)
	}

	// egress not open → nothing published (-p can't bind under --network none).
	if got := appendPublish(nil, &config.Config{Egress: "none"}, RunSpec{Repo: repo}); len(got) != 0 {
		t.Errorf("egress-off must not publish, got %v", got)
	}

	// no serve config → nothing published.
	if got := appendPublish(nil, &config.Config{Egress: "open"}, RunSpec{Repo: t.TempDir()}); len(got) != 0 {
		t.Errorf("no serve config must not publish, got %v", got)
	}
}
