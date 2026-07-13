package box

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/config"
)

// prepareCommitMsgHook replaces the agent CLI's machine co-author line with coop's own, so a box
// commit is attributed to coop (and the exact target) whichever agent made it. It runs even under
// `git commit --no-verify` (that skips commit-msg/pre-commit, NOT prepare-commit-msg), and leaves
// merge/squash messages and any HUMAN Co-authored-by line untouched. The trailer value comes from
// git config coop.trailer (set in the box gitconfig), so the script itself is static.
const prepareCommitMsgHook = `#!/bin/sh
case "$2" in
	merge|squash) exit 0 ;;
esac
trailer=$(git config coop.trailer 2>/dev/null) || exit 0
[ -n "$trailer" ] || exit 0
f="$1"
# Drop machine co-author lines — the agent CLIs' and any prior coop one (so --amend stays idempotent)
# — keyed off the vendor name or noreply domain on the Co-authored-by line; a human co-author matches
# none of these and survives.
tmp="$f.coop.$$"
grep -viE '^Co-authored-by:.*(claude|chatgpt|codex|gemini|grok|coop|noreply@(anthropic|openai|google|x\.ai|coop))' "$f" > "$tmp" && mv "$tmp" "$f"
# Append coop's line with correct trailer placement; addIfDifferent keeps it to one.
git interpret-trailers --if-exists addIfDifferent --trailer "Co-authored-by: $trailer" --in-place "$f"
`

// The box has no ambient ~/.gitconfig of its own, so without this an agent would
// commit with no author and ignore none of the user's global gitignore patterns. We
// mount a curated global config + the user's global gitignore into every Homes run.

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

// gitConfigForBox is buildGitConfig with the host user's global identity, plus (when set) the coop
// co-author trailer and the hooksPath that points git at the prepare-commit-msg hook applying it.
func gitConfigForBox(coAuthor, hooksPath string) string {
	var b strings.Builder
	b.WriteString(buildGitConfig(hostGitGlobal("user.name"), hostGitGlobal("user.email")))
	if hooksPath != "" {
		b.WriteString("[core]\n\thooksPath = " + hooksPath + "\n")
	}
	if coAuthor != "" {
		b.WriteString("[coop]\n\ttrailer = " + coAuthor + "\n")
	}
	return b.String()
}

// boxCommitTrailer is the coop co-author line for a box's commits — attributing coop and the exact
// target that ran (provider:model@account). Empty for a raw/maintenance run (no agent session, no
// attributed commits). The committing agent is the fusion governor when set, else the launched one.
func boxCommitTrailer(cfg *config.Config, spec RunSpec) string {
	agent := acpPrimary(spec)
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
