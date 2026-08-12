package forkspace

import (
	"context"
	"errors"
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
// TrustedSignArgs).
//
// A value coop reads then EXECUTES (or reads a host file from) — your editor, signing program,
// global excludesfile — must not come from the agent-writable repo at all: those use gitGlobalOut
// to read your trusted global config, never these helpers.
//
// The one residual GitHardening alone can't blank (the driver names are arbitrary) — an in-tree
// .gitattributes plus a fork-local filter/merge/diff driver that runs on the land rebase's
// checkout — is closed by DriverNeutralizer, which enumerates the fork's driver names and
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
	return GitCloneContext(context.Background(), src, dst)
}

func GitCloneContext(ctx context.Context, src, dst string) error {
	args := append(append([]string{}, GitHardening...), "clone", "--quiet", src, dst)
	err := exec.CommandContext(ctx, "git", args...).Run()
	if ctx.Err() != nil {
		return errors.Join(ctx.Err(), err)
	}
	return err
}

func gitCheckoutNewBranchContext(ctx context.Context, repo, branch string) error {
	err := exec.CommandContext(ctx, "git", "-C", repo, "checkout", "--quiet", "-b", branch).Run()
	if ctx.Err() != nil {
		return errors.Join(ctx.Err(), err)
	}
	return err
}

// gitArgs builds `git -C dir <hardening> <args>`, the same shape internal/cli uses.
func gitArgs(dir string, args []string) []string {
	return append(append([]string{"-C", dir}, GitHardening...), args...)
}

// gitOut runs `git -C dir <args>` hardened and returns trimmed stdout, or "" on error.
func gitOut(dir string, args ...string) string {
	return gitOutContext(context.Background(), dir, args...)
}

func gitOutContext(ctx context.Context, dir string, args ...string) string {
	out, err := exec.CommandContext(ctx, "git", gitArgs(dir, args)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitRun runs `git -C dir <args>` hardened, for effect, returning its error.
func gitRun(dir string, args ...string) error {
	return gitRunContext(context.Background(), dir, args...)
}

func gitRunContext(ctx context.Context, dir string, args ...string) error {
	return exec.CommandContext(ctx, "git", gitArgs(dir, args)...).Run()
}

// gitGlobalOut reads from the host user's GLOBAL git config (`git config --global …`) — the
// trusted scope an agent can't write — for any value coop reads then EXECUTES or reads a host file
// from. The repo's own .git/config is agent-writable, so reading these from it would let a poisoned
// repo redirect coop to run or exfiltrate whatever it names. Returns "" when unset or git is
// unavailable.
func gitGlobalOut(args ...string) string {
	return gitGlobalOutContext(context.Background(), args...)
}

func gitGlobalOutContext(ctx context.Context, args ...string) string {
	out, err := exec.CommandContext(ctx, "git", append([]string{"config", "--global"}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// WantsSigning reports whether you sign commits (commit.gpgsign=true in your git
// config), so a fork's unsigned box commits can be signed with your key on land.
func WantsSigning() bool {
	// Read from your GLOBAL config, never the agent-writable repo: a poisoned repo could otherwise
	// force signing on so its planted gpg.program runs — and your signing preference is global anyway.
	return gitGlobalOut("--bool", "--get", "commit.gpgsign") == "true"
}

// TrustedSignArgs returns the -c flags to sign the rebased commits with the host's key, every
// value read from your GLOBAL git config so neither the fork NOR the agent-writable parent repo can
// point gpg.program at a planted binary. They are appended after GitHardening — which turns signing
// off by default — so these re-enable it with vetted values. The program key tracks gpg.format
// (openpgp/ssh/x509).
func TrustedSignArgs() []string {
	// Blank identity selection first so an agent-writable local config cannot choose a key or
	// execute gpg.ssh.defaultKeyCommand. Trusted global values, when present, are appended last.
	args := []string{
		"-c", "commit.gpgsign=true",
		"-c", "user.signingkey=",
		"-c", "gpg.ssh.defaultKeyCommand=",
	}
	format := gitGlobalOut("--get", "gpg.format")
	progKey, def := "gpg.program", "gpg"
	switch format {
	case "ssh":
		progKey, def = "gpg.ssh.program", "ssh-keygen"
	case "x509":
		progKey, def = "gpg.x509.program", "gpgsm"
	}
	if format != "" {
		args = append(args, "-c", "gpg.format="+format)
	}
	prog := gitGlobalOut("--get", progKey)
	if prog == "" {
		prog = def // git's built-in default — set explicitly so the hardening's "=false" loses
	}
	args = append(args, "-c", progKey+"="+prog)
	if key := gitGlobalOut("--get", "user.signingkey"); key != "" {
		args = append(args, "-c", "user.signingkey="+key)
	} else if format == "ssh" {
		if command := gitGlobalOut("--get", "gpg.ssh.defaultKeyCommand"); command != "" {
			args = append(args, "-c", "gpg.ssh.defaultKeyCommand="+command)
		}
	}
	return args
}

// DriverNeutralizer returns -c flags that blank every filter/merge/diff driver defined in dir's OWN
// (local) git config, by name. GitHardening can't cover these — the driver names are arbitrary —
// but they're enumerable: an in-tree .gitattributes assigning `filter=x` (or merge/diff) to a path
// plus a repo-local filter.x.smudge / merge.x.driver / diff.x.command runs host code on the
// checkout/merge/diff of the land rebase. We read the repo's local driver names and blank each
// (filter.required=false so a blanked smudge doesn't hard-fail the checkout). A legit clone has no
// local filter/merge/diff config — those live in your global — so this blanks only what the agent
// planted; policyScan stays the human-facing backstop for the committed .gitattributes.
func DriverNeutralizer(dir string) []string {
	keys := gitOut(dir, "config", "--local", "--name-only", "--get-regexp", `^(filter|merge|diff)\.`)
	if keys == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, key := range strings.Split(keys, "\n") {
		var typ string
		for _, t := range []string{"filter", "merge", "diff"} {
			if strings.HasPrefix(key, t+".") {
				typ = t
				break
			}
		}
		if typ == "" {
			continue
		}
		rest := key[len(typ)+1:] // "<name>.<leaf>"
		dot := strings.LastIndex(rest, ".")
		if dot <= 0 {
			continue // a 2-part key (e.g. diff.external) has no <name> driver to neutralize
		}
		name := rest[:dot]
		if id := typ + "\x00" + name; !seen[id] {
			seen[id] = true
			switch typ {
			case "filter":
				out = append(out, "-c", "filter."+name+".smudge=", "-c", "filter."+name+".clean=",
					"-c", "filter."+name+".process=", "-c", "filter."+name+".required=false")
			case "merge":
				out = append(out, "-c", "merge."+name+".driver=")
			case "diff":
				out = append(out, "-c", "diff."+name+".command=", "-c", "diff."+name+".textconv=")
			}
		}
	}
	return out
}
