package forkctl

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

// setupMergeFork is a parent on main with fork name one commit ahead of it, adding name.txt.
func setupMergeFork(t *testing.T, repo, name string) string {
	t.Helper()
	ws, err := forkspace.Setup(repo, name)
	if err != nil {
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(ws, name+".txt"), name+"\n")
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", name)
	return ws
}

// checkoutState is what a refused merge must leave alone in a checkout: where HEAD is and the changes
// in its files. It leaves out refs, because a merge fetches the fork's branch to review/<name> in the
// parent before it asks.
func checkoutState(dir string) string {
	return forkspace.GitOut(dir, "rev-parse", "HEAD") + "\n" + gitBranch(dir) + "\n" + forkspace.GitOut(dir, "status", "--porcelain")
}

// requireBlock is err as the block a command refused with, failing the test when it is anything else.
func requireBlock(t *testing.T, code int, err error, wantCode int) *ui.UsageError {
	t.Helper()
	var block *ui.UsageError
	if code != wantCode || !errors.As(err, &block) {
		t.Fatalf("command = (%d, %v), want exit %d in the shared block", code, err, wantCode)
	}
	return block
}

// Every fork command refuses a fork that does not exist with the one block `coop fork <name>` draws,
// and exits 2 as for any rejected argument; none of them makes the fork.
func TestEveryForkCommandRefusesAMissingForkInOneBlock(t *testing.T) {
	repo := initRepo(t)
	c := &Control{cfg: &config.Config{RepoOverride: repo, ConfigDir: t.TempDir()}}
	want := NoSuchFork("nosuch")
	if want.Headline != `No fork named "nosuch"` || !slices.Equal(want.Rows, [][2]string{
		{"Create it:", "coop fork nosuch <target|preset>"}, {"Forks:", "coop fork ls"}}) {
		t.Fatalf("the missing-fork block = %#v", want)
	}
	for name, run := range map[string]func() (int, error){
		"path":   func() (int, error) { return c.ForkPath([]string{"nosuch"}) },
		"open":   func() (int, error) { return c.ForkOpenEditor([]string{"nosuch"}) },
		"review": func() (int, error) { return c.ForkReview([]string{"nosuch"}) },
		"merge":  func() (int, error) { return c.ForkMerge([]string{"nosuch"}) },
		"stop":   func() (int, error) { return c.ForkStop([]string{"nosuch"}) },
		"logs":   func() (int, error) { return c.ForkLogs([]string{"nosuch"}) },
		"rm":     func() (int, error) { return c.ForkRm([]string{"nosuch"}) },
	} {
		code, err := run()
		block := requireBlock(t, code, err, 2)
		if block.Headline != want.Headline || !slices.Equal(block.Rows, want.Rows) || block.ExitCode != 0 {
			t.Errorf("fork %s nosuch = %#v, want the block of %#v", name, block, want)
		}
	}
	if pathExists(forkspace.Workspace(repo, "nosuch")) {
		t.Error("a command on a missing fork made it")
	}
}

// A fork whose agent is still in its box says so in plain words and how to go on, not the registry's
// sandbox id, and the merge changes nothing: not the parent, not the fork.
func TestForkMergeSaysAForkIsStillRunning(t *testing.T) {
	repo := initRepo(t)
	ws := setupMergeFork(t, repo, "perf")
	unlock, err := forkspace.LockState(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := forkspace.EnsureGenerationLocked(repo, "perf")
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	execution, err := forkspace.BeginExecution(repo, forkspace.ExecutionSpec{
		Kind: forkspace.ExecutionForkInteractive, Role: forkspace.ExecutionRoleSandbox,
		Workspace: ws, Fork: &identity, SourceID: "foreground",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = forkspace.EndExecution(repo, execution) })
	parentBefore, forkBefore := checkoutState(repo), checkoutState(ws)
	c := &Control{cfg: &config.Config{RepoOverride: repo, ConfigDir: t.TempDir()}}
	code, err := c.ForkMerge([]string{"perf", "--yes"})
	block := requireBlock(t, code, err, 1)
	if block.Headline != "Fork perf is still running" ||
		!slices.Equal(block.Rows, [][2]string{{"", "Exit its agent or run coop fork stop perf, then merge again."}}) {
		t.Errorf("refusal = %#v", block)
	}
	if strings.Contains(block.Error(), execution.ID) {
		t.Errorf("the refusal shows the sandbox id %s: %s", execution.ID, block.Error())
	}
	if checkoutState(repo) != parentBefore || checkoutState(ws) != forkBefore {
		t.Error("a refused merge changed the parent or the fork")
	}
}

// Each way a fork's sandbox can stand has its own plain words, with the command that settles it and
// never the sandbox's id.
func TestForkBusyBlockWordsEachState(t *testing.T) {
	for _, tc := range []struct {
		state forkspace.ForkActivityState
		head  string
		fix   string
	}{
		{forkspace.ActivityRunning, "Fork perf is still running", "Exit its agent or run coop fork stop perf, then merge again."},
		{forkspace.ActivityCleanupPending, "Fork perf has a stopped box left to clean up", "Run coop fork stop perf, then merge again."},
		{forkspace.ActivityUnverified, "Coop cannot tell whether fork perf is still running", "Run coop fork ls to see why, then merge again."},
	} {
		activity := &forkspace.ForkActivityError{Fork: "perf", State: tc.state, Role: forkspace.ExecutionRoleSandbox, ID: strings.Repeat("ab", 16)}
		block := mergeRefusal(activity)
		if block == nil || block.Headline != tc.head || !slices.Equal(block.Rows, [][2]string{{"", tc.fix}}) || block.ExitCode != 1 ||
			strings.Contains(block.Error(), activity.ID) {
			t.Errorf("%s = %#v, want %q then %q", tc.state, block, tc.head, tc.fix)
		}
	}
}

// A rebase cannot run over uncommitted changes to tracked files, so a fork with any is refused first,
// with the way out, before the rebase that would only fail with Git's words. A fork whose only extra
// is an untracked file still merges and is kept with a note, and the batch names the refused fork.
func TestForkMergeRefusesTrackedChangesBeforeAnyRebase(t *testing.T) {
	repo := initRepo(t)
	ws := setupMergeFork(t, repo, "perf")
	writeTaskFile(t, filepath.Join(repo, "parent.txt"), "the parent moves on\n") // a rebase would have work to do
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "parent moves")
	writeTaskFile(t, filepath.Join(ws, "README.md"), "edited in the fork\n")
	snapshot := func() string { return checkoutState(repo) + "\n" + checkoutState(ws) }
	before := snapshot()
	c := &Control{cfg: &config.Config{RepoOverride: repo, ConfigDir: t.TempDir()}}

	refused := func(args ...string) (*ui.UsageError, string) {
		t.Helper()
		var code int
		var err error
		stderr := captureStderr(t, func() { code, err = c.ForkMerge(args) })
		return requireBlock(t, code, err, 1), stderr
	}
	wantRows := [][2]string{{"", "Commit or stash them in the fork, then merge again."}, {"Open the fork:", "coop fork open perf"}}
	block, stderr := refused("perf", "--yes")
	if block.Headline != "Fork perf has uncommitted changes to tracked files" || !slices.Equal(block.Rows, wantRows) {
		t.Errorf("refusal = %#v", block)
	}
	if strings.Contains(stderr, "Rebasing onto") || leftoverRebaseState(ws) != "" {
		t.Errorf("the merge reached the rebase:\n%s", stderr)
	}
	if got, err := os.ReadFile(filepath.Join(ws, "README.md")); err != nil || string(got) != "edited in the fork\n" || snapshot() != before {
		t.Errorf("the refusal changed what it protects: %q, %v\n%s\nwant\n%s", got, err, snapshot(), before)
	}
	// A staged change is a change to a tracked file too.
	git(t, ws, "add", "README.md")
	if block, _ = refused("perf", "--yes"); block.Headline != "Fork perf has uncommitted changes to tracked files" {
		t.Errorf("a staged change was not refused: %#v", block)
	}
	// In a batch, the refused fork is named with what it did and left, and the parent stays as it was.
	setupMergeFork(t, repo, "other")
	parentHead := forkspace.GitOut(repo, "rev-parse", "HEAD")
	var code int
	var err error
	stderr = captureStderr(t, func() { code, err = c.forkMergeAll(repo, []string{"perf", "other"}, "", false, true) })
	if code != 1 || err == nil {
		t.Fatalf("forkMergeAll = (%d, %v), want a refusal", code, err)
	}
	for _, want := range []string{"Fork perf has uncommitted changes to tracked files", "Merged: nothing.", "Untouched: other.",
		"Commit or stash them in the fork, then merge again."} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the batch refusal lacks %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "rebase onto") || forkspace.GitOut(repo, "rev-parse", "HEAD") != parentHead {
		t.Errorf("the batch went on:\n%s", stderr)
	}
	// Once the change is committed, the same merge goes through.
	git(t, ws, "commit", "-qam", "keep the edit")
	if code, err := c.ForkMerge([]string{"perf", "--yes"}); code != 0 || err != nil {
		t.Fatalf("merge after the commit = (%d, %v)", code, err)
	}
	if got, err := os.ReadFile(filepath.Join(repo, "README.md")); err != nil || string(got) != "edited in the fork\n" {
		t.Errorf("the committed edit did not land: %q, %v", got, err)
	}
}

// An untracked file is not a change a rebase refuses: the fork merges, and is kept so the file can be
// reviewed.
func TestForkMergeStillMergesAForkWithOnlyAnUntrackedFile(t *testing.T) {
	repo := initRepo(t)
	ws := setupMergeFork(t, repo, "perf")
	writeTaskFile(t, filepath.Join(ws, "scratch.txt"), "not committed\n")
	c := &Control{cfg: &config.Config{RepoOverride: repo, ConfigDir: t.TempDir()}}
	var code int
	var err error
	stderr := captureStderr(t, func() { code, err = c.ForkMerge([]string{"perf", "--yes"}) })
	if code != 0 || err != nil || !strings.Contains(stderr, "The fork still has uncommitted changes") {
		t.Fatalf("ForkMerge = (%d, %v):\n%s", code, err, stderr)
	}
	if !pathExists(filepath.Join(repo, "perf.txt")) || !pathExists(ws) {
		t.Error("the fork's commit did not land, or the fork was not kept")
	}
}

// A gate whose box cannot start never ran. The merge says that, with the box's own reason and what it
// asks for, instead of reporting a gate that failed; nothing lands, and the fork is kept to merge
// again. Here the project asks for network access nobody approved, which refuses the gate's box.
func TestForkMergeSaysTheChecksCouldNotStart(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := initRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, project.File), []byte("box:\n  egress: filtered\n  egress_rules:\n    - to: {domain: project.example}\n      protocol: tls\n      ports: [443]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", project.File)
	git(t, repo, "commit", "-qm", "request filtered network")
	ws := setupMergeFork(t, repo, "perf")
	parentBefore := checkoutState(repo)
	c := &Control{cfg: &config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), BaseImage: "gate-image", Gate: []string{"sh", "-c", "test -f f.txt"}}}
	var code int
	var err error
	stderr := captureStderr(t, func() { code, err = c.ForkMerge([]string{"perf", "--yes"}) })
	// The line naming the checks keeps the quotes, so pasted it runs the command that was configured.
	if !strings.Contains(stderr, "  Running: sh -c 'test -f f.txt'\n") {
		t.Errorf("the Running line is not a command to paste:\n%s", stderr)
	}
	block := requireBlock(t, code, err, 1)
	if block.Headline != "The project checks could not start for fork perf" ||
		!strings.Contains(block.Cause, "this project asks for network access that has not been approved") ||
		!strings.Contains(block.Cause, "Review it: coop approve") ||
		!slices.Equal(block.Rows, [][2]string{{"", "Nothing was merged. Fix this, then merge again."}}) {
		t.Errorf("refusal = %#v", block)
	}
	if strings.Contains(block.Error(), "gate failed") || strings.Contains(stderr, "gate failed") {
		t.Errorf("a gate that never ran was called failed:\n%s\n%s", stderr, block.Error())
	}
	if checkoutState(repo) != parentBefore || !pathExists(ws) || pathExists(filepath.Join(repo, "perf.txt")) {
		t.Error("a merge whose checks could not start changed the parent or removed the fork")
	}
}

// The same holds when the fork lands through its reviewed task candidate: the checks that could not
// start put the reviewed candidate back, retire the land journal, and the retry lands.
func TestTaskLandSaysTheChecksCouldNotStart(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo, ws, root, identity, c := prepareForkTaskCandidate(t, "task-gate-start")
	candidate := forkspace.GitOut(ws, "rev-parse", "HEAD")
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, project.File), []byte("box:\n  egress: filtered\n  egress_rules:\n    - to: {domain: project.example}\n      protocol: tls\n      ports: [443]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", project.File)
	git(t, repo, "commit", "-qm", "request filtered network")
	parentHead := forkspace.GitOut(repo, "rev-parse", "HEAD")
	c.cfg.RepoOverride, c.cfg.BaseImage, c.cfg.Gate = repo, "gate-image", []string{"true"}
	landed, err := mergeOneForTest(t, c, repo, "gate-image", identity.Name, false)
	var start *gateStartError
	if landed || !errors.As(err, &start) || !strings.Contains(err.Error(), "Review it: coop approve") {
		t.Fatalf("merge whose checks cannot start = (%v, %v)", landed, err)
	}
	if got := forkspace.GitOut(ws, "rev-parse", "HEAD"); got != candidate {
		t.Errorf("the reviewed candidate was not put back: %s, want %s", got, candidate)
	}
	if _, pending, err := readLandIntent(repo, identity); err != nil || pending {
		t.Errorf("land journal after the refusal: pending=%v err=%v", pending, err)
	}
	if forkspace.GitOut(repo, "rev-parse", "HEAD") != parentHead {
		t.Error("the parent moved")
	}
	if item, _ := mustCurrentTask(t, root, "canonical-task"); item.State == tasks.StateDone {
		t.Errorf("the task was finished by a merge that did not land: %s", item.State)
	}
	c.gateOK = func(_, _, _ string) bool { return true }
	if landed, err := mergeOneForTest(t, c, repo, "gate-image", identity.Name, false); !landed || err != nil {
		t.Fatalf("retry once the checks can run = (%v, %v)", landed, err)
	}
}
