package ui

import (
	"fmt"
	"os"
	"strings"
)

// UsageError is coop's ONE shape for a command that could not proceed: a red headline naming
// exactly what was refused and the full command it was refused for, an optional six-space cause
// explaining the constraint, then aligned label rows pointing at the fix. Every parser builds one
// from DATA (the command path, the offending token, a usage line) instead of formatting its own
// sentence, so a new command gets the approved shape for free and no family drifts into its own
// layout. Rejected INPUT is the common case (see the constructors below); a RUNTIME failure a
// person reads the same way uses the same block through Failure.
//
// Callers return it like any error; Main renders it (see Render) and exits 2 for rejected input,
// or the status the command returned.
type UsageError struct {
	Headline string      // "Unknown command \"coop doctro\"" — what was refused, with the full command
	Cause    string      // optional reason, indented six spaces ("Choose claude, codex, …"); one sentence per line
	Choices  []string    // optional values that would settle it (the run IDs a prefix matched)
	Rows     [][2]string // label → command ("Did you mean:" → "coop doctor"), aligned on the label
	ExitCode int         // process exit code; 0 means the usual 2 for rejected input (CommandFailed sets 1)
}

// Error flattens the block to one line, for a log, a wrap, or a machine consumer that only ever
// sees an error string. The human at a terminal gets Render instead.
func (e *UsageError) Error() string {
	parts := []string{e.Headline}
	if e.Cause != "" {
		parts = append(parts, e.Cause)
	}
	parts = append(parts, e.Choices...)
	for _, r := range e.Rows {
		parts = append(parts, r[0]+" "+r[1])
	}
	return strings.Join(parts, " — ")
}

// Continuation is the label of a row that CONTINUES the row above it — a second spelling of the
// same usage. It renders under that row's value instead of at the label column, and never widens
// the label column. A row labeled "" is an ordinary action sentence at the label column.
const Continuation = "\x00continuation"

// Render is the exact approved block: a leading blank line, the red ✗ headline, a blank line, the
// six-space cause between blank lines when there is one, then the two-space rows whose commands
// all start past the widest label. Padding is computed on PLAIN text and color applied after, so
// a NO_COLOR or redirected run has the same columns with no escape sequences. A cause carrying
// newlines keeps every line at the same six spaces — the constraint and the fix it implies are one
// block, not a second paragraph.
func (e *UsageError) Render(p Palette) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n%s\n", p.Red("✗ "+e.Headline))
	if e.Cause != "" {
		b.WriteString("\n")
		for _, line := range strings.Split(e.Cause, "\n") {
			fmt.Fprintf(&b, "      %s\n", line)
		}
	}
	// The values that would settle the refusal are the answer itself, not a
	// labeled action: they sit in their own two-space block above the rows.
	if len(e.Choices) > 0 {
		b.WriteString("\n")
		for _, choice := range e.Choices {
			fmt.Fprintf(&b, "  %s\n", choice)
		}
	}
	if len(e.Rows) > 0 {
		w := 0
		for _, r := range e.Rows {
			if r[0] == Continuation {
				continue // a continuation borrows the width above it
			}
			if n := len([]rune(r[0])); n > w {
				w = n
			}
		}
		b.WriteString("\n")
		for _, r := range e.Rows {
			switch r[0] {
			case Continuation:
				// A second spelling of the row above — it sits under that row's VALUE, so the two
				// read as one usage rather than as two unrelated instructions.
				fmt.Fprintf(&b, "  %s %s\n", strings.Repeat(" ", w), r[1])
				continue
			case "":
				// A plain action sentence: it answers the headline on its own, so it sits at the
				// label column rather than pretending to continue a labeled row.
				b.WriteString("  " + r[1] + "\n")
				continue
			}
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

// UnknownValue refuses a positional VALUE its command does not accept — an agent name, a preset,
// a credential attribute. noun names the slot ("agent"), so the headline reads like the option
// form. suggestion, when the value is a near miss, is the whole corrected command; otherwise cause
// names the values the slot does accept and the block points at the command's own page.
func UnknownValue(noun, value, command, cause, suggestion string) *UsageError {
	e := &UsageError{Headline: fmt.Sprintf("Unknown %s %q for %q", noun, value, command)}
	if suggestion != "" {
		e.Rows = [][2]string{{"Did you mean:", suggestion}}
		return e
	}
	e.Cause = cause
	e.Rows = [][2]string{{"Help:", HelpCommand(command)}}
	return e
}

// ConfirmationRequired refuses an unrecoverable deletion nothing could confirm: no terminal to ask
// at, and no --yes standing in for the answer (see DestroyGate). command is the deleting command,
// whose page documents --yes.
func ConfirmationRequired(command string) *UsageError {
	return &UsageError{
		Headline: "Confirmation required",
		Cause:    "Run this command in a terminal, or pass --yes to confirm deletion.",
		Rows:     [][2]string{{"Help:", HelpCommand(command)}},
	}
}

// MissingOptionValue refuses a recognized option whose REQUIRED value is absent. It says Example,
// not "did you mean": nothing here proves which value was intended.
func MissingOptionValue(option, command, example string) *UsageError {
	return &UsageError{
		Headline: fmt.Sprintf("Missing value for %q in %q", option, command),
		Rows:     [][2]string{{"Example:", example}, {"Help:", HelpCommand(command)}},
	}
}

// MissingRepeatableOptionValue refuses a REPEATABLE option given with no value. It carries the
// usage row the plain form does not need: that the option may be given more than once is part of
// how to fix this, and one example cannot show it.
func MissingRepeatableOptionValue(option, command, usage, example string) *UsageError {
	return &UsageError{
		Headline: fmt.Sprintf("Missing value for %q in %q", option, command),
		Rows:     [][2]string{{"Usage:", usage}, {"Example:", example}, {"Help:", HelpCommand(command)}},
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

// CommandFailed is a RUNTIME failure the command RETURNS, drawn in the same block as a rejected
// input: the red headline, the bounded cause six spaces in, and the labeled route to the page that
// explains it. Rejected input and a command that could not do its job are different kinds of wrong,
// but a person reads them the same way, so they share one shape instead of drifting into two — they
// part only at the exit code, which stays 1 here because nothing about the input was wrong. cause is
// the ACTUAL bounded reason — a validator's message, a runtime's error — never a guessed diagnosis;
// rows are the command's own follow-up (`Help:` → its page), empty when there is nothing useful to
// say. Use ui.Failure instead when the failure must print as it happens (a loop mid-run) rather than
// end the command.
func CommandFailed(headline, cause string, rows ...[2]string) *UsageError {
	return &UsageError{Headline: headline, Cause: cause, Rows: rows, ExitCode: 1}
}
