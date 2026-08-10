package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

const progressActivityWidth = 48

func progressState(c tasks.TaskCounts) string {
	s := fmt.Sprintf("%s/%d done", paintCount(c.Done, ui.Green), c.Total())
	if c.Blocked > 0 {
		s += fmt.Sprintf(" · %s blocked", paintCount(c.Blocked, ui.Red))
	}
	return s
}

func progressStateWidth(c tasks.TaskCounts) int {
	s := fmt.Sprintf("%d/%d done", c.Done, c.Total())
	if c.Blocked > 0 {
		s += fmt.Sprintf(" · %d blocked", c.Blocked)
	}
	return len([]rune(s))
}

// progressLine is the queue's at-a-glance state: done/total (done greened when nonzero), a
// blocked tally only when there is one, and the task being worked. The loop prints it both
// in the per-iteration banner and live, on its own, whenever a task changes state mid-run.
func progressLine(c tasks.TaskCounts, activity string) string {
	s := progressState(c)
	if activity != "" {
		s += " · now: " + truncate(activity, progressActivityWidth)
	}
	return s
}

// progressLineWidth fits the optional activity into a complete line budget. Structural queue
// state is never abbreviated; on an impossibly narrow row Region remains the final clip guard.
func progressLineWidth(c tasks.TaskCounts, activity string, width int) string {
	s := progressState(c)
	const separator = " · now: "
	activityW := width - progressStateWidth(c) - len([]rune(separator))
	if activity == "" || activityW <= 0 {
		return s
	}
	return s + separator + truncate(activity, activityW)
}

// progressBanner is progressLine prefixed with the iteration number, printed at the top of
// each loop iteration.
func progressBanner(n int, c tasks.TaskCounts, active string) string {
	return fmt.Sprintf("iteration %d · %s", n, progressLine(c, active))
}

func progressBannerWidth(n int, c tasks.TaskCounts, active string, width int) string {
	prefix := fmt.Sprintf("iteration %d · ", n)
	return prefix + progressLineWidth(c, active, width-len([]rune(prefix)))
}

// paintCount renders a count, applying paint only when it's nonzero so a zero stays
// plain — a "0 blocked" shouldn't read as an alarm. Shared by the `coop tasks` summary
// and the loop banner.
func paintCount(v int, paint func(string) string) string {
	if v > 0 {
		return paint(strconv.Itoa(v))
	}
	return strconv.Itoa(v)
}

// truncate shortens s to n runes, marking elision with an ellipsis.
func truncate(s string, n int) string {
	if n <= 0 {
		return "" // guards the r[:n-1] / r[:n] negative-index panic on a non-positive width
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// levenshtein returns the edit distance between a and b, for "did you mean" suggestions.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}

// nearestCommand suggests the candidate closest to a mistyped command. The allowed edit distance
// scales with input length so short words don't attract noise: 1-2 runes get no suggestion (fuzzy
// matches on `ls`/`go`/`cp` are mostly noise, and the caller's "run it in the box" hint covers them),
// 3 runes match only at distance 1 (`lop`→loop, `lss`→ls — a single slip of the most-typed verbs),
// and 4+ runes match within 2. This is what catches a distance-1 typo of `ls` before `coop fork lss`
// silently clones a stray fork.
func nearestCommand(input string, candidates []string) (string, bool) {
	n := len([]rune(input))
	if n < 3 {
		return "", false
	}
	maxDist := 2
	if n == 3 {
		maxDist = 1
	}
	best, bestDist := "", -1
	for _, c := range candidates {
		if d := levenshtein(input, c); bestDist < 0 || d < bestDist {
			best, bestDist = c, d
		}
	}
	if bestDist >= 0 && bestDist <= maxDist {
		return best, true
	}
	return "", false
}

// rejectArgs returns a usage error when a command that takes no arguments is given some,
// so a stray token fails clearly instead of being silently ignored. (A `help`/`--help`
// arg is intercepted earlier, so it never reaches here.) No leading "coop " — ui.Error already
// prefixes "coop:", so this would otherwise double it ("coop: coop doctor …").
func rejectArgs(cmd string, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return fmt.Errorf("%s takes no arguments (got %q) — see 'coop %s --help'", cmd, strings.Join(args, " "), cmd)
}

// colWidth is the width to size a table column to: the widest value (counted in runes), clamped
// to [min, max]. Values longer than max are meant to be ellipsis-truncated to max by the caller.
func colWidth(values []string, min, max int) int {
	w := min
	for _, v := range values {
		if n := utf8.RuneCountInString(v); n > w {
			w = n
		}
	}
	if w > max {
		w = max
	}
	return w
}

// padRight right-pads s to w columns counted in RUNES — unlike fmt's %-Ns, which counts bytes and
// so mis-pads a value carrying a multibyte glyph (e.g. a truncated name's "…").
func padRight(s string, w int) string {
	if n := utf8.RuneCountInString(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

// unknownErr is the one shape for a rejected subcommand / agent / value: `unknown <noun>
// "<token>" — use: a, b, c`, with a "did you mean X?" when the token is a near-miss. Shared by the
// sub-command groups (tasks/fleet/pool/profiles) so a bad input reads the same everywhere.
func unknownErr(noun, token string, valid []string) error {
	if guess, ok := nearestCommand(token, valid); ok {
		return fmt.Errorf("unknown %s %q — use: %s (did you mean %q?)", noun, token, strings.Join(valid, ", "), guess)
	}
	return fmt.Errorf("unknown %s %q — use: %s", noun, token, strings.Join(valid, ", "))
}

// gitArgs builds `git -C dir <hardening> <args>`. The hardening goes first so a caller's own
// trailing -c flags (e.g. forkspace.TrustedSignArgs) still win — git's last -c for a key takes effect.
// The list itself lives in internal/forkspace, next to the clone that creates a fork, so the whole
// repo has exactly one hardening set to audit.
func gitArgs(dir string, args []string) []string {
	return append(append([]string{"-C", dir}, forkspace.GitHardening...), args...)
}

// gitOut runs `git -C dir <args>` hardened and returns trimmed stdout, or "" on error. Every repo
// coop runs git against is agent-writable, so hardening is the default; to read a value coop will
// execute or read a host file from, use gitGlobalOut (the trusted global scope), never the repo.
// It CONFLATES a failed read with an empty one — fine for display, wrong for a decision: read those
// with gitOutErr.
func gitOut(dir string, args ...string) string {
	out, _ := gitOutErr(dir, args...)
	return out
}

// gitOutErr is gitOut for a read coop ACTS on: same hardened command, but a failure comes back as an
// error instead of an empty string, so "git broke" can't pass for "git said nothing" (an unreadable
// HEAD read as "" perturbs the loop's stall bookkeeping; an unreadable range reconciles no tasks and
// looks clean). The message carries git's own stderr — os/exec caps that capture at 32KB — because a
// caller surfacing this to a human has nothing else to explain the failure with.
func gitOutErr(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", gitArgs(dir, args)...).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if detail := strings.TrimSpace(string(exitErr.Stderr)); detail != "" {
				return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, detail)
			}
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitRun runs `git -C dir <args>` hardened, for effect, returning its error.
func gitRun(dir string, args ...string) error {
	return exec.Command("git", gitArgs(dir, args)...).Run()
}

// gitInteractive runs a hardened git command wired to the real stdio (a diff to the terminal, a
// signing pinentry prompt, etc).
func gitInteractive(dir string, args ...string) error {
	cmd := exec.Command("git", gitArgs(dir, args)...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	return cmd.Run()
}

// gitSign runs a hardened git command (like a rebase with signing), wiring Stdin
// so a TTY pinentry still works, but capturing CombinedOutput to silence benign chatter.
// The captured output is replayed to Stderr only on failure, or if GIT_TRACE is set.
func gitSign(dir string, args ...string) error {
	return gitSignTo(os.Stderr, dir, args...)
}

func gitSignTo(stderr io.Writer, dir string, args ...string) error {
	cmd := exec.Command("git", gitArgs(dir, args)...)
	cmd.Stdin = os.Stdin
	out, err := cmd.CombinedOutput()
	trace := strings.TrimSpace(os.Getenv("GIT_TRACE"))
	if err != nil || (trace != "" && trace != "0" && !strings.EqualFold(trace, "false")) {
		_, _ = stderr.Write(out)
	}
	return err
}

// gitGlobalOut reads from the host user's GLOBAL git config (`git config --global …`) — the
// trusted scope an agent can't write — for any value coop reads then EXECUTES or reads a host file
// from: your core.editor, your signing program, your global core.excludesfile. The repo's own
// .git/config is agent-writable, so reading these from it would let a poisoned repo redirect coop
// to run or exfiltrate whatever it names. They're all user-identity settings that live in your
// global config anyway; a value only in repo config is treated as unset (fail closed). Returns ""
// when unset or git is unavailable.
func gitGlobalOut(args ...string) string {
	out, err := exec.Command("git", append([]string{"config", "--global"}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitBranch(dir string) string { return gitOut(dir, "rev-parse", "--abbrev-ref", "HEAD") }

func gitDirty(dir string) bool { return gitOut(dir, "status", "--porcelain") != "" }

// parseShortstat pulls insertion/deletion counts out of a `git diff --shortstat`
// line ("N files changed, I insertions(+), D deletions(-)").
func parseShortstat(s string) (ins, del int) {
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		n := 0
		fmt.Sscanf(part, "%d", &n)
		switch {
		case strings.Contains(part, "insertion"):
			ins = n
		case strings.Contains(part, "deletion"):
			del = n
		}
	}
	return ins, del
}

// indent prefixes every line of s with two spaces.
func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

// approve reports whether a destructive step is approved. --yes approves without
// asking; otherwise it prompts interactively. In a non-interactive run (no TTY)
// without --yes it refuses rather than silently taking the default — so a pipe or CI
// job can't land work and delete a fork on its own. Callers gate the whole command on
// this up front (with a clear "pass --yes" error); this is also the safe fallback.
func approve(prompt string, yes bool) bool {
	if yes {
		return true
	}
	if !ui.IsTerminal(os.Stdin) {
		return false
	}
	return ui.Confirm(prompt, true)
}

// hasYes reports whether args carry the -y/--yes confirmation-skip flag that destructive commands
// accept to run unattended (distinct from --force, which overrides a safety guard, not the prompt).
func hasYes(args []string) bool {
	for _, a := range args {
		if a == "-y" || a == "--yes" {
			return true
		}
	}
	return false
}
