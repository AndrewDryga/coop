package ui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
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
	return fmt.Sprintf("refusing to %s without confirmation — re-run with --yes (no terminal to prompt)", e.what)
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
	if !IsTerminal(os.Stdin) {
		return needsConfirmation{what: what}
	}
	if !Confirm(what+"?", false) {
		return ErrCancelled
	}
	return nil
}

// confirmInput is a narrow test seam for the one thing Confirm cannot do without a terminal: read
// the answer back. nil — which it always is outside this package's own tests — means the real
// interactive path, os.Stdin gated by IsTerminal, so an interactive user's behavior is unchanged.
// A test points it at a reader standing in for the terminal; that is the only way the bare-Enter,
// explicit-answer, and EOF branches below are reachable in a gate that has no tty.
var confirmInput io.Reader

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
