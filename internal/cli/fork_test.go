package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestForkWorkspace(t *testing.T) {
	repo := "/home/me/proj"
	if got, want := forkHome(repo), "/home/me/proj-forks"; got != want {
		t.Errorf("forkHome = %q, want %q", got, want)
	}
	if got, want := forkWorkspace(repo, "perf"), "/home/me/proj-forks/perf"; got != want {
		t.Errorf("forkWorkspace = %q, want %q", got, want)
	}
}

func TestValidForkName(t *testing.T) {
	for _, n := range []string{"perf", "deps", "fix-1", "a.b"} {
		if !validForkName(n) {
			t.Errorf("validForkName(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"", "ls", "review", "merge", "rm", "open", "a/b", `a\b`, "..", ".", "-x"} {
		if validForkName(n) {
			t.Errorf("validForkName(%q) = true, want false", n)
		}
	}
}

func TestParseForkCreate(t *testing.T) {
	tests := []struct {
		args      []string
		wantAgent string
		wantFresh bool
		wantErr   bool
	}{
		{[]string{"perf"}, "claude", false, false},
		{[]string{"perf", "codex"}, "codex", false, false},
		{[]string{"perf", "gemini", "--fresh"}, "gemini", true, false},
		{[]string{}, "", false, true},
		{[]string{"perf", "bogus"}, "", false, true},
		{[]string{"ls"}, "", false, true}, // reserved name
	}
	for _, tc := range tests {
		fa, err := parseForkCreate(tc.args)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseForkCreate(%v) err = %v, wantErr %v", tc.args, err, tc.wantErr)
			continue
		}
		if tc.wantErr {
			continue
		}
		if fa.agent != tc.wantAgent || fa.fresh != tc.wantFresh {
			t.Errorf("parseForkCreate(%v) = {agent:%q fresh:%v}, want {agent:%q fresh:%v}",
				tc.args, fa.agent, fa.fresh, tc.wantAgent, tc.wantFresh)
		}
	}
}

func TestForkRmSafe(t *testing.T) {
	tests := []struct {
		unmerged, dirty, force bool
		wantErr                bool
	}{
		{false, false, false, false}, // clean & merged → ok
		{true, false, false, true},   // unmerged → blocked
		{false, true, false, true},   // dirty → blocked
		{true, true, true, false},    // force overrides everything
	}
	for _, tc := range tests {
		err := forkRmSafe(tc.unmerged, tc.dirty, tc.force)
		if (err != nil) != tc.wantErr {
			t.Errorf("forkRmSafe(unmerged=%v dirty=%v force=%v) err = %v, wantErr %v",
				tc.unmerged, tc.dirty, tc.force, err, tc.wantErr)
		}
	}
}

func TestParseShortstat(t *testing.T) {
	ins, del := parseShortstat(" 3 files changed, 42 insertions(+), 7 deletions(-)")
	if ins != 42 || del != 7 {
		t.Errorf("parseShortstat = (%d, %d), want (42, 7)", ins, del)
	}
	if ins, del := parseShortstat(""); ins != 0 || del != 0 {
		t.Errorf("parseShortstat(empty) = (%d, %d), want (0, 0)", ins, del)
	}
}

func TestIndentLastLines(t *testing.T) {
	if got := indent("a\nb"); got != "  a\n  b" {
		t.Errorf("indent = %q, want %q", got, "  a\n  b")
	}
	if got := lastLines("a\nb\nc\nd", 2); got != "c\nd" {
		t.Errorf("lastLines(.., 2) = %q, want %q", got, "c\nd")
	}
	if got := lastLines("a\nb", 5); got != "a\nb" {
		t.Errorf("lastLines short = %q, want %q", got, "a\nb")
	}
}

// --- git-backed lifecycle ---

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func initRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "myrepo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q")
	git(t, repo, "checkout", "-q", "-b", "main")
	git(t, repo, "config", "user.email", "t@t") // so merge-commits work without ambient identity
	git(t, repo, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "init")
	return repo
}

func TestForkLifecycle(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)

	// setupFork clones + branches.
	ws, err := setupFork(repo, "perf")
	if err != nil {
		t.Fatalf("setupFork: %v", err)
	}
	if !pathExists(ws) {
		t.Fatalf("workspace %s not created", ws)
	}
	if got := gitBranch(ws); got != "perf" {
		t.Errorf("fork branch = %q, want %q", got, "perf")
	}
	// The fork must carry the parent's git identity so an agent can commit in it.
	if got := gitOut(ws, "config", "user.email"); got != "t@t" {
		t.Errorf("fork git identity not propagated: user.email = %q, want %q", got, "t@t")
	}

	// A commit in the fork is "unmerged" from the parent's point of view.
	if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "do the work")
	if !forkUnmerged(repo, ws) {
		t.Error("fork with new commit should be unmerged")
	}

	// review fetches the branch into review/perf.
	if err := gitFetchInto(repo, ws, "perf"); err != nil {
		t.Fatalf("gitFetchInto: %v", err)
	}
	if gitOut(repo, "rev-parse", "--verify", "-q", "review/perf") == "" {
		t.Error("review/perf ref not created")
	}

	// merge lands it; now it's merged.
	git(t, repo, "merge", "--no-edit", "review/perf")
	if forkUnmerged(repo, ws) {
		t.Error("fork should be merged after git merge")
	}
	if !pathExists(filepath.Join(repo, "feature.txt")) {
		t.Error("merged file not present in parent repo")
	}

	// destroyFork removes the workspace and the review ref.
	if err := destroyFork(repo, "perf"); err != nil {
		t.Fatalf("destroyFork: %v", err)
	}
	if pathExists(ws) {
		t.Error("workspace not removed")
	}
	if gitOut(repo, "rev-parse", "--verify", "-q", "review/perf") != "" {
		t.Error("review/perf ref not removed")
	}
}

func TestForkCarriesGlobalIgnore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	// Stand in for the user's global gitignore via a (repo-local) core.excludesfile.
	ignore := filepath.Join(t.TempDir(), "ignore")
	if err := os.WriteFile(ignore, []byte("*.tmp\n.DS_Store\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "config", "core.excludesfile", ignore)

	ws, err := setupFork(repo, "ig")
	if err != nil {
		t.Fatalf("setupFork: %v", err)
	}
	excl, err := os.ReadFile(filepath.Join(ws, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(excl), "*.tmp") || !strings.Contains(string(excl), ".DS_Store") {
		t.Errorf("global ignore not carried into the fork's .git/info/exclude:\n%s", excl)
	}
}
