// Package gitrepo makes the hermetic temp git repository that coop's git-touching tests run
// against. It is test infrastructure only.
//
// Hermetic means the repo answers to nothing outside itself: GIT_CONFIG_GLOBAL and
// GIT_CONFIG_SYSTEM are pinned at empty files, so the developer's own config — a signing key
// that pops pinentry, a commit template, an alias, a hooksPath — can never reach the commands
// the test runs, and the test behaves the same on a laptop and in CI.
package gitrepo

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// New returns a fresh initialized repository and a `git` runner bound to it that fails the test
// on a non-zero exit, so a test's setup commands read as the git commands they are.
func New(t *testing.T) (string, func(...string)) {
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
	// The test deliberately ignores the developer's global config, including init.defaultBranch.
	// Pin HEAD explicitly so a distro whose compiled default is master creates the same fixture as
	// a laptop configured for main. symbolic-ref works on older supported Git versions too; unlike
	// `git init -b`, it does not raise the fixture's Git-version floor.
	run("symbolic-ref", "HEAD", "refs/heads/main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "T")
	return repo, run
}
