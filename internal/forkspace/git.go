package forkspace

import (
	"os/exec"
	"strings"
)

// GitHardening are -c overrides applied to EVERY git command coop runs for effect on a working
// tree, because every repo coop touches is agent-writable: the box binds the repo (its .git
// included) read-write on a normal run, so an agent can plant hooks, replacement objects, or local
// config that changes what the host executes or considers to be Git history. We ignore replacement
// objects, turn hooks off, and blank every config knob that shells out. Verified host-exec vectors:
// .git/hooks/* (and core.hooksPath), core.fsmonitor, core.pager, diff.external, and a forced
// commit.gpgsign with a planted gpg.program; the rest are defense in depth. Signing on land is
// re-enabled with trusted values appended after these (git's last -c for a key wins; see
// trustedSignArgs).
//
// A value coop reads then EXECUTES (or reads a host file from) — your editor, signing program,
// global excludesfile — must not come from the agent-writable repo at all: those use gitGlobalOut
// to read your trusted global config, never these helpers.
//
// The one residual GitHardening alone can't blank (the driver names are arbitrary) — an in-tree
// .gitattributes plus a fork-local filter/merge/diff driver that runs on the land rebase's
// checkout — is closed by forkDriverNeutralizer, which enumerates the fork's driver names and
// blanks each before that rebase. policyScan stays the human-facing backstop for the .gitattributes.
//
// It lives here, with the clone that creates a fork, because this leaf is the lowest thing in the
// tree that runs git — internal/cli's own helpers build on this ONE list, so there is exactly one
// hardening set in the repo to audit.
var GitHardening = []string{
	"--no-replace-objects",
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=",
	"-c", "core.sshCommand=",
	"-c", "core.pager=cat",
	"-c", "core.editor=true",
	"-c", "sequence.editor=true",
	"-c", "diff.external=",
	"-c", "uploadpack.packObjectsHook=",
	"-c", "protocol.ext.allow=never",
	"-c", "rebase.updateRefs=false",
	"-c", "commit.gpgsign=false",
	"-c", "gpg.program=false",
	"-c", "gpg.ssh.program=false",
	"-c", "gpg.x509.program=false",
}

// GitClone clones src→dst carrying GitHardening like every other repo-touching git call:
// inert for a plain local clone, but defense in depth if the source's config was poisoned.
func GitClone(src, dst string) error {
	args := append(append([]string{}, GitHardening...), "clone", "--quiet", src, dst)
	return exec.Command("git", args...).Run()
}

func gitCheckoutNewBranch(repo, branch string) error {
	return exec.Command("git", "-C", repo, "checkout", "--quiet", "-b", branch).Run()
}

// gitArgs builds `git -C dir <hardening> <args>`, the same shape internal/cli uses.
func gitArgs(dir string, args []string) []string {
	return append(append([]string{"-C", dir}, GitHardening...), args...)
}

// gitOut runs `git -C dir <args>` hardened and returns trimmed stdout, or "" on error.
func gitOut(dir string, args ...string) string {
	out, err := exec.Command("git", gitArgs(dir, args)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitRun runs `git -C dir <args>` hardened, for effect, returning its error.
func gitRun(dir string, args ...string) error {
	return exec.Command("git", gitArgs(dir, args)...).Run()
}

// gitGlobalOut reads from the host user's GLOBAL git config (`git config --global …`) — the
// trusted scope an agent can't write — for any value coop reads then EXECUTES or reads a host file
// from. The repo's own .git/config is agent-writable, so reading these from it would let a poisoned
// repo redirect coop to run or exfiltrate whatever it names. Returns "" when unset or git is
// unavailable.
func gitGlobalOut(args ...string) string {
	out, err := exec.Command("git", append([]string{"config", "--global"}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
