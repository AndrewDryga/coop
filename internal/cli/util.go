package cli

import (
	"context"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/forkspace"
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

// rejectArgs returns the shared extra-argument refusal when a command that takes no arguments is
// given some, so a stray token fails clearly instead of being silently ignored. It names the FIRST
// extra token, not every one. (A `help`/`--help` arg is intercepted earlier, so it never reaches
// here.) cmd is the command path without "coop" — "version", "approve".
func rejectArgs(cmd string, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return ui.UnexpectedArgument(args[0], "coop "+cmd, "coop "+cmd)
}

// padRight right-pads s to w columns counted in RUNES — unlike fmt's %-Ns, which counts bytes and
// so mis-pads a value carrying a multibyte glyph (e.g. a truncated name's "…").
func padRight(s string, w int) string {
	if n := utf8.RuneCountInString(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

// unknownSubcommandErr refuses a verb a command family does not have, correcting a near miss from
// that family's OWN verb list and otherwise pointing at the family's help.
func unknownSubcommandErr(family, verb string, valid []string) error {
	guess, _ := nearestCommand(verb, valid)
	return ui.UnknownCommandPath([]string{family, verb}, guess, false)
}

// unknownOptionErr refuses an option its command does not accept, suggesting the whole corrected
// command when the typo has an obvious fix. command is the full owning path ("coop models"); valid
// is that command's real option list, so the suggestion can never name an option it would refuse.
func unknownOptionErr(option, command string, valid []string) error {
	guess, _ := nearestCommand(option, valid)
	suggestion := ""
	if guess != "" {
		suggestion = command + " " + guess
	}
	return ui.UnknownOption(option, command, suggestion)
}

// gitOutputBytes is forkspace.GitOutErr for NUL-separated or whitespace-significant output: the raw bytes,
// untrimmed, from the same trusted-view command.
func gitOutputBytes(dir string, args ...string) ([]byte, error) {
	cmd, err := forkspace.GitCommand(context.Background(), dir, args...)
	if err != nil {
		return nil, err
	}
	out, err := cmd.Output()
	return out, forkspace.GitStalled(cmd, err)
}

// gitSign runs a hardened git command (like a rebase with signing), wiring Stdin
// so a TTY pinentry still works, but capturing CombinedOutput to silence benign chatter.
// The captured output is replayed to Stderr only on failure, or if GIT_TRACE is set.
func gitSign(dir string, args ...string) error {
	return gitSignTo(os.Stderr, dir, args...)
}

func gitSignTo(stderr io.Writer, dir string, args ...string) error {
	cmd, err := forkspace.GitCommand(context.Background(), dir, args...)
	if err != nil {
		return err
	}
	cmd.Stdin = os.Stdin
	out, err := cmd.CombinedOutput()
	trace := strings.TrimSpace(os.Getenv("GIT_TRACE"))
	if err != nil || (trace != "" && trace != "0" && !strings.EqualFold(trace, "false")) {
		_, _ = stderr.Write(out)
	}
	return err
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
