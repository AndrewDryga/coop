package cli

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/ui"
)

// cmdCredentials drives the credentials family with a resource-path grammar — each token
// narrows: `coop credentials` lists every agent, `coop credentials claude` one agent,
// `coop credentials claude personal` one account, and a trailing attribute reads or writes
// one property of it: `default` (mark it the agent's default) or `rm` (delete it). An account
// is one stored login — the model rides a launch target or preset, never a
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
			return 2, &ui.UsageError{
				Headline: `"coop credentials" already lists every account`,
				Cause:    `Run it without "ls".`,
				Rows:     [][2]string{{"Accounts:", "coop credentials"}},
			}
		}
	}
	names := agents.Names()
	if len(args) > 0 {
		if _, ok := agents.Get(args[0]); !ok {
			return 2, unknownAgentErr(args[0], "coop credentials")
		}
		names = []string{args[0]}
	}
	pal := ui.For(os.Stdout) // stdout view — gate color on stdout so a pipe stays clean
	blocks := make([]accountBlock, 0, len(names))
	for _, agent := range names {
		blocks = append(blocks, a.accountBlock(agent))
	}
	// One width per column across EVERY block, so `default` starts at the same visible column
	// down the whole listing. The warning marker is part of the fact it leads, so it is measured
	// with it. Pad the plain strings, then style — never color inside a width.
	var profileW, factW int
	for _, block := range blocks {
		for _, row := range block.rows {
			profileW = max(profileW, utf8.RuneCountInString(row.profile))
			factW = max(factW, utf8.RuneCountInString(row.fact))
		}
	}
	fmt.Println(pal.Bold("Accounts available to Coop"))
	for _, block := range blocks {
		fmt.Println()
		fmt.Println(pal.Bold(block.title))
		if len(block.rows) == 0 {
			fmt.Printf("  No accounts. Sign in: %s\n", pal.Cyan("coop login "+block.agent))
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
			line := fmt.Sprintf("  %s%s   %s  %s", mark, padRight(row.profile, profileW), fact, tag)
			fmt.Println(strings.TrimRight(line, " ")) // a row with no tag ends at its last word
			if row.remedy != "" {
				// Aligned under the warning's TEXT, not under its glyph, so the remedy reads as
				// that row's own continuation.
				fmt.Printf("%s%s\n", strings.Repeat(" ", 4+profileW+3+2), pal.Cyan(row.remedy))
			}
		}
		// Surface a dangling default: the marked (or built-in) default points at an account that
		// doesn't exist, so an interactive run would land on nothing. Don't leave it silent.
		if block.missingDefault != "" {
			fmt.Println()
			fmt.Printf("  %s\n", pal.Yellow(fmt.Sprintf("⚠ Default account %q is missing", block.missingDefault)))
			fmt.Println()
			fmt.Printf("      Choose a default: %s\n", pal.Cyan(fmt.Sprintf("coop credentials %s %s default", block.agent, block.rows[0].profile)))
		}
	}
	return 0, nil
}

// unknownAgentErr refuses a token in an agent slot — the shared rejected-value block, correcting a
// near miss against the registered agents and otherwise naming the ones that exist.
func unknownAgentErr(token, command string) error {
	suggestion := ""
	if guess, ok := nearestCommand(token, agents.Names()); ok {
		suggestion = command + " " + guess
	}
	return ui.UnknownValue("agent", agents.DisplayTarget(token), command,
		"Choose "+ui.List(agents.Names(), "or")+".", suggestion)
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
	block := accountBlock{agent: agent, title: titleName(agent)}
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
		switch issue := a.profileIssue(agent, p); {
		case issue != "":
			row.fact, row.issue, row.remedy = "⚠ "+issue, true, agents.LoginCommand(agent+"@"+p)
		case a.envBackedAccount(agent, p):
			// An env key has no token file to date; saying when it was "refreshed" would be
			// inventing an mtime, so name the authority instead.
			row.fact = "environment file"
		default:
			row.fact = "refreshed " + a.credentialAge(agent, p)
		}
		block.rows = append(block.rows, row)
	}
	return block
}

// profileIssue reports what is wrong with a stored account, as the short sentence the listing and
// the detail view both print — "" when nothing is. Presence is enough for opaque stores and env
// keys; adapters that can inspect their native OAuth marker reject malformed, stripped, or expired
// credentials while accepting refreshable ones, so a renewable expiry is never called unusable.
func (a *app) profileIssue(agent, p string) string {
	switch {
	case !box.ProfileAuthed(a.cfg, agent, p):
		return "Not signed in"
	case !box.ProfileCredentialReady(a.cfg, agent, p, time.Now()):
		return "Sign in again"
	}
	return ""
}

// envBackedAccount reports whether this account's authority is the env file rather than a stored
// login: it is signed in, but has no marker file of its own.
func (a *app) envBackedAccount(agent, profile string) bool {
	return box.ProfileAuthed(a.cfg, agent, profile) && !box.ProfileMarkerPresent(a.cfg, agent, profile)
}

// credentialAge renders how long ago agent's profile token material last changed ("19 days ago"),
// or "—" when that's unknowable — a missing or unreadable marker. mtime is the honest proxy: a
// refresh or a fresh login rewrites the material and retires the old token, which is exactly what
// this clock should read. See box.ProfileTokenMtime.
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

// profilePath routes the path grammar's per-profile tail: bare shows the account, an
// attribute token reads or writes one property. Attribute handlers delegate to the same
// functions the verb forms use, so the two grammars can't drift.
func (a *app) profilePath(agent, profile string, rest []string) (int, error) {
	if len(rest) == 0 {
		return a.showProfile(agent, profile)
	}
	switch rest[0] {
	case "default":
		if len(rest) > 1 {
			return 2, ui.UnexpectedArgument(rest[1], "coop credentials", fmt.Sprintf("coop credentials %s %s default", agent, profile))
		}
		return a.setProfileDefault(agent, profile)
	case "rm":
		for _, x := range rest[1:] { // only --yes may follow rm here; anything else is a mistake
			if x != "-y" && x != "--yes" {
				return 2, ui.UnexpectedArgument(x, "coop credentials", fmt.Sprintf("coop credentials %s %s rm [--yes]", agent, profile))
			}
		}
		return a.removeProfile(agent, profile, hasYes(rest))
	default:
		suggestion := ""
		if guess, ok := nearestCommand(rest[0], []string{"default", "rm"}); ok {
			suggestion = fmt.Sprintf("coop credentials %s %s %s", agent, profile, guess)
		}
		return 2, ui.UnknownValue("action", rest[0], "coop credentials",
			"Choose default or rm.", suggestion)
	}
}

// requireProfile refuses an account this agent has no login for, with the two commands that
// resolve it — the agent's own list, and signing this one in.
func (a *app) requireProfile(agent, profile string) error {
	if slices.Contains(box.EffectiveProfiles(a.cfg, agent), profile) {
		return nil
	}
	return noAccountErr(agent, profile)
}

// showProfile answers the one question this depth is asked: which account a run will use, and the
// commands that act on it. No storage path, token age or "default: yes" ledger — those say
// nothing a person can act on (see .agent/kb/rules/entity-blocks-with-labeled-fields.md).
func (a *app) showProfile(agent, profile string) (int, error) {
	if err := a.requireProfile(agent, profile); err != nil {
		return 2, err
	}
	pal := ui.For(os.Stdout) // stdout view — gate color on stdout so a pipe stays clean
	name := titleName(agent)
	isDefault := profile == a.cfg.DefaultProfileOf(agent)
	env := a.envBackedAccount(agent, profile)
	issue := a.profileIssue(agent, profile)
	login := agents.LoginCommand(agent + "@" + profile)

	switch {
	case issue != "":
		// A broken account cannot answer "which account will be used", so it leads with the
		// problem and the exact login that fixes it.
		fmt.Println(pal.Yellow("⚠ " + issue))
		fmt.Println()
		fmt.Println(pal.Bold(signInLabel(issue)))
		fmt.Printf("  %s\n\n", pal.Cyan(login))
	case env:
		fmt.Printf("%s uses this account's configured environment key.\n\n", name)
	case isDefault:
		fmt.Printf("%s uses %s by default.\n\n", name, profile)
	}

	fmt.Println(pal.Bold("Use this account:"))
	fmt.Printf("  %s\n", pal.Cyan("coop "+agent+"@"+profile))
	if !isDefault {
		fmt.Printf("\n%s\n", pal.Bold("Use it by default:"))
		fmt.Printf("  %s\n", pal.Cyan(fmt.Sprintf("coop credentials %s %s default", agent, profile)))
	}
	// An env-backed account is refreshed by editing the env file, so offering `coop login` for it
	// would name the wrong mechanism; a broken one already printed its login above.
	if issue == "" && !env {
		fmt.Printf("\n%s\n", pal.Bold("Sign in again:"))
		fmt.Printf("  %s\n", pal.Cyan(login))
	}
	if isDefault {
		fmt.Printf("\n%s\n", pal.Bold("Manage accounts:"))
		fmt.Printf("  %s\n", pal.Cyan("coop credentials "+agent))
	}
	return 0, nil
}

// signInLabel keeps the detail view's action heading in the listing's vocabulary: an account that
// never had a login says "Sign in", one whose stored login went bad says "Sign in again".
func signInLabel(issue string) string {
	if issue == "Not signed in" {
		return "Sign in:"
	}
	return "Sign in again:"
}

// setProfileDefault marks <account> as <agent>'s default, so a run with no @account uses it. It
// rejects an unknown agent or an account that doesn't exist, and reports the choice alone — the
// full overview after a one-account change is noise.
func (a *app) setProfileDefault(agent, name string) (int, error) {
	if _, ok := agents.Get(agent); !ok {
		return 2, unknownAgentErr(agent, "coop credentials")
	}
	if err := a.requireProfile(agent, name); err != nil {
		return 2, err
	}
	if a.cfg.DefaultProfileOf(agent) == name {
		ui.Note("%s already uses %s by default.", titleName(agent), name)
		return 0, nil
	}
	if err := a.cfg.SetDefaultProfile(agent, name); err != nil {
		return -1, err
	}
	ui.OK("%s will use %s by default", titleName(agent), name)
	if issue := a.profileIssue(agent, name); issue != "" {
		warnRows(fmt.Sprintf("%s is %s", name, strings.ToLower(issue)),
			[2]string{signInLabel(issue), agents.LoginCommand(agent + "@" + name)})
	}
	return 0, nil
}

// warnRows prints a heads-up in the shared refusal's COLUMN shape — a leading blank line, the ⚠
// headline, then two-space label rows whose commands start past the widest label. It is NOT
// rejected input (that is ui.UsageError, which exits 2), and it is not warnBlock (headline,
// reason, actions): this is for a warning whose body is label→command pairs a person scans.
func warnRows(headline string, rows ...[2]string) {
	ui.Note("")
	ui.Warn("%s", headline)
	if len(rows) == 0 {
		return
	}
	w := 0
	for _, r := range rows {
		w = max(w, utf8.RuneCountInString(r[0]))
	}
	ui.Note("")
	for _, r := range rows {
		ui.Note("  %s %s", padRight(r[0], w), r[1])
	}
}

// removeProfile deletes a stored account's login token and session history. It refuses to
// delete the agent's marked default (set another first, so a run never lands on an account
// that's gone). A preset ladder that still names the account is harmless: expandLadder skips a
// target that isn't signed in.
func (a *app) removeProfile(agent, name string, yes bool) (int, error) {
	if _, ok := agents.Get(agent); !ok {
		return 2, unknownAgentErr(agent, "coop credentials")
	}
	if err := a.requireProfile(agent, name); err != nil {
		return 2, err
	}
	title := titleName(agent)
	if name == a.cfg.DefaultProfileOf(agent) {
		return 2, &ui.UsageError{
			Headline: fmt.Sprintf("Cannot remove %s account %q", title, name),
			Cause:    "It is the default account. Choose another default first.",
			Rows: [][2]string{
				{"Accounts:", "coop credentials " + agent},
				{"Help:", "coop help credentials default"},
			},
		}
	}
	dir := a.cfg.AgentProfileDir(agent, name)
	// Deleting an account drops its login token AND all session history, with no undo — preview
	// exactly that in future tense, then let the shared gate ask.
	ui.Note("Permanently delete %s account %q along with its saved login\nand local session history?\n", title, name)
	if err := ui.DestroyGate("Continue", yes); err != nil {
		if errors.Is(err, ui.ErrNeedsConfirmation) {
			return 2, ui.ConfirmationRequired("coop credentials rm")
		}
		removalCancelled()
		return 2, ui.ErrReported
	}
	if !yes {
		ui.Note("") // the Enter that answered the prompt ended its line; keep the result a paragraph
	}
	if err := os.RemoveAll(dir); err != nil {
		// A partial delete is durable: never claim nothing was removed unless that is proved.
		return -1, fmt.Errorf("could not finish removing %s account %q: %w", title, name, err)
	}
	ui.OK("Deleted %s account %q", title, name)
	return 0, nil
}

// removalCancelled is coop's whole say after a declined deletion: an answer, not a failure, so it
// reports what did NOT happen instead of a red ✗.
func removalCancelled() { ui.Note("\nCancelled. Nothing was removed.") }
