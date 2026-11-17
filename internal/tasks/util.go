package tasks

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/randid"
	"github.com/AndrewDryga/coop/internal/safetext"
	"github.com/AndrewDryga/coop/internal/ui"
)

// newSupervisorID mints a fresh random id to embed in a completion window's lock name, so nothing
// else can collide with it.
func newSupervisorID() string { return randid.Hex(8) }

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// fenceMarker reports whether a line opens or closes a Markdown fenced code block (``` or ~~~ —
// three or more, ignoring leading whitespace and any info string). Task-body scanners toggle on
// it so a "- [ ]" documented INSIDE a fence (e.g. an example in a task body) isn't read as a real
// subtask.
func fenceMarker(line string) bool {
	t := strings.TrimLeft(line, " \t")
	return strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")
}

// sanitizeCell drops the characters safetext.Hidden names from a one-line display string, so a task
// title or decision text — which can come from an untrusted agent's task.md — can't inject ANSI
// escapes into coop's output (corrupting a redirect/pipe, or spoofing the colored UI on a
// terminal), reorder the row with a direction override, split it with a line separator, or hide
// characters in it. A cell loses little without them: a family emoji shows as its members. A
// variation selector is judged by the character it would follow once the others are gone, so
// dropping a zero-width space can't leave one hidden after a letter.
func sanitizeCell(s string) string {
	kept := rune(-1)
	return strings.Map(func(r rune) rune {
		if safetext.Hidden(kept, r) {
			return -1
		}
		kept = r
		return r
	}, s)
}

// shellJoin writes argv the way a shell reads it back: a plain argument as is, any other in single
// quotes, closing and reopening them around each single quote it holds.
func shellJoin(argv []string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		plain := arg != ""
		for _, r := range arg {
			plain = plain && (r < utf8.RuneSelf && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./=:@%+,", r)))
		}
		if plain {
			quoted[i] = arg
		} else {
			quoted[i] = "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
		}
	}
	return strings.Join(quoted, " ")
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

// lastLines returns the last n lines of s (trailing blank lines trimmed first).
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
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

// padRight right-pads s to w columns counted in RUNES — unlike fmt's %-Ns, which counts bytes and
// so mis-pads a value carrying a multibyte glyph (e.g. a truncated name's "…"). internal/cli/util.go
// keeps its own copy, used well beyond the task views.
func padRight(s string, w int) string {
	if n := utf8.RuneCountInString(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

// unknownSubcommandErr refuses a verb a task family does not have, correcting a near miss from that
// family's own verb list.
func unknownSubcommandErr(family, verb string, valid []string) error {
	guess, _ := ui.Nearest(verb, valid)
	return ui.UnknownCommandPath([]string{family, verb}, guess, false)
}

// unknownOptionErr refuses an option its command does not accept, suggesting the whole corrected
// command when the typo has an obvious fix. command is the full owning path ("coop tasks claim");
// valid is that command's real option list, so a suggestion can never name one it would refuse.
func unknownOptionErr(option, command string, valid []string) error {
	option, _, _ = strings.Cut(option, "=") // a value is not part of the name, and may be secret
	if guess, _ := ui.Nearest(option, valid); guess != "" {
		return ui.UnknownOptionNear(option, command, guess)
	}
	return ui.UnknownOption(option, command, "")
}

// flagValue parses one `--flag value` or `--flag=value` occurrence at args[i]. ok reports whether
// args[i] names flag at all; ok can be true with a non-nil err (e.g. a bare trailing --flag with no
// value). consumed is how many args[i:] elements the flag+value occupy (1 for `--flag=value`, 2 for
// `--flag value`) — internal/cli/commands.go keeps its own copy, used across many
// non-task flags.
func flagValue(args []string, i int, flag string) (val string, consumed int, ok bool, err error) {
	switch a := args[i]; {
	case a == flag:
		if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
			return "", 0, true, fmt.Errorf("%s needs a value", flag)
		}
		return args[i+1], 2, true, nil
	case strings.HasPrefix(a, flag+"="):
		if v := strings.TrimPrefix(a, flag+"="); v != "" {
			return v, 1, true, nil
		}
		return "", 0, true, fmt.Errorf("%s needs a value", flag)
	}
	return "", 0, false, nil
}
