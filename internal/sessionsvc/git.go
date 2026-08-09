package sessionsvc

import (
	"os/exec"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

// gitArgs prepends the ONE hardening list in the tree (forkspace.GitHardening) to every git
// command this package runs, because a session's workspace is agent-writable: hooks, replacement
// objects, and any config knob that shells out are turned off before git reads the repo.
func gitArgs(dir string, args []string) []string {
	return append(append([]string{"-C", dir}, forkspace.GitHardening...), args...)
}

// gitRun runs `git -C dir <args>` hardened, for effect, returning its error.
func gitRun(dir string, args ...string) error {
	return exec.Command("git", gitArgs(dir, args)...).Run()
}
