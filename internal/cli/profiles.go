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
// is just an account — the model rides a launch target or preset, never a
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
	pal := ui.For(os.Stdout) // stdout view — gate color on stdout so a pipe stays clean
	blocks := make([]accountBlock, 0, len(names))
	for _, agent := range names {
		blocks = append(blocks, a.accountBlock(agent))
	}
	// One width per column across EVERY block, so `default` starts at the same visible column
	// down the whole listing. Pad the plain strings, then style — never color inside a width.
	var profileW, factW int
	for _, block := range blocks {
		for _, row := range block.rows {
			profileW = max(profileW, len([]rune(row.profile)))
			factW = max(factW, len([]rune(row.fact)))
		}
	}
	fmt.Println(pal.Bold("Accounts available to Coop"))
	for _, block := range blocks {
		fmt.Println()
		fmt.Println(pal.Bold(block.title))
		if len(block.rows) == 0 {
			fmt.Printf("  no accounts — run: coop login %s[@<account>]\n", block.agent)
			continue
		}
		for _, row := range block.rows {
			// The leading accent makes the try-first account scannable; the explicit `default`
			// in the fixed third column explains the accent without a legend. Neither is a
			// success mark, so neither is green.
			mark := "  "
			tag := ""
			if row.isDefault {
				mark, tag = pal.Cyan("*")+" ", pal.Dim("default")
			}
			fact := padRight(row.fact, factW)
			if row.issue {
				fact = pal.Yellow(fact)
			}
			line := fmt.Sprintf("  %s%s   %s   %s", mark, padRight(row.profile, profileW), fact, tag)
			fmt.Println(strings.TrimRight(line, " ")) // a row with no tag ends at its last word
			if row.remedy != "" {
				fmt.Printf("      %s\n", pal.Dim(row.remedy))
			}
		}
		// Surface a dangling default: the marked (or built-in) default points at an account that
		// doesn't exist, so an interactive run would land on nothing. Don't leave it silent.
		if block.missingDefault != "" {
			fmt.Printf("  %s\n", pal.Yellow(fmt.Sprintf("default account %s is gone", block.missingDefault)))
			fmt.Printf("      %s\n", pal.Dim(fmt.Sprintf("coop credentials %s <account> default", block.agent)))
		}
	}
	return 0, nil
}

// accountBlock is one agent's accounts as the listing renders them. Building every block before
// printing is what lets the columns be measured once across all of them.
type accountBlock struct {
	agent, title   string
	rows           []accountRow
	missingDefault string // the marked default, when no account of that name exists
}

// accountRow is one stored account: what a person needs to know about it, and — only when
// something is wrong — the exact command that fixes it.
type accountRow struct {
	profile   string
	fact      string // "refreshed 7 hours ago", or the problem in the same column
	issue     bool
	remedy    string
	isDefault bool
}

// accountBlock reads one agent's accounts. Being listed here already says an account is usable,
// so a healthy row carries no status word: it says when its token material last changed, which is
// the one fact a person cannot see for themselves.
func (a *app) accountBlock(agent string) accountBlock {
	block := accountBlock{agent: agent, title: displayAgentName(agent)}
	profiles := box.EffectiveProfiles(a.cfg, agent)
	def := a.cfg.DefaultProfileOf(agent)
	// The marked default first, then the rest — the same order a loop's rotation fans out over
	// accounts (accountsFor), so this listing reflects the try order.
	if slices.Contains(profiles, def) {
		ordered := []string{def}
		for _, p := range profiles {
			if p != def {
				ordered = append(ordered, p)
			}
		}
		profiles = ordered
	} else if len(profiles) > 0 {
		block.missingDefault = def
	}
	for _, p := range profiles {
		row := accountRow{profile: p, isDefault: p == def}
		switch label, needsLogin := a.profileState(agent, p); {
		case needsLogin:
			row.fact, row.issue, row.remedy = "re-login required", true, "coop login "+agent+"@"+p
		case label == "not signed in":
			row.fact, row.issue, row.remedy = "not signed in", true, "coop login "+agent+"@"+p
		default:
			row.fact = "refreshed " + a.credentialAge(agent, p)
		}
		block.rows = append(block.rows, row)
	}
	return block
}

// displayAgentName title-cases an agent's own name for a block heading. Not DisplayName(), which
// is the product ("Claude Code"): the heading names the accounts' agent as the command spells it.
func displayAgentName(agent string) string {
	if agent == "" {
		return agent
	}
	return strings.ToUpper(agent[:1]) + agent[1:]
}

// profileState reports a profile's short sign-in label and whether it needs a re-login. Presence is
// enough for opaque stores and env keys; adapters that can inspect their native OAuth marker reject
// malformed, stripped, or expired credentials while accepting refreshable ones.
func (a *app) profileState(agent, p string) (label string, needsLogin bool) {
	if !box.ProfileAuthed(a.cfg, agent, p) {
		return "not signed in", false
	}
	if !box.ProfileCredentialReady(a.cfg, agent, p, time.Now()) {
		return "re-login required", true
	}
	return "signed in", false
}

// credentialAge renders how long ago agent's profile token material last changed ("19 days ago"),
// or "—" when that's unknowable — an env-key login with no marker file, or a missing/unreadable
// one. mtime is the honest proxy: a refresh or a fresh login rewrites the material and retires the
// old token, which is exactly what this clock should read. See box.ProfileTokenMtime.
func (a *app) credentialAge(agent, profile string) string {
	if t, ok := box.ProfileTokenMtime(a.cfg, agent, profile); ok {
		return humanAge(t)
	}
	return "—"
}

// humanAge is a duration a person reads without decoding it — "7 hours ago", "yesterday",
// "19 days ago" — for facts an operator judges by feel rather than by arithmetic. Shared with the
// model menu, which says the same way how old a list it could not refresh is.
func humanAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < 2*time.Minute:
		return "a minute ago"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 2*time.Hour:
		return "an hour ago"
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	case d < 48*time.Hour:
		return "yesterday"
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
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
	fmt.Println(pal.Bold(displayAgentName(agent) + " / " + profile))
	// The same vocabulary the listing uses, so narrowing the command never contradicts it: a
	// healthy account says when it was refreshed, a broken one says what is wrong and how to fix it.
	label, needsLogin := a.profileState(agent, profile)
	switch {
	case needsLogin:
		fmt.Printf("  %s\n", pal.Yellow("re-login required"))
	case label == "not signed in":
		fmt.Printf("  %s\n", pal.Yellow("not signed in"))
	default:
		fmt.Printf("  refreshed  %s\n", a.credentialAge(agent, profile))
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
	if label == "not signed in" || needsLogin {
		fmt.Printf("  %s\n", pal.Dim("coop login "+agent+"@"+profile))
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

// removeProfile deletes a stored credential's login token and session history. It refuses to
// delete the agent's marked default (set another first, so a run never lands on a credential
// that's gone). A preset ladder that still names the account is harmless: expandLadder skips a
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
	if err := ui.DestroyGate(fmt.Sprintf("delete %s credential %q (login token + session history)", agent, name), yes); err != nil {
		return 2, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return -1, err
	}
	ui.OK("removed %s credential %q", agent, name)
	return a.cmdCredentials([]string{agent})
}
