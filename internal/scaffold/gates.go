package scaffold

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// GateLangs is the ordered set of stacks coop can scaffold a commit format gate for. It's
// the menu the interactive prompt offers and what DetectStacks chooses from.
var GateLangs = []string{"go", "terraform", "elixir", "rust"}

// knownStacks pairs each gate language with the signals that detect it: marker files in the
// repo and tool names in .tool-versions. Terraform is matched by any *.tf file too (handled
// in DetectStacks), since it has no single marker file. Coop's default box carries none of these
// toolchains, so each also names the asdf tools that install it.
var knownStacks = []struct {
	lang    string
	name    string                   // the language as coop init names it
	markers []string                 // files at the repo root that imply this stack
	tools   []string                 // .tool-versions tool names (lowercased)
	pins    []string                 // the asdf tools that install it, in install order
	version func(repo string) string // the version the repository names for its one pin, or ""
}{
	{"go", "Go", []string{"go.mod"}, []string{"go", "golang"}, []string{"golang"}, goVersion},
	{"terraform", "Terraform", nil, []string{"terraform", "opentofu", "tofu"}, []string{"terraform"}, nil},
	{"elixir", "Elixir", []string{"mix.exs"}, []string{"elixir"}, []string{"erlang", "elixir"}, nil},
	{"rust", "Rust", []string{"Cargo.toml"}, []string{"rust"}, []string{"rust"}, nil},
}

// Toolchain is a language the project's box cannot run yet, with the .tool-versions lines that
// would pin it for the asdf box `coop init --stack asdf` writes.
type Toolchain struct {
	Name string   // "Go"
	Pins []string // "golang 1.26.5", or "<version>" where the repository names none
}

// Toolchains are the toolchains langs need that Coop's default box lacks and .tool-versions does
// not pin, in GateLangs order. On that box agents cannot build those languages, and the commit
// format check skips them.
func Toolchains(repo string, langs []string) []Toolchain {
	// An unreadable .tool-versions pins nothing; init refuses it before getting here.
	pinned, _ := toolVersions(repo)
	var out []Toolchain
	for _, st := range knownStacks {
		if !slices.Contains(langs, st.lang) || slices.ContainsFunc(st.tools, func(t string) bool { return pinned[t] }) {
			continue
		}
		tc := Toolchain{Name: st.name}
		for _, tool := range st.pins {
			version := ""
			if st.version != nil && len(st.pins) == 1 {
				version = st.version(repo)
			}
			if version == "" {
				version = "<version>"
			}
			tc.Pins = append(tc.Pins, tool+" "+version)
		}
		out = append(out, tc)
	}
	return out
}

// goVersion is the Go release go.mod names, the way asdf installs it: its toolchain line, else
// its go line. Go 1.21 and later number every release x.y.z, so a bare "go 1.26" there means
// 1.26.0. A box can write go.mod, so anything but plain digits and dots names nothing.
func goVersion(repo string) string {
	root, err := os.OpenRoot(repo)
	if err != nil {
		return ""
	}
	defer root.Close()
	data, err := readRegular(root, "go.mod", "go.mod")
	if err != nil {
		return ""
	}
	var goLine, toolchain string
	for _, line := range strings.Split(string(data), "\n") {
		switch f := strings.Fields(line); {
		case len(f) == 2 && f[0] == "go":
			goLine = f[1]
		case len(f) == 2 && f[0] == "toolchain":
			toolchain = strings.TrimPrefix(f[1], "go")
		}
	}
	if goRelease.MatchString(toolchain) {
		return toolchain
	}
	if !goRelease.MatchString(goLine) {
		return ""
	}
	if parts := strings.Split(goLine, "."); len(parts) == 2 {
		if minor, err := strconv.Atoi(parts[1]); err == nil && parts[0] == "1" && minor >= 21 {
			return goLine + ".0"
		}
	}
	return goLine
}

// goRelease is a Go release number as go.mod and asdf write it: 1.26 or 1.26.5.
var goRelease = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?$`)

// DetectStacks reports which gate languages the repo uses, by marker file, *.tf presence, or
// .tool-versions — in GateLangs order, so the generated gate is deterministic.
func DetectStacks(repo string) []string {
	// A .tool-versions coop cannot read adds no stack here: CheckStack and Init read it too, and
	// refuse it with a message naming it.
	tools, _ := toolVersions(repo)
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

// ToolVersionsUnreadable is a .tool-versions coop found but cannot read: not a regular file, larger
// than 1 MiB, or refused by the system. That is the file's own problem whichever --stack was asked
// for, so coop init names the file, not the stack.
type ToolVersionsUnreadable struct{ Err error }

func (e ToolVersionsUnreadable) Error() string { return e.Err.Error() }
func (e ToolVersionsUnreadable) Unwrap() error { return e.Err }

// toolVersions returns the set of tool names declared in the repo's .tool-versions (the
// first field of each non-comment line, lowercased). Missing file → empty set. A box can leave a
// FIFO, a device or a link there, so it reads only a regular file of at most 1 MiB, without
// waiting, and refuses anything else with a message naming .tool-versions.
func toolVersions(repo string) (map[string]bool, error) {
	root, err := os.OpenRoot(repo)
	if err != nil {
		return nil, fmt.Errorf("open scaffold root %s: %w", repo, err)
	}
	defer root.Close()
	out := map[string]bool{}
	data, err := readRegular(root, ".tool-versions", ".tool-versions")
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	} else if err != nil {
		return nil, ToolVersionsUnreadable{err}
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && !strings.HasPrefix(f[0], "#") {
			out[strings.ToLower(f[0])] = true
		}
	}
	return out, nil
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

// gateSnippets is the per-language format check, command-v guarded so it runs where the
// toolchain is installed and, where it is not, skips with a warning rather than in silence: a box
// without the toolchain would otherwise pass every commit unchecked. @EXIT@ is the exit code
// to block with (1 for the git hook, 2 for the Claude hook). Every snippet names exactly the
// files that need formatting. gofmt and terraform fmt read no project settings, so when either
// fails (a file it cannot parse, a shim with no version set) nothing was checked and the commit
// stops on the tool's first complaint. mix format and rustfmt also load the project's own setup
// (its formatter config and dependencies, rustfmt.toml, the edition read from Cargo.toml), so a
// failure there is often that setup rather than the staged file: they fail open, with one
// warning naming the tool, the files it could not check and its first complaint, and only a
// real diff blocks.
var gateSnippets = map[string]string{
	"go": `# Go: block the commit if any staged .go file isn't gofmt-clean, or if gofmt can't check them.
go_files=$(echo "$staged" | grep '\.go$' || true)
if [ -n "$go_files" ] && ! command -v gofmt >/dev/null 2>&1; then
  echo "pre-commit: gofmt is not installed here, so the Go format check was skipped" >&2
elif [ -n "$go_files" ]; then
  # shellcheck disable=SC2086  # intentional: split the file list into separate gofmt args
  bad=$(gofmt -l $go_files 2>/dev/null); status=$?
  if [ "$status" -ne 0 ]; then
    # A file gofmt can't parse, or a gofmt that can't run: run it again for its first complaint.
    # shellcheck disable=SC2086  # intentional: split the file list into separate gofmt args
    why=$(gofmt -l $go_files 2>&1 >/dev/null | grep -m1 .)
    echo "pre-commit blocked — gofmt failed: ${why:-exit status $status}" >&2; exit @EXIT@
  fi
  if [ -n "$bad" ]; then
    echo "pre-commit blocked — these need gofmt:" >&2; echo "  ${bad//$'\n'/$'\n'  }" >&2
    echo "fix: gofmt -w <files>   (skip once: git commit --no-verify)" >&2; exit @EXIT@
  fi
fi`,
	"terraform": `# Terraform: block the commit if any staged .tf file isn't terraform-fmt clean, or if terraform
# can't check it.
tf_files=$(echo "$staged" | grep '\.tf$' || true)
if [ -n "$tf_files" ] && ! command -v terraform >/dev/null 2>&1; then
  echo "pre-commit: terraform is not installed here, so the Terraform format check was skipped" >&2
elif [ -n "$tf_files" ]; then
  bad=""
  for f in $tf_files; do
    # -check exits non-zero for a file it lists; a non-zero exit that lists nothing is a failure.
    out=$(terraform fmt -check -list=true "$f" 2>/dev/null); status=$?
    if [ "$status" -ne 0 ] && [ -z "$out" ]; then
      why=$(terraform fmt -no-color -check -list=true "$f" 2>&1 >/dev/null | grep -m1 .)
      echo "pre-commit blocked — terraform fmt failed on $f: ${why:-exit status $status}" >&2; exit @EXIT@
    fi
    [ -n "$out" ] && bad="$bad $f"
  done
  if [ -n "$bad" ]; then
    echo "pre-commit blocked — these need terraform fmt:$bad" >&2
    echo "fix: terraform fmt <files>   (skip once: git commit --no-verify)" >&2; exit @EXIT@
  fi
fi`,
	"elixir": `# Elixir: block the commit if any staged .ex/.exs file isn't mix-format clean. mix has no
# flag to list many files at once, so check one at a time and match its stable
# "--check-formatted" failure text — any other failure (crash, syntax error, missing deps) fails
# open, with one warning naming it.
ex_files=$(echo "$staged" | grep -E '\.exs?$' || true)
if [ -n "$ex_files" ] && ! command -v mix >/dev/null 2>&1; then
  echo "pre-commit: mix is not installed here, so the Elixir format check was skipped" >&2
elif [ -n "$ex_files" ]; then
  bad=""; failed=""
  for f in $ex_files; do
    out=$(mix format --check-formatted "$f" 2>&1 >/dev/null); status=$?
    if echo "$out" | grep -q "failed due to --check-formatted"; then
      bad="$bad $f"
    elif [ "$status" -ne 0 ]; then
      if [ -z "$failed" ]; then
        # The cause, not the "mix format failed for file:" line that only repeats the file.
        why=$(echo "$out" | grep -v '^mix format failed for file' | grep -m1 .); why=${why:-exit status $status}
      fi
      failed="$failed $f"
    fi
  done
  if [ -n "$failed" ]; then
    echo "pre-commit: mix format failed on$failed, so formatting was not checked: $why" >&2
  fi
  if [ -n "$bad" ]; then
    echo "pre-commit blocked — these need mix format:$bad" >&2
    echo "fix: mix format <files>   (skip once: git commit --no-verify)" >&2; exit @EXIT@
  fi
fi`,
	"rust": `# Rust: block the commit if staged .rs files aren't rustfmt-clean. rustfmt defaults to the
# 2015 edition unless told otherwise, which hard-fails to parse async/gen code, so read the
# crate's real edition off Cargo.toml first. -l lists exactly the files that need formatting;
# any other failure (crash, syntax error, a broken rustfmt.toml) fails open, with one warning
# naming it.
rs_files=$(echo "$staged" | grep '\.rs$' || true)
if [ -n "$rs_files" ] && ! command -v rustfmt >/dev/null 2>&1; then
  echo "pre-commit: rustfmt is not installed here, so the Rust format check was skipped" >&2
elif [ -n "$rs_files" ]; then
  edition_line=$(grep -m1 '^edition' Cargo.toml 2>/dev/null)
  edition=${edition_line#*\"}; edition=${edition%%\"*}
  edflag=""; [ -n "$edition" ] && edflag="--edition=$edition"
  bad=""; failed=""
  for f in $rs_files; do
    # shellcheck disable=SC2086  # intentional: $edflag is empty or exactly one --edition=X token
    out=$(rustfmt --check -l $edflag "$f" 2>/dev/null); status=$?
    if [ -n "$out" ]; then
      bad="$bad $f"
    elif [ "$status" -ne 0 ]; then
      if [ -z "$failed" ]; then
        # shellcheck disable=SC2086  # intentional: $edflag is empty or exactly one --edition=X token
        why=$(rustfmt --check -l $edflag "$f" 2>&1 >/dev/null | grep -m1 .); why=${why:-exit status $status}
      fi
      failed="$failed $f"
    fi
  done
  if [ -n "$failed" ]; then
    echo "pre-commit: rustfmt failed on$failed, so formatting was not checked: $why" >&2
  fi
  if [ -n "$bad" ]; then
    echo "pre-commit blocked — these need rustfmt:$bad" >&2
    echo "fix: rustfmt <files>   (skip once: git commit --no-verify)" >&2; exit @EXIT@
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
hook="$HOME/.coop-git-hooks/prepare-commit-msg"
[ -x "$hook" ] || exit 0
exec "$hook" "$@"
`
