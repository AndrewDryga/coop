package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
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

	// The declared paths may also be a YAML flow or block list, not just a bare scalar.
	for name, frontmatter := range map[string]string{
		"flow":  "---\npaths: [portal/lib/auth.ex, runner/x.go]\n---\n# Portal auth\n",
		"block": "---\npaths:\n  - portal/lib/auth.ex\n  - runner/x.go\n---\n# Portal auth\n",
	} {
		ctxWrite(t, filepath.Join(repo, ".agent", "tasks", "00_todo", "2026-01-01-portal-auth", "task.md"), frontmatter)
		r = parse(t, "--json", "--task", "portal-auth")
		if len(r.Scope) != 2 || !strings.Contains(r.fileset(), ".agent/kb/portal.md") {
			t.Errorf("%s list: scope %v, files %v", name, r.Scope, r.Files)
		}
	}

	// Absolute (and escaping) scope paths are rejected.
	if code, err := a.cmdContext([]string{"/etc/passwd"}); code == 0 || err == nil {
		t.Errorf("absolute path must be rejected, got (%d, %v)", code, err)
	}
	if code, err := a.cmdContext([]string{"../outside"}); code == 0 || err == nil {
		t.Errorf("escaping path must be rejected, got (%d, %v)", code, err)
	}
}

// coop context refuses its options and paths in the shared block, as rejected input.
func TestContextRefusesInputInTheSharedBlock(t *testing.T) {
	a := &app{cfg: &config.Config{RepoOverride: t.TempDir()}}
	for _, tc := range []struct {
		args     []string
		headline string
	}{
		{[]string{"--json", "--rendered"}, `Cannot combine "--json" and "--rendered" in "coop context"`},
		{[]string{"--tasks", ".agent/tasks"}, `Missing --task for "coop context"`},
		{[]string{"--task", "x", "--tasks", ""}, `Missing value for "--tasks" in "coop context"`},
		{[]string{"--verbose"}, `Unknown option "--verbose" for "coop context"`},
		{[]string{"/etc/passwd"}, `Invalid path "/etc/passwd" for "coop context"`},
		{[]string{"../outside"}, `Invalid path "../outside" for "coop context"`},
	} {
		code, err := a.cmdContext(tc.args)
		if usage := asUsage(err); code != 2 || usage == nil || usage.Headline != tc.headline {
			t.Errorf("coop context %q = (%d, %v), want (2, %q)", tc.args, code, err, tc.headline)
		}
	}
}

func TestParseGitStatusPaths(t *testing.T) {
	out := []byte(" M plain.txt\x00R  renamed \"ü\" .txt \x00old -> name.txt\x00C  copied.txt\x00source.txt\x00?? nested/untracked file.txt\x00")
	got, err := parseGitStatusPaths(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"plain.txt", "renamed \"ü\" .txt ", "copied.txt", "nested/untracked file.txt"}
	if !slices.Equal(got, want) {
		t.Fatalf("parsed paths = %#v, want %#v", got, want)
	}

	for _, malformed := range [][]byte{
		[]byte(" M missing-nul"),
		[]byte("short\x00"),
		[]byte("R  target\x00"),
	} {
		if _, err := parseGitStatusPaths(malformed); err == nil {
			t.Fatalf("malformed status %q was accepted", malformed)
		}
	}
}

func TestContextChangedPreservesGitPaths(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	pinGitConfig(t)
	repo, run := gitrepo.New(t)
	ctxWrite(t, filepath.Join(repo, "plain.txt"), "base\n")
	ctxWrite(t, filepath.Join(repo, "old name.txt"), "rename me\n")
	run("add", ".")
	run("commit", "-qm", "base")

	ctxWrite(t, filepath.Join(repo, "plain.txt"), "changed\n")
	target := " renamed ü .txt "
	run("mv", "old name.txt", target)
	run("add", "-A")
	ctxWrite(t, filepath.Join(repo, "nested", "untracked file.txt"), "new\n")

	a := &app{}
	got, err := a.contextScope(repo, &project.Project{}, nil, true, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"plain.txt": true, target: true, "nested/untracked file.txt": true,
	}
	if len(got) != len(want) {
		t.Fatalf("changed scope = %#v, want exactly %#v", got, want)
	}
	for _, path := range got {
		if !want[path] {
			t.Errorf("unexpected changed path %q in %#v", path, got)
		}
	}
}

func TestContextChangedPropagatesGitFailure(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nprintf 'fatal: status broke\\n' >&2\nexit 128\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	a := &app{}
	if _, err := a.contextScope(t.TempDir(), &project.Project{}, nil, true, "", nil); err == nil || !strings.Contains(err.Error(), "fatal: status broke") {
		t.Fatalf("contextScope Git error = %v, want captured stderr", err)
	}
}

func TestContextChangedGitStatusIsHardened(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	pinGitConfig(t)
	repo, run := gitrepo.New(t)
	run("commit", "-q", "--allow-empty", "-m", "base")
	marker := filepath.Join(t.TempDir(), "PWNED")
	evil := filepath.Join(repo, ".git", "evil.sh")
	if err := os.WriteFile(evil, []byte("#!/bin/sh\necho pwned >> "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run("config", "core.fsmonitor", evil)
	if _, err := gitChangedPaths(repo); err != nil {
		t.Fatalf("gitChangedPaths: %v", err)
	}
	if pathExists(marker) {
		t.Fatal("gitChangedPaths ran the repository's core.fsmonitor on the host")
	}
	_ = exec.Command("git", "-C", repo, "status", "--porcelain=v1", "-z", "--untracked-files=all").Run()
	if !pathExists(marker) {
		t.Fatal("positive control failed: raw git status did not fire the planted fsmonitor")
	}
}

// Inside a subproject, coop context lists that subproject's AGENTS.md after the root's. Paths stay
// relative to the repository root, as documented; one that names nothing there but a file in the
// working folder gets a warning with the command for that file, since shell completion offers
// folder-relative paths. The refusal of a path outside the repository shows a generic example.
func TestContextInsideASubproject(t *testing.T) {
	repo := t.TempDir()
	ctxWrite(t, filepath.Join(repo, "AGENTS.md"), "agents")
	ctxWrite(t, filepath.Join(repo, "web", "AGENTS.md"), "web agents")
	ctxWrite(t, filepath.Join(repo, "web", "src", "x.ts"), "x")
	ctxWrite(t, filepath.Join(repo, "web", "src", "both.ts"), "both")
	ctxWrite(t, filepath.Join(repo, "src", "both.ts"), "both")
	ctxWrite(t, filepath.Join(repo, ".agent", "kb", "src.md"), "src kb")
	ctxWrite(t, filepath.Join(repo, ".agent", "project.yaml"),
		"subprojects: [web]\ncontext:\n  routes:\n    - paths: [src/**]\n      include: [.agent/kb/src.md]\n")
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	t.Chdir(filepath.Join(repo, "web"))
	run := func(args ...string) (stdout, stderr string) {
		t.Helper()
		stderr = captureStderr(t, func() {
			stdout = captureStdout(t, func() {
				if code, err := a.cmdContext(args); code != 0 || err != nil {
					t.Fatalf("coop context %q = (%d, %v)", args, code, err)
				}
			})
		})
		return stdout, stderr
	}

	out, warn := run("--json")
	var r ctxResult
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}
	if want := []contextcSelected{{"AGENTS.md", "canonical"}, {"web/AGENTS.md", "subproject web"}}; !slices.Equal(r.Files, want) || warn != "" {
		t.Errorf("files in web = %+v (warned %q), want %+v", r.Files, warn, want)
	}
	if out, _ := run(); !strings.Contains(out, "  web/AGENTS.md\n    Instructions for the subproject web\n") {
		t.Errorf("the report does not explain the subproject's file:\n%s", out)
	}

	_, warn = run("--json", "src/x.ts")
	for _, want := range []string{"Paths are relative to the repository root", "src/x.ts is read from the root, not from web.", "coop context web/src/x.ts"} {
		if !strings.Contains(warn, want) {
			t.Errorf("a folder-relative path warned %q, want it to contain %q", warn, want)
		}
	}
	if _, warn = run("--json", "src/both.ts"); warn != "" {
		t.Errorf("a path the root has was warned about: %q", warn)
	}
	if _, warn = run("--json", "web/src/x.ts"); warn != "" {
		t.Errorf("a repo-relative path was warned about: %q", warn)
	}

	_, err := a.cmdContext([]string{"../outside"})
	if usage := asUsage(err); usage == nil || !slices.Contains(usage.Rows, [2]string{"Example:", "coop context src/app.ts"}) {
		t.Errorf("outside path refusal = %#v, want the generic example", err)
	}
}
