package ui

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// The interactive half of owning the terminal. Reading one line back from the tty ui already
// detects (IsTerminal) is the same contract as painting the alt screen onto it: a confirmation is
// the one thing a command CANNOT return as data for its caller to print, because the answer has to
// come back from the same terminal. It lives here so the destructive verbs in every package — task
// rm, profile rm, fork rm — ask the identical question with the identical default.

// DestroyGate guards an UNRECOVERABLE deletion, returning nil only when it may proceed. With yes (the
// caller saw -y/--yes) it proceeds silently. Otherwise, piped (no TTY) it REFUSES — there's nothing
// to confirm against, so a script must opt in with --yes; at a TTY it asks "<what>? …" defaulting to
// No, so a stray Enter cancels. `what` names the blast radius, e.g. "delete task X (todo)". One gate
// for every rm (tasks, profiles, forks) so they can't drift. See rule destructive-confirm-gate.
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
		if !asks[0](what + "? this can't be undone") {
			return errors.New("cancelled")
		}
		return nil
	}
	if !IsTerminal(os.Stdin) {
		return fmt.Errorf("refusing to %s without confirmation — re-run with --yes (no terminal to prompt)", what)
	}
	if !Confirm(what+"? this can't be undone", false) {
		return errors.New("cancelled")
	}
	return nil
}

// Confirm asks a yes/no question, returning def with no tty (batch runs) or on a
// bare Enter.
func Confirm(prompt string, def bool) bool {
	if !IsTerminal(os.Stdin) {
		return def
	}
	hint := "Y/n"
	if !def {
		hint = "y/N"
	}
	fmt.Fprintf(os.Stderr, "%s [%s] ", prompt, hint)
	var resp string
	fmt.Scanln(&resp)
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
