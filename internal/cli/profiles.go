package cli

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/ui"
)

// cmdCredentials drives the credentials family with a resource-path grammar — each token
// narrows: `coop credentials` lists every agent, `coop credentials claude` one agent,
// `coop credentials claude personal` one credential, and a trailing attribute reads or writes
// one property of it: `default` (mark it the agent's default) or `rm` (delete it). A credential
// is just an account — the model is a separate axis (set it with --model or a preset), never a
// property here. So marking the default reads as a path, not a verb sandwich:
//
//	coop credentials claude personal default
//
// A leading verb (`default|rm|model …`) reads as an unknown agent now — the path grammar above is
// the only spelling for an edit.
func (a *app) cmdCredentials(args []string) (int, error) {
	if len(args) > 0 {
		if _, ok := agents.Get(args[0]); ok && len(args) > 1 {
			return a.profilePath(args[0], args[1], args[2:])
		}
		switch args[0] {
		case "ls":
			// Bare `coop credentials` already lists — steer `ls` there instead of "unknown agent" (rule:
			// `ls` is the list verb, so it must lead somewhere useful, not read as an agent filter).
			return 2, fmt.Errorf("coop credentials already lists every credential — just run `coop credentials` (no %q)", args[0])
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
	// NAME · STATUS · (default). The status stays a short label — the re-login remedy for an
	// expired token gets its own dim line under the block instead of blowing the row sideways.
	var allProfiles []string
	for _, agent := range names {
		allProfiles = append(allProfiles, box.EffectiveProfiles(a.cfg, agent)...)
	}
	pal := ui.For(os.Stdout) // stdout view — gate color on stdout so a pipe stays clean (p is the profile loop var below)
	width := colWidth(allProfiles, 0, 40)
	statusW := len("re-login required") // the widest short status label
	first := true
	for _, agent := range names {
		if !first {
			fmt.Println() // a blank line between agents, so each block scans on its own
		}
		first = false
		fmt.Println(pal.Bold(agent))
		profiles := box.EffectiveProfiles(a.cfg, agent)
		if len(profiles) == 0 {
			fmt.Printf("  no credentials — run: coop login %s[@<account>]\n", agent)
			continue
		}
		def := a.cfg.DefaultProfileOf(agent)
		// List the marked default first, then the rest — the same order a loop's rotation fans
		// out over accounts (accountsFor), so "see coop credentials" reflects the try order.
		if slices.Contains(profiles, def) {
			ordered := []string{def}
			for _, p := range profiles {
				if p != def {
					ordered = append(ordered, p)
				}
			}
			profiles = ordered
		}
		var relogin []string
		for _, p := range profiles {
			label, needsLogin := a.profileState(agent, p)
			if needsLogin {
				relogin = append(relogin, p)
			}
			tag := ""
			if p == def {
				tag = pal.Dim("  (default)")
			}
			// How stale the token material is — the rotation clock behind a blast-radius fix. Only
			// for a signed-in credential (a "not signed in" one has nothing to rotate). Dim, after the
			// padded status so the column lines up across profiles.
			rot := ""
			if label != "not signed in" {
				rot = pal.Dim("  rotated " + a.credentialAge(agent, p))
			}
			// Pad the plain strings (rune-aware), then style — never color inside a width.
			fmt.Printf("  %s  %s%s%s\n", padRight(p, width), paintStatus(pal, padRight(label, statusW)), rot, tag)
		}
		for _, p := range relogin {
			fmt.Printf("  %s\n", pal.Dim("↻ re-login: coop login "+agent+"@"+p))
		}
		// Surface a dangling default: the marked (or built-in) default points at a profile that
		// doesn't exist, so an interactive run would land on nothing. Don't leave it silent.
		if !slices.Contains(profiles, def) {
			fmt.Printf("  %s\n", pal.Dim(fmt.Sprintf("default → %s (missing — set one: coop credentials %s <name> default)", def, agent)))
		}
	}
	return 0, nil
}

// profileState reports a profile's short sign-in label and whether it needs a re-login. Presence is
// enough for opaque stores and env keys; adapters that can inspect their native OAuth marker reject
// malformed, stripped, or expired credentials while accepting refreshable ones.
func (a *app) profileState(agent, p string) (label string, needsLogin bool) {
	if !box.ProfileAuthed(a.cfg, agent, p) {
		return "not signed in", false
	}
	ag, ok := agents.Get(agent)
	if ok && box.ProfileMarkerPresent(a.cfg, agent, p) &&
		ag.StoredCredentialStatus(a.cfg.AgentProfileDir(agent, p), time.Now()) == agents.StoredCredentialReauthRequired {
		return "re-login required", true
	}
	return "signed in", false
}

// credentialAge renders how long ago agent's profile token material last changed ("3d ago"), or
// "—" when that's unknowable — an env-key login with no marker file, or a missing/unreadable one.
// mtime is the honest proxy: a refresh or a fresh login rewrites the material and retires the old
// token, which is exactly what the rotation clock should read. See box.ProfileTokenMtime.
func (a *app) credentialAge(agent, profile string) string {
	if t, ok := box.ProfileTokenMtime(a.cfg, agent, profile); ok {
		return agoStr(t)
	}
	return "—"
}

// paintStatus colors a profileState label (possibly padded): green signed in, yellow when a
// re-login is required, dim otherwise.
func paintStatus(pal ui.Palette, label string) string {
	switch strings.TrimSpace(label) {
	case "signed in":
		return pal.Green(label)
	case "re-login required":
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
	case "default":
		if len(rest) > 1 {
			return 2, fmt.Errorf("unexpected argument %q (usage: coop credentials %s %s default)", rest[1], agent, profile)
		}
		return a.setProfileDefault([]string{agent, profile})
	case "rm":
		for _, x := range rest[1:] { // only --yes may follow rm here; anything else is a mistake
			if x != "-y" && x != "--yes" {
				return 2, fmt.Errorf("unexpected argument %q (usage: coop credentials %s %s rm [--yes])", x, agent, profile)
			}
		}
		return a.removeProfile(append([]string{agent, profile}, rest[1:]...))
	default:
		return 2, unknownErr("credential attribute", rest[0], []string{"default", "rm"})
	}
}

// requireProfile errors (usage-style) when agent has no profile by that name.
func (a *app) requireProfile(agent, profile string) error {
	have := a.cfg.Profiles(agent)
	if slices.Contains(have, profile) {
		return nil
	}
	if len(have) == 0 {
		return fmt.Errorf("%s has no credentials yet — run: coop login %s@%s", agent, agent, profile)
	}
	return fmt.Errorf("%s has no credential %q — have: %s", agent, profile, strings.Join(have, ", "))
}

// showProfile prints one profile's detail — the path grammar's read at profile depth.
func (a *app) showProfile(agent, profile string) (int, error) {
	if !slices.Contains(box.EffectiveProfiles(a.cfg, agent), profile) {
		if err := a.requireProfile(agent, profile); err != nil {
			return 2, err
		}
	}
	pal := ui.For(os.Stdout)
	fmt.Println(pal.Bold(agent + " / " + profile))
	label, needsLogin := a.profileState(agent, profile)
	fmt.Printf("  status     %s\n", paintStatus(pal, label))
	if label != "not signed in" {
		fmt.Printf("  rotated    %s\n", a.credentialAge(agent, profile))
	}
	def := "no"
	if profile == a.cfg.DefaultProfileOf(agent) {
		def = "yes"
	}
	fmt.Printf("  default    %s\n", def)
	if box.ProfileMarkerPresent(a.cfg, agent, profile) || !box.ProfileAuthed(a.cfg, agent, profile) {
		fmt.Printf("  dir        %s\n", a.cfg.AgentProfileDir(agent, profile))
	} else {
		fmt.Println("  source     env file")
	}
	if needsLogin {
		fmt.Printf("  %s\n", ui.Dim("↻ re-login: coop login "+agent+"@"+profile))
	}
	return 0, nil
}

// setProfileDefault marks <name> as <agent>'s default profile, so an interactive run with
// no profile given uses it. It rejects an unknown agent or a profile that doesn't exist.
func (a *app) setProfileDefault(args []string) (int, error) {
	if len(args) != 2 {
		return 2, errors.New("usage: coop credentials <agent> <credential> default")
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
		ui.Warn("%s account %q isn't signed in yet — run: coop login %s@%s", agent, name, agent, name)
	}
	ui.OK("%s default credential → %s", agent, name)
	return a.cmdCredentials([]string{agent})
}

// removeProfile deletes a stored credential profile's directory — its login token and that
// profile's session history. It refuses to delete the agent's marked default (set another first,
// so a run never lands on a profile that's gone) and never deletes the legacy flat layout's whole
// agent dir. A preset ladder that still names the account is harmless: expandLadder skips a
// target that isn't signed in.
func (a *app) removeProfile(args []string) (int, error) {
	yes := hasYes(args)
	var pos []string
	for _, x := range args {
		if !strings.HasPrefix(x, "-") {
			pos = append(pos, x)
		}
	}
	if len(pos) != 2 {
		return 2, errors.New("usage: coop credentials <agent> <credential> rm [--yes]")
	}
	agent, name := pos[0], pos[1]
	if _, ok := agents.Get(agent); !ok {
		return 2, unknownErr("agent", agent, agents.Names())
	}
	if err := a.requireProfile(agent, name); err != nil {
		return 2, err
	}
	if name == a.cfg.DefaultProfileOf(agent) {
		return 2, fmt.Errorf("%s credential %q is the default — set another first: coop credentials %s <other> default", agent, name, agent)
	}
	dir := a.cfg.AgentProfileDir(agent, name)
	// Deleting a profile drops its login token AND all session history, with no undo — gate it.
	if err := destroyGate(fmt.Sprintf("delete %s credential %q (login token + session history)", agent, name), yes); err != nil {
		return 2, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return -1, err
	}
	ui.OK("removed %s credential %q", agent, name)
	return a.cmdCredentials([]string{agent})
}
