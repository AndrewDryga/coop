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

// cmdProfiles drives the profiles family with a resource-path grammar — each token
// narrows: `coop profiles` lists every agent, `coop profiles claude` one agent,
// `coop profiles claude personal` one profile, and a trailing attribute reads or writes
// one property of it: `model [<m> | --clear]`, `default` (mark it the agent's default),
// `rm` (delete it). So setting a profile's model reads as a path, not a verb sandwich:
//
//	coop profiles claude personal model opus
//
// The older verb-first forms (`default|model|rm <agent> <profile> …`) stay accepted —
// they shipped, and aliasing them here is cheaper than breaking fingers — but the path
// grammar is the documented one.
func (a *app) cmdProfiles(args []string) (int, error) {
	if len(args) > 0 {
		if _, ok := agents.Get(args[0]); ok && len(args) > 1 {
			return a.profilePath(args[0], args[1], args[2:])
		}
		switch args[0] {
		case "default":
			return a.setProfileDefault(args[1:])
		case "model":
			return a.markProfileModel(args[1:])
		case "rm", "remove":
			return a.removeProfile(args[1:])
		case "ls", "list":
			// Bare `coop profiles` already lists — steer `ls` there instead of "unknown agent" (rule:
			// `ls` is the list verb, so it must lead somewhere useful, not read as an agent filter).
			return 2, fmt.Errorf("coop profiles already lists every profile — just run `coop profiles` (no %q)", args[0])
		}
	}
	names := agents.Names()
	if len(args) > 0 {
		if _, ok := agents.Get(args[0]); !ok {
			return 2, unknownErr("agent", args[0], agents.Names())
		}
		names = []string{args[0]}
	}
	// One width per column across every agent's block, so the listing reads as one table:
	// NAME · STATUS · MODEL · (default). The models are the per-profile marks; the status
	// stays a short label — the re-login remedy for an expired token gets its own dim line
	// under the block instead of blowing the row out sideways.
	var allProfiles, allModels []string
	for _, agent := range names {
		for _, p := range a.cfg.Profiles(agent) {
			allProfiles = append(allProfiles, p)
			if m := a.cfg.ProfileModelOf(agent, p); m != "" {
				allModels = append(allModels, m)
			}
		}
	}
	pal := ui.For(os.Stdout) // stdout view — gate color on stdout so a pipe stays clean (p is the profile loop var below)
	width := colWidth(allProfiles, 0, 40)
	statusW := len("not signed in") // the widest short status label
	modelW := colWidth(allModels, 1, 24)
	first := true
	for _, agent := range names {
		if !first {
			fmt.Println() // a blank line between agents, so each block scans on its own
		}
		first = false
		fmt.Println(pal.Bold(agent))
		profiles := a.cfg.Profiles(agent)
		if len(profiles) == 0 {
			fmt.Printf("  no profiles — run: coop login %s [--profile <name>]\n", agent)
			continue
		}
		def := a.cfg.DefaultProfileOf(agent)
		var relogin []string
		for _, p := range profiles {
			label, expired := a.profileState(agent, p)
			if expired {
				relogin = append(relogin, p)
			}
			model := a.cfg.ProfileModelOf(agent, p)
			modelCell := padRight(model, modelW)
			if model == "" {
				modelCell = pal.Dim(padRight("—", modelW))
			}
			tag := ""
			if p == def {
				tag = pal.Dim("  (default)")
			}
			// Pad the plain strings (rune-aware), then style — never color inside a width.
			fmt.Printf("  %s  %s  %s%s\n", padRight(p, width), paintStatus(pal, padRight(label, statusW)), modelCell, tag)
		}
		for _, p := range relogin {
			fmt.Printf("  %s\n", pal.Dim("↻ re-login: coop login "+agent+" --profile "+p))
		}
		// Surface a dangling default: the marked (or built-in) default points at a profile that
		// doesn't exist, so an interactive run would land on nothing. Don't leave it silent.
		if !slices.Contains(profiles, def) {
			fmt.Printf("  %s\n", pal.Dim(fmt.Sprintf("default → %s (missing — set one: coop profiles %s <name> default)", def, agent)))
		}
	}
	return 0, nil
}

// profileState reports a profile's short sign-in label and whether its token is expired —
// present but past expiry, which reads as signed in yet 401s in a run; the caller surfaces
// the re-login remedy on its own line. Shared by the listing and the single-profile view.
func (a *app) profileState(agent, p string) (label string, expired bool) {
	if !box.ProfileAuthed(a.cfg, agent, p) {
		return "not signed in", false
	}
	if exp, ok := box.ProfileTokenExpiry(a.cfg, agent, p); ok && time.Now().After(exp) {
		return "token expired", true
	}
	return "signed in", false
}

// paintStatus colors a profileState label (possibly padded): green signed in, yellow
// expired, dim otherwise.
func paintStatus(pal ui.Palette, label string) string {
	switch strings.TrimSpace(label) {
	case "signed in":
		return pal.Green(label)
	case "token expired":
		return pal.Yellow(label)
	default:
		return pal.Dim(label)
	}
}

// profilePath routes the path grammar's per-profile tail: bare shows the profile, an
// attribute token reads or writes one property. Attribute handlers delegate to the same
// functions the verb forms use, so the two grammars can't drift.
func (a *app) profilePath(agent, profile string, rest []string) (int, error) {
	if len(rest) == 0 {
		return a.showProfile(agent, profile)
	}
	switch rest[0] {
	case "model":
		switch len(rest) {
		case 1: // read — print the bare mark, pipe-friendly
			if err := a.requireProfile(agent, profile); err != nil {
				return 2, err
			}
			if m := a.cfg.ProfileModelOf(agent, profile); m != "" {
				fmt.Println(m)
			} else {
				ui.Note("no default model marked — set one: coop profiles %s %s model <model>", agent, profile)
			}
			return 0, nil
		case 2: // write (or --clear)
			return a.markProfileModel([]string{agent, profile, rest[1]})
		}
		return 2, fmt.Errorf("usage: coop profiles %s %s model [<model> | --clear]", agent, profile)
	case "default":
		if len(rest) > 1 {
			return 2, fmt.Errorf("unexpected argument %q (usage: coop profiles %s %s default)", rest[1], agent, profile)
		}
		return a.setProfileDefault([]string{agent, profile})
	case "rm", "remove":
		if len(rest) > 1 {
			return 2, fmt.Errorf("unexpected argument %q (usage: coop profiles %s %s rm)", rest[1], agent, profile)
		}
		return a.removeProfile([]string{agent, profile})
	default:
		return 2, unknownErr("profile attribute", rest[0], []string{"model", "default", "rm"})
	}
}

// requireProfile errors (usage-style) when agent has no profile by that name.
func (a *app) requireProfile(agent, profile string) error {
	have := a.cfg.Profiles(agent)
	if slices.Contains(have, profile) {
		return nil
	}
	if len(have) == 0 {
		return fmt.Errorf("%s has no profiles yet — run: coop login %s --profile %s", agent, agent, profile)
	}
	return fmt.Errorf("%s has no profile %q — have: %s", agent, profile, strings.Join(have, ", "))
}

// showProfile prints one profile's detail — the path grammar's read at profile depth.
func (a *app) showProfile(agent, profile string) (int, error) {
	if err := a.requireProfile(agent, profile); err != nil {
		return 2, err
	}
	pal := ui.For(os.Stdout)
	fmt.Println(pal.Bold(agent + " / " + profile))
	label, expired := a.profileState(agent, profile)
	fmt.Printf("  status     %s\n", paintStatus(pal, label))
	def := "no"
	if profile == a.cfg.DefaultProfileOf(agent) {
		def = "yes"
	}
	fmt.Printf("  default    %s\n", def)
	model := a.cfg.ProfileModelOf(agent, profile)
	if model == "" {
		model = ui.Dim("—  (set: coop profiles " + agent + " " + profile + " model <model>)")
	}
	fmt.Printf("  model      %s\n", model)
	fmt.Printf("  dir        %s\n", a.cfg.AgentProfileDir(agent, profile))
	if expired {
		fmt.Printf("  %s\n", ui.Dim("↻ re-login: coop login "+agent+" --profile "+profile))
	}
	return 0, nil
}

// setProfileDefault marks <name> as <agent>'s default profile, so an interactive run with
// no profile given uses it. It rejects an unknown agent or a profile that doesn't exist.
func (a *app) setProfileDefault(args []string) (int, error) {
	if len(args) != 2 {
		return 2, errors.New("usage: coop profiles <agent> <profile> default")
	}
	agent, name := args[0], args[1]
	if _, ok := agents.Get(agent); !ok {
		return 2, unknownErr("agent", agent, agents.Names())
	}
	if err := a.requireProfile(agent, name); err != nil {
		return 2, err
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

// validModelName keeps a model id sane for the models file (one KEY=VALUE line per mark):
// non-empty, no whitespace or control characters (a newline would corrupt the file), and no
// leading '-' (a mistyped flag isn't a model). Deliberately loose otherwise — ids churn.
func validModelName(model string) bool {
	if model == "" || strings.HasPrefix(model, "-") {
		return false
	}
	return !strings.ContainsFunc(model, func(r rune) bool { return r <= ' ' || r == 0x7f })
}

// markProfileModel sets (or, with --clear, removes) a profile's default model — the model
// every run on that profile uses unless --model overrides (see 'coop models' for the menu
// and the precedence). The model id itself is deliberately unvalidated beyond file safety:
// ids churn, and the agent CLI's own error is the gate for a bad one.
func (a *app) markProfileModel(args []string) (int, error) {
	if len(args) != 3 {
		return 2, errors.New("usage: coop profiles <agent> <profile> model <model | --clear>")
	}
	agent, profile, model := args[0], args[1], args[2]
	if _, ok := agents.Get(agent); !ok {
		return 2, unknownErr("agent", agent, agents.Names())
	}
	if model == "--clear" {
		// Clearing skips the profile-existence check, so a stale mark (its profile removed by
		// an older coop, before rm dropped marks) never becomes unremovable.
		if a.cfg.ProfileModelOf(agent, profile) == "" {
			ui.Note("%s profile %q has no default model marked — nothing to clear", agent, profile)
			return 0, nil
		}
		if err := a.cfg.SetProfileModel(agent, profile, ""); err != nil {
			return -1, err
		}
		ui.OK("cleared %s profile %q default model", agent, profile)
		return 0, nil
	}
	if err := a.requireProfile(agent, profile); err != nil {
		return 2, err
	}
	if !validModelName(model) {
		return 2, fmt.Errorf("invalid model name %q — a model id has no spaces and no leading '-' (clear a mark with --clear)", model)
	}
	if err := a.cfg.SetProfileModel(agent, profile, model); err != nil {
		return -1, err
	}
	ui.OK("%s profile %q default model → %s", agent, profile, model)
	return a.cmdProfiles([]string{agent})
}

// removeProfile deletes a stored credential profile's directory — its login token and that
// profile's session history. It refuses to delete the agent's marked default (set another first,
// so a run never lands on a profile that's gone) and never deletes the legacy flat layout's whole
// agent dir. A pool that still names the profile is harmless: buildPool drops members that aren't
// signed in.
func (a *app) removeProfile(args []string) (int, error) {
	if len(args) != 2 {
		return 2, errors.New("usage: coop profiles <agent> <profile> rm")
	}
	agent, name := args[0], args[1]
	if _, ok := agents.Get(agent); !ok {
		return 2, unknownErr("agent", agent, agents.Names())
	}
	if err := a.requireProfile(agent, name); err != nil {
		return 2, err
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
	// Drop the profile's default-model mark with it — best-effort: a stale mark is inert
	// (nothing resolves the missing profile) but would linger confusingly in `coop models`.
	_ = a.cfg.SetProfileModel(agent, name, "")
	ui.OK("removed %s profile %q", agent, name)
	return a.cmdProfiles([]string{agent})
}
