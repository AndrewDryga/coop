package forkctl

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func TestForkAgentMemory(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	forkspace.Exclude(ws, ".coop/") // forkspace.Setup establishes this before the first provider run
	// A fork with no memory yet.
	if got := ReadForkAgent(ws); got != "" {
		t.Errorf("ReadForkAgent(fresh) = %q, want empty", got)
	}
	// Persist, read back, and confirm it's git-excluded so it never lands.
	SaveForkAgent(ws, "codex")
	if got := ReadForkAgent(ws); got != "codex" {
		t.Errorf("ReadForkAgent after save = %q, want codex", got)
	}
	excl, _ := os.ReadFile(filepath.Join(ws, ".git", "info", "exclude"))
	if !strings.Contains(string(excl), ".coop/") {
		t.Errorf(".git/info/exclude missing .coop/: %q", excl)
	}
	// An explicit switch updates the memory; the exclude isn't duplicated.
	SaveForkAgent(ws, "gemini")
	if got := ReadForkAgent(ws); got != "gemini" {
		t.Errorf("ReadForkAgent after switch = %q, want gemini", got)
	}
	excl2, _ := os.ReadFile(filepath.Join(ws, ".git", "info", "exclude"))
	if strings.Count(string(excl2), ".coop/") != 1 {
		t.Errorf("exclude duplicated .coop/: %q", excl2)
	}
	// A garbage value is ignored (not a known agent).
	if err := os.WriteFile(forkAgentFile(ws), []byte("bogus\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ReadForkAgent(ws); got != "" {
		t.Errorf("ReadForkAgent(bogus) = %q, want empty", got)
	}
}

func TestForkMetadataRefusesSymlinks(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel")
	want := "11111111-2222-4333-8444-555555555555\n"
	if err := os.WriteFile(sentinel, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}

	meta := filepath.Join(ws, ".coop")
	if err := os.Symlink(outside, meta); err != nil {
		t.Fatal(err)
	}
	SaveForkAgent(ws, "claude")
	if pathExists(filepath.Join(outside, "agent")) {
		t.Fatal("fork metadata write followed a symlinked .coop directory")
	}
	if err := os.Remove(meta); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(meta, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{forkAgentFile(ws), ForkSessionFile(ws, "claude", "work")} {
		if err := os.Symlink(sentinel, path); err != nil {
			t.Fatal(err)
		}
	}
	if got := ReadForkAgent(ws); got != "" {
		t.Fatalf("ReadForkAgent followed a symlink: %q", got)
	}
	if got := ReadForkSession(ws, "claude", "work"); got != "" {
		t.Fatalf("ReadForkSession followed a symlink: %q", got)
	}
	SaveForkAgent(ws, "gemini")
	SaveForkSession(ws, "claude", "work", strings.TrimSpace(want))
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != want {
		t.Fatalf("fork metadata write changed symlink target: %q, %v", data, err)
	}
	if got := ReadForkAgent(ws); got != "gemini" {
		t.Fatalf("safe agent replacement = %q, want gemini", got)
	}
	if got := ReadForkSession(ws, "claude", "work"); got != strings.TrimSpace(want) {
		t.Fatalf("safe session replacement = %q", got)
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
		err := ForkRmSafe(tc.unmerged, tc.dirty, tc.force)
		if (err != nil) != tc.wantErr {
			t.Errorf("ForkRmSafe(unmerged=%v dirty=%v force=%v) err = %v, wantErr %v",
				tc.unmerged, tc.dirty, tc.force, err, tc.wantErr)
		}
	}
}

// TestForkRmRefusesRunning: `coop fork rm` must refuse a fork whose loop is running (its worktree
// is bind-mounted RW into a live container) and must NOT delete the worktree — like merge/prune do.
func TestForkRmRefusesRunning(t *testing.T) {
	repo := t.TempDir()
	a := &Control{cfg: &config.Config{RepoOverride: repo}}
	ws := forkspace.Workspace(repo, "busy")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	// Mark it running by claiming the pidfile for THIS (live) process.
	if err := forkspace.WritePid(repo, "busy", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	code, err := a.ForkRm([]string{"busy"})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "running or awaiting cleanup") {
		t.Errorf("fork rm of a running fork = (%d, %v), want (1, running or awaiting cleanup)", code, err)
	}
	if !pathExists(ws) {
		t.Error("fork rm refused but still deleted the running fork's worktree")
	}
}

func TestOneForkName(t *testing.T) {
	if n, err := oneForkName("rm", []string{"x"}); n != "x" || err != nil {
		t.Errorf("oneForkName(1) = (%q, %v), want (x, nil)", n, err)
	}
	if n, err := oneForkName("rm", nil); n != "" || err != nil {
		t.Errorf("oneForkName(0) = (%q, %v), want (\"\", nil)", n, err)
	}
	if _, err := oneForkName("rm", []string{"a", "b"}); err == nil || !strings.Contains(err.Error(), "got a, b") {
		t.Errorf("oneForkName(2) should error naming both, got %v", err)
	}
}

// rm/merge/stop/logs must reject a SECOND positional (they used to silently act on only the last and
// report success). The check fires before any repo/clone work, so a bare app suffices.
func TestForkVerbsRejectSecondPositional(t *testing.T) {
	a := &Control{cfg: &config.Config{RepoOverride: t.TempDir()}}
	for verb, fn := range map[string]func([]string) (int, error){
		"rm": a.ForkRm, "merge": a.ForkMerge, "stop": a.ForkStop, "logs": a.ForkLogs,
	} {
		code, err := fn([]string{"aaa", "bbb"})
		if code != 2 || err == nil || !strings.Contains(err.Error(), "one name (got aaa, bbb)") {
			t.Errorf("fork %s a b = (%d, %v), want (2, 'takes one name (got aaa, bbb)')", verb, code, err)
		}
	}
}

// `coop fork rm` confirms before the unrecoverable delete: without --yes and no TTY it refuses and
// keeps the fork; --yes deletes. (The unmerged/dirty guard is separate — see TestForkRmSafe.)
func TestForkRmGate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	a := &Control{cfg: &config.Config{RepoOverride: repo}}
	ws, err := forkspace.Setup(repo, "perf")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
	}
	code, err := a.ForkRm([]string{"perf"}) // no --yes, no TTY → refuse
	if code != 2 || err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("fork rm without --yes = (%d, %v), want (2, a refusal naming --yes)", code, err)
	}
	if !pathExists(ws) {
		t.Error("a refused fork rm must not delete the fork")
	}
	if code, err := a.ForkRm([]string{"perf", "--yes"}); code != 0 || err != nil {
		t.Fatalf("fork rm --yes = (%d, %v), want (0, nil)", code, err)
	}
	if pathExists(ws) {
		t.Error("fork rm --yes should delete the fork")
	}
}

func TestForkRmConfirmsBeforeForcedStop(t *testing.T) {
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	if err := forkspace.WritePid(repo, "perf", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	a := &Control{cfg: &config.Config{RepoOverride: repo}}
	code, err := a.ForkRm([]string{"perf", "--force"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("forced fork rm without confirmation = (%d, %v), want refusal naming --yes", code, err)
	}
	if !pathExists(ws) || forkspace.RunningPid(repo, "perf") != os.Getpid() {
		t.Fatal("unconfirmed forced fork rm stopped its worker or deleted the workspace")
	}
}

func TestForkRmRechecksLifecycleAfterConfirmation(t *testing.T) {
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "race")
	if err != nil {
		t.Fatal(err)
	}
	a := &Control{cfg: &config.Config{RepoOverride: repo}}
	got := runForkCommandAcrossLockedMutation(t, repo, "race", func() (int, error) {
		return a.ForkRm([]string{"race", "--yes"})
	}, func() {
		if err := os.WriteFile(forkspace.PidPath(repo, "race"), []byte(forkspace.ReapPending), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if got.code != 1 || got.err == nil || !strings.Contains(got.err.Error(), "started while awaiting confirmation") {
		t.Fatalf("fork rm after lifecycle change = (%d, %v), want retryable refusal", got.code, got.err)
	}
	if !pathExists(ws) {
		t.Fatal("fork rm deleted a workspace that became reserved while confirmation was open")
	}
}

func TestForkRmRefusesReplacementAfterConfirmation(t *testing.T) {
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "replacement")
	if err != nil {
		t.Fatal(err)
	}
	a := &Control{cfg: &config.Config{RepoOverride: repo}}
	marker := filepath.Join(ws, "replacement.txt")
	got := runForkCommandAcrossLockedMutation(t, repo, "replacement", func() (int, error) {
		return a.ForkRm([]string{"replacement", "--force", "--yes"})
	}, func() {
		if err := os.RemoveAll(ws); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(marker, []byte("keep\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if got.code != 1 || got.err == nil || !strings.Contains(got.err.Error(), "was replaced") {
		t.Fatalf("fork rm after replacement = (%d, %v), want replacement refusal", got.code, got.err)
	}
	if !pathExists(marker) {
		t.Fatal("fork rm deleted the replacement workspace")
	}
}

func TestForkRmRefusesSymlinkReplacementAfterConfirmation(t *testing.T) {
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "replacement")
	if err != nil {
		t.Fatal(err)
	}
	a := &Control{cfg: &config.Config{RepoOverride: repo}}
	moved := ws + "-moved"
	marker := filepath.Join(moved, "replacement.txt")
	got := runForkCommandAcrossLockedMutation(t, repo, "replacement", func() (int, error) {
		return a.ForkRm([]string{"replacement", "--force", "--yes"})
	}, func() {
		if err := os.Rename(ws, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(moved, ws); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(marker, []byte("keep\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if got.code != 1 || got.err == nil || !strings.Contains(got.err.Error(), "was replaced") {
		t.Fatalf("fork rm after symlink replacement = (%d, %v), want replacement refusal", got.code, got.err)
	}
	if !pathExists(marker) {
		t.Fatal("fork rm followed and deleted the symlink replacement")
	}
}

// `coop fork ls` UPDATED must reflect the fork's OWN activity, not the base commit time it inherited
// from the clone — a fresh fork off a year-old base shows ~its creation, not "1 year ago".
func TestForkUpdatedShowsOwnActivityNotInheritedBase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	// Backdate the parent HEAD so an inherited base time would read as clearly stale.
	old := exec.Command("git", "-C", repo, "commit", "--allow-empty", "-qm", "old base")
	old.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_AUTHOR_DATE=2020-01-01T00:00:00", "GIT_COMMITTER_DATE=2020-01-01T00:00:00")
	if out, err := old.CombinedOutput(); err != nil {
		t.Fatalf("backdate base: %v\n%s", err, out)
	}
	ws, err := forkspace.Setup(repo, "perf")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
	}
	if got := forkUpdated(repo, ws); strings.Contains(got, "year") {
		t.Errorf("fresh fork UPDATED = %q, want ~creation time, not the inherited 2020 base commit", got)
	}
	// Its own commit is what should show once it works.
	if err := os.WriteFile(filepath.Join(ws, "x.txt"), []byte("w\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "fork work")
	if got := forkUpdated(repo, ws); got == "—" || strings.Contains(got, "year") {
		t.Errorf("fork with its own commit should show that commit's recent time, got %q", got)
	}
}

// review's "why" reads only COMPLETED (99_done) task logs, so a seeded 00_todo template — even with a
// newer mtime — never masquerades as the fork's work; no completed task → empty (caller says none yet).
func TestLatestTaskLogOnlyDone(t *testing.T) {
	ws := t.TempDir()
	writeTaskFile(t, filepath.Join(ws, tasksRoot, stateDone, "mine", "log.md"), "FORK OWN WORK\n")
	writeTaskFile(t, filepath.Join(ws, tasksRoot, stateTodo, "seed", "log.md"), "SEEDED TEMPLATE\n") // newer mtime
	if got := latestTaskLog(ws, 5); !strings.Contains(got, "FORK OWN WORK") || strings.Contains(got, "SEEDED") {
		t.Errorf("latestTaskLog = %q, want the 99_done log, not the newer seeded todo template", got)
	}
	empty := t.TempDir()
	writeTaskFile(t, filepath.Join(empty, tasksRoot, stateTodo, "x", "log.md"), "todo only\n")
	if got := latestTaskLog(empty, 5); got != "" {
		t.Errorf("latestTaskLog with no completed tasks = %q, want empty (caller renders 'none yet')", got)
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

func TestForkLifecycle(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)

	// forkspace.Setup clones + branches.
	ws, err := forkspace.Setup(repo, "perf")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
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
	if !ForkUnmerged(repo, ws) {
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
	if ForkUnmerged(repo, ws) {
		t.Error("fork should be merged after git merge")
	}
	if !pathExists(filepath.Join(repo, "feature.txt")) {
		t.Error("merged file not present in parent repo")
	}

	// DestroyFork removes the workspace and the review ref.
	if err := DestroyFork(runtime.Runtime{}, repo, "perf"); err != nil {
		t.Fatalf("DestroyFork: %v", err)
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
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo := initRepo(t)
	// Your real GLOBAL gitignore is carried into the fork; a repo-local core.excludesfile is
	// IGNORED — it's agent-writable, so reading it would let a poisoned repo point us at a host
	// secret (e.g. ~/.ssh/id_rsa) and copy its content into the fork. (`--global` ignores -C.)
	ignore := filepath.Join(t.TempDir(), "ignore")
	if err := os.WriteFile(ignore, []byte("*.tmp\n.DS_Store\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "config", "--global", "core.excludesfile", ignore)
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("SECRET_TOKEN_xyz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "config", "core.excludesfile", secret) // repo-local poison: must NOT be read

	ws, err := forkspace.Setup(repo, "ig")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
	}
	excl, err := os.ReadFile(filepath.Join(ws, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(excl), "*.tmp") || !strings.Contains(string(excl), ".DS_Store") {
		t.Errorf("global ignore not carried into the fork's .git/info/exclude:\n%s", excl)
	}
	if strings.Contains(string(excl), "SECRET_TOKEN") {
		t.Fatalf("a repo-local core.excludesfile (a host secret) was read and copied into the fork:\n%s", excl)
	}
}

func TestForkCarriesSigningMaterials(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	git(t, repo, "config", "gpg.format", "ssh")
	git(t, repo, "config", "user.signingkey", "/home/me/.ssh/id_ed25519.pub")
	git(t, repo, "config", "commit.gpgsign", "true")

	ws, err := forkspace.Setup(repo, "s")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
	}
	// The key + format travel so the rebase-on-land can sign.
	if got := gitOut(ws, "config", "--get", "gpg.format"); got != "ssh" {
		t.Errorf("gpg.format not propagated: %q", got)
	}
	if gitOut(ws, "config", "--get", "user.signingkey") == "" {
		t.Error("user.signingkey not propagated")
	}
	// commit.gpgsign must NOT be in the fork's local config, or the keyless box would
	// try to sign and every agent commit would fail.
	if got := gitOut(ws, "config", "--local", "--get", "commit.gpgsign"); got != "" {
		t.Errorf("commit.gpgsign must not be copied to the fork's local config, got %q", got)
	}
}

func TestDetectEditor(t *testing.T) {
	// With no GUI editor reachable on PATH, fall back to $VISUAL, then $EDITOR.
	t.Setenv("PATH", t.TempDir()) // empty dir → none of code/cursor/zed/idea/subl found
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "myeditor")
	if got := detectEditor(); got != "myeditor" {
		t.Errorf("detectEditor() = %q, want %q ($EDITOR fallback)", got, "myeditor")
	}
	t.Setenv("VISUAL", "myvisual")
	if got := detectEditor(); got != "myvisual" {
		t.Errorf("detectEditor() = %q, want %q ($VISUAL beats $EDITOR)", got, "myvisual")
	}
}

func TestResolveEditor(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// Isolate from the host's global/system git config so core.editor is only what we set.
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo := initRepo(t)
	// Your GLOBAL core.editor is honored; a repo-local one is IGNORED — the repo is agent-writable,
	// so reading core.editor from it would let a poisoned repo point the editor at a planted binary
	// that runs on `coop fork review --open`. (`git config --global` ignores -C, writing the env file.)
	git(t, repo, "config", "--global", "core.editor", "zed --wait")
	git(t, repo, "config", "core.editor", "/tmp/evil --pwn") // repo-local: must NEVER be used

	// $COOP_EDITOR wins over everything.
	if got := resolveEditor("nvim"); got != "nvim" {
		t.Errorf("resolveEditor(COOP_EDITOR) = %q, want %q", got, "nvim")
	}
	// With no $COOP_EDITOR, the GLOBAL core.editor is honored — never the repo-local one.
	if got := resolveEditor(""); got != "zed --wait" {
		t.Errorf("resolveEditor = %q, want %q (must ignore the repo-local editor)", got, "zed --wait")
	}
	// With neither set, fall through to detection ($VISUAL; PATH has no GUI editor).
	git(t, repo, "config", "--global", "--unset", "core.editor")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("EDITOR", "")
	t.Setenv("VISUAL", "myvisual")
	if got := resolveEditor(""); got != "myvisual" {
		t.Errorf("resolveEditor(fallback) = %q, want %q", got, "myvisual")
	}
}

func TestRunReviewCmd(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	// COOP_REVIEW_CMD runs via sh -c, but the fork name is environment data, never shell source.
	// Use command-substitution syntax here so a future interpolation would be immediately visible.
	name := `$(printf injected)`
	a := &Control{cfg: &config.Config{ReviewCmd: `printf '%s|%s' "$COOP_FORK_NAME" "$COOP_FORK_PATH" > ` + out}}
	if code, err := a.runReviewCmd(dir, "/the/fork", name, "review/demo"); err != nil || code != 0 {
		t.Fatalf("runReviewCmd = (%d, %v), want (0, nil)", code, err)
	}
	if data, _ := os.ReadFile(out); string(data) != name+"|/the/fork" {
		t.Errorf("COOP_REVIEW_CMD env not passed literally: got %q, want %q", data, name+"|/the/fork")
	}
}

// `coop fork logs <unknown>` must error like `fork path`/`fork review`, not exit 0 silently.
func TestForkLogsUnknownErrors(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	a := &Control{cfg: &config.Config{RepoOverride: repo}}
	code, err := a.ForkLogs([]string{"nope"})
	if err == nil || code == 0 {
		t.Fatalf("ForkLogs(unknown) = (%d, %v), want a no-such-fork error", code, err)
	}
	if !strings.Contains(err.Error(), "no such fork") {
		t.Errorf("error = %q, want it to mention 'no such fork'", err)
	}
}

// Removing a fork must stop its sibling services, and must do it WHILE the fork's compose file
// still exists: teardown is driven by that file, so once the worktree is deleted DownServices
// finds nothing and silently no-ops. That ordering bug left a removed fork's containers running
// for five days, holding disk the whole time.
func TestDestroyForkStopsServicesBeforeRemovingTheWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	stubDir := t.TempDir()
	record := filepath.Join(stubDir, "invocations")
	// The stub stands in for the container runtime: it records its argv, and — the point of the
	// test — whether the compose file it was pointed at still existed when it ran.
	stub := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >>" + record + "\n" +
		"for a in \"$@\"; do\n" +
		"  case \"$a\" in *compose.yml)\n" +
		"    if [ -f \"$a\" ]; then printf 'compose-file-present\\n' >>" + record + "\n" +
		"    else printf 'compose-file-ALREADY-GONE\\n' >>" + record + "; fi ;;\n" +
		"  esac\n" +
		"done\n"
	if err := os.WriteFile(filepath.Join(stubDir, "stubruntime"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "svc")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
	}
	compose := filepath.Join(ws, filepath.FromSlash(project.DefaultCompose))
	if err := os.MkdirAll(filepath.Dir(compose), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(compose, []byte("services:\n  db:\n    image: postgres:18.4-alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := DestroyFork(runtime.Runtime{Name: "stubruntime"}, repo, "svc"); err != nil {
		t.Fatalf("DestroyFork: %v", err)
	}

	got, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the runtime was never invoked, so the fork's services were left running: %v", err)
	}
	calls := string(got)
	if !strings.Contains(calls, "down") {
		t.Errorf("DestroyFork did not bring the fork's services down:\n%s", calls)
	}
	if !strings.Contains(calls, "--volumes") {
		t.Errorf("a disposable fork's volumes were left behind:\n%s", calls)
	}
	if strings.Contains(calls, "compose-file-ALREADY-GONE") {
		t.Errorf("services were stopped AFTER the worktree was deleted, so the teardown was a no-op:\n%s", calls)
	}
	if !strings.Contains(calls, "compose-file-present") {
		t.Errorf("teardown never saw the fork's compose file:\n%s", calls)
	}
	if pathExists(ws) {
		t.Error("workspace was not removed")
	}
}
