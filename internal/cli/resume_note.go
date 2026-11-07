package cli

import (
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/ui"
)

// resumeNoteAfter watches the sessions an agent keeps in this repository's native home during a
// launch, and returns what to call once the box has stopped: it prints the Coop command that
// resumes the session the run wrote. The agent prints a resume command of its own as it exits, but
// that one runs its CLI on the host, which cannot see these sessions, and Coop never sees the line
// because the terminal goes straight to the container runtime. The command printed is the one the
// person typed, so its account, model and preset come with it, plus the agent's own words for
// resuming. A run that wrote no session prints nothing, and neither does one that wrote several,
// because another box shares the home and which one is this run's cannot be told: a command that
// might open a different conversation is worse than none.
func (a *app) resumeNoteAfter(agent, repo string, cmd []string) func() {
	nothing := func() {}
	ag, ok := agents.Get(agent)
	if !ok || !a.cfg.Homes || len(a.argv) == 0 {
		return nothing
	}
	resumer, ok := ag.(agents.SessionResumer)
	if !ok || !ag.InteractiveInvocation(cmd) {
		return nothing
	}
	home, err := box.NativeHomePath(a.cfg, agent, a.cfg.ActiveProfile(agent), repo, false)
	if err != nil {
		return nothing
	}
	written := resumer.WatchSessions(home, box.Workdir(a.cfg, repo))
	return func() {
		ids := written()
		if len(ids) != 1 {
			return
		}
		words := append([]string{"coop", shellWord(a.argv[0])}, resumer.ResumeArgs(ids[0])...)
		ui.Note("\nResume this session with Coop:\n  %s", strings.Join(words, " "))
	}
}
