package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

// gitRepo runs git in a fresh temp repo with an isolated global/system config, returning the repo
// path and a runner. Callers add signing config as needed.
func gitRepo(t *testing.T) (string, func(...string)) {
	t.Helper()
	repo := t.TempDir()
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "noglobal"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(t.TempDir(), "nosystem"))
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = repo, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "T")
	return repo, run
}

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

	if got, err := signRangeBase(repo, iterHead); err != nil || got != iterHead {
		t.Fatalf("descendant base = %q, %v; want %q", got, err, iterHead)
	}
	git("commit", "--amend", "-q", "--allow-empty", "-m", "review amendment")
	if got, err := signRangeBase(repo, iterHead); err != nil || got != base {
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
	if _, err := signRangeBase(repo, leftMerge); err == nil || !strings.Contains(err.Error(), "multiple common bases") {
		t.Fatalf("ambiguous history error = %v; want clear multiple-common-bases failure", err)
	}

	git("checkout", "--orphan", "unrelated")
	git("commit", "-q", "--allow-empty", "-m", "unrelated")
	if _, err := signRangeBase(repo, iterHead); err == nil || !strings.Contains(err.Error(), "no common base") {
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
	runIn("commit", "-q", "--allow-empty", "-m", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	runIn("commit", "-q", "--allow-empty", "-m", "c1")
	runIn("commit", "-q", "--allow-empty", "-m", "c2")

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
	// Idempotent: a second run re-signs cleanly and they stay signed.
	if _, err := a.signUnpushed(repo, base); err != nil {
		t.Fatalf("second signUnpushed: %v", err)
	}
	if signed() != 2 {
		t.Errorf("after a second sign, both should still be signed, got %d", signed())
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
}
