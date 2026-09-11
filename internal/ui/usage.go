package ui

import (
	"fmt"
	"os"
	"strings"
)

// UsageError is coop's ONE shape for rejected input: a red headline naming exactly what was
// refused and the full command it was refused for, an optional six-space cause explaining the
// constraint, then aligned label rows pointing at the fix. Every parser builds one from DATA
// (the command path, the offending token, a usage line) instead of formatting its own sentence,
// so a new command gets the approved shape for free and no family drifts into its own layout.
//
// Callers return it like any error; Main renders it (see Render) and exits 2.
type UsageError struct {
	Headline string      // "Unknown command \"coop doctro\"" — what was refused, with the full command
	Cause    string      // optional one-line reason, indented six spaces ("Choose claude, codex, …")
	Rows     [][2]string // label → command ("Did you mean:" → "coop doctor"), aligned on the label
}

// Error flattens the block to one line, for a log, a wrap, or a machine consumer that only ever
// sees an error string. The human at a terminal gets Render instead.
func (e *UsageError) Error() string {
	parts := []string{e.Headline}
	if e.Cause != "" {
		parts = append(parts, e.Cause)
	}
	for _, r := range e.Rows {
		parts = append(parts, r[0]+" "+r[1])
	}
	return strings.Join(parts, " — ")
}

// Render is the exact approved block: a leading blank line, the red ✗ headline, a blank line, the
// six-space cause between blank lines when there is one, then the two-space rows whose commands
// all start past the widest label. Padding is computed on PLAIN text and color applied after, so
// a NO_COLOR or redirected run has the same columns with no escape sequences.
func (e *UsageError) Render(p Palette) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n%s\n", p.Red("✗ "+e.Headline))
	if e.Cause != "" {
		fmt.Fprintf(&b, "\n      %s\n", e.Cause)
	}
	if len(e.Rows) > 0 {
		w := 0
		for _, r := range e.Rows {
			if n := len([]rune(r[0])); n > w {
				w = n
			}
		}
		b.WriteString("\n")
		for _, r := range e.Rows {
			label := r[0] + strings.Repeat(" ", w-len([]rune(r[0])))
			fmt.Fprintf(&b, "  %s %s\n", label, r[1])
		}
	}
	return b.String()
}

// PrintUsageError writes the block to stderr (stdout stays empty for a rejected command), colored
// only when stderr is a real terminal and NO_COLOR is unset.
func PrintUsageError(e *UsageError) { emit(e.Render(For(os.Stderr))) }

// HelpCommand is the focused help route for a command path — "coop models" → "coop help models",
// "coop tasks add" → "coop help tasks add". Every family derives its Help: row from its own
// command instead of carrying a second registry of help routes.
func HelpCommand(command string) string {
	return "coop help " + strings.TrimPrefix(command, "coop ")
}

// UnknownCommand refuses a command or subcommand coop doesn't have. rejected and suggestion are
// FULL commands including "coop" ("coop tasks watxh", "coop tasks watch"), because a bare token
// reads like a rejected shell command. With no usable suggestion it points at help instead —
// helpCommand is the nearest valid family's page ("coop help", "coop help tasks").
func UnknownCommand(rejected, suggestion, helpCommand string) *UsageError {
	e := &UsageError{Headline: fmt.Sprintf("Unknown command %q", rejected)}
	if suggestion != "" {
		e.Rows = [][2]string{{"Did you mean:", suggestion}}
		return e
	}
	e.Rows = [][2]string{{"See available commands:", helpCommand}}
	return e
}

// UnknownCommandPath is UnknownCommand for a rejected command path — path is the tokens after
// "coop" ("tasks", "watxh"), guess is the corrected LAST token when its family had a near miss,
// and asHelp forms the suggestion as a help route so a help request stays a help request. With no
// guess it points at the nearest valid family's page, which is the path without its last token.
// Each package computes guess with its own spelling helper; the SHAPE is assembled only here.
func UnknownCommandPath(path []string, guess string, asHelp bool) *UsageError {
	prefix := "coop "
	if asHelp {
		prefix = "coop help "
	}
	family := strings.Join(path[:len(path)-1], " ")
	if family != "" {
		family += " "
	}
	suggestion := ""
	if guess != "" {
		suggestion = prefix + family + guess
	}
	return UnknownCommand("coop "+strings.Join(path, " "), suggestion, strings.TrimRight("coop help "+family, " "))
}

// UnknownOption refuses an option its owning command does not accept. suggestion, when the typo
// has an obvious correction, is the whole corrected command — never a bare flag.
func UnknownOption(option, command, suggestion string) *UsageError {
	e := &UsageError{Headline: fmt.Sprintf("Unknown option %q for %q", option, command)}
	if suggestion != "" {
		e.Rows = [][2]string{{"Did you mean:", suggestion}}
		return e
	}
	e.Rows = [][2]string{{"See available options:", HelpCommand(command)}}
	return e
}

// MissingOptionValue refuses a recognized option whose REQUIRED value is absent. It says Example,
// not "did you mean": nothing here proves which value was intended.
func MissingOptionValue(option, command, example string) *UsageError {
	return &UsageError{
		Headline: fmt.Sprintf("Missing value for %q in %q", option, command),
		Rows:     [][2]string{{"Example:", example}, {"Help:", HelpCommand(command)}},
	}
}

// InvalidOptionValue refuses a supplied value the parser cannot accept. cause states the ACTUAL
// constraint ("Choose claude, codex, gemini, or all.", "Use a whole number greater than 0.") —
// the accepted tokens or the failing bound, never a bare "invalid".
func InvalidOptionValue(value, option, command, cause, example string) *UsageError {
	return &UsageError{
		Headline: fmt.Sprintf("Invalid value %q for %q in %q", value, option, command),
		Cause:    cause,
		Rows:     [][2]string{{"Example:", example}, {"Help:", HelpCommand(command)}},
	}
}

// MissingArgument refuses a command missing a REQUIRED positional. name is the human word for it
// ("title", "task ID"), not "positional argument"; usage is the command's own syntax line.
func MissingArgument(name, command, usage string) *UsageError {
	return &UsageError{
		Headline: fmt.Sprintf("Missing %s for %q", name, command),
		Rows:     [][2]string{{"Usage:", usage}, {"Help:", HelpCommand(command)}},
	}
}

// UnexpectedArgument refuses the FIRST genuinely extra token — not a count, and not every
// argument supplied. It reuses MissingArgument's Usage:/Help: rows on purpose.
func UnexpectedArgument(arg, command, usage string) *UsageError {
	return &UsageError{
		Headline: fmt.Sprintf("Unexpected argument %q for %q", arg, command),
		Rows:     [][2]string{{"Usage:", usage}, {"Help:", HelpCommand(command)}},
	}
}

// ConflictingOptions refuses two options that cannot be combined. The constraint is already in the
// headline, so the block shows only the focused help route — no usage block, and no guess about
// which side the user meant to drop.
func ConflictingOptions(first, second, command string) *UsageError {
	return &UsageError{
		Headline: fmt.Sprintf("Cannot combine %q and %q in %q", first, second, command),
		Rows:     [][2]string{{"Help:", HelpCommand(command)}},
	}
}

// RepeatedOption refuses a second copy of an option that takes only one value. Genuinely
// repeatable options (peers, subtasks, domains) must not route here.
func RepeatedOption(option, command string) *UsageError {
	return &UsageError{
		Headline: fmt.Sprintf("Option %q can only be used once in %q", option, command),
		Rows:     [][2]string{{"Help:", HelpCommand(command)}},
	}
}

// StandaloneValue refuses a sentinel value ("all", "none") mixed into a list it must stand alone in.
func StandaloneValue(value, option, command string) *UsageError {
	return &UsageError{
		Headline: fmt.Sprintf("Value %q must be used alone for %q in %q", value, option, command),
		Rows:     [][2]string{{"Help:", HelpCommand(command)}},
	}
}
