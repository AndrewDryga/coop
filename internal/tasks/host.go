package tasks

import "github.com/AndrewDryga/coop/internal/ui"

// Host is what the `coop tasks`/`coop backlog` verb family needs from the CLI that owns the
// command catalog and cannot own itself: the full help text for every command group (GroupHelp)
// lives in internal/cli/help.go alongside every OTHER command's help, and the live-board driver
// (RunWatchLoop) owns terminal and signal handling. internal/cli keeps both because they're
// genuinely cli-wide, not task-specific, the same reasoning sessionsvc/acpctl apply to their own
// injected Host functions.
//
// Unlike sessionsvc's Host, both fields are direct 1:1 assignments of an existing cli function
// (cli's groupHelp/runWatchLoop match these signatures exactly) — no policy decision happens at
// the injection point itself, only in the two functions being handed over.
//
// Every field is optional and a zero Host is usable — a test that drives CmdTasks/TasksWatch
// directly and doesn't care about help text or the live board gets a harmless no-op default,
// the same "zero Host is usable" contract sessionsvc's Host documents.
type Host struct {
	// GroupHelp prints a command group's help (or the full help, if the group is unknown) and
	// returns its exit code — called when `coop tasks` finds no configured queue and no
	// subcommand, so a first run explains itself instead of erroring.
	GroupHelp func(group string) (int, error)

	// RunWatchLoop drives the alternate-screen live board (enter/leave, signal handling, the poll
	// ticker, and settled-debounce auto-exit); see internal/cli/watch.go for the full contract.
	RunWatchLoop func(screen *ui.AltScreen, tick func(spin int) (frame []string, settled bool), done func()) (int, error)
}

func (h Host) groupHelp(group string) (int, error) {
	if h.GroupHelp == nil {
		return 0, nil
	}
	return h.GroupHelp(group)
}

func (h Host) runWatchLoop(screen *ui.AltScreen, tick func(spin int) (frame []string, settled bool), done func()) (int, error) {
	if h.RunWatchLoop == nil {
		return 0, nil
	}
	return h.RunWatchLoop(screen, tick, done)
}
