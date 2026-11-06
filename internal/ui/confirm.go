package ui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The interactive half of owning the terminal. Reading one line back from the tty ui already
// detects (IsTerminal) is the same contract as painting the alt screen onto it: a confirmation is
// the one thing a command CANNOT return as data for its caller to print, because the answer has to
// come back from the same terminal. It lives here so the destructive verbs in every package — task
// rm, profile rm, fork rm — ask the identical question with the identical default.

// ErrCancelled is a gate the human answered No to — an answer, not a failure, so a caller reports
// what did not happen instead of a red error. ErrNeedsConfirmation is a gate nothing could answer
// (piped, no --yes); a caller renders it as ConfirmationRequired with its own help route.
var (
	ErrCancelled         = errors.New("cancelled")
	ErrNeedsConfirmation = errors.New("confirmation required")
)

// needsConfirmation keeps the gate's own actionable sentence for a caller that only prints an
// error, while still answering errors.Is(err, ErrNeedsConfirmation) for one that renders the
// shared refusal block.
type needsConfirmation struct{ what string }

func (e needsConfirmation) Error() string {
	// what is the prompt's question ("Delete this fork"), which starts with a capital; mid-sentence
	// it reads as an ordinary verb.
	first, size := utf8.DecodeRuneInString(e.what)
	what := string(unicode.ToLower(first)) + e.what[size:]
	return fmt.Sprintf("cannot %s without confirmation. No terminal is available; run again with --yes", what)
}

func (e needsConfirmation) Is(target error) bool { return target == ErrNeedsConfirmation }

// DestroyGate guards an UNRECOVERABLE deletion, returning nil only when it may proceed. With yes (the
// caller saw -y/--yes) it proceeds silently. Otherwise, piped (no TTY) it REFUSES — there's nothing
// to confirm against, so a script must opt in with --yes; at a TTY it asks "<what>? [y/N]" defaulting
// to No, so a stray Enter cancels. `what` is the SHORT question ("Delete this task", "Continue"):
// the caller has already previewed the blast radius in future tense above the prompt, and repeating
// the whole consequence inside the question only buries it. One gate for every rm (tasks, profiles,
// forks) so they can't drift. See rule destructive-confirm-gate.
//
// An interactive flow that already owns its input scanner may provide one ask callback. That keeps
// the destructive decision in this gate without making the flow compete with fmt.Scanln for stdin.
func DestroyGate(what string, yes bool, asks ...func(string) bool) error {
	if yes {
		return nil
	}
	if len(asks) > 1 {
		return errors.New("destroy gate accepts at most one prompt callback")
	}
	if len(asks) == 1 {
		if !asks[0](what + "?") {
			return ErrCancelled
		}
		return nil
	}
	if !CanAsk() {
		return needsConfirmation{what: what}
	}
	if !Confirm(what+"?", false) {
		return ErrCancelled
	}
	return nil
}

// CanAsk reports whether a confirmation has someone to answer it: a terminal, or a test standing in
// for one (AnswerConfirmationsFrom). A command that will ask checks it before any work.
func CanAsk() bool { return confirmInput != nil || IsTerminal(os.Stdin) }

// DeclineExit is the exit status of a command whose confirmation did not let it go on: 1 when a
// person answered no, since nothing changed and nothing was wrong with the command, and 2 only when
// nothing could answer (no terminal and no --yes), a usage error the run fixes with --yes.
func DeclineExit(err error) int {
	if errors.Is(err, ErrNeedsConfirmation) {
		return 2
	}
	return 1
}

// confirmInput is a narrow test seam for the one thing a confirmation cannot do without a terminal:
// read the answer back. nil — which it always is outside tests — means the real interactive path,
// os.Stdin gated by IsTerminal, so an interactive user's behavior is unchanged. A test points it at
// a reader standing in for the terminal (another package's test through AnswerConfirmationsFrom);
// that is the only way the bare-Enter, explicit-answer, and EOF branches below are reachable in a
// gate that has no tty.
var confirmInput io.Reader

// AnswerConfirmationsFrom makes DestroyGate and Confirm read each answer from r, as though a person
// typed it at a terminal, until restore runs. Only tests call it: a run asks its own terminal.
func AnswerConfirmationsFrom(r io.Reader) (restore func()) {
	old := confirmInput
	confirmInput = r
	return func() { confirmInput = old }
}

// Confirm asks a yes/no question, returning def with no tty (batch runs) or on a
// bare Enter.
func Confirm(prompt string, def bool) bool {
	in := confirmInput
	if in == nil {
		if !IsTerminal(os.Stdin) {
			return def
		}
		in = os.Stdin // fmt.Scanln IS Fscanln(os.Stdin, …), so the read below is the same call
	}
	hint := "Y/n"
	if !def {
		hint = "y/N"
	}
	fmt.Fprintf(os.Stderr, "%s [%s] ", prompt, hint)
	var resp string
	fmt.Fscanln(in, &resp)
	return ConfirmationResponse(resp, def)
}

// ConfirmationResponse applies the shared y/N parsing after a caller has read a response.
func ConfirmationResponse(resp string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(resp)) {
	case "":
		return def
	case "y", "yes":
		return true
	default:
		return false
	}
}
