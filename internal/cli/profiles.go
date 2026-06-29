package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/ui"
)

// cmdProfiles lists each agent's stored credential profiles and whether they're signed in, or —
// with a subcommand — marks the default (`default`) or deletes a profile (`rm`).
func (a *app) cmdProfiles(args []string) (int, error) {
	if len(args) > 0 && args[0] == "default" {
		return a.setProfileDefault(args[1:])
	}
	if len(args) > 0 && args[0] == "rm" {
		return a.removeProfile(args[1:])
	}
	names := agents.Names()
	if len(args) > 0 {
		if _, ok := agents.Get(args[0]); !ok {
			return 2, unknownErr("agent", args[0], agents.Names())
		}
		names = []string{args[0]}
	}
	// One width across every agent's profiles, so the "signed in" column lines up down the whole
	// listing — not just within each agent's block.
	var allProfiles []string
	for _, agent := range names {
		allProfiles = append(allProfiles, a.cfg.Profiles(agent)...)
	}
	width := colWidth(allProfiles, 0, 40)
	for _, agent := range names {
		fmt.Println(ui.Bold(agent))
		profiles := a.cfg.Profiles(agent)
		if len(profiles) == 0 {
			fmt.Printf("  no profiles — run: coop login %s [--profile <name>]\n", agent)
			continue
		}
		def := a.cfg.DefaultProfileOf(agent)
		for _, p := range profiles {
			status := ui.Dim("not signed in")
			if box.ProfileAuthed(a.cfg, agent, p) {
				status = ui.Green("signed in")
				// "signed in" only means a creds file exists — an OAuth token can be expired but
				// present, which reads as fine yet 401s in a run. Flag the expired case with the fix.
				if exp, ok := box.ProfileTokenExpiry(a.cfg, agent, p); ok && time.Now().After(exp) {
					status = ui.Yellow("token expired — re-login: coop login " + agent + " --profile " + p)
				}
			}
			tag := ""
			if p == def {
				tag = ui.Dim("  (default)")
			}
			// Pad the plain name (rune-aware), then style the status — never color inside the width.
			fmt.Printf("  %s  %s%s\n", padRight(p, width), status, tag)
		}
		// Surface a dangling default: the marked (or built-in) default points at a profile that
		// doesn't exist, so an interactive run would land on nothing. Don't leave it silent.
		if !slices.Contains(profiles, def) {
			fmt.Printf("  %s\n", ui.Dim(fmt.Sprintf("default → %s (missing — set one: coop profiles default %s <name>)", def, agent)))
		}
	}
	return 0, nil
}

// setProfileDefault marks <name> as <agent>'s default profile, so an interactive run with
// no profile given uses it. It rejects an unknown agent or a profile that doesn't exist.
func (a *app) setProfileDefault(args []string) (int, error) {
	if len(args) != 2 {
		return 2, errors.New("usage: coop profiles default <agent> <profile>")
	}
	agent, name := args[0], args[1]
	if _, ok := agents.Get(agent); !ok {
		return 2, unknownErr("agent", agent, agents.Names())
	}
	have := a.cfg.Profiles(agent)
	if !slices.Contains(have, name) {
		if len(have) == 0 {
			return 2, fmt.Errorf("%s has no profiles yet — run: coop login %s --profile %s", agent, agent, name)
		}
		return 2, fmt.Errorf("%s has no profile %q — have: %s", agent, name, strings.Join(have, ", "))
	}
	if err := a.cfg.SetDefaultProfile(agent, name); err != nil {
		return -1, err
	}
	if !box.ProfileAuthed(a.cfg, agent, name) {
		ui.Warn("%s profile %q isn't signed in yet — run: coop login %s --profile %s", agent, name, agent, name)
	}
	ui.OK("%s default profile → %s", agent, name)
	return a.cmdProfiles([]string{agent})
}

// removeProfile deletes a stored credential profile's directory — its login token and that
// profile's session history. It refuses to delete the agent's marked default (set another first,
// so a run never lands on a profile that's gone) and never deletes the legacy flat layout's whole
// agent dir. A pool that still names the profile is harmless: buildPool drops members that aren't
// signed in.
func (a *app) removeProfile(args []string) (int, error) {
	if len(args) != 2 {
		return 2, errors.New("usage: coop profiles rm <agent> <profile>")
	}
	agent, name := args[0], args[1]
	if _, ok := agents.Get(agent); !ok {
		return 2, unknownErr("agent", agent, agents.Names())
	}
	have := a.cfg.Profiles(agent)
	if !slices.Contains(have, name) {
		if len(have) == 0 {
			return 2, fmt.Errorf("%s has no profiles", agent)
		}
		return 2, fmt.Errorf("%s has no profile %q — have: %s", agent, name, strings.Join(have, ", "))
	}
	if name == a.cfg.DefaultProfileOf(agent) {
		return 2, fmt.Errorf("%s profile %q is the default — set another first: coop profiles default %s <other>", agent, name, agent)
	}
	dir := a.cfg.AgentProfileDir(agent, name)
	// Guard the legacy flat layout, where the "default" profile resolves to the agent dir itself:
	// removing that would wipe every profile, not one.
	if dir == filepath.Join(a.cfg.ConfigDir, agent) {
		return 2, fmt.Errorf("%s has no separate %q profile directory to remove", agent, name)
	}
	if err := os.RemoveAll(dir); err != nil {
		return -1, err
	}
	ui.OK("removed %s profile %q", agent, name)
	return a.cmdProfiles([]string{agent})
}
