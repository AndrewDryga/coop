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

// nearestCommand suggests the candidate closest to a mistyped command (ui.Nearest). A word of one
// or two runes gets no suggestion, which the caller's "run it in the box" hint covers.
func nearestCommand(input string, candidates []string) (string, bool) {
	return ui.Nearest(input, candidates)
}

// rejectArgs returns the shared extra-argument refusal when a command that takes no arguments is
// given some, so a stray token fails clearly instead of being silently ignored. It names the FIRST
// extra token, not every one. (A `help`/`--help` arg is intercepted earlier, so it never reaches
// here.) cmd is the command path without "coop" — "version", "doctor".
func rejectArgs(cmd string, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return extraArgErr(args[0], "coop "+cmd, "coop "+cmd, nil)
}

// extraArgErr refuses a token its command did not take, by the one rule every command shares
// (ui.ExtraArgument): a token that starts with "-" is an unknown option, any other an unexpected
// argument shown with usage, the syntax line of the command's own help page.
func extraArgErr(arg, command, usage string, options []string) error {
	return ui.ExtraArgument(arg, command, usage, options)
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

// unknownOptionErr refuses an option its command does not accept (ui.UnknownOptionAmong). command
// is the full owning path ("coop models"); valid is that command's real option list, so a suggested
// correction can never name an option the command would refuse.
func unknownOptionErr(option, command string, valid []string) error {
	return ui.UnknownOptionAmong(option, command, valid)
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
