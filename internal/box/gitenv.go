package box

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/config"
)

// prepareCommitMsgHook stamps the two trailers a box commit must carry, from git config values the
// box gitconfig sets (so the script itself is static): coop.trailer replaces the agent CLI's machine
// co-author line, so the commit is attributed to coop and the exact target whichever agent made it;
// coop.task stamps the assigned loop task's Coop-Task binding.
//
// The task stamp exists because a loop iteration's completion is REJECTED outright when its commit
// carries no parseable Coop-Task trailer: an agent that simply forgets one loses the whole
// iteration, and the trailer-less commit is then invisible to the informed-resume hint, leaving the
// retry on the blind path. Remembering should not be the agent's job.
//
// The two are INDEPENDENT — neither may gate the other, or a run that sets only one silently loses
// it. Both use addIfDifferent, so an agent that wrote the correct trailer itself gets no duplicate
// and --amend stays idempotent (a duplicate binding is itself a rejection cause). Runs even under
// `git commit --no-verify` (that skips commit-msg/pre-commit, NOT prepare-commit-msg), and leaves
// merge/squash messages and any HUMAN Co-authored-by line untouched.
const prepareCommitMsgHook = `#!/bin/sh
case "$2" in
	merge|squash) exit 0 ;;
esac
f="$1"
trailer=$(git config coop.trailer 2>/dev/null || true)
if [ -n "$trailer" ]; then
	# Drop machine co-author lines — the agent CLIs' and any prior coop one (so --amend stays
	# idempotent) — keyed off the vendor name or noreply domain on the Co-authored-by line; a human
	# co-author matches none of these and survives.
	tmp="$f.coop.$$"
	grep -viE '^Co-authored-by:.*(claude|chatgpt|codex|gemini|grok|coop|noreply@(anthropic|openai|google|x\.ai|coop))' "$f" > "$tmp" && mv "$tmp" "$f"
	git interpret-trailers --if-exists addIfDifferent --trailer "Co-authored-by: $trailer" --in-place "$f"
fi
task=$(git config coop.task 2>/dev/null || true)
if [ -n "$task" ]; then
	git interpret-trailers --if-exists addIfDifferent --trailer "Coop-Task: $task" --in-place "$f"
fi
`

// The box has no ambient ~/.gitconfig of its own, so without this an agent would
// commit with no author and ignore none of the user's global gitignore patterns. We
// mount a curated global config + the user's global gitignore into every Homes run.
const (
	boxGitHooksName  = ".coop-git-hooks"
	boxGitIgnoreName = ".coop-gitignore"
)

// hostGitGlobal reads a value from the host's GLOBAL git config (~/.gitconfig), or
// "" if unset or git is unavailable.
func hostGitGlobal(args ...string) string {
	out, err := exec.Command("git", append([]string{"config", "--global"}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// buildGitConfig renders a curated ~/.gitconfig for the box: the given identity
// (omitted when empty) plus signing turned off — the box holds no GPG/SSH key, so a
// global commit.gpgsign=true would make every agent commit fail.
func buildGitConfig(name, email string) string {
	var b strings.Builder
	if name != "" || email != "" {
		b.WriteString("[user]\n")
		if name != "" {
			b.WriteString("\tname = " + name + "\n")
		}
		if email != "" {
			b.WriteString("\temail = " + email + "\n")
		}
	}
	b.WriteString("[commit]\n\tgpgsign = false\n[tag]\n\tgpgsign = false\n")
	return b.String()
}

// gitConfigForBox is buildGitConfig with the host user's global identity, plus the optional coop
// co-author hook, global ignore file, and trailer used inside the box.
func gitConfigForBox(coAuthor, hooksPath, excludesPath, assignedTask string) string {
	var b strings.Builder
	b.WriteString(buildGitConfig(hostGitGlobal("user.name"), hostGitGlobal("user.email")))
	if hooksPath != "" || excludesPath != "" {
		b.WriteString("[core]\n")
		if hooksPath != "" {
			b.WriteString("\thooksPath = " + hooksPath + "\n")
		}
		if excludesPath != "" {
			b.WriteString("\texcludesFile = " + excludesPath + "\n")
		}
	}
	if coAuthor != "" || assignedTask != "" {
		b.WriteString("[coop]\n")
		if coAuthor != "" {
			b.WriteString("\ttrailer = " + coAuthor + "\n")
		}
		if assignedTask != "" {
			b.WriteString("\ttask = " + assignedTask + "\n")
		}
	}
	return b.String()
}

// boxCommitTrailer is the coop co-author line for a box's commits — attributing coop and the exact
// target that ran (provider:model@account). Empty for a raw/maintenance run (no agent session, no
// attributed commits). The committing agent is the run's lead.
func boxCommitTrailer(cfg *config.Config, spec RunSpec) string {
	agent := runPrimary(spec)
	if agent == "" {
		return ""
	}
	desc := agent
	if m := cfg.ModelFor(agent); m != "" {
		desc += ":" + m
	}
	if p := cfg.ActiveProfile(agent); p != "" {
		desc += "@" + p
	}
	return "coop (" + desc + ") <noreply@coop.dev>"
}

// gitHookDir writes the coop prepare-commit-msg hook (executable) into a fresh temp dir, for
// mounting as the box's core.hooksPath. The caller cleans it up (tmpDirs).
func gitHookDir() (string, error) {
	dir, err := os.MkdirTemp("", "coop-githooks-")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "prepare-commit-msg"), []byte(prepareCommitMsgHook), 0o755); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// hostGlobalGitignore returns the content of the user's global gitignore
// (core.excludesfile, with ~ expanded), or "" if there is none.
func hostGlobalGitignore() string {
	path := hostGitGlobal("--path", "core.excludesfile")
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}
