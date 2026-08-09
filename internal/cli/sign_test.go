package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

// gitRepo runs git in a fresh temp repo with an isolated global/system config, returning the repo
// path and a runner. Callers add signing config as needed. The hermetic repo itself is shared with
// internal/sessionsvc's tests, which drive the same fork workspaces this package creates.
func gitRepo(t *testing.T) (string, func(...string)) { return gitrepo.New(t) }

func TestSignBase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	git("commit", "-q", "--allow-empty", "-m", "c1")

	// No upstream and no --from → a clear error (not a guess).
	if _, err := signBase(repo, ""); err == nil {
		t.Error("no upstream + no --from should error")
	}
	// An explicit --from resolves to that base.
	if got, err := signBase(repo, base); err != nil || got != base {
		t.Errorf("signBase(--from base) = %q, %v; want %q", got, err, base)
	}
	// A nonexistent ref errors.
	if _, err := signBase(repo, "deadbeef"); err == nil {
		t.Error("a nonexistent --from ref should error")
	}
	// A range containing a merge commit is refused (a rebase would linearize it).
	git("checkout", "-q", "-b", "side")
	git("commit", "-q", "--allow-empty", "-m", "side work")
	git("checkout", "-q", "-")
	git("merge", "--no-ff", "--no-edit", "-q", "side")
	if _, err := signBase(repo, base); err == nil {
		t.Error("a range with a merge commit should be refused")
	}
}

func TestHeadUnsigned(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "plain")
	if !headUnsigned(repo) {
		t.Error("a plain commit has no gpgsig header — should read as unsigned")
	}
	// (The signed→false path shares the exact gpgsig-header check that TestSignUnpushed asserts.)
}

func TestSignRangeBase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	git("commit", "-q", "--allow-empty", "-m", "iteration start")
	iterHead := gitOut(repo, "rev-parse", "HEAD")

	if got, err := signRangeBase(repo, iterHead, "HEAD"); err != nil || got != iterHead {
		t.Fatalf("descendant base = %q, %v; want %q", got, err, iterHead)
	}
	git("commit", "--amend", "-q", "--allow-empty", "-m", "review amendment")
	if got, err := signRangeBase(repo, iterHead, "HEAD"); err != nil || got != base {
		t.Fatalf("amended-sibling base = %q, %v; want common base %q", got, err, base)
	}

	// Two merges with reversed parents form a criss-cross: neither shared parent is better
	// than the other, so choosing either one would make the signing range ambiguous.
	tree := gitOut(repo, "write-tree")
	left := gitOut(repo, "commit-tree", tree, "-p", base, "-m", "left")
	right := gitOut(repo, "commit-tree", tree, "-p", base, "-m", "right")
	leftMerge := gitOut(repo, "commit-tree", tree, "-p", left, "-p", right, "-m", "left merge")
	rightMerge := gitOut(repo, "commit-tree", tree, "-p", right, "-p", left, "-m", "right merge")
	git("reset", "-q", "--hard", rightMerge)
	if _, err := signRangeBase(repo, leftMerge, "HEAD"); err == nil || !strings.Contains(err.Error(), "multiple common bases") {
		t.Fatalf("ambiguous history error = %v; want clear multiple-common-bases failure", err)
	}

	git("checkout", "--orphan", "unrelated")
	git("commit", "-q", "--allow-empty", "-m", "unrelated")
	if _, err := signRangeBase(repo, iterHead, "HEAD"); err == nil || !strings.Contains(err.Error(), "no common base") {
		t.Fatalf("unrelated history error = %v; want clear no-common-base failure", err)
	}
}

func TestSignUnpushed(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not available")
	}
	// A throwaway SSH signing key, wired via a GLOBAL config trustedSignArgs will read.
	keyDir := t.TempDir()
	key := filepath.Join(keyDir, "sk")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-f", key, "-N", "", "-C", "coop-test").CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	globalCfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(globalCfg, []byte("[commit]\n\tgpgsign = true\n[gpg]\n\tformat = ssh\n[user]\n\tsigningkey = "+key+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// t.Setenv so the app's OWN git calls (trustedSignArgs → git config --global, and the rebase)
	// read this signing config, not the developer's.
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))

	repo := t.TempDir()
	runIn := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo // inherits the process env, incl. the t.Setenv'd GIT_CONFIG_GLOBAL
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runIn("init", "-q")
	runIn("config", "user.email", "t@t")
	runIn("config", "user.name", "T")
	runIn("config", "commit.gpgsign", "false") // box commits are unsigned
	for name, body := range map[string]string{
		".gitattributes": "filtered.txt filter=coop-sign-test\n",
		"filtered.txt":   "filter fixture\n",
		"staged.txt":     "staged original\n",
		"unstaged.txt":   "unstaged original\n",
		"fixture.key":    "real secret fixture\n",
	} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runIn("add", ".")
	runIn("commit", "-q", "--allow-empty", "-m", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	filterMarker := filepath.Join(t.TempDir(), "smudge-ran")
	runIn("config", "filter.coop-sign-test.smudge", "touch "+filterMarker+"; cat")
	runIn("config", "filter.coop-sign-test.clean", "cat")
	runIn("config", "filter.coop-sign-test.required", "true")
	for i, name := range []string{"c1.txt", "c2.txt"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runIn("add", name)
		runIn("commit", "-q", "-m", "c"+fmt.Sprint(i+1))
	}
	firstUnsigned := strings.Fields(gitOut(repo, "rev-list", "--reverse", base+"..HEAD"))[0]
	runIn("branch", "side-ref", firstUnsigned)
	runIn("config", "rebase.updateRefs", "true")
	sideRefBefore := gitOut(repo, "rev-parse", "refs/heads/side-ref")

	signed := func() int {
		n := 0
		for _, sha := range strings.Fields(gitOut(repo, "rev-list", base+"..HEAD")) {
			if strings.Contains(gitOut(repo, "cat-file", "commit", sha), "gpgsig") {
				n++
			}
		}
		return n
	}
	if signed() != 0 {
		t.Fatalf("precondition: commits should start unsigned, %d signed", signed())
	}

	if err := os.WriteFile(filepath.Join(repo, "staged.txt"), []byte("staged edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runIn("add", "staged.txt")
	for name, body := range map[string]string{
		"unstaged.txt":  "unstaged edit\n",
		"fixture.key":   "",
		"untracked.txt": "untracked edit\n",
	} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	checkoutSnapshot := func() string {
		parts := []string{
			gitOut(repo, "status", "--porcelain=v2", "--untracked-files=all"),
			gitOut(repo, "diff", "--binary"),
			gitOut(repo, "diff", "--cached", "--binary"),
			gitOut(repo, "show", ":staged.txt"),
		}
		for _, name := range []string{"staged.txt", "unstaged.txt", "fixture.key", "untracked.txt"} {
			data, err := os.ReadFile(filepath.Join(repo, name))
			if err != nil {
				t.Fatalf("snapshot %s: %v", name, err)
			}
			parts = append(parts, name+"="+string(data))
		}
		return strings.Join(parts, "\x00")
	}
	dirtyBefore := checkoutSnapshot()
	unsignedHead := gitOut(repo, "rev-parse", "HEAD")
	unsignedTree := gitOut(repo, "rev-parse", "HEAD^{tree}")

	a := &app{cfg: &config.Config{}}
	n, err := a.signUnpushed(repo, base)
	if err != nil {
		t.Fatalf("signUnpushed: %v", err)
	}
	if n != 2 {
		t.Errorf("re-signed count = %d, want 2", n)
	}
	if signed() != 2 {
		t.Errorf("both unpushed commits should carry a signature, got %d", signed())
	}
	if signedHead := gitOut(repo, "rev-parse", "HEAD"); signedHead == unsignedHead {
		t.Error("signing did not rewrite HEAD")
	}
	if signedTree := gitOut(repo, "rev-parse", "HEAD^{tree}"); signedTree != unsignedTree {
		t.Errorf("signing changed HEAD tree %s to %s", unsignedTree, signedTree)
	}
	if sideRefAfter := gitOut(repo, "rev-parse", "refs/heads/side-ref"); sideRefAfter != sideRefBefore {
		t.Errorf("signing changed side ref from %s to %s", sideRefBefore, sideRefAfter)
	}
	if _, err := os.Stat(filterMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("signing worktree executed a repository-local smudge filter: %v", err)
	}
	if dirtyAfter := checkoutSnapshot(); dirtyAfter != dirtyBefore {
		t.Errorf("signing changed the dirty checkout\nbefore:\n%q\nafter:\n%q", dirtyBefore, dirtyAfter)
	}
	// Idempotent: a second run re-signs cleanly and they stay signed.
	if _, err := a.signUnpushed(repo, base); err != nil {
		t.Fatalf("second signUnpushed: %v", err)
	}
	if signed() != 2 {
		t.Errorf("after a second sign, both should still be signed, got %d", signed())
	}
	if dirtyAfter := checkoutSnapshot(); dirtyAfter != dirtyBefore {
		t.Errorf("second signing changed the dirty checkout\nbefore:\n%q\nafter:\n%q", dirtyBefore, dirtyAfter)
	}
	if worktrees := strings.Count(gitOut(repo, "worktree", "list", "--porcelain"), "worktree "); worktrees != 1 {
		t.Errorf("signing left %d registered worktrees, want 1", worktrees)
	}
	// The base itself (pushed history) is untouched — never rewritten.
	if gitOut(repo, "rev-parse", base+"^{commit}") == "" {
		t.Error("the base commit should still exist (not rewritten)")
	}

	// A review may amend the commit that was HEAD when the iteration began. Re-sign the amended
	// sibling from their common parent, preserving both the reviewed message and tree.
	runIn("reset", "-q", "--hard", base)
	if err := os.WriteFile(filepath.Join(repo, "reviewed.txt"), []byte("amended\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runIn("add", "reviewed.txt")
	runIn("commit", "-q", "-m", "before review")
	iterHead := gitOut(repo, "rev-parse", "HEAD")
	runIn("commit", "--amend", "-q", "-m", "review rationale preserved")
	if _, err := a.signUnpushed(repo, iterHead); err != nil {
		t.Fatalf("sign amended sibling: %v", err)
	}
	if got := gitOut(repo, "show", "-s", "--format=%s", "HEAD"); got != "review rationale preserved" {
		t.Errorf("signed message = %q; want review amendment", got)
	}
	if got := gitOut(repo, "show", "HEAD:reviewed.txt"); got != "amended" {
		t.Errorf("signed tree content = %q; want amended", got)
	}
	if headUnsigned(repo) {
		t.Error("amended sibling should carry a signature after signing")
	}

	// A signing failure leaves both the branch and checkout untouched and cleans up its worktree.
	goodConfig, err := os.ReadFile(globalCfg)
	if err != nil {
		t.Fatal(err)
	}
	badConfig := strings.Replace(string(goodConfig), key, filepath.Join(t.TempDir(), "missing-key"), 1)
	if err := os.WriteFile(globalCfg, []byte(badConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	failedHead := gitOut(repo, "rev-parse", "HEAD")
	failedSnapshot := checkoutSnapshot()
	runIn("update-ref", "refs/heads/side-ref", failedHead)
	failedSideRef := gitOut(repo, "rev-parse", "refs/heads/side-ref")
	if _, err := a.signUnpushed(repo, base); err == nil {
		t.Fatal("signing with a missing key succeeded")
	}
	if got := gitOut(repo, "rev-parse", "HEAD"); got != failedHead {
		t.Errorf("failed signing moved HEAD from %s to %s", failedHead, got)
	}
	if got := checkoutSnapshot(); got != failedSnapshot {
		t.Errorf("failed signing changed checkout\nbefore:\n%q\nafter:\n%q", failedSnapshot, got)
	}
	if got := gitOut(repo, "rev-parse", "refs/heads/side-ref"); got != failedSideRef {
		t.Errorf("failed signing changed side ref from %s to %s", failedSideRef, got)
	}
	if worktrees := strings.Count(gitOut(repo, "worktree", "list", "--porcelain"), "worktree "); worktrees != 1 {
		t.Errorf("failed signing left %d registered worktrees, want 1", worktrees)
	}
	if err := os.WriteFile(globalCfg, goodConfig, 0o644); err != nil {
		t.Fatal(err)
	}

	// A concurrent branch move wins the compare-and-swap. The signed candidate is not applied.
	var hookErr error
	var competing, candidate string
	runIn("update-ref", "refs/heads/side-ref", failedHead)
	casSideRef := gitOut(repo, "rev-parse", "refs/heads/side-ref")
	a.beforeSignRefUpdate = func(repo, ref, oldHead, newHead string) {
		candidate = newHead
		tree := gitOut(repo, "rev-parse", oldHead+"^{tree}")
		competing = gitOut(repo, "commit-tree", tree, "-p", oldHead, "-m", "concurrent commit")
		if competing == "" {
			hookErr = errors.New("create concurrent commit")
			return
		}
		hookErr = gitRun(repo, "update-ref", ref, competing, oldHead)
	}
	if _, err := a.signUnpushed(repo, base); err == nil || !strings.Contains(err.Error(), "branch moved during re-signing") {
		t.Fatalf("concurrent ref move error = %v", err)
	}
	if hookErr != nil {
		t.Fatalf("concurrent ref hook: %v", hookErr)
	}
	if got := gitOut(repo, "rev-parse", "HEAD"); got != competing || got == candidate {
		t.Errorf("CAS result HEAD = %s, competing = %s, signed candidate = %s", got, competing, candidate)
	}
	if got := gitOut(repo, "rev-parse", "refs/heads/side-ref"); got != casSideRef {
		t.Errorf("CAS refusal changed side ref from %s to %s", casSideRef, got)
	}
	if worktrees := strings.Count(gitOut(repo, "worktree", "list", "--porcelain"), "worktree "); worktrees != 1 {
		t.Errorf("CAS refusal left %d registered worktrees, want 1", worktrees)
	}
}

func TestSignUnpushedIgnoresLocalSSHDefaultKeyCommand(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	globalCfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(globalCfg, []byte("[commit]\n\tgpgsign = true\n[gpg]\n\tformat = ssh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))

	repo := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "T")
	run("config", "commit.gpgsign", "false")
	run("commit", "-q", "--allow-empty", "-m", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	run("commit", "-q", "--allow-empty", "-m", "unsigned")
	marker := filepath.Join(t.TempDir(), "local-key-command-ran")
	run("config", "gpg.ssh.defaultKeyCommand", "touch "+marker)

	a := &app{cfg: &config.Config{}}
	if _, err := a.signUnpushed(repo, base); err == nil {
		t.Fatal("signing without a trusted global key unexpectedly succeeded")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repository-local SSH defaultKeyCommand executed: %v", err)
	}
}
