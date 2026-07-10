package cli

import (
	"fmt"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

// noProviderErr is coop's one actionable message when a launch names no provider (and no
// preset supplies a lead) — the implicit claude default is gone, so every agent-launching
// surface routes an empty agent here. cmd is the command word for the usage line
// ("loop", "acp", "fork", "" for a bare `coop`).
func noProviderErr(cmd string) error {
	usage := "coop " + cmd
	if cmd == "" {
		usage = "coop"
	}
	return fmt.Errorf("name the agent — %s <%s> (or a preset name); see 'coop credentials' for who's signed in",
		strings.TrimSpace(usage), strings.Join(agents.Names(), "|"))
}
