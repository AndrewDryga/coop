package cli

import (
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ui"
)

// resumeNoteAfter compares native history before and after a launch. It prints
// a host command for one identifiable session, skipping ambiguous changes from
// conversation switches or boxes that share the same home.
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
	profile := a.cfg.ActiveProfile(agent)
	home, err := box.NativeHomePath(a.cfg, agent, profile, repo, false)
	if err != nil {
		return nothing
	}
	written := resumer.WatchSessions(home, box.Workdir(a.cfg, repo))
	return func() {
		ids := written()
		if len(ids) != 1 {
			return
		}
		target := a.argv[0]
		if a.preset == nil {
			// Pin the selected account if the launch used the default. An
			// unsigned built-in default may have no addressable account.
			if parsed, err := agents.ParseTarget(target); err == nil && len(parsed.Accounts) == 0 &&
				(profile != config.DefaultProfile || a.requireProfile(agent, profile) == nil) {
				target += "@" + profile
			}
		}
		words := append([]string{"coop", shellWord(target)}, resumer.ResumeArgs(ids[0])...)
		ui.Note("\nResume this session with Coop:\n  %s", strings.Join(words, " "))
	}
}
