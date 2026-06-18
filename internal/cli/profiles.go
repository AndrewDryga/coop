package cli

import (
	"fmt"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ui"
)

// cmdProfiles lists each agent's stored credential profiles and whether they're signed
// in, so you can see which subscriptions are available to the loop. Read-only.
func (a *app) cmdProfiles(args []string) (int, error) {
	names := agents.Names()
	if len(args) > 0 {
		if _, ok := agents.Get(args[0]); !ok {
			return 2, fmt.Errorf("unknown agent %q — use %s", args[0], strings.Join(agents.Names(), ", "))
		}
		names = []string{args[0]}
	}
	for _, agent := range names {
		fmt.Println(ui.Bold(agent))
		profiles := a.cfg.Profiles(agent)
		if len(profiles) == 0 {
			fmt.Printf("  no profiles — run: coop login %s [--profile <name>]\n", agent)
			continue
		}
		width := 0 // widest name, so the status column lines up for any-length profiles
		for _, p := range profiles {
			if len(p) > width {
				width = len(p)
			}
		}
		for _, p := range profiles {
			status := ui.Dim("not signed in")
			if box.ProfileAuthed(a.cfg, agent, p) {
				status = ui.Green("signed in")
			}
			tag := ""
			if p == config.DefaultProfile {
				tag = ui.Dim("  (default)")
			}
			// Pad the plain name, then style the status — never color inside the width verb.
			fmt.Printf("  %-*s  %s%s\n", width, p, status, tag)
		}
	}
	return 0, nil
}
