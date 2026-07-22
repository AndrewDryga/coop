package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func ctxWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

type ctxResult struct {
	Scope []string           `json:"scope"`
	Files []contextcSelected `json:"files"`
}
type contextcSelected struct {
	File   string `json:"file"`
	Reason string `json:"reason"`
}

func (r ctxResult) fileset() string {
	var b strings.Builder
	for _, f := range r.Files {
		b.WriteString(f.File + " ")
	}
	return b.String()
}

func TestCmdContext(t *testing.T) {
	repo := t.TempDir()
	ctxWrite(t, filepath.Join(repo, "AGENTS.md"), "agents")
	ctxWrite(t, filepath.Join(repo, ".agent", "kb", "portal.md"), "portal kb")
	ctxWrite(t, filepath.Join(repo, ".agent", "project.yaml"),
		"context:\n  routes:\n    - paths: [portal/**]\n      include: [.agent/kb/portal.md]\n")
	a := &app{cfg: &config.Config{RepoOverride: repo}}

	parse := func(t *testing.T, args ...string) ctxResult {
		t.Helper()
		var code int
		var err error
		out := captureStdout(t, func() { code, err = a.cmdContext(args) })
		if code != 0 || err != nil {
			t.Fatalf("coop context %v: (%d, %v)", args, code, err)
		}
		var r ctxResult
		if e := json.Unmarshal([]byte(out), &r); e != nil {
			t.Fatalf("bad json for %v: %v\n%s", args, e, out)
		}
		return r
	}

	// A touched path under portal → canonical AGENTS.md + the matched route's doc.
	r := parse(t, "--json", "portal/lib/x.ex")
	if fs := r.fileset(); !strings.Contains(fs, "AGENTS.md") || !strings.Contains(fs, ".agent/kb/portal.md") {
		t.Errorf("expected canonical + portal kb, got %v", r.Files)
	}

	// A non-matching path → only the canonical instruction file.
	r = parse(t, "--json", "runner/x.go")
	if len(r.Files) != 1 || r.Files[0].File != "AGENTS.md" {
		t.Errorf("non-matching scope should give only canonical, got %v", r.Files)
	}

	// A task's declared paths (frontmatter) feed the scope.
	ctxWrite(t, filepath.Join(repo, ".agent", "tasks", "00_todo", "2026-01-01-portal-auth", "task.md"),
		"---\npaths: portal/lib/auth.ex\n---\n# Portal auth\n")
	r = parse(t, "--json", "--task", "portal-auth")
	if !strings.Contains(r.fileset(), ".agent/kb/portal.md") {
		t.Errorf("task-declared path should route the portal doc, got %v", r.Files)
	}

	// Absolute (and escaping) scope paths are rejected.
	if code, err := a.cmdContext([]string{"/etc/passwd"}); code == 0 || err == nil {
		t.Errorf("absolute path must be rejected, got (%d, %v)", code, err)
	}
	if code, err := a.cmdContext([]string{"../outside"}); code == 0 || err == nil {
		t.Errorf("escaping path must be rejected, got (%d, %v)", code, err)
	}
}
