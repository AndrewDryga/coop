package cli

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/ui"
)

// cmdProfiles lists each agent's stored credential profiles and whether they're signed in,
// or — with the `default` subcommand — marks which profile an interactive run uses.
func (a *app) cmdProfiles(args []string) (int, error) {
	if len(args) > 0 && args[0] == "default" {
		return a.setProfileDefault(args[1:])
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
			}
			tag := ""
			if p == def {
				tag = ui.Dim("  (default)")
			}
			// Pad the plain name (rune-aware), then style the status — never color inside the width.
			fmt.Printf("  %s  %s%s\n", padRight(p, width), status, tag)
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
		ui.Info("note: %s profile %q isn't signed in yet — run: coop login %s --profile %s", agent, name, agent, name)
	}
	ui.Info("%s default profile → %s", agent, name)
	return a.cmdProfiles([]string{agent})
}
