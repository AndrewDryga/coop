package tasks

import "github.com/AndrewDryga/coop/internal/ui"

// Host is what the `coop tasks` verb family needs from the CLI and cannot own itself: the
// live-board driver (RunWatchLoop) owns terminal and signal handling, which is cli-wide, not
// task-specific — the same reasoning sessionsvc/acpctl apply to their own injected Host functions.
//
// Unlike sessionsvc's Host, the field is a direct 1:1 assignment of an existing cli function (cli's
// runWatchLoop matches this signature exactly) — no policy decision happens at the injection point.
//
// Every field is optional and a zero Host is usable — a test that drives CmdTasks/TasksWatch
// directly and doesn't care about the live board gets a harmless no-op default, the same "zero Host
// is usable" contract sessionsvc's Host documents.
type Host struct {
	// RunWatchLoop drives the alternate-screen live board (enter/leave, signal handling, the poll
	// ticker, and settled-debounce auto-exit); see internal/cli/watch.go for the full contract.
	RunWatchLoop func(screen *ui.AltScreen, tick func(spin int) (frame []string, settled bool), done func()) (int, error)
}

func (h Host) runWatchLoop(screen *ui.AltScreen, tick func(spin int) (frame []string, settled bool), done func()) (int, error) {
	if h.RunWatchLoop == nil {
		return 0, nil
	}
	return h.RunWatchLoop(screen, tick, done)
}
