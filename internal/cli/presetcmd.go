package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/ui"
)

// cmdPresets is the presets view, path-grammar like credentials: bare lists every
// preset under .agent/presets/ (a broken one shows its error instead of hiding),
// `coop presets <name>` shows one — the lead and each role with its mode/model/
// credentials/routing. Read-only: presets are edited as YAML files, not via the CLI.
func (a *app) cmdPresets(args []string) (int, error) {
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	if len(args) > 0 && args[0] == "init" {
		return a.presetsInit(repo, args[1:])
	}
	if len(args) > 1 {
		return 2, ui.UnexpectedArgument(args[1], "coop presets", "coop presets [init] [<preset>]")
	}
	if len(args) == 1 {
		if args[0] == "ls" { // rule: `ls` must lead somewhere useful, not read as a preset name
			return 2, &ui.UsageError{
				Headline: `"coop presets" already lists every preset`,
				Cause:    `Run it without "ls".`,
				Rows:     [][2]string{{"Presets:", "coop presets"}},
			}
		}
		return a.showPreset(repo, args[0])
	}
	return a.listPresets(repo)
}

// listPresets compares the presets a run can name: one row each, so the reader can pick between
// them, with the lead/roles relationship said once in front (a table of targets means nothing to
// someone who has not been told what a preset is). A preset that will not load keeps its own block
// with the problem in it — it would fail every run under it, so hiding it helps nobody.
func (a *app) listPresets(repo string) (int, error) {
	globalDir := a.cfg.GlobalPresetsDir()
	names := preset.List(repo, globalDir)
	pal := ui.For(os.Stdout) // stdout view — gate color on stdout so a pipe stays clean
	if len(names) == 0 {
		fmt.Println("No presets yet.")
		fmt.Printf("\n%s\n  %s\n", pal.Bold("Create one"), pal.Cyan("coop presets init"))
		fmt.Printf("\n%s\n  %s\n", pal.Bold("Learn how presets work"), pal.Cyan("coop help presets"))
		return 0, nil
	}
	type row struct{ name, lead, roles, source string }
	var rows []row
	var broken []*preset.LoadError
	for _, name := range names {
		source := "project"
		if preset.Origin(repo, globalDir, name) {
			source = "global"
		}
		p, err := preset.Load(repo, globalDir, name)
		if err != nil {
			var le *preset.LoadError
			if errors.As(err, &le) {
				broken = append(broken, le)
				continue
			}
			return -1, err
		}
		rows = append(rows, row{name, p.Lead().String(), presetRoleCounts(p), source})
	}
	if len(rows) > 0 {
		fmt.Println("Presets let several AI agents work together.")
		fmt.Println("One agent leads the session and can delegate tasks to others.")
		fmt.Println()
		nameW, leadW, rolesW := len("PRESET"), len("FIRST LEAD MODEL"), len("ROLES")
		for _, r := range rows {
			nameW = max(nameW, utf8.RuneCountInString(r.name))
			leadW = max(leadW, utf8.RuneCountInString(r.lead))
			rolesW = max(rolesW, utf8.RuneCountInString(r.roles))
		}
		// Pad the plain header, then bold the finished line — ANSI never counts toward a width.
		fmt.Println(pal.Bold(fmt.Sprintf("%s  %s  %s  %s",
			padRight("PRESET", nameW), padRight("FIRST LEAD MODEL", leadW), padRight("ROLES", rolesW), "SOURCE")))
		for _, r := range rows {
			fmt.Printf("%s  %s  %s  %s\n", padRight(r.name, nameW), padRight(r.lead, leadW), padRight(r.roles, rolesW), r.source)
		}
	} else {
		fmt.Println(pal.Bold("Presets"))
	}
	for _, le := range broken {
		fmt.Printf("\n%s\n", pal.Bold(le.Name))
		fmt.Printf("  %s\n\n", pal.Red("✗ Could not load this preset"))
		fmt.Printf("      %s\n", presetDisplayPath(repo, le.Path))
		for _, line := range le.Detail {
			fmt.Printf("      %s\n", line)
		}
	}
	if len(broken) > 0 {
		fmt.Printf("\n%s\n  %s\n", pal.Bold("Show the problem"), pal.Cyan("coop presets "+broken[0].Name))
	}
	if len(rows) > 0 {
		// The runnable example is a preset that actually loads: offering a broken one would
		// hand the reader a command that fails.
		example := rows[0].name
		fmt.Printf("\n%s\n", pal.Bold("Use a preset:"))
		w := utf8.RuneCountInString("coop presets " + example)
		fmt.Printf("  %s  %s\n", pal.Cyan(padRight("coop "+example, w)), "start the lead agent with its team")
		fmt.Printf("  %s  %s\n", pal.Cyan("coop presets "+example), "show its agents, roles, and prompts")
	}
	fmt.Printf("\n%s\n  %s\n", pal.Bold("Create a preset:"), pal.Cyan("coop presets init"))
	if len(rows) > 0 {
		fmt.Printf("\n%s\n  %s\n", pal.Bold("For more details see:"), pal.Cyan("coop help presets"))
	}
	return 0, nil
}

// presetRoleCounts says what a preset's roles DO, counted by the mode that decides it: a consult
// role advises, a delegate role edits, and a native role runs inside the lead's own session. "none"
// is a real answer here — a single-agent preset is a normal thing to have.
func presetRoleCounts(p *preset.Preset) string {
	var consult, delegate, native int
	for _, r := range p.Roles {
		switch r.Mode {
		case preset.ModeConsult:
			consult++
		case preset.ModeDelegate:
			delegate++
		case preset.ModeNative:
			native++
		}
	}
	var parts []string
	if consult > 0 {
		parts = append(parts, ui.Count(consult, "adviser"))
	}
	if delegate > 0 {
		parts = append(parts, ui.Count(delegate, "editor"))
	}
	if native > 0 {
		parts = append(parts, ui.Count(native, "in-session role"))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

// presetsInit scaffolds a ready-to-edit preset from the documented frontier template
// (`coop presets init [<preset>]`, name defaulting to "frontier"). The template loads
// cleanly as written, so the new preset lists and runs immediately.
func (a *app) presetsInit(repo string, args []string) (int, error) {
	name := "frontier"
	switch {
	case len(args) > 1:
		return 2, ui.UnexpectedArgument(args[1], "coop presets init", "coop presets init [<name>]")
	case len(args) == 1:
		name = args[0]
	}
	path, err := preset.Scaffold(repo, name)
	if err != nil {
		return 2, presetCreateErr(name, err)
	}
	pal := ui.For(os.Stderr)
	ui.OK("Created preset %q", name)
	ui.Note("\nPlease edit the preset template: %s", presetDisplayPath(repo, path))
	ui.Note("\nTo run it, use:\n  %s", pal.Cyan("coop "+name))
	return 0, nil
}

// presetCreateErr refuses a create that could not publish. An existing COMPLETE preset is a
// different answer from an incomplete folder: the first one can be shown, while the second is
// somebody's half-finished work coop must never merge into or repair.
func presetCreateErr(name string, err error) error {
	var exists *preset.ExistsError
	if errors.As(err, &exists) {
		if exists.Complete {
			return &ui.UsageError{
				Headline: fmt.Sprintf("Preset %q already exists", name),
				Cause:    exists.Path,
				Rows: [][2]string{
					{"Show it:", "coop presets " + name},
					{"Help:", "coop help presets"},
				},
			}
		}
		return &ui.UsageError{
			Headline: fmt.Sprintf("Could not create preset %q", name),
			Cause:    exists.Path + " already exists but is incomplete.\nInspect that folder before creating the preset again.",
			Rows:     [][2]string{{"Help:", "coop help presets init"}},
		}
	}
	return &ui.UsageError{
		Headline: fmt.Sprintf("Could not create preset %q", name),
		Cause:    upperFirst(err.Error()) + ".",
		Rows:     [][2]string{{"Help:", "coop help presets init"}},
	}
}

// upperFirst starts a borrowed message (a filesystem error, a loader detail) as the sentence the
// block renders it as, without rewriting what it says.
func upperFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

// showPreset prints one preset's full recipe — the path grammar's read at preset depth.
func (a *app) showPreset(repo, name string) (int, error) {
	p, err := preset.Load(repo, a.cfg.GlobalPresetsDir(), name)
	if err != nil {
		return 2, presetLoadErr(repo, name, err)
	}
	fmt.Print(presetDetail(p, repo, ui.For(os.Stdout)))
	return 0, nil
}

// presetLoadErr refuses a named preset: one that does not exist points at the list, while one that
// exists but will not load shows the file and the loader's own sentences — calling that a typo
// would send its author hunting for the wrong mistake.
func presetLoadErr(repo, name string, err error) error {
	var le *preset.LoadError
	if !errors.As(err, &le) {
		return err
	}
	if le.NotFound {
		return &ui.UsageError{
			Headline: fmt.Sprintf("No preset named %q", name),
			Rows: [][2]string{
				{"Presets:", "coop presets"},
				{"Help:", "coop help presets"},
			},
		}
	}
	cause := append([]string{presetDisplayPath(repo, le.Path)}, le.Detail...)
	return &ui.UsageError{
		Headline: fmt.Sprintf("Could not load preset %q", name),
		Cause:    strings.Join(cause, "\n"),
		Rows:     [][2]string{{"Help:", "coop help presets"}},
	}
}

// presetDisplayPath writes a preset file the way the reader can use it: relative to the project
// inside the repo, ~-shortened for a global one.
func presetDisplayPath(repo, path string) string {
	if rel, err := filepath.Rel(repo, path); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return tildeify(path)
}

// helpForPreset answers `coop help <name>` for a preset, through the same roots and
// repo-over-global precedence a run uses — files only, so it works with no container runtime.
// ok=false means no such preset FILE exists (the caller then reports an unknown command); a
// preset whose YAML is broken returns its validation error instead, because calling the name
// unknown would send its author hunting for a typo instead of the actual mistake.
func helpForPreset(name string, cfg *config.Config) (code int, ok bool) {
	if !preset.ValidName(name) {
		return 0, false
	}
	repo, err := box.ResolveRepo(cfg.RepoOverride)
	if err != nil {
		return 0, false
	}
	globalDir := cfg.GlobalPresetsDir()
	if _, err := os.Stat(preset.Path(repo, globalDir, name)); err != nil {
		return 0, false
	}
	p, err := preset.Load(repo, globalDir, name)
	if err != nil {
		var usage *ui.UsageError
		if errors.As(presetLoadErr(repo, name, err), &usage) {
			ui.PrintUsageError(usage)
		} else {
			ui.Error("%v", err)
		}
		return 2, true
	}
	fmt.Print(presetDetail(p, repo, ui.For(os.Stdout)))
	return 0, true
}

// presetDetail renders a loaded preset for a human: what it is, how to run it, the lead's
// models, one labeled block per role, and the file to edit. It is the ONE projection behind
// both `coop presets <name>` and `coop help <name>`, so the two can never drift.
//
// Every column is measured on plain text and styled afterwards, and every path is written the
// way the reader can use it — project-relative inside the repo, ~-shortened for a global preset
// (see .agent/kb/rules/no-color-in-width-fields.md, entity-blocks-with-labeled-fields.md).
func presetDetail(p *preset.Preset, repo string, pal ui.Palette) string {
	path, global := presetPaths(repo, p.Dir)
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", pal.Bold(p.Name+" — "+presetSummary(p)))

	fmt.Fprintf(&b, "%s\n", pal.Bold("RUN IT"))
	runs := [][2]string{
		{"coop " + p.Name, "start an interactive session with the lead agent"},
		{"coop loop " + p.Name, "work through tasks with this preset"},
		{"coop acp " + p.Name, "use this preset in your editor"},
	}
	w := 0
	for _, r := range runs {
		w = max(w, utf8.RuneCountInString(r[0]))
	}
	for _, r := range runs {
		fmt.Fprintf(&b, "  %s  %s\n", pal.Cyan(padRight(r[0], w)), r[1])
	}

	lead := "LEAD MODEL"
	if len(p.LeadTargets) > 1 { // only a ladder can fall back, so only a ladder says so
		lead = "LEAD MODELS — next selected when the previous is unavailable:"
	}
	fmt.Fprintf(&b, "\n%s\n", pal.Bold(lead))
	targets := make([]string, len(p.LeadTargets))
	for i, t := range p.LeadTargets {
		targets[i] = t.String()
	}
	// A long ladder wraps under its own value: the continuation lines are still the Agent field,
	// not unlabeled model rows someone has to guess the meaning of. Agent and Prompt share one
	// gutter, so their values start at the same column.
	for i, row := range wrapValues(targets, ui.TermWidth(os.Stdout)-leadFieldIndent, 1) {
		label := pal.Dim(padRight("Agent:", leadFieldIndent-3))
		if i > 0 {
			label = strings.Repeat(" ", leadFieldIndent-3)
		}
		fmt.Fprintf(&b, "  %s %s\n", label, strings.Join(row, " "))
	}
	if p.LeadPromptPath != "" {
		fmt.Fprintf(&b, "  %s %s\n", pal.Dim("Prompt:"), path(p.LeadPromptPath))
	}

	if len(p.Roles) > 0 {
		fmt.Fprintf(&b, "\n%s\n", pal.Bold("ROLES AVAILABLE TO THE LEAD"))
		// One gutter for the whole preset: Mode/Agent/When/Prompt start at the same column in
		// every block, so the labels read as a column instead of a ragged edge.
		gutter := 0
		for _, r := range p.Roles {
			if n := utf8.RuneCountInString(r.Name); n > gutter {
				gutter = n
			}
		}
		indent := strings.Repeat(" ", 2+gutter+3)
		for i, r := range p.Roles {
			if i > 0 {
				fmt.Fprintln(&b)
			}
			mode := pal.Dim("Mode:") + " " + r.Mode
			if meaning := presetModeMeaning(r.Mode); meaning != "" {
				mode += pal.Dim(" — " + meaning)
			}
			fmt.Fprintf(&b, "  %s   %s\n", pal.Bold(padRight(r.Name, gutter)), mode)
			roleTargets := make([]string, len(r.Targets))
			for j, t := range r.Targets {
				roleTargets[j] = t.String()
			}
			fmt.Fprintf(&b, "%s%s %s\n", indent, pal.Dim("Agent:"), strings.Join(roleTargets, ", "))
			if r.Subagent != "" {
				fmt.Fprintf(&b, "%s%s %s\n", indent, pal.Dim("Subagent:"), r.Subagent)
			}
			if len(r.When) > 0 {
				fmt.Fprintf(&b, "%s%s %s\n", indent, pal.Dim("When:"), presetWhen(r.When))
			}
			if r.PromptPath != "" {
				fmt.Fprintf(&b, "%s%s %s\n", indent, pal.Dim("Prompt:"), path(r.PromptPath))
			}
		}
	}

	fmt.Fprintf(&b, "\n%s\n  %s", pal.Bold("EDIT THIS PRESET"), path("preset.yaml"))
	if global {
		b.WriteString(" " + pal.Dim("(global)"))
	}
	fmt.Fprintf(&b, "\n\nFor a guide to creating and using presets:\n  coop help presets\n")
	return b.String()
}

// leadFieldIndent is the column the lead's Agent/Prompt VALUES start at — two spaces, the widest
// label ("Prompt:"), one space — so a wrapped ladder measures its width against the room it has.
const leadFieldIndent = 2 + len("Prompt:") + 1

// presetSummary says what this preset is FOR, derived from the targets it actually names —
// never from its name, which its author chose, and never a purpose nobody declared.
func presetSummary(p *preset.Preset) string {
	providers, models := map[string]bool{}, map[string]bool{}
	targets := append([]agents.Target{}, p.LeadTargets...)
	for _, r := range p.Roles {
		targets = append(targets, r.Targets...)
	}
	for _, t := range targets {
		providers[t.Provider] = true
		if t.Model != "" {
			models[t.Model] = true
		}
	}
	switch {
	case len(providers) > 1 && len(models) > 1:
		return "a preset for multiple models and providers to work together"
	case len(providers) > 1:
		return "a preset for multiple providers to work together"
	case len(models) > 1:
		return "a preset for multiple " + titleName(p.Lead().Provider) + " models"
	case len(p.Roles) > 0:
		return "a preset for " + titleName(p.Lead().Provider) + " to work with focused roles"
	default:
		return "a preset for " + titleName(p.Lead().Provider)
	}
}

// presetModeMeaning is the short human meaning printed right on a role's Mode: line — the
// editable YAML value first, then what it does. `coop help presets` carries the complete legend;
// a preset explains only the modes it uses. Delegate's meaning absorbs its two fixed invariants
// (commit: never, concurrent: never) instead of spending a row on each.
func presetModeMeaning(mode string) string {
	switch mode {
	case preset.ModeNative:
		return "runs inside the lead agent’s session"
	case preset.ModeConsult:
		return "read-only advice"
	case preset.ModeDelegate:
		return "edits files; never commits; runs one at a time"
	}
	return ""
}

// presetWhen reads a role's routing hints as a phrase: the YAML tokens with their dashes
// relaxed, joined the way a person would say them. The words stay the author's.
func presetWhen(when []string) string {
	hints := make([]string, len(when))
	for i, w := range when {
		hints[i] = strings.ReplaceAll(w, "-", " ")
	}
	if len(hints) == 2 {
		return hints[0] + " and " + hints[1]
	}
	return strings.Join(hints, ", ")
}

// presetPaths renders paths inside a preset folder the way the reader can use them: relative to
// the repo for a project preset, ~-shortened for a global one (global=true, which the caller
// labels, so an origin outside the checkout is never mistaken for a file in it).
func presetPaths(repo, dir string) (path func(rel string) string, global bool) {
	base := tildeify(dir)
	global = true
	if rel, err := filepath.Rel(repo, dir); err == nil && !strings.HasPrefix(rel, "..") {
		base, global = filepath.ToSlash(rel), false
	}
	return func(rel string) string {
		if rel == "" {
			return base
		}
		return base + "/" + filepath.ToSlash(rel)
	}, global
}
