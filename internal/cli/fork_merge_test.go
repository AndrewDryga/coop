package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

// trustedSignArgs must read signing config from the parent (so a fork can't redirect
// the program) and track gpg.format to the matching program key.
func TestTrustedSignArgs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Run("openpgp default", func(t *testing.T) {
		repo := initRepo(t)
		git(t, repo, "config", "commit.gpgsign", "true")
		git(t, repo, "config", "user.signingkey", "ABCD1234")
		want := []string{"-c", "commit.gpgsign=true", "-c", "gpg.program=gpg", "-c", "user.signingkey=ABCD1234"}
		if got := trustedSignArgs(repo); !slices.Equal(got, want) {
			t.Errorf("trustedSignArgs = %v, want %v", got, want)
		}
	})
	t.Run("ssh format picks gpg.ssh.program", func(t *testing.T) {
		repo := initRepo(t)
		git(t, repo, "config", "commit.gpgsign", "true")
		git(t, repo, "config", "gpg.format", "ssh")
		git(t, repo, "config", "user.signingkey", "/k.pub")
		want := []string{"-c", "commit.gpgsign=true", "-c", "gpg.format=ssh", "-c", "gpg.ssh.program=ssh-keygen", "-c", "user.signingkey=/k.pub"}
		if got := trustedSignArgs(repo); !slices.Equal(got, want) {
			t.Errorf("trustedSignArgs = %v, want %v", got, want)
		}
	})
}

func TestMergeOneNoGate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	a := &app{cfg: &config.Config{}} // no COOP_GATE → no box needed

	ws, err := setupFork(repo, "perf")
	if err != nil {
		t.Fatalf("setupFork: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "work")

	landed, err := a.mergeOne(repo, "", "perf", false)
	if err != nil || !landed {
		t.Fatalf("mergeOne = (%v, %v), want (true, nil)", landed, err)
	}
	if !pathExists(filepath.Join(repo, "feature.txt")) {
		t.Error("merge did not land the fork's file")
	}
}

func TestMergeOneConflictRollsBack(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	a := &app{cfg: &config.Config{}}

	ws, err := setupFork(repo, "a")
	if err != nil {
		t.Fatalf("setupFork: %v", err)
	}
	// Fork and parent edit the same line → a merge conflict.
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("fork-version\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "commit", "-aqm", "fork edit")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("parent-version\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "commit", "-aqm", "parent edit")

	landed, err := a.mergeOne(repo, "", "a", false)
	if landed || err == nil {
		t.Fatalf("mergeOne = (%v, %v), want (false, error)", landed, err)
	}
	// The conflicted merge must be fully aborted: tree clean, parent content intact.
	if gitDirty(repo) {
		t.Error("working tree left dirty after a conflicted merge")
	}
	if data, _ := os.ReadFile(filepath.Join(repo, "README.md")); string(data) != "parent-version\n" {
		t.Errorf("README.md = %q, want %q", data, "parent-version\n")
	}
}

func TestMergeOnePolicyForce(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	a := &app{cfg: &config.Config{}}
	ws, err := setupFork(repo, "leak")
	if err != nil {
		t.Fatalf("setupFork: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".env"), []byte("S=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "leak")

	// Without --force the policy guard blocks the secret-like file.
	if landed, err := a.mergeOne(repo, "", "leak", false); landed || err == nil {
		t.Fatalf("mergeOne(force=false) = (%v, %v), want blocked", landed, err)
	}
	if pathExists(filepath.Join(repo, ".env")) {
		t.Fatal(".env landed despite the policy block")
	}
	// With --force it lands.
	if landed, err := a.mergeOne(repo, "", "leak", true); !landed || err != nil {
		t.Fatalf("mergeOne(force=true) = (%v, %v), want landed", landed, err)
	}
	if !pathExists(filepath.Join(repo, ".env")) {
		t.Error(".env not landed with --force")
	}
}

// plantForkBooby rigs a fork's agent-writable .git/ to run host commands: every git
// hook plus the config knobs that shell out (core.fsmonitor, core.hooksPath, and a
// forced commit.gpgsign with a planted gpg.program). Each writes a line to marker, so
// its existence proves something in the fork executed on the host.
func plantForkBooby(t *testing.T, ws, marker string) {
	t.Helper()
	hooks := filepath.Join(ws, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho pwned >> " + marker + "\n"
	for _, h := range []string{"pre-rebase", "post-rewrite", "post-checkout", "post-commit", "post-merge"} {
		if err := os.WriteFile(filepath.Join(hooks, h), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A standalone script reused for the command-running config knobs (gpg.program
	// must still emit a signature, so it cats stdin after marking).
	evil := filepath.Join(ws, ".git", "evil.sh")
	if err := os.WriteFile(evil, []byte(script+"cat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "config", "core.hooksPath", hooks)
	git(t, ws, "config", "core.fsmonitor", evil)
	git(t, ws, "config", "commit.gpgsign", "true")
	git(t, ws, "config", "gpg.program", evil)
}

// A fork's .git/ is agent-writable, so the host-side git commands a merge runs in it
// must not execute fork-planted hooks or command-running config.
func TestMergeOneIgnoresForkBooby(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	a := &app{cfg: &config.Config{}}

	ws, err := setupFork(repo, "evil")
	if err != nil {
		t.Fatalf("setupFork: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "work")
	// Advance the parent on a different file so landing must rebase/replay (which is
	// what fires pre-rebase/post-checkout/post-rewrite) rather than fast-forward.
	if err := os.WriteFile(filepath.Join(repo, "other.txt"), []byte("p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "parent moves")

	marker := filepath.Join(t.TempDir(), "PWNED")
	plantForkBooby(t, ws, marker)

	landed, err := a.mergeOne(repo, "", "evil", false)
	if err != nil || !landed {
		t.Fatalf("mergeOne = (%v, %v), want landed", landed, err)
	}
	if pathExists(marker) {
		t.Fatalf("a fork-controlled git hook/config executed on the host during merge (marker created)")
	}
	if !pathExists(filepath.Join(repo, "feature.txt")) {
		t.Error("merge did not land the fork's work")
	}
	// Positive control: the trap is live — an *unhardened* fork git command fires it,
	// so the clean run above is the hardening working, not a no-op test.
	_ = gitDirty(ws) // raw `git -C ws status` → runs the planted core.fsmonitor
	if !pathExists(marker) {
		t.Fatal("positive control failed: raw fork git did not trigger the booby trap, so the test proves nothing")
	}
}

// A non-interactive `coop fork merge` must refuse without --yes (it lands work and
// deletes the fork), and proceed with it.
func TestForkMergeNonTTYRequiresYes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// Force a non-interactive stdin regardless of how the suite is run — a real TTY
	// would send the un-gated path into an interactive prompt and block.
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	saved := os.Stdin
	os.Stdin = devnull
	defer func() { os.Stdin = saved }()

	repo := initRepo(t)
	a := &app{cfg: &config.Config{RepoOverride: repo}} // no gate → no box
	ws, err := setupFork(repo, "a")
	if err != nil {
		t.Fatalf("setupFork: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "work")

	// Without --yes: refuse, before landing, fork intact.
	if code, err := a.forkMerge([]string{"a"}); err == nil || code == 0 {
		t.Fatalf("forkMerge(no --yes) = (%d, %v), want a refusal", code, err)
	}
	if pathExists(filepath.Join(repo, "a.txt")) {
		t.Error("a.txt landed despite the non-interactive refusal")
	}
	if !pathExists(ws) {
		t.Error("fork was removed despite the refusal")
	}

	// With --yes: lands and removes the fork.
	if code, err := a.forkMerge([]string{"a", "--yes"}); err != nil || code != 0 {
		t.Fatalf("forkMerge(--yes) = (%d, %v), want (0, nil)", code, err)
	}
	if !pathExists(filepath.Join(repo, "a.txt")) {
		t.Error("a.txt did not land with --yes")
	}
	if pathExists(ws) {
		t.Error("fork not removed after a --yes land")
	}
}

func TestForkMergeQueue(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	a := &app{cfg: &config.Config{}}
	// Two independent forks, each adding a distinct file.
	for _, n := range []string{"a", "b"} {
		ws, err := setupFork(repo, n)
		if err != nil {
			t.Fatalf("setupFork %s: %v", n, err)
		}
		if err := os.WriteFile(filepath.Join(ws, n+".txt"), []byte(n+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, ws, "add", "-A")
		git(t, ws, "commit", "-qm", n)
	}
	if code, err := a.forkMergeAll(repo, "", false); err != nil || code != 0 {
		t.Fatalf("forkMergeAll = (%d, %v), want (0, nil)", code, err)
	}
	if !pathExists(filepath.Join(repo, "a.txt")) || !pathExists(filepath.Join(repo, "b.txt")) {
		t.Error("merge queue did not land both forks")
	}
	if got := forkNames(repo); len(got) != 0 {
		t.Errorf("forks remain after the queue closed them: %v", got)
	}
	// Rebasing must keep history linear — no merge commits.
	if merges := gitOut(repo, "rev-list", "--merges", "HEAD"); merges != "" {
		t.Errorf("rebase queue produced merge commits (history not linear):\n%s", merges)
	}
}
