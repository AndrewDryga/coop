package sessionsvc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

// boundedHistoryRepo builds a repository whose commits are spread across the
// past year, so a window genuinely cuts it somewhere in the middle.
func boundedHistoryRepo(t *testing.T, commits int, apart time.Duration) (string, string) {
	t.Helper()
	repo, git := gitrepo.New(t)
	now := time.Now().UTC()
	for index := range commits {
		body := fmt.Sprintf("commit %d\n", index)
		if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		// Oldest first, so the newest commit lands at roughly now.
		at := now.Add(-time.Duration(commits-1-index) * apart).Format(time.RFC3339)
		git("add", "file.txt")
		cmd := exec.Command("git", "commit", "-q", "-m", body)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "noglobal"),
			"GIT_CONFIG_SYSTEM="+filepath.Join(t.TempDir(), "nosystem"),
			"GIT_AUTHOR_DATE="+at, "GIT_COMMITTER_DATE="+at)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("commit %d: %v\n%s", index, err, out)
		}
	}
	head, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return repo, strings.TrimSpace(string(head))
}

func companionGit(t *testing.T, workspace string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", workspace}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A repository too large for its whole history should still carry recent
// history rather than one commit. The choice used to be all or nothing, and
// nothing meant an agent asked what changed last week could see the current
// source and no history at all — it could not even deepen it, because a
// commit-only companion has no remote to fetch from.
func TestCompanionCarriesRecentHistoryWhenFullHistoryDoesNotFit(t *testing.T) {
	repo, head := boundedHistoryRepo(t, 40, 24*time.Hour)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	binding := session.CompanionRepository{
		Name: "bounded", Repository: repo, BaseCommit: head,
		Workspace: filepath.Join(root, "bounded"),
	}
	// A budget under the whole history but over the newest week of it.
	whole, err := sessionCompanionHistoryWithinLimit(context.Background(), binding, 1<<62, "")
	if err != nil || !whole {
		t.Fatalf("measuring the whole history failed: %v", err)
	}
	limit := companionHistoryBytes(t, binding, "") - 1
	if err := createSessionCompanionWithHistoryLimit(context.Background(), binding, limit); err != nil {
		t.Fatalf("create companion: %v", err)
	}

	mode := readFile(t, filepath.Join(binding.Workspace, ".git", sessionCompanionHistoryFile))
	if mode != sessionCompanionHistoryBounded {
		t.Fatalf("history mode = %q, want a bounded history rather than all or nothing", mode)
	}
	if got := companionGit(t, binding.Workspace, "rev-parse", "--is-shallow-repository"); got != "true" {
		t.Fatalf("bounded companion reports shallow = %q", got)
	}
	count := companionGit(t, binding.Workspace, "rev-list", "--count", "HEAD")
	if count == "1" {
		t.Fatal("bounded companion carried only the pinned commit")
	}
	if companionGit(t, binding.Workspace, "rev-parse", "HEAD") != head {
		t.Fatal("bounded companion is not checked out at the pinned commit")
	}
	// The point of the whole change: history is walkable, and git stops at the
	// boundary cleanly instead of failing on an object that was never packed.
	if out := companionGit(t, binding.Workspace,
		"fsck", "--connectivity-only", "--no-dangling", "--no-reflogs"); out != "" {
		t.Fatalf("bounded companion history is inconsistent: %s", out)
	}
	if companionGit(t, binding.Workspace, "log", "--oneline") == "" {
		t.Fatal("bounded companion cannot walk its own log")
	}
	// A companion that already exists must pass its own reuse check.
	if err := verifySessionCompanionContext(context.Background(), binding); err != nil {
		t.Fatalf("bounded companion failed its reuse check: %v", err)
	}
}

// The shallow file names the commits this companion holds whose parents it
// does not. Naming the missing parents instead — which is what rev-list
// --boundary prints — leaves git believing it holds them, and the first walk
// that reaches the edge dies on an absent object.
func TestShallowBoundaryNamesCarriedCommitsNotMissingParents(t *testing.T) {
	repo, head := boundedHistoryRepo(t, 12, 24*time.Hour)
	binding := session.CompanionRepository{Name: "b", Repository: repo, BaseCommit: head}
	since := time.Now().UTC().Add(-4 * 24 * time.Hour).Format(time.RFC3339)
	boundary, err := sessionCompanionShallowBoundary(context.Background(), binding,
		sessionCompanionHistory{mode: sessionCompanionHistoryBounded, since: since})
	if err != nil {
		t.Fatal(err)
	}
	points := strings.Fields(boundary)
	if len(points) == 0 {
		t.Fatal("a window that cuts the history recorded no shallow point")
	}
	carried := map[string]bool{}
	for _, commit := range strings.Fields(
		companionGit(t, repo, "rev-list", "--since="+since, head)) {
		carried[commit] = true
	}
	for _, point := range points {
		if !carried[point] {
			t.Fatalf("shallow point %s is not a commit this companion carries", point)
		}
	}
}

// A window wide enough to reach the root truncates nothing, so the companion
// must not be labelled shallow over history it actually holds.
func TestWindowReachingTheRootIsNotMarkedShallow(t *testing.T) {
	repo, head := boundedHistoryRepo(t, 5, time.Hour)
	binding := session.CompanionRepository{Name: "b", Repository: repo, BaseCommit: head}
	boundary, err := sessionCompanionShallowBoundary(context.Background(), binding,
		sessionCompanionHistory{
			mode:  sessionCompanionHistoryBounded,
			since: time.Now().UTC().Add(-365 * 24 * time.Hour).Format(time.RFC3339),
		})
	if err != nil {
		t.Fatal(err)
	}
	if boundary != "" {
		t.Fatalf("boundary = %q, want none when the window reaches the root", boundary)
	}
}

func companionHistoryBytes(t *testing.T, binding session.CompanionRepository, since string) uint64 {
	t.Helper()
	// Binary search the smallest limit the measurement still accepts, which is
	// the history's own size — the measurement reports a verdict, not a total.
	low, high := uint64(0), uint64(1)<<40
	for low < high {
		mid := low + (high-low)/2
		fits, err := sessionCompanionHistoryWithinLimit(context.Background(), binding, mid, since)
		if err != nil {
			t.Fatal(err)
		}
		if fits {
			high = mid
		} else {
			low = mid + 1
		}
	}
	return low
}
