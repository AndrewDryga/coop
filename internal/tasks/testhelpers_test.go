package tasks

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"

	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

// defaultSessionStateRootForTest mirrors internal/cli/session_cmd.go's unexported
// defaultSessionStateRoot formula — the two can't share code across the package boundary (an
// internal *_test.go file may not import back into cli; see internal/importdag_test.go invariant
// 1), so this asserts the two independently-computed paths still agree, the same "local-redeclare
// a trivial, stateless leaf" shape gitOut uses (see git.go), sized for a test rather than a shared
// package.
func defaultSessionStateRootForTest() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "coop", "sessions"), nil
}

// git runs a plain (unhardened — this is test setup, not code under test) git command against dir
// with a fixed author/committer identity, failing the test on a non-zero exit. internal/cli's own
// fork_test.go keeps an identical copy for the same reason gitOut does; see git.go.
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

// gitRepo returns a fresh hermetic git repository — internal/cli/sign_test.go keeps its own
// identical one-line wrapper (the actual implementation is internal/testutil/gitrepo, shared by
// both packages) rather than exporting a test helper across the package boundary.
func gitRepo(t *testing.T) (string, func(...string)) { return gitrepo.New(t) }

// initRepo returns a fresh repo with one trailerless commit (a README) — internal/cli/fork_test.go
// keeps its own equivalent (an explicit "main" checkout plus the same one commit) for the same
// reason gitOut does; see git.go. Neither test suite depends on the branch's name, only that HEAD
// resolves to one commit with no Coop-Task trailer.
func initRepo(t *testing.T) string {
	t.Helper()
	// Later fixture commits and product Git queries inherit the process environment.
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo, run := gitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "init")
	return repo
}

func mustReadTaskTree(t testing.TB, root string) []Item {
	t.Helper()
	items, err := ReadTaskTree(root)
	if err != nil {
		t.Fatalf("ReadTaskTree(%q): %v", root, err)
	}
	return items
}

func mustReadBacklog(t testing.TB, root string) []Item {
	t.Helper()
	items, err := ReadBacklog(root)
	if err != nil {
		t.Fatalf("ReadBacklog(%q): %v", root, err)
	}
	return items
}

func mustCurrentTask(t testing.TB, root, id string) (Item, bool) {
	t.Helper()
	item, ok, err := CurrentTask(root, id)
	if err != nil {
		t.Fatalf("CurrentTask(%q, %q): %v", root, id, err)
	}
	return item, ok
}

func mustQueueCounts(t testing.TB, root string) (TaskCounts, string) {
	t.Helper()
	counts, active, err := QueueCounts(root)
	if err != nil {
		t.Fatalf("QueueCounts(%q): %v", root, err)
	}
	return counts, active
}

func mustQueueProgress(t testing.TB, hosts []string) (TaskCounts, string) {
	t.Helper()
	counts, active, err := QueueProgress(hosts)
	if err != nil {
		t.Fatalf("QueueProgress: %v", err)
	}
	return counts, active
}

func mustAlreadyCommittedInProgress(t testing.TB, repo string, hosts []string) []struct {
	ID, Commit string
	Depth      int
} {
	t.Helper()
	items, err := AlreadyCommittedInProgress(repo, hosts)
	if err != nil {
		t.Fatalf("AlreadyCommittedInProgress: %v", err)
	}
	return items
}

func mustNonArchivedDuplicateTaskIDs(t testing.TB, hosts []string) []string {
	t.Helper()
	ids, err := NonArchivedDuplicateTaskIDs(hosts)
	if err != nil {
		t.Fatalf("NonArchivedDuplicateTaskIDs: %v", err)
	}
	return ids
}

func mustUnblockResolved(t testing.TB, hosts []string) []string {
	t.Helper()
	ids, err := UnblockResolved(hosts)
	if err != nil {
		t.Fatalf("UnblockResolved: %v", err)
	}
	return ids
}

func mustAggregateDuplicateTaskIDs(t testing.TB, hosts []string) []string {
	t.Helper()
	ids, err := aggregateDuplicateTaskIDs(hosts)
	if err != nil {
		t.Fatalf("aggregateDuplicateTaskIDs: %v", err)
	}
	return ids
}

func mustDecisionResolved(t testing.TB, path string) bool {
	t.Helper()
	resolved, err := decisionResolved(path)
	if err != nil {
		t.Fatalf("decisionResolved(%q): %v", path, err)
	}
	return resolved
}

func mustCountDone(t testing.TB, root string) int {
	t.Helper()
	count, err := countDone(root)
	if err != nil {
		t.Fatalf("countDone(%q): %v", root, err)
	}
	return count
}

func mustParseTaskFolder(t testing.TB, dir, state string) (Item, bool) {
	t.Helper()
	item, ok, err := parseTaskFolder(dir, state)
	if err != nil {
		t.Fatalf("parseTaskFolder(%q, %q): %v", dir, state, err)
	}
	return item, ok
}

// captureStderr returns whatever fn writes to os.Stderr (ui.Warn/ui.Note/ui.Error go there).
// internal/cli/status_test.go keeps its own identical copy for the same reason gitOut does; see
// git.go.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	out, err := procharness.CaptureOutput(fn, &os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// captureStdout returns whatever fn writes to os.Stdout (list/decisions print there;
// ui.Info goes to stderr). Colors are off under `go test` (no tty), so output is plain.
// internal/cli/integration_test.go keeps its own copy of this same helper (used well beyond the
// task views) for the same reason gitOut does; see git.go.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	out, err := procharness.CaptureOutput(fn, &os.Stdout)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// assignLoopTask scans in stable queue/id order and atomically leases exactly one task before the
// box starts. An available in-progress task remains preferred, but a foreign-held one is skipped so
// another controller can take independent todo work. The flock is obtained while a todo folder is
// still in todo, then rides its atomic rename to in_progress by inode.
func assignLoopTask(hosts []string, owner TaskLeaseOwner) (TaskAssignment, error) {
	return AssignLoopTaskOnly(hosts, owner, "")
}

// tasksFolderMove relocates a task's folder to newState (claim/done). verb is the imperative used
// in the usage line ("claim"); pastVerb is the past tense for the success note ("claimed"). Moving
// to the state it's already in is a no-op note, not an error.
func tasksFolderMove(root string, args []string, newState, verb, pastVerb string) (int, error) {
	return tasksFolderMoveWith(root, args, newState, verb, pastVerb, claimOptions{})
}

// completionReplayCandidates separates lifecycle arrivals from mutations of folders that were
// already archived when the window began. Startup recovery re-runs this classification from
// authority-locked fingerprints before it writes markers or mutates lifecycle state.
func completionReplayCandidates(
	root string,
	record CompletionWindowRecord,
	candidates []QueuedTask,
) (replay []QueuedTask, baselineMutations []QueuedTask, err error) {
	for _, candidate := range candidates {
		current, fingerprintErr := CompletionFingerprintFor(root, candidate.Item)
		if fingerprintErr != nil {
			return nil, nil, fingerprintErr
		}
		if current.ReceiptBusy {
			return nil, nil, fmt.Errorf("task %s completion receipt is busy", candidate.Item.ID)
		}
		switch classifyCompletionCandidate(record, candidate.Item.ID, current) {
		case completionCandidateReplay:
			replay = append(replay, candidate)
		case completionCandidateMutation:
			baselineMutations = append(baselineMutations, candidate)
		}
	}
	return replay, baselineMutations, nil
}

func beginCompletionWindowsAllowing(hosts []string, taskID string) (*CompletionWindowSet, error) {
	return beginCompletionWindowsAllowingTasks(hosts, []string{taskID})
}

func ReconcileCompletionWindows(hosts []string) error {
	_, err := ReconcileCompletionWindowsWithActivity(hosts)
	return err
}

// openLeaseAuthority opens a task's authority record WITHOUT locking it. Production code takes the
// lock through lockLeaseAuthority instead, so that every held lock has been proved to be on the
// inode the registry name still resolves to; an unlocked open carries no such guarantee.
func OpenLeaseAuthority(root, id string, create bool) (*os.File, error) {
	key, err := LeaseAuthorityKey(root, id)
	if err != nil {
		return nil, err
	}
	return openLeaseAuthorityRecord(key+".lock", create)
}

func leaseCompletionReceiptMatches(authority *os.File, taskDir string) bool {
	_, ok := readLeaseCompletionReceipt(authority, taskDir)
	return ok
}

func ensureForkAssignmentIndexDir(repo string, identity forkspace.Identity) error {
	root, err := openStateRoot(repo, forkAssignmentIndexDir(repo, identity), true)
	if err != nil {
		return err
	}
	return root.Close()
}
