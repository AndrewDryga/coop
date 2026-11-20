package forkspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// What docs/forks.md says an --isolated fork holds at creation: every branch of the parent as it was
// then, no commit the parent makes afterwards, and object storage of its own (no alternates pointing
// back at the parent). forkctl's TestIsolatedConflictNamesTheWaysOutAndShowsInLs checks that later
// parent commits stay out after a conflicting merge and after a land.
func TestIsolatedForkHoldsHistoryUpToCreationAndNothingLater(t *testing.T) {
	repo := committedSetupRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "early.txt"), []byte("early\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "early.txt")
	gitIn(t, repo, "commit", "-qm", "early commit")
	// A branch the fork is not created from, with a commit of its own.
	gitIn(t, repo, "checkout", "-q", "-b", "side")
	if err := os.WriteFile(filepath.Join(repo, "side.txt"), []byte("side\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "side.txt")
	gitIn(t, repo, "commit", "-qm", "side commit")
	gitIn(t, repo, "checkout", "-q", "-")
	created, err := gitOutputContext(t.Context(), repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := SetupIsolatedContext(t.Context(), repo, "boundary", created)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "late.txt"), []byte("late\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "late.txt")
	gitIn(t, repo, "commit", "-qm", "late commit")
	late, err := gitOutputContext(t.Context(), repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	log, err := exec.Command("git", "-C", ws, "log", "--format=%s", "--all").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "early commit") || strings.Contains(string(log), "late commit") {
		t.Fatalf("isolated fork history:\n%s\nwant the commits up to creation and none after", log)
	}
	if err := exec.Command("git", "-C", ws, "cat-file", "-e", late+"^{commit}").Run(); err == nil {
		t.Fatal("a commit the parent made after creation is in the isolated fork")
	}
	// The clone copies every branch the parent had at creation, as remote-tracking refs, not only
	// the one it was created from; docs/forks.md says so.
	refs, _ := exec.Command("git", "-C", ws, "for-each-ref", "--format=%(refname)").Output()
	if !strings.Contains(string(log), "side commit") || !strings.Contains(string(refs), "refs/remotes/origin/side") {
		t.Errorf("isolated fork refs:\n%s\nlog:\n%s\nwant the parent's other branch as it was at creation", refs, log)
	}
	// It names no remote, so a person's git fetch there cannot bring the parent's later commits in,
	// while origin/HEAD, the base coop fork ls counts the fork's changes from, still resolves.
	if remotes, err := exec.Command("git", "-C", ws, "remote").Output(); err != nil || len(remotes) != 0 {
		t.Errorf("isolated fork remotes = %q, %v; want none", remotes, err)
	}
	if config, err := exec.Command("git", "-C", ws, "config", "--get-regexp", `^remote\.`).Output(); len(config) != 0 {
		t.Errorf("isolated fork remote config = %q, %v; want none", config, err)
	}
	if base, err := exec.Command("git", "-C", ws, "rev-parse", "--verify", "origin/HEAD").Output(); err != nil || strings.TrimSpace(string(base)) != created {
		t.Errorf("isolated fork origin/HEAD = %q, %v; want %s", base, err, created)
	}
	if out, err := exec.Command("git", "-C", ws, "fetch", "--quiet", "origin").CombinedOutput(); err == nil {
		t.Errorf("git fetch origin in the isolated fork succeeded: %s", out)
	}
	if err := exec.Command("git", "-C", ws, "cat-file", "-e", late+"^{commit}").Run(); err == nil {
		t.Fatal("git fetch brought a later parent commit into the isolated fork")
	}
	// Its remote-tracking branches are copies made at creation: log --all above saw no later commit.
	objects, err := exec.Command("git", "-C", ws, "rev-parse", "--git-path", "objects/info/alternates").Output()
	if err != nil {
		t.Fatal(err)
	}
	alternates := strings.TrimSpace(string(objects))
	if !filepath.IsAbs(alternates) {
		alternates = filepath.Join(ws, alternates)
	}
	if _, err := os.Stat(alternates); err == nil {
		t.Fatalf("isolated fork borrows objects through %s", alternates)
	}
}
