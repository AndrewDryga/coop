package scaffold

import (
	"os"
	"path/filepath"
	"strings"
)

// GateLangs is the ordered set of stacks coop can scaffold a commit format gate for. It's
// the menu the interactive prompt offers and what DetectStacks chooses from.
var GateLangs = []string{"go", "terraform", "elixir", "rust"}

// knownStacks pairs each gate language with the signals that detect it: marker files in the
// repo and tool names in .tool-versions. Terraform is matched by any *.tf file too (handled
// in stackPresent), since it has no single marker file.
var knownStacks = []struct {
	lang    string
	markers []string // files at the repo root that imply this stack
	tools   []string // .tool-versions tool names (lowercased)
}{
	{"go", []string{"go.mod"}, []string{"go", "golang"}},
	{"terraform", nil, []string{"terraform", "opentofu", "tofu"}},
	{"elixir", []string{"mix.exs"}, []string{"elixir"}},
	{"rust", []string{"Cargo.toml"}, []string{"rust"}},
}

// DetectStacks reports which gate languages the repo uses, by marker file, *.tf presence, or
// .tool-versions — in GateLangs order, so the generated gate is deterministic.
func DetectStacks(repo string) []string {
	tools := toolVersions(repo)
	var out []string
	for _, st := range knownStacks {
		present := false
		for _, m := range st.markers {
			if _, err := os.Stat(filepath.Join(repo, m)); err == nil {
				present = true
			}
		}
		for _, t := range st.tools {
			if tools[t] {
				present = true
			}
		}
		if st.lang == "terraform" && hasTerraformFiles(repo) {
			present = true
		}
		if present {
			out = append(out, st.lang)
		}
	}
	return out
}

// toolVersions returns the set of tool names declared in the repo's .tool-versions (the
// first field of each non-comment line, lowercased). Missing file → empty set.
func toolVersions(repo string) map[string]bool {
	out := map[string]bool{}
	data, err := os.ReadFile(filepath.Join(repo, ".tool-versions"))
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && !strings.HasPrefix(f[0], "#") {
			out[strings.ToLower(f[0])] = true
		}
	}
	return out
}

// hasTerraformFiles reports whether the repo has a *.tf at its root or one level down (a
// cheap check; a deeper layout is covered by the .tool-versions terraform signal).
func hasTerraformFiles(repo string) bool {
	for _, pat := range []string{"*.tf", filepath.Join("*", "*.tf")} {
		if m, _ := filepath.Glob(filepath.Join(repo, pat)); len(m) > 0 {
			return true
		}
	}
	return false
}

// gateSnippets is the per-language format check, command-v guarded so it runs in the box
// (toolchain provisioned) and skips on a host that lacks the tool. @EXIT@ is the exit code
// to block with (1 for the git hook, 2 for the Claude hook). Go and Terraform are list-based
// so a tool error fails open (only a real diff blocks).
var gateSnippets = map[string]string{
	"go": `# Go: block the commit if any staged .go file isn't gofmt-clean.
go_files=$(echo "$staged" | grep '\.go$' || true)
if [ -n "$go_files" ] && command -v gofmt >/dev/null 2>&1; then
  # shellcheck disable=SC2086  # intentional: split the file list into separate gofmt args
  bad=$(gofmt -l $go_files 2>/dev/null || true)
  if [ -n "$bad" ]; then
    echo "pre-commit blocked — these need gofmt:" >&2; echo "  ${bad//$'\n'/$'\n'  }" >&2
    echo "fix: gofmt -w <files>   (skip once: git commit --no-verify)" >&2; exit @EXIT@
  fi
fi`,
	"terraform": `# Terraform: block the commit if any staged .tf file isn't terraform-fmt clean.
tf_files=$(echo "$staged" | grep '\.tf$' || true)
if [ -n "$tf_files" ] && command -v terraform >/dev/null 2>&1; then
  bad=""
  for f in $tf_files; do [ -n "$(terraform fmt -check -list=true "$f" 2>/dev/null)" ] && bad="$bad $f"; done
  if [ -n "$bad" ]; then
    echo "pre-commit blocked — these need terraform fmt:$bad" >&2
    echo "fix: terraform fmt <files>   (skip once: git commit --no-verify)" >&2; exit @EXIT@
  fi
fi`,
	"elixir": `# Elixir: block the commit if any staged .ex/.exs file isn't mix-format clean.
ex_files=$(echo "$staged" | grep -E '\.exs?$' || true)
if [ -n "$ex_files" ] && command -v mix >/dev/null 2>&1; then
  # shellcheck disable=SC2086  # intentional: split the file list into separate mix-format args
  if ! mix format --check-formatted $ex_files >/dev/null 2>&1; then
    echo "pre-commit blocked — these need mix format:" >&2; echo "  ${ex_files//$'\n'/$'\n'  }" >&2
    echo "fix: mix format   (skip once: git commit --no-verify)" >&2; exit @EXIT@
  fi
fi`,
	"rust": `# Rust: block the commit if staged .rs files aren't rustfmt-clean.
rs_files=$(echo "$staged" | grep '\.rs$' || true)
if [ -n "$rs_files" ] && command -v cargo >/dev/null 2>&1; then
  if ! cargo fmt --check >/dev/null 2>&1; then
    echo "pre-commit blocked — run rustfmt on the staged Rust files" >&2
    echo "fix: cargo fmt   (skip once: git commit --no-verify)" >&2; exit @EXIT@
  fi
fi`,
}

// neutralNote is the body of a gate when no stack is detected: intentionally inert, with
// commented examples, so coop never imposes checks a repo doesn't use.
const neutralNote = `# No language was auto-detected, so this gate is intentionally empty — coop won't impose
# checks your repo doesn't use. Add fast checks below (re-run 'coop init' after adding a
# go.mod / *.tf / mix.exs / Cargo.toml, or a .tool-versions, to get them generated):
#   go_files=$(echo "$staged" | grep '\.go$');  [ -n "$go_files" ] && gofmt -l $go_files
#   tf_files=$(echo "$staged" | grep '\.tf$');  [ -n "$tf_files" ] && terraform fmt -check $tf_files
: "${staged:-}"  # no checks yet — reference $staged so the empty gate stays shellcheck-clean`

func gateSnippet(lang, exitCode string) string {
	return strings.ReplaceAll(gateSnippets[lang], "@EXIT@", exitCode)
}

// gateBody renders the detected languages' checks (or the neutral note) joined for a hook,
// with each block exiting via exitCode on failure.
func gateBody(langs []string, exitCode string) string {
	if len(langs) == 0 {
		return neutralNote
	}
	var blocks []string
	for _, l := range langs {
		if s := gateSnippet(l, exitCode); s != "" {
			blocks = append(blocks, s)
		}
	}
	if len(blocks) == 0 {
		return neutralNote
	}
	return strings.Join(blocks, "\n\n")
}

// preCommitHook is the .githooks/pre-commit gate (every committer; a git hook blocks on a
// nonzero exit, so failures exit 1).
func preCommitHook(langs []string) string {
	return `#!/bin/bash
# coop pre-commit gate — runs for every committer (agent or human) via
# core.hooksPath=.githooks, so Codex/Gemini and a plain git commit can't bypass the format
# check the way they bypass Claude-only hooks. Fast by design. Skip once: git commit --no-verify.
set -f          # the file lists below are word-split on purpose; don't also glob-expand a name
IFS=$'\n'       # …and split only on newlines, so a staged filename with a space stays one path
staged=$(git diff --cached --name-only --diff-filter=ACM 2>/dev/null) || exit 0

` + gateBody(langs, "1") + "\n\nexit 0\n"
}

// prepareCommitMsgChainHook lets a repo-local hooksPath retain the box's co-author hook. On the
// host that hook is absent, so normal commits remain unchanged.
const prepareCommitMsgChainHook = `#!/bin/sh
hook="$HOME/.config/coop/git-hooks/prepare-commit-msg"
[ -x "$hook" ] || exit 0
exec "$hook" "$@"
`

// claudeCommitGate is the .claude/hooks/commit-gate.sh gate (Claude only; a Claude hook
// blocks the tool call on exit 2). Reads the tool call on stdin and acts only on git commit.
func claudeCommitGate(langs []string) string {
	return `#!/bin/bash
# Fast commit gate: format staged files, block the commit if they're dirty.
# Reads the tool call on stdin; only acts on git commit. Fails open.
set -f          # the file lists below are word-split on purpose; don't also glob-expand a name
IFS=$'\n'       # …and split only on newlines, so a staged filename with a space stays one path
input=$(cat)
echo "$input" | grep -q '"command"[^}]*git commit' || exit 0
staged=$(git diff --cached --name-only --diff-filter=ACM 2>/dev/null) || exit 0

` + gateBody(langs, "2") + "\n\nexit 0\n"
}
