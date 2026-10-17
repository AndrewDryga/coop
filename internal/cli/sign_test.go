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
	"github.com/AndrewDryga/coop/internal/tasks"
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
	// A merge in the range is signUnpushed's to judge: it may sit below the commits a pass rewrites.
	git("checkout", "-q", "-b", "side")
	git("commit", "-q", "--allow-empty", "-m", "side work")
	git("checkout", "-q", "-")
	git("merge", "--no-ff", "--no-edit", "-q", "side")
	if got, err := signBase(repo, base); err != nil || got != base {
		t.Errorf("signBase(--from base) over a merge = %q, %v; want %q", got, err, base)
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
	// (The signed→false path is commitSigned, which TestCommitSigned covers.)
}

// Only the commit's own signature header counts, in either object format.
func TestCommitSigned(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      bool
	}{
		{"signed", "tree t\nauthor a\ncommitter c\ngpgsig -----BEGIN SSH SIGNATURE-----\n s\n -----END SSH SIGNATURE-----\n\nsubject\n", true},
		{"signed in a SHA-256 repository", "tree t\nauthor a\ncommitter c\ngpgsig-sha256 -----BEGIN PGP SIGNATURE-----\n \n s\n\nsubject\n", true},
		{"unsigned", "tree t\nauthor a\ncommitter c\n\nsubject\n", false},
		{"a message line is not a signature", "tree t\nauthor a\ncommitter c\n\nsubject\n\ngpgsig -----BEGIN SSH SIGNATURE-----\n", false},
		{"a merged tag's signature is not the commit's", "tree t\nmergetag object o\n -----BEGIN PGP SIGNATURE-----\n\nsubject\n", false},
	} {
		if got := commitSigned(tc.raw); got != tc.want {
			t.Errorf("%s: commitSigned = %v, want %v", tc.name, got, tc.want)
		}
	}
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
	// A throwaway SSH signing key, wired via a GLOBAL config forkspace.TrustedSignArgs will read.
	keyDir := t.TempDir()
	key := filepath.Join(keyDir, "sk")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-f", key, "-N", "", "-C", "coop-test").CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	globalCfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(globalCfg, []byte("[commit]\n\tgpgsign = true\n[gpg]\n\tformat = ssh\n[user]\n\tsigningkey = "+key+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// t.Setenv so the app's OWN git calls (forkspace.TrustedSignArgs → git config --global, and the rebase)
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
	// Already signed: a second run rewrites nothing, so every commit keeps the ID it was signed with.
	signedHead := gitOut(repo, "rev-parse", "HEAD")
	if n, err := a.signUnpushed(repo, base); err != nil || n != 0 {
		t.Fatalf("second signUnpushed = %d, %v; want a no-op", n, err)
	}
	if got := gitOut(repo, "rev-parse", "HEAD"); got != signedHead {
		t.Errorf("signing an already-signed range rewrote HEAD %s to %s", signedHead, got)
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
	// The amended commit is signed now, so a new unsigned commit is what the pass has to sign.
	runIn("commit", "-q", "--allow-empty", "-m", "unsigned after review")
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
	if _, err := a.signUnpushed(repo, base); err == nil || !strings.Contains(err.Error(), "The branch changed while Coop was signing") {
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

// signingRepo is a hermetic repository whose plain commits are unsigned, like a box's, while
// `commit -S` and the app's own signing pass sign with a throwaway SSH key. Both config doors are
// pinned in the PROCESS env, because signUnpushed runs git itself. origin is a local bare
// repository: push to give the branch an upstream.
func signingRepo(t *testing.T) (string, func(...string)) {
	t.Helper()
	for _, tool := range []string{"git", "ssh-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not available")
		}
	}
	key := filepath.Join(t.TempDir(), "sk")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-f", key, "-N", "", "-C", "coop-test").CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	globalCfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(globalCfg, []byte("[gpg]\n\tformat = ssh\n[user]\n\tsigningkey = "+key+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo, origin := t.TempDir(), filepath.Join(t.TempDir(), "origin.git")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("symbolic-ref", "HEAD", "refs/heads/main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "T")
	run("config", "commit.gpgsign", "false") // box commits are unsigned
	run("init", "-q", "--bare", origin)
	run("remote", "add", "origin", origin)
	return repo, run
}

// hasSignature reads the raw commit object, independently of the code under test.
func hasSignature(repo, rev string) bool {
	return strings.Contains(gitOut(repo, "cat-file", "commit", rev), "\ngpgsig ")
}

// Signing starts at the first unsigned commit. The signed commits below it — another agent's in
// the same checkout, an earlier cycle's — keep their IDs, and an already-signed range is left as it
// is, so the loop's end-of-run sweep over the whole run is a no-op after per-cycle signing.
func TestSignUnpushedStartsAtTheFirstUnsignedCommit(t *testing.T) {
	repo, git := signingRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	git("commit", "-q", "--allow-empty", "-S", "-m", "signed by an outside agent")
	git("commit", "-q", "--allow-empty", "-S", "-m", "signed by an earlier cycle")
	git("commit", "-q", "--allow-empty", "-m", "box commit")
	git("commit", "-q", "--allow-empty", "-m", "another box commit")
	before := strings.Fields(gitOut(repo, "rev-list", "--reverse", base+"..HEAD"))
	trees := gitOut(repo, "log", "--format=%T", base+"..HEAD")

	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	if n, err := a.signUnpushed(repo, base); err != nil || n != 2 {
		t.Fatalf("signUnpushed = %d, %v; want only the 2 unsigned commits signed", n, err)
	}
	after := strings.Fields(gitOut(repo, "rev-list", "--reverse", base+"..HEAD"))
	if len(after) != 4 || after[0] != before[0] || after[1] != before[1] {
		t.Fatalf("signing rewrote the signed commits below the first unsigned one:\nbefore %v\nafter  %v", before, after)
	}
	for _, sha := range after {
		if !hasSignature(repo, sha) {
			t.Errorf("commit %s is not signed", sha)
		}
	}
	if got := gitOut(repo, "log", "--format=%T", base+"..HEAD"); got != trees {
		t.Errorf("signing changed committed trees:\nbefore %s\nafter  %s", trees, got)
	}

	// The whole range is signed now, so a second pass from the same base rewrites nothing.
	signedHead := gitOut(repo, "rev-parse", "HEAD")
	if n, err := a.signUnpushed(repo, base); err != nil || n != 0 {
		t.Fatalf("signing an already-signed range = %d, %v; want a no-op", n, err)
	}
	if got := gitOut(repo, "rev-parse", "HEAD"); got != signedHead {
		t.Errorf("signing an already-signed range moved HEAD from %s to %s", signedHead, got)
	}

	// Only the LEADING signed commits are skipped: a signed commit above an unsigned one is
	// replayed with it, because its parent changes.
	git("commit", "-q", "--allow-empty", "-m", "late box commit")
	git("commit", "-q", "--allow-empty", "-S", "-m", "signed on top of it")
	if n, err := a.signUnpushed(repo, base); err != nil || n != 2 {
		t.Fatalf("signUnpushed = %d, %v; want the unsigned commit and the one above it", n, err)
	}
	if got := gitOut(repo, "rev-parse", "HEAD~2"); got != signedHead {
		t.Errorf("signing rewrote %s, which was already signed, into %s", signedHead, got)
	}
	if !hasSignature(repo, "HEAD~1") || !hasSignature(repo, "HEAD") {
		t.Error("both replayed commits should carry a signature")
	}
}

// A signed merge below the first unsigned commit — a mid-run git pull, say — is not rewritten, so
// it no longer stops the pass; a merge among the commits a pass would rewrite still does, because
// re-signing would linearize it.
func TestSignUnpushedSignsAroundASignedMerge(t *testing.T) {
	repo, git := signingRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	git("checkout", "-q", "-b", "side")
	git("commit", "-q", "--allow-empty", "-S", "-m", "signed side work")
	git("checkout", "-q", "main")
	git("commit", "-q", "--allow-empty", "-S", "-m", "signed main work")
	git("merge", "--no-ff", "--no-edit", "-q", "-S", "side")
	merge := gitOut(repo, "rev-parse", "HEAD")
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}

	// Everything signed, the merge included: nothing to do, and no refusal.
	if n, err := a.signUnpushed(repo, base); err != nil || n != 0 {
		t.Fatalf("an all-signed range with a merge = %d, %v; want a no-op", n, err)
	}

	git("commit", "-q", "--allow-empty", "-m", "box commit")
	git("commit", "-q", "--allow-empty", "-m", "another box commit")
	if n, err := a.signUnpushed(repo, base); err != nil || n != 2 {
		t.Fatalf("signUnpushed above a signed merge = %d, %v; want the 2 unsigned commits signed", n, err)
	}
	if got := gitOut(repo, "rev-parse", "HEAD~2"); got != merge {
		t.Fatalf("signing rewrote the signed merge %s into %s", merge, got)
	}
	if !hasSignature(repo, "HEAD") || !hasSignature(repo, "HEAD~1") {
		t.Error("both commits above the merge should carry a signature")
	}

	// An unsigned commit below a merge would need the merge rewritten too: still refused.
	git("checkout", "-q", "-b", "later-side")
	git("commit", "-q", "--allow-empty", "-S", "-m", "signed later side work")
	git("checkout", "-q", "main")
	git("commit", "-q", "--allow-empty", "-m", "unsigned below the next merge")
	git("merge", "--no-ff", "--no-edit", "-q", "-S", "later-side")
	head := gitOut(repo, "rev-parse", "HEAD")
	if _, err := a.signUnpushed(repo, base); err == nil || !strings.Contains(err.Error(), "merge commit") {
		t.Fatalf("an unsigned commit below a merge = %v; want the merge refusal", err)
	}
	if got := gitOut(repo, "rev-parse", "HEAD"); got != head {
		t.Fatalf("a refused pass moved HEAD from %s to %s", head, got)
	}
}

// A task the final review reopened records the commits its rework must keep by ID. A signing pass
// replaces them with signed copies and carries that authority along, so the loop does not park the
// task as stale, and the pending review still finds the task's commit by its trailer.
func TestSignUnpushedCarriesAReopenedTaskAuthority(t *testing.T) {
	repo, git := signingRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	root := filepath.Join(repo, ".agent", "tasks")
	id := "reopened-task"
	dir := filepath.Join(root, tasks.StateTodo, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.md"), []byte("# "+id+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tasks.RewriteSubtasks(dir, []tasks.Subtask{{Text: "work verified", Done: true}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "work.txt"), []byte("work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "work.txt")
	git("commit", "-q", "-m", "box work\n\nCoop-Task: "+id)
	hosts := []string{root}
	item, ok, err := tasks.CurrentTask(root, id)
	if err != nil || !ok {
		t.Fatalf("task = %+v, %v, %v", item, ok, err)
	}
	if err := tasks.CompleteTrustedTask(root, item); err != nil {
		t.Fatal(err)
	}
	plan, err := tasks.NewPendingReviewPlan(repo, hosts, base, "digest", "coop loop",
		tasks.PendingReviewStage{Targets: []string{"codex:test@personal"}, Writes: "tasks"}, 3, false, tasks.PendingReviewStage{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.EnrollExistingPendingReviews(repo, hosts, []string{id}, plan); err != nil {
		t.Fatal(err)
	}
	if err := tasks.MarkPendingReviewReopened(hosts, []string{id}); err != nil {
		t.Fatal(err)
	}
	reopen, err := tasks.CaptureAuditReopen(repo, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.WriteAuditReopenRecord(root, reopen); err != nil {
		t.Fatal(err)
	}
	done, _, err := tasks.CurrentTask(root, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.MoveTaskDir(root, done, tasks.StateTodo); err != nil {
		t.Fatal(err)
	}

	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	if n, err := a.signUnpushed(repo, base); err != nil || n != 1 {
		t.Fatalf("signUnpushed = %d, %v; want the task commit signed", n, err)
	}
	head := gitOut(repo, "rev-parse", "HEAD")
	if head == reopen.BaselineHead || !hasSignature(repo, head) {
		t.Fatalf("signing did not replace the task commit %s", reopen.BaselineHead)
	}
	got, ok, err := tasks.ReadAuditReopenRecord(root, id)
	if err != nil || !ok || got.Generation != reopen.Generation || got.BaselineHead != head ||
		!tasks.AuditReopenCurrentValid(repo, head, id, got) {
		t.Fatalf("authority after signing = %+v, ok=%v, err=%v", got, ok, err)
	}
	if cohort, err := tasks.LoadPendingReviews(repo, hosts); err != nil || len(cohort.Subjects) != 1 {
		t.Fatalf("pending review after signing = %+v, %v", cohort, err)
	}
}

// A signing pass never rewrites a commit the branch's upstream already has. Pushed signed commits
// below the first unsigned one are kept; an unsigned commit below a pushed one stops the pass with
// a refusal instead, because signing it would make the branch diverge from what was pushed.
func TestSignUnpushedNeverRewritesPushedCommits(t *testing.T) {
	repo, git := signingRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	git("commit", "-q", "--allow-empty", "-S", "-m", "signed, pushed mid-run")
	git("push", "-q", "-u", "origin", "main")
	pushed := gitOut(repo, "rev-parse", "HEAD")
	git("commit", "-q", "--allow-empty", "-m", "box commit")

	// A loop-style pass from a base below the upstream signs only the unpushed commit.
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	if n, err := a.signUnpushed(repo, base); err != nil || n != 1 {
		t.Fatalf("signUnpushed = %d, %v; want only the unpushed commit signed", n, err)
	}
	if got := gitOut(repo, "rev-parse", "HEAD~1"); got != pushed {
		t.Fatalf("signing rewrote pushed commit %s into %s", pushed, got)
	}
	if !hasSignature(repo, "HEAD") {
		t.Error("the unpushed box commit should carry a signature")
	}

	// An unsigned commit sits below a pushed one: nothing is rewritten, and the refusal says why.
	git("commit", "-q", "--allow-empty", "-m", "unsigned, then pushed")
	unsignedPushed := gitOut(repo, "rev-parse", "HEAD")
	git("commit", "-q", "--allow-empty", "-S", "-m", "signed and pushed")
	git("push", "-q", "origin", "main")
	upstream := gitOut(repo, "rev-parse", "origin/main")
	git("commit", "-q", "--allow-empty", "-m", "unpushed box commit")
	head := gitOut(repo, "rev-parse", "HEAD")
	_, err := a.signUnpushed(repo, base)
	if err == nil || !strings.Contains(err.Error(), "already pushed to origin/main") {
		t.Fatalf("signing across an unsigned pushed commit = %v; want a refusal naming the upstream", err)
	}
	if !strings.Contains(err.Error(), gitOut(repo, "rev-parse", "--short", unsignedPushed)) {
		t.Errorf("the refusal should name the unsigned pushed commit %s: %v", unsignedPushed, err)
	}
	if got := gitOut(repo, "rev-parse", "HEAD"); got != head {
		t.Errorf("a refused pass moved HEAD from %s to %s", head, got)
	}
	if got := gitOut(repo, "rev-parse", "origin/main"); got != upstream {
		t.Errorf("a refused pass moved the upstream from %s to %s", upstream, got)
	}
	if worktrees := strings.Count(gitOut(repo, "worktree", "list", "--porcelain"), "worktree "); worktrees != 1 {
		t.Errorf("a refused pass left %d registered worktrees, want 1", worktrees)
	}

	// The refusal's advice works: coop sign's default range starts at the upstream, so it signs
	// what is not pushed and leaves the pushed history alone.
	from, err := signBase(repo, "")
	if err != nil {
		t.Fatalf("signBase: %v", err)
	}
	if n, err := a.signUnpushed(repo, from); err != nil || n != 1 {
		t.Fatalf("coop sign's default range = %d, %v; want the one unpushed commit", n, err)
	}
	if got := gitOut(repo, "rev-parse", "HEAD~1"); got != upstream {
		t.Errorf("coop sign rewrote pushed commit %s into %s", upstream, got)
	}
	if got := gitOut(repo, "rev-parse", "HEAD~2"); got != unsignedPushed {
		t.Errorf("coop sign rewrote pushed commit %s into %s", unsignedPushed, got)
	}
	if !hasSignature(repo, "HEAD") {
		t.Error("the unpushed box commit should carry a signature")
	}

	// A push made while the rebase ran is caught too: the upstream is read again under the
	// ref-authority lock, just before the branch would move, so the pass refuses and moves nothing.
	git("commit", "-q", "--allow-empty", "-m", "box commit pushed while signing")
	head = gitOut(repo, "rev-parse", "HEAD")
	racing := &app{cfg: a.cfg, beforeSignRefUpdate: func(string, string, string, string) {
		git("push", "-q", "origin", "main")
	}}
	if _, err := racing.signUnpushed(repo, from); err == nil || !strings.Contains(err.Error(), "already pushed to origin/main") {
		t.Fatalf("signing across a push made mid-pass = %v; want the pushed-history refusal", err)
	}
	if got := gitOut(repo, "rev-parse", "origin/main"); got != head {
		t.Fatalf("the hook's push did not land: origin/main = %s, want %s", got, head)
	}
	if got := gitOut(repo, "rev-parse", "HEAD"); got != head || hasSignature(repo, "HEAD") {
		t.Errorf("a pass refused under the lock rewrote the pushed commit %s into %s", head, got)
	}
	if worktrees := strings.Count(gitOut(repo, "worktree", "list", "--porcelain"), "worktree "); worktrees != 1 {
		t.Errorf("a pass refused under the lock left %d registered worktrees, want 1", worktrees)
	}
}

// A box-exit warning carries the signer's own cause. Only the signing rebase failing adds the advice
// to fix the Git signing setup: a refusal, structured or plain, has nothing to do with that setup.
func TestSignOnBoxExitWarningFitsTheCause(t *testing.T) {
	repo, git := signingRepo(t)
	git("config", "--global", "commit.gpgsign", "true") // you sign by default; the repo's own commits stay unsigned
	git("commit", "-q", "--allow-empty", "-m", "base")
	preHead := gitOut(repo, "rev-parse", "HEAD")
	git("commit", "-q", "--allow-empty", "-m", "box commit, pushed before the session ended")
	git("push", "-q", "-u", "origin", "main")
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	if warning := captureStderr(t, func() { a.signOnBoxExit(repo, preHead, false) }); !strings.Contains(warning, "already pushed to origin/main") ||
		strings.Contains(warning, "signing setup") {
		t.Errorf("pushed-history warning = %q; want the refusal with only its own next step", warning)
	}
	unrelated := gitOut(repo, "commit-tree", gitOut(repo, "rev-parse", "HEAD^{tree}"), "-m", "unrelated history")
	if warning := captureStderr(t, func() { a.signOnBoxExit(repo, unrelated, false) }); !strings.Contains(warning, "it has no common base with") ||
		strings.Contains(warning, "signing setup") {
		t.Errorf("unrelated-history warning = %q; want the refusal's own message and no setup advice", warning)
	}

	preHead = gitOut(repo, "rev-parse", "HEAD")
	git("commit", "-q", "--allow-empty", "-m", "unpushed box commit")
	git("config", "--global", "user.signingkey", filepath.Join(t.TempDir(), "missing-key"))
	if warning := captureStderr(t, func() { a.signOnBoxExit(repo, preHead, false) }); !strings.Contains(warning, "Run coop sign after fixing your Git signing setup.") {
		t.Errorf("key-failure warning = %q; want the signing-setup advice", warning)
	}
}

// When nothing needs a signature, coop sign says whether the range was empty or already signed.
func TestCmdSignTellsAnEmptyRangeFromASignedOne(t *testing.T) {
	repo, git := signingRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	git("push", "-q", "-u", "origin", "main")
	base := gitOut(repo, "rev-parse", "HEAD")
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir(), RepoOverride: repo}}
	sign := func(args ...string) string {
		return captureStderr(t, func() {
			if code, err := a.cmdSign(args); code != 0 || err != nil {
				t.Errorf("coop sign %v = %d, %v", args, code, err)
			}
		})
	}
	if out := sign(); !strings.Contains(out, "No unpushed commits to sign.") {
		t.Errorf("coop sign with nothing unpushed printed %q", out)
	}
	git("commit", "-q", "--allow-empty", "-S", "-m", "signed, not pushed")
	if out := sign(); !strings.Contains(out, "Every unpushed commit is already signed.") {
		t.Errorf("coop sign over a signed unpushed commit printed %q", out)
	}
	if out := sign("--from", base); !strings.Contains(out, "Every commit after "+base+" is already signed.") {
		t.Errorf("coop sign --from over a signed range printed %q", out)
	}
	if out := sign("--from", "HEAD"); !strings.Contains(out, "No commits after HEAD to sign.") {
		t.Errorf("coop sign --from HEAD printed %q", out)
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
	t.Chdir(repo)

	a := &app{cfg: &config.Config{}}
	if _, err := a.signUnpushed(repo, base); err == nil {
		t.Fatal("signing without a trusted global key unexpectedly succeeded")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repository-local SSH defaultKeyCommand executed: %v", err)
	}
}
